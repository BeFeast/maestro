// Package review is the Go llm-review producer (#1162 S4): it runs the
// configured model lenses over a PR head diff and publishes, per lens, inline
// findings plus a commit status `llm-review-<lens>` — the exact two surfaces
// the daemon's review gate reads (internal/github llmReviewStreamSpec).
//
// It is a transliteration of scripts/llm-review.sh with the #1148/#1152
// hardening preserved:
//
//   - pending-first: every lens's status settles (pending or skipped-error)
//     before any model output or comment lands, so the gate never observes
//     the half-state "comments exist but a stream has no status";
//   - per-head idempotency: a lens whose status already settled
//     (success/failure) on the current head is skipped; a fresh pending means
//     another run is mid-flight; a stale pending is a crashed run and retries;
//   - fail-closed: output matching neither findings nor NO_FINDINGS posts an
//     error status — a refusal or format drift never reads as a clean review;
//   - no-creds→error: a configured lens without credentials posts an explicit
//     error status instead of leaving the stream silently unobserved.
//
// All forge traffic goes through forge.Client, so GitHub and Forgejo are
// interchangeable here. Model traffic policy lives in the Lens
// implementations (CLIProxy-only for API lenses; see lens_chat.go).
package review

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/forge"
)

// Lens runs one review model. Implementations: ChatLens (any OpenAI-compatible
// endpoint — for the fleet that means CLIProxy), CursorLens (cursor-agent).
type Lens interface {
	// Name is the stream name and status context, e.g. "llm-review-opus".
	Name() string
	// Available reports whether the lens can run: nil when its credentials
	// are configured, an error describing what is missing otherwise.
	Available() error
	// Run executes the model over the prompt and returns its raw text output.
	Run(ctx context.Context, prompt string) (string, error)
}

// Producer publishes reviews for one repository through one forge client.
type Producer struct {
	Forge forge.Client
	Repo  string
	// Lenses are the streams to produce. The caller (daemon trigger, S5) maps
	// the project's effective review-gate streams here — the two sets MUST
	// match or the gate degrades on an unproduced stream.
	Lenses []Lens

	// PendingStaleAfter is the age past which a pending status is treated as
	// a crashed run and retried. Zero means the bash default, 30 minutes.
	PendingStaleAfter time.Duration
	// MaxDiffBytes caps the diff fed to the models. Zero means the bash
	// default, 400000. The cap is surfaced in the status description when it
	// truncates — a verdict over a partial diff must be operator-visible.
	MaxDiffBytes int
	// Now is the clock (tests). Nil means time.Now.
	Now func() time.Time
	// Logf reports progress (journal). Nil means log.Printf.
	Logf func(format string, args ...any)
}

const (
	defaultPendingStaleAfter = 30 * time.Minute
	defaultMaxDiffBytes      = 400000
)

// promptTemplate is the shared review prompt, byte-identical to the bash
// glue's REVIEW_PROMPT_TEMPLATE so verdicts stay comparable across the
// producers during the cutover. %s is the PR title.
const promptTemplate = `You are a strict senior code reviewer. Review ONLY the diff below (PR "%s").
Report genuine defects, not style preferences.

Output format — one line per finding, nothing else:
[P0] path/to/file.ext:LINE — one-sentence description
[P1] path/to/file.ext:LINE — one-sentence description
[P2] path/to/file.ext:LINE — one-sentence description
[P3] path/to/file.ext:LINE — one-sentence description

Severity contract:
- P0: guaranteed breakage, data loss, security hole. Blocks merge.
- P1: real bug or correctness risk on a main path. Blocks merge.
- P2: minor defect or risky pattern. Advisory only.
- P3: nitpick. Advisory only.

LINE must be a new-file line number that appears in the diff. If there are no
findings, output exactly: NO_FINDINGS. Do not output anything else — no
preamble, no summary, no markdown fences.

DIFF:
`

func (p *Producer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Producer) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
		return
	}
	log.Printf("[review] "+format, args...)
}

func (p *Producer) pendingStaleAfter() time.Duration {
	if p.PendingStaleAfter > 0 {
		return p.PendingStaleAfter
	}
	return defaultPendingStaleAfter
}

