package orchestrator

// Opt-in visual-evidence verify for UI-affecting PRs (#705).
//
// When verify.visual is configured, workers are instructed (via the prompt)
// to capture screenshots and attach them to the PR before declaring done.
// The merge flow then runs a one-shot post-implementation check per session:
// classify the PR by its changed files against the configured globs, and —
// when it is UI-affecting but carries no attached image evidence — run the
// capture command in the session worktree as a best-effort diagnostic, post
// ONE warning comment on the PR, and stamp the session so the supervisor
// records a visual_evidence_missing finding. The check is advisory in v1:
// it never blocks or delays merge.

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
)

func (o *Orchestrator) prChangedFiles(prNumber int) ([]string, error) {
	if o.ghPRChangedFilesFn != nil {
		return o.ghPRChangedFilesFn(prNumber)
	}
	if o.gh == nil {
		return nil, fmt.Errorf("no github client configured for changed files")
	}
	return o.gh.PRChangedFiles(prNumber)
}

func (o *Orchestrator) prVisualEvidenceAttached(prNumber int) (bool, error) {
	if o.ghPRVisualEvidenceAttachedFn != nil {
		return o.ghPRVisualEvidenceAttachedFn(prNumber)
	}
	if o.gh == nil {
		return false, fmt.Errorf("no github client configured for visual evidence")
	}
	return o.gh.PRVisualEvidenceAttached(prNumber)
}

// ensureVisualEvidence runs the one-shot verify.visual check for a session's
// open PR. Transient GitHub read errors leave the session unstamped so the
// next cycle retries; every terminal outcome stamps sess.VisualEvidence so
// the check (and its warning comment) never repeats for this session.
func (o *Orchestrator) ensureVisualEvidence(slotName string, sess *state.Session, pr github.PR) {
	if o == nil || o.cfg == nil || sess == nil {
		return
	}
	visual := o.cfg.Verify.Visual
	if !visual.Active() {
		return
	}
	if sess.VisualEvidence != "" {
		return // one-shot per session
	}

	files, err := o.prChangedFiles(pr.Number)
	if err != nil {
		log.Printf("[orch] verify.visual: changed-files lookup for PR #%d failed (will retry next cycle): %v", pr.Number, err)
		return
	}
	matched := pipeline.MatchUIAffectingFiles(visual.Paths, files)
	if len(matched) == 0 {
		sess.VisualEvidence = state.VisualEvidenceNotRequired
		return
	}

	attached, err := o.prVisualEvidenceAttached(pr.Number)
	if err != nil {
		log.Printf("[orch] verify.visual: evidence lookup for PR #%d failed (will retry next cycle): %v", pr.Number, err)
		return
	}
	if attached {
		sess.VisualEvidence = state.VisualEvidenceAttached
		log.Printf("[orch] verify.visual: PR #%d (%s) is UI-affecting and has visual evidence attached", pr.Number, slotName)
		return
	}

	// Post-implementation verify: the worker declared done without attaching
	// evidence. Run the capture command in the session worktree so the warning
	// can say whether the harness itself works.
	detail := o.runVisualCaptureFallback(sess, visual)
	sess.VisualEvidence = state.VisualEvidenceMissing
	sess.VisualEvidenceDetail = detail
	log.Printf("[orch] verify.visual: PR #%d (%s) is UI-affecting with no visual evidence — %s", pr.Number, slotName, detail)

	if err := o.commentPR(pr.Number, visualEvidenceWarningComment(matched, detail)); err != nil {
		log.Printf("[orch] verify.visual: warning comment on PR #%d failed: %v", pr.Number, err)
	}
	o.notifier.Sendf("⚠️ maestro: PR #%d (issue #%d: %s) touches UI paths but has no visual evidence attached — %s",
		pr.Number, sess.IssueNumber, sess.IssueTitle, detail)
}

// runVisualCaptureFallback executes the configured capture command in the
// session worktree and describes the outcome in one operator-facing sentence.
// Best-effort: every failure mode degrades to a description, never an error.
func (o *Orchestrator) runVisualCaptureFallback(sess *state.Session, visual config.VerifyVisualConfig) string {
	runCapture := pipeline.RunVisualCapture
	if o.runVisualCaptureFn != nil {
		runCapture = o.runVisualCaptureFn
	}

	worktree := strings.TrimSpace(sess.Worktree)
	if worktree == "" {
		return "no worktree is recorded for the session, so the capture command could not be run"
	}
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		return fmt.Sprintf("worktree %s is gone, so the capture command could not be run", worktree)
	}

	shots, err := runCapture(visual, worktree)
	if err != nil {
		return fmt.Sprintf("capture command failed: %v", err)
	}
	if len(shots) == 0 {
		return fmt.Sprintf("capture command succeeded but produced no screenshots in %s", visual.ResolvedOutputDir())
	}
	return fmt.Sprintf("capture command produced %d screenshot(s) in %s on the worker worktree, but none are attached to the PR", len(shots), visual.ResolvedOutputDir())
}

// visualEvidenceWarningComment renders the single advisory comment posted on
// a UI-affecting PR that lacks attached screenshots.
func visualEvidenceWarningComment(matchedFiles []string, detail string) string {
	const maxListed = 5
	listed := matchedFiles
	more := ""
	if len(listed) > maxListed {
		more = fmt.Sprintf("\n- … and %d more", len(listed)-maxListed)
		listed = listed[:maxListed]
	}
	var b strings.Builder
	b.WriteString("⚠️ **maestro verify.visual:** this PR changes UI paths but no visual evidence (screenshots) is attached.\n\n")
	b.WriteString("UI-affecting files:\n")
	for _, f := range listed {
		fmt.Fprintf(&b, "- `%s`\n", f)
	}
	b.WriteString(more)
	fmt.Fprintf(&b, "\nCapture status: %s.\n\n", detail)
	b.WriteString("This does not block merge (v1) — please verify the rendered UI manually, or run the project's `verify.visual` capture command and attach the screenshots to this PR.")
	return b.String()
}
