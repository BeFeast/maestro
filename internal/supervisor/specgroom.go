package supervisor

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/specgroom"
	"github.com/befeast/maestro/internal/state"
)

// maxSpecGroomIssuesPerCycle bounds how many issues one supervisor cycle will
// examine for spec-lint/grooming (comment fetch + LLM pass). It caps both the
// GitHub API calls and the backend spend per cycle; issues past the cap are
// picked up on the next cycle. Lint is already once-per-body-change, so a
// backlog drains within a few cycles and then stays quiet.
const maxSpecGroomIssuesPerCycle = 30

// issueCommentLister is the optional reader capability the spec-groom step needs
// to poll `@maestro groom` mentions. *github.Client satisfies it; a reader that
// does not implement it simply disables mention-triggered grooming (lint still
// runs). Mirrors the prCIStatusReader / prMergeableReader optional-interface
// pattern the engine already uses.
type issueCommentLister interface {
	ListIssueComments(issueNumber int) ([]github.IssueComment, error)
}

// runSpecGroom lints ready-candidate issues and answers `@maestro groom`
// mentions (#851). It is a no-op unless supervisor.spec_groom.enabled is set.
// One LLM pass per issue produces both the lint verdict and (when failing or
// when grooming was requested) the rewrite proposal. All side effects go
// through safe/cautious surfaces: a single lint checklist comment, a groom
// proposal comment, and an approval-gated edit_issue_body mint. It records
// per-issue body hashes so an unchanged/passing issue generates zero churn.
//
// Called from RunOnce inside the !DryRun guard, so its state mutations are
// persisted by the cycle's single state.Save.
func (e *Engine) runSpecGroom(st *state.State, mutator Mutator) {
	if e == nil || e.cfg == nil || st == nil || !e.cfg.Supervisor.SpecGroomOn() {
		return
	}
	// Respect the metered-backend refusal (#838): the lint pass dispatches to
	// the same supervisor backend, so a per-token model without opt-in must not
	// silently burn on grooming either.
	if backend, refused := e.meteredBackendRefused(); refused {
		log.Printf("[supervisor] spec-groom: skipped; supervisor backend %q is metered and allow_metered_backend is not set", backend)
		return
	}
	if mutator == nil {
		// No safe write surface — cannot post comments; nothing to do.
		return
	}
	llm := e.llm
	if llm == nil {
		llm = NewBackendLLMClient(e.cfg)
	}

	issues, err := e.reader.ListOpenIssues(nil)
	if err != nil {
		log.Printf("[supervisor] spec-groom: list open issues: %v", err)
		return
	}
	commentsReader, hasComments := e.reader.(issueCommentLister)
	readyLabel := e.readyLabel()
	excluded := e.excludeLabels()
	now := e.now()

	// examined bounds how many issues get a comment fetch + (maybe) an LLM pass
	// this cycle, capping GitHub and backend cost. truncated counts the
	// non-excluded issues deferred to the next cycle so the cap is never silent.
	examined := 0
	truncated := 0
	for _, issue := range issues {
		if len(excluded) > 0 && github.HasLabel(issue, excluded) {
			continue
		}
		if examined >= maxSpecGroomIssuesPerCycle {
			truncated++
			continue
		}
		examined++

		isReady := readyLabel != "" && github.HasLabel(issue, []string{readyLabel})

		bodyHash := specgroom.BodyHash(issue.Body)
		// Passive lint targets ready-candidate issues only (unlabeled), so a
		// well-formed maestro-ready issue is never re-linted → zero churn.
		needLint := !isReady && !st.IssueLintedForBody(issue.Number, bodyHash)

		mention, haveMention := e.newGroomMention(st, issue, commentsReader, hasComments)

		if !needLint && !haveMention {
			continue
		}

		verdict, err := specgroom.Evaluate(llm, toSpecgroomIssue(issue), haveMention)
		if err != nil {
			log.Printf("[supervisor] spec-groom: evaluate issue #%d: %v", issue.Number, err)
			continue
		}

		if needLint {
			e.applyLintVerdict(st, mutator, issue.Number, bodyHash, verdict, now)
		}
		if haveMention {
			e.applyGroomProposal(st, mutator, issue.Number, mention, verdict, now)
		}
	}
	if truncated > 0 {
		log.Printf("[supervisor] spec-groom: examined the first %d issues this cycle; %d more will be examined next cycle", maxSpecGroomIssuesPerCycle, truncated)
	}
}