func (p *Producer) maxDiffBytes() int {
	if p.MaxDiffBytes > 0 {
		return p.MaxDiffBytes
	}
	return defaultMaxDiffBytes
}

// ProducePR runs every configured lens over the PR's current head. A settled
// or in-flight lens is a clean skip. The returned error aggregates per-lens
// failures; idempotent skips never contribute to it.
func (p *Producer) ProducePR(ctx context.Context, prNumber int) error {
	pr, err := p.Forge.GetPR(ctx, p.Repo, prNumber)
	if err != nil {
		return fmt.Errorf("review %s#%d: %w", p.Repo, prNumber, err)
	}
	diff, err := p.Forge.GetPRDiff(ctx, p.Repo, prNumber)
	if err != nil {
		return fmt.Errorf("review %s#%d: %w", p.Repo, prNumber, err)
	}
	if len(strings.TrimSpace(string(diff))) == 0 {
		p.logf("%s#%d: empty diff — nothing to review", p.Repo, prNumber)
		return nil
	}
	truncNote := ""
	if len(diff) > p.maxDiffBytes() {
		p.logf("%s#%d: diff is %d bytes; truncating to %d", p.Repo, prNumber, len(diff), p.maxDiffBytes())
		diff = diff[:p.maxDiffBytes()]
		// Surfaced in the final status description: a verdict over a
		// truncated diff is weaker evidence and the operator must see that.
		truncNote = fmt.Sprintf("; warning: diff truncated to %d bytes", p.maxDiffBytes())
	}

	statuses, err := p.Forge.CommitStatuses(ctx, p.Repo, pr.HeadSHA)
	if err != nil {
		return fmt.Errorf("review %s#%d: statuses on %s: %w", p.Repo, prNumber, pr.HeadSHA, err)
	}

	// Phase 1: settle every lens's status (pending / skipped-error) before
	// any model runs or any comment is posted.
	var errs []error
	var runnable []Lens
	for _, lens := range p.Lenses {
		switch p.prepare(ctx, lens, pr.HeadSHA, statuses) {
		case prepareRun:
			runnable = append(runnable, lens)
		case prepareSkip:
		case prepareCredsMissing:
			// Error status already posted; the run itself still fails so the
			// operator sees a degraded producer pass.
			errs = append(errs, fmt.Errorf("%s: credentials not configured", lens.Name()))
		case prepareAbort:
			// Pending post failed: pretending phase 1 happened would hide the
			// protocol violation. Abort this lens without running the model.
			errs = append(errs, fmt.Errorf("%s: could not post pending status", lens.Name()))
		}
	}

	// Phase 2: run the reviews and flip each pending to its final state.
	prompt := fmt.Sprintf(promptTemplate, pr.Title)
	for _, lens := range runnable {
		if err := p.runLens(ctx, lens, pr, prompt+string(diff), truncNote); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", lens.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("review %s#%d: %w", p.Repo, prNumber, errors.Join(errs...))
	}
	return nil
}

type prepareDecision int

const (
	prepareRun prepareDecision = iota
	prepareSkip
	prepareCredsMissing
	prepareAbort
)

// prepare is phase 1 for one lens: the settled/stale idempotency decision and
// the pending-first status publication, mirroring the bash prepare_model.
func (p *Producer) prepare(ctx context.Context, lens Lens, headSHA string, statuses []forge.Status) prepareDecision {
	if settled, why := p.statusSettled(lens.Name(), statuses); settled {
		p.logf("%s needs no run on %s — %s", lens.Name(), headSHA, why)
		return prepareSkip
	}
	if err := lens.Available(); err != nil {
		p.logf("%s: %v — skipping with error status", lens.Name(), err)
		// An explicit error status instead of silence: with one stream
		// settled and the other absent, the gate's aggregate used to sit at
		// Pending+Observed forever with no escape (#1148 round 1, P1-2).
		if postErr := p.Forge.CreateCommitStatus(ctx, p.Repo, headSHA, forge.Status{
			Context:     lens.Name(),
			State:       forge.StatusError,
			Description: "skipped: credentials not configured",
		}); postErr != nil {
			p.logf("%s: failed to post creds-missing status: %v", lens.Name(), postErr)
		}
		return prepareCredsMissing
	}
	if err := p.Forge.CreateCommitStatus(ctx, p.Repo, headSHA, forge.Status{
		Context:     lens.Name(),
		State:       forge.StatusPending,
		Description: "review in progress",
	}); err != nil {
		p.logf("%s: failed to post pending on %s: %v", lens.Name(), headSHA, err)
		return prepareAbort
	}
	return prepareRun
}

