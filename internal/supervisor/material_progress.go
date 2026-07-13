package supervisor

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
)

// recordMaterialProgress evaluates the durable per-project stalled-progress
// watchdog once and persists the advanced watermark + decision onto st (#887).
//
// It is called from RunOnce just before the LastRunOnceAt heartbeat, inside the
// non-dry-run guard, so the watermark is saved by the same state.Save. It only
// records the durable watermark and the recovery decision; the actual recovery
// actuation (stop the single stale worker / surface delivery reconciliation)
// stays gated behind live canary evidence (issue #887 is completion type
// runtime-live). Recording the watermark is the safe, durable half: it survives
// daemon restart and truthfully feeds the Fleet watchdog view.
func recordMaterialProgress(cfg *config.Config, st *state.State, now time.Time) progress.Decision {
	if cfg == nil || st == nil {
		return progress.Decision{}
	}
	now = now.UTC()
	budget := cfg.StalledProgressWatchdog.EffectiveMaxSilence()
	interval := cfg.StalledProgressWatchdog.EffectiveEvalInterval()
	// Respect the watchdog's own evaluation cadence (#887): the supervisor loop
	// may cycle faster (or slower) than the configured watchdog interval, so we
	// only re-evaluate once at least one interval has elapsed since the last
	// evaluation. Between evaluations the durable watermark and the reported
	// last decision are unchanged, so skipping is truthful — and it keeps the
	// bounded worktree probe below off the hot path of a fast supervisor.
	if mp := st.MaterialProgress; mp != nil && !mp.LastEvaluatedAt.IsZero() &&
		now.Sub(mp.LastEvaluatedAt.UTC()) < interval {
		if mp.LastDecision != nil {
			return *mp.LastDecision
		}
		return progress.Decision{}
	}
	observed, phase := collectMaterialProgressSignals(st, now)
	dec := st.RecordMaterialProgress(observed, phase, budget, interval, now)
	if dec.Acted() {
		// Reporting only — the recovery is surfaced through the durable
		// MaterialProgress.LastRecovery field and the Fleet watchdog view.
		log.Printf("[supervisor/watchdog] stalled-progress %s: %s (phase=%s, deadline=%s)",
			dec.Action, dec.Reason, dec.Phase, dec.Deadline.Format(time.RFC3339))
	}
	return dec
}

// collectMaterialProgressSignals derives the phase-appropriate material-progress
// signal set for the project from in-memory state plus a bounded worktree probe
// (newest source mtime + git index/HEAD identity for live workers; no shelling
// out to git). Evaluation is gated to the watchdog cadence by the caller, so the
// per-tick cost stays modest even for a fast supervisor loop.
//
// Each signal is a non-reversible digest built from stable identity fields, so
// no raw path, secret, or command output is persisted. A signal is absent (and
// thus not evidence of a stall) when its inputs are empty. The combined
// watermark advances whenever ANY signal advances — a single stale signal is
// never treated as proof of a stall.
func collectMaterialProgressSignals(st *state.State, now time.Time) (progress.SignalSet, progress.Phase) {
	slots := make([]string, 0, len(st.Sessions))
	for slot := range st.Sessions {
		slots = append(slots, slot)
	}
	sort.Strings(slots)

	var (
		issueSession []string
		processTmux  []string
		terminal     []string
		worktreeGit  []string
		prReview     []string
		anyLive      bool
	)
	for _, slot := range slots {
		sess := st.Sessions[slot]
		if sess == nil {
			continue
		}
		live := sess.Status == state.StatusRunning || sess.Status == state.StatusPROpen
		if live {
			anyLive = true
		}
		// issue/session state + lease identity.
		issueSession = append(issueSession, fmt.Sprintf("%s#%d=%s;retry=%d;maint=%d",
			slot, sess.IssueNumber, sess.Status, sess.RetryCount, sess.MaintenanceRetryCount))
		// process + exact tmux/session identity (live sessions only — a dead
		// pid is not progress evidence).
		if live {
			processTmux = append(processTmux, fmt.Sprintf("%s=pid%d:%s", slot, sess.PID, sess.TmuxSession))
		}
		// terminal output or checkpoint advancement.
		if h := strings.TrimSpace(sess.LastOutputHash); h != "" || sess.CheckpointFile != "" {
			terminal = append(terminal, fmt.Sprintf("%s=%s:%s", slot, h, sess.CheckpointFile))
		}
		// bounded worktree/branch + PR-head proxy. Branch + PR number alone left
		// active file edits invisible: a quiet worker editing files for the whole
		// silence budget without emitting terminal output, committing, or opening
		// a PR kept every fingerprint frozen and tripped a false stall. For a live
		// worker we fold in a bounded worktree probe (newest non-volatile source
		// mtime + git index/HEAD identity), so an actively-editing worker keeps
		// advancing the watermark (#887). The probe is off the hot path — it only
		// runs on the watchdog cadence and only for live sessions.
		wtEvidence := ""
		if live {
			wtEvidence = worktreeProgressFingerprint(sess.Worktree)
		}
		if b := strings.TrimSpace(sess.Branch); b != "" || sess.PRNumber != 0 || wtEvidence != "" {
			worktreeGit = append(worktreeGit, fmt.Sprintf("%s=%s:pr%d:wt%s", slot, b, sess.PRNumber, wtEvidence))
		}
		// PR head, CI/check/review, merge/release identity.
		if sess.PRNumber != 0 || sess.ReviewPendingHeadSHA != "" || sess.LastNotifiedStatus != "" {
			prReview = append(prReview, fmt.Sprintf("%s=pr%d:%s:%s:vis=%s",
				slot, sess.PRNumber, sess.ReviewPendingHeadSHA, sess.LastNotifiedStatus, sess.VisualEvidence))
		}
	}

	// delivery approval generation, execution lease, and terminal receipt.
	var delivery []string
	executing := st.ListExecutingDeliveries()
	for _, a := range executing {
		if a == nil {
			continue
		}
		gen := 0
		if a.Delivery != nil {
			gen = a.Delivery.ApprovalGeneration
		}
		delivery = append(delivery, fmt.Sprintf("%s=%s:gen%d:%s", a.ID, a.Status, gen, a.UpdatedAt.UTC().Format(time.RFC3339)))
	}
	sort.Strings(delivery)
	if !st.LastMergeAt.IsZero() {
		prReview = append(prReview, "last_merge="+st.LastMergeAt.UTC().Format(time.RFC3339))
	}

	observedAt := now.UTC()
	set := progress.SignalSet{
		{Kind: progress.SignalIssueSession, Fingerprint: progress.Fingerprint(issueSession...), ObservedAt: observedAt},
		{Kind: progress.SignalProcessTmux, Fingerprint: progress.Fingerprint(processTmux...), ObservedAt: observedAt},
		{Kind: progress.SignalTerminalCheckpoint, Fingerprint: progress.Fingerprint(terminal...), ObservedAt: observedAt},
		{Kind: progress.SignalWorktreeGit, Fingerprint: progress.Fingerprint(worktreeGit...), ObservedAt: observedAt},
		{Kind: progress.SignalPRReview, Fingerprint: progress.Fingerprint(prReview...), ObservedAt: observedAt},
		{Kind: progress.SignalDelivery, Fingerprint: progress.Fingerprint(delivery...), ObservedAt: observedAt},
	}

	// An executing/uncertain delivery lease crosses the replay boundary: a stall
	// here must go to operator reconciliation, never an automatic retry (#872).
	phase := progress.PhasePreDelivery
	if len(executing) > 0 {
		phase = progress.PhaseDeliveryExecuting
	} else if !anyLive {
		// No live worker to retry; a stall is an operator-facing wait, not a
		// pre-delivery worker recovery.
		phase = progress.PhaseDeliveryPending
	}
	return set, phase
}