// newGroomMention returns the newest unhandled `@maestro groom` mention on an
// issue, if any. Returns (_, false) when the reader cannot list comments, the
// fetch fails, or the latest mention was already handled.
func (e *Engine) newGroomMention(st *state.State, issue github.Issue, reader issueCommentLister, hasComments bool) (specgroom.Comment, bool) {
	if !hasComments {
		return specgroom.Comment{}, false
	}
	comments, err := reader.ListIssueComments(issue.Number)
	if err != nil {
		log.Printf("[supervisor] spec-groom: list comments for issue #%d: %v", issue.Number, err)
		return specgroom.Comment{}, false
	}
	mention, ok := specgroom.DetectGroomMention(toSpecgroomComments(comments))
	if !ok || st.GroomMentionHandled(issue.Number, mention.ID) {
		return specgroom.Comment{}, false
	}
	return mention, true
}

// applyLintVerdict records the lint result and, on failure, posts the single
// checklist comment. The lint mark is recorded only after a successful comment
// post (on failure) so a transient GitHub error retries next cycle instead of
// silently swallowing the checklist.
func (e *Engine) applyLintVerdict(st *state.State, mutator Mutator, issueNumber int, bodyHash string, verdict specgroom.Verdict, now time.Time) {
	if verdict.Pass {
		st.RecordSpecLint(issueNumber, bodyHash, true, now)
		return
	}
	comment := RedactSensitive(specgroom.RenderLintComment(verdict))
	if err := mutator.CommentIssue(issueNumber, comment); err != nil {
		log.Printf("[supervisor] spec-groom: post lint comment on issue #%d: %v", issueNumber, err)
		return
	}
	st.RecordSpecLint(issueNumber, bodyHash, false, now)
	log.Printf("[supervisor] spec-groom: posted spec-lint checklist on issue #%d", issueNumber)
}

// applyGroomProposal posts the rewrite proposal comment and mints the
// approval-gated edit_issue_body verb carrying the rewrite. The mention is
// marked handled only after the proposal is posted, so a transient error
// retries next cycle. When the model returned no rewrite the mention is marked
// handled without a proposal (nothing to apply).
func (e *Engine) applyGroomProposal(st *state.State, mutator Mutator, issueNumber int, mention specgroom.Comment, verdict specgroom.Verdict, now time.Time) {
	proposal := specgroom.RenderGroomComment(verdict)
	if proposal == "" {
		st.MarkGroomMentionHandled(issueNumber, mention.ID, now)
		return
	}
	if err := mutator.CommentIssue(issueNumber, RedactSensitive(proposal)); err != nil {
		log.Printf("[supervisor] spec-groom: post groom proposal on issue #%d: %v", issueNumber, err)
		return
	}
	summary := fmt.Sprintf("Apply groomed spec rewrite to issue #%d", issueNumber)
	evidence := []string{
		"Proposed by the spec-groom agent in response to an @maestro groom mention.",
		"Approve to replace the issue body with the proposed rewrite; reject to leave it untouched.",
	}
	st.RecordEditIssueBodyApproval(issueNumber, verdict.RewrittenBody, summary, e.cfg.Repo, e.cfg.Repo, evidence, now)
	st.MarkGroomMentionHandled(issueNumber, mention.ID, now)
	log.Printf("[supervisor] spec-groom: posted groom proposal + minted edit_issue_body approval for issue #%d", issueNumber)
}

// specLintAllowsReady reports whether the ready label may be applied to an
// issue under the require_lint_pass gate (#851). Default (gate off) always
// allows. When on, the label is withheld until spec-lint has PASSED for the
// issue's current body — default-closed so a failing or not-yet-linted issue
// cannot be promoted. Withholding is logged so an operator sees why.
func (e *Engine) specLintAllowsReady(st *state.State, issue github.Issue) bool {
	if e == nil || e.cfg == nil || !e.cfg.Supervisor.SpecGroomRequireLintPass() {
		return true
	}
	if st == nil {
		return false
	}
	bodyHash := specgroom.BodyHash(issue.Body)
	if st.SpecLintPassedForBody(issue.Number, bodyHash) {
		return true
	}
	log.Printf("[supervisor] spec-groom: withholding %s from issue #%d — require_lint_pass is set and spec-lint has not passed for the current body", MutationAddReadyLabel, issue.Number)
	return false
}

func toSpecgroomIssue(issue github.Issue) specgroom.Issue {
	labels := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		if name := strings.TrimSpace(l.Name); name != "" {
			labels = append(labels, name)
		}
	}
	return specgroom.Issue{
		Number: issue.Number,
		Title:  issue.Title,
		Body:   issue.Body,
		Labels: labels,
	}
}

func toSpecgroomComments(comments []github.IssueComment) []specgroom.Comment {
	out := make([]specgroom.Comment, 0, len(comments))
	for _, c := range comments {
		out = append(out, specgroom.Comment{ID: c.ID, Body: c.Body, Author: c.Author})
	}
	return out
}