// statusSettled decides whether this lens needs no run on the head: a settled
// state (success/failure — the idempotency key) or a fresh pending (another
// run is mid-flight). Retryable instead: error statuses, stale pendings (the
// posting run died after phase 1 — the crashed-pending self-heal), and
// pendings with no usable age (zero CreatedAt: assume crashed, not wedged).
// The forge's combined status is latest-per-context, so the first match wins.
func (p *Producer) statusSettled(context string, statuses []forge.Status) (bool, string) {
	for _, st := range statuses {
		if st.Context != context {
			continue
		}
		switch st.State {
		case forge.StatusSuccess, forge.StatusFailure:
			return true, "already settled (idempotent)"
		case forge.StatusPending:
			if st.CreatedAt.IsZero() {
				return false, ""
			}
			if p.now().Sub(st.CreatedAt) < p.pendingStaleAfter() {
				return true, fmt.Sprintf("pending since %s — another run appears to be in flight", st.CreatedAt.Format(time.RFC3339))
			}
			return false, ""
		default:
			return false, ""
		}
		// Latest-per-context: the first matching entry is the verdict.
	}
	return false, ""
}

// findingLine matches one contract line: severity, a path:line anchor token,
// and the message. The separator is optional — bash's anchor extraction
// (`[^ ]+:[0-9]+`) never required the dash, so a model that drifts to
// "[P1] foo.go:12 fix the check" still gets an anchored inline comment; the
// gate's blocking-findings collectors only read inline comments, so demoting
// such a finding to a plain comment would drop it from the repair scope.
var findingLine = regexp.MustCompile(`^\[(P[0-3])\] +(\S+:\d+)(?:(?: *[—–-]+ *| +)(.*))?$`)

// looseFindingLine catches a contract-shaped severity line whose anchor part
// did not parse (missing line number, spaces in the path). The finding is
// kept — posted as a plain comment — instead of being dropped or, worse,
// failing the whole output as unparseable.
var looseFindingLine = regexp.MustCompile(`^\[(P[0-3])\] +(.*)$`)

var noFindings = regexp.MustCompile(`(?m)^NO_FINDINGS[ \t]*$`)

// Finding is one parsed review finding.
type Finding struct {
	Severity string
	Path     string
	Line     int
	Message  string
}

// Blocking reports whether the finding blocks merge (P0/P1).
func (f Finding) Blocking() bool { return f.Severity == "P0" || f.Severity == "P1" }

// parseOutput ports the bash parser: strip markdown fences, leading
// indentation, and CRLF carriage returns (bash's `[[:space:]]` matched \r, so
// a CRLF-emitting model must not fail closed here), keep contract lines. ok
// is false when the output matched neither findings nor NO_FINDINGS — the
// fail-closed case.
func parseOutput(output string) (findings []Finding, ok bool) {
	var cleaned []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimRight(strings.TrimLeft(line, " \t"), "\r")
		if strings.HasPrefix(trimmed, "```") {
			continue
		}
		cleaned = append(cleaned, trimmed)
	}
	for _, line := range cleaned {
		if m := findingLine.FindStringSubmatch(line); m != nil {
			// The anchor token splits like bash: path up to the FIRST colon,
			// line after the LAST — "foo.go:42:13" anchors at (foo.go, 13).
			token := m[2]
			path := token[:strings.Index(token, ":")]
			lineNum, _ := strconv.Atoi(token[strings.LastIndex(token, ":")+1:])
			msg := m[3]
			if msg == "" {
				msg = token
			}
			findings = append(findings, Finding{Severity: m[1], Path: path, Line: lineNum, Message: msg})
			continue
		}
		if m := looseFindingLine.FindStringSubmatch(line); m != nil {
			findings = append(findings, Finding{Severity: m[1], Message: m[2]})
		}
	}
	if len(findings) > 0 {
		return findings, true
	}
	if noFindings.MatchString(strings.Join(cleaned, "\n")) {
		return nil, true
	}
	return nil, false
}