// worktreeVolatileDirs are directory names excluded from the bounded worktree
// progress probe: they churn (or are regenerated) independently of the worker's
// material progress, so counting their mtimes would both mask a real stall and
// add cost. `.git` is skipped in the walk and read explicitly for its
// index/HEAD identity instead.
var worktreeVolatileDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	"target":        true,
	"dist":          true,
	"build":         true,
	".next":         true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
	"coverage":      true,
	".gradle":       true,
	".terraform":    true,
	".idea":         true,
	".vscode":       true,
}

// worktreeProgressMaxEntries bounds the worktree walk so the probe stays cheap
// on the watchdog cadence even for a large repository.
const worktreeProgressMaxEntries = 20000

// worktreeProgressFingerprint returns bounded worktree evidence for the
// worktree_git signal: the newest non-volatile source-file mtime plus the git
// index/HEAD identity. This makes an actively-editing-but-quiet worker (no new
// terminal output, no commit, no PR) visibly progress, so the watchdog does not
// record a false stall against ongoing filesystem work (#887).
//
// It reads only mtimes and the git index mtime / HEAD ref content — never file
// contents — so no source, secret, or path survives past the non-reversible
// digest. Any error (missing/unreadable worktree) yields an empty string, which
// marks the signal absent rather than fabricating a stall. The walk skips known
// volatile/generated directories and is capped at worktreeProgressMaxEntries.
func worktreeProgressFingerprint(worktree string) string {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return ""
	}
	info, err := os.Stat(worktree)
	if err != nil || !info.IsDir() {
		return ""
	}

	var (
		newest  time.Time
		scanned int
	)
	_ = filepath.WalkDir(worktree, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries; never fail the probe
		}
		if scanned >= worktreeProgressMaxEntries {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if path != worktree && worktreeVolatileDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		scanned++
		if fi, e := d.Info(); e == nil {
			if mt := fi.ModTime(); mt.After(newest) {
				newest = mt
			}
		}
		return nil
	})

	parts := make([]string, 0, 3)
	if !newest.IsZero() {
		parts = append(parts, "mtime="+newest.UTC().Format(time.RFC3339Nano))
	}
	if gitDir := resolveGitDir(worktree); gitDir != "" {
		// git index mtime advances on `git add`; HEAD content advances on a
		// commit or branch switch. Both are cheap identity, no file content.
		if fi, e := os.Stat(filepath.Join(gitDir, "index")); e == nil {
			parts = append(parts, "index="+fi.ModTime().UTC().Format(time.RFC3339Nano))
		}
		if head, e := os.ReadFile(filepath.Join(gitDir, "HEAD")); e == nil {
			parts = append(parts, "head="+strings.TrimSpace(string(head)))
		}
	}
	return progress.Fingerprint(parts...)
}

// resolveGitDir returns the real git directory for a worktree. For a primary
// checkout `.git` is a directory; for a linked git worktree `.git` is a file
// containing `gitdir: <path>`. Returns an empty string when neither resolves.
func resolveGitDir(worktree string) string {
	dotGit := filepath.Join(worktree, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return dotGit
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	const prefix = "gitdir:"
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if gitDir == "" {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktree, gitDir)
	}
	return gitDir
}
