package supervisor

import (
	"fmt"
	"log"
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
	observed, phase := collectMaterialProgressSignals(st, now)
	budget := cfg.StalledProgressWatchdog.EffectiveMaxSilence()
	interval := cfg.StalledProgressWatchdog.EffectiveEvalInterval()
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
// signal set for the project from in-memory state, without shelling out to git
// (the daemon evaluates every cycle, so the signals must be cheap and pure).
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
		// bounded worktree/branch + PR-head proxy (branch + PR number; the daemon
		// does not shell git here, so the PR head is the durable git identity we
		// can read cheaply).
		if b := strings.TrimSpace(sess.Branch); b != "" || sess.PRNumber != 0 {
			worktreeGit = append(worktreeGit, fmt.Sprintf("%s=%s:pr%d", slot, b, sess.PRNumber))
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