// runLens is phase 2 for one lens: run the model, parse fail-closed, post the
// findings and flip the pending status to its final state.
func (p *Producer) runLens(ctx context.Context, lens Lens, pr forge.PR, prompt, truncNote string) error {
	output, err := lens.Run(ctx, prompt)
	if err != nil {
		p.logf("%s run failed: %v", lens.Name(), err)
		p.postStatus(ctx, lens.Name(), pr.HeadSHA, forge.StatusError, "review run failed")
		return fmt.Errorf("run: %w", err)
	}
	findings, ok := parseOutput(output)
	if !ok {
		// Fail closed (#1148 round 1, P1-4): a refusal, an apology, or a
		// format drift must never read as "clean review".
		p.logf("%s output matched neither findings nor NO_FINDINGS: %.500s", lens.Name(), output)
		p.postStatus(ctx, lens.Name(), pr.HeadSHA, forge.StatusError, "review output unparseable")
		return fmt.Errorf("unparseable output")
	}

	blocking := 0
	for _, f := range findings {
		if f.Blocking() {
			blocking++
		}
		p.postFinding(ctx, lens.Name(), pr, f)
	}
	if blocking > 0 {
		return p.postFinalStatus(ctx, lens.Name(), pr.HeadSHA, forge.StatusFailure,
			fmt.Sprintf("%d blocking (P0/P1) of %d findings%s", blocking, len(findings), truncNote))
	}
	return p.postFinalStatus(ctx, lens.Name(), pr.HeadSHA, forge.StatusSuccess,
		fmt.Sprintf("%d advisory findings, none blocking%s", len(findings), truncNote))
}

// postFinding publishes one finding: inline when it carries a usable anchor,
// falling back to a plain PR comment when the forge rejects the position
// (line outside the diff, renamed file) — the finding must stay visible
// either way. The [Px] marker is what the gate's severity parser matches; the
// trailing stream marker attributes the comment to this stream.
func (p *Producer) postFinding(ctx context.Context, stream string, pr forge.PR, f Finding) {
	shortSHA := pr.HeadSHA
	if len(shortSHA) > 12 {
		shortSHA = shortSHA[:12]
	}
	body := fmt.Sprintf("[%s] %s\n\n<sub>%s @ %s</sub>", f.Severity, f.Message, stream, shortSHA)
	if f.Path != "" && f.Line > 0 {
		if err := p.Forge.CreateReviewComment(ctx, p.Repo, pr.Number, pr.HeadSHA, f.Path, f.Line, body); err == nil {
			return
		}
	}
	fallback := fmt.Sprintf("%s (at `%s:%d`)", body, f.Path, f.Line)
	if err := p.Forge.CreateComment(ctx, p.Repo, pr.Number, fallback); err != nil {
		// Comment loss is tolerated (bash parity: `|| true`) — the blocking
		// signal is the status, and the finding is preserved in the journal.
		p.logf("%s: failed to post finding comment on #%d: %v", stream, pr.Number, err)
	}
}

// postStatus posts a non-final (error) state, logging a failed post — there
// is nothing better to do with it mid-failure.
func (p *Producer) postStatus(ctx context.Context, context, sha string, state forge.StatusState, desc string) {
	if err := p.Forge.CreateCommitStatus(ctx, p.Repo, sha, forge.Status{
		Context:     context,
		State:       state,
		Description: desc,
	}); err != nil {
		p.logf("%s: failed to post status %s on %s: %v", context, state, sha, err)
	}
}

// postFinalStatus posts the lens's verdict. A failed post is a hard error: a
// swallowed failure here is what used to let a transient forge error read as
// a green no-op run.
func (p *Producer) postFinalStatus(ctx context.Context, context, sha string, state forge.StatusState, desc string) error {
	if err := p.Forge.CreateCommitStatus(ctx, p.Repo, sha, forge.Status{
		Context:     context,
		State:       state,
		Description: desc,
	}); err != nil {
		return fmt.Errorf("post final status %s: %w", state, err)
	}
	p.logf("%s: status %s on %s", context, state, sha)
	return nil
}
