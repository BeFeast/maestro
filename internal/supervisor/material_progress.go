package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
)

// recordMaterialProgress collects and independently evaluates every exact
// worker, PR-gate, post-merge verification, and delivery target. The complete
// snapshot retires targets that disappeared; an idle project therefore has no
// armed synthetic target.
func recordMaterialProgress(cfg *config.Config, st *state.State, now time.Time) ([]progress.Decision, error) {
	if cfg == nil || st == nil {
		return nil, nil
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
	if mp := st.MaterialProgress; mp != nil && !mp.EvaluationDue(budget, interval, now) {
		return nil, nil
	}
	var observations []progress.Observation
	if budget > 0 {
		observations = collectMaterialProgressObservations(st, now)
	}
	decisions, err := st.RecordMaterialProgress(observations, budget, interval, now)
	if err != nil {
		return nil, err
	}
	for _, dec := range decisions {
		if !dec.RecommendsRecovery() {
			continue
		}
		// Reporting only: a recommendation is not an actuation attempt. The
		// exact target and RecommendationID let a later actuator record one
		// attempt/result without conflating evaluation with recovery.
		log.Printf("[supervisor/watchdog] stalled-progress %s: %s (target=%s issue=%d slot=%s phase=%s deadline=%s recommendation=%s)",
			dec.Action, dec.Reason, dec.Target.Key(), dec.Target.IssueNumber, dec.Target.Slot,
			dec.Phase, dec.Deadline.Format(time.RFC3339), dec.RecommendationID)
	}
	return decisions, nil
}

// collectMaterialProgressObservations derives a complete snapshot of exact,
// independently-watermarked lifecycle targets. A running worker is bound to
// issue+slot+spawn session+PID/tmux+lease; pr_open/queued sessions are PR gates
// with deliberately zero process identity; code_landed is a distinct
// post-merge/live-verification target; and each delivery approval generation is
// its own durable lease. Progress by one target cannot reset another's deadline.
//
// Signals contain only non-reversible digests. A failed bounded worktree or
// checkpoint read is absent evidence, never a fabricated progress event.
func collectMaterialProgressObservations(st *state.State, now time.Time) []progress.Observation {
	if st == nil {
		return nil
	}
	now = now.UTC()
	slots := make([]string, 0, len(st.Sessions))
	for slot := range st.Sessions {
		slots = append(slots, slot)
	}
	sort.Strings(slots)

	observations := make([]progress.Observation, 0, len(slots)+len(st.Approvals))
	for _, slot := range slots {
		sess := st.Sessions[slot]
		if sess == nil {
			continue
		}
		switch sess.Status {
		case state.StatusRunning:
			if observation, ok := workerProgressObservation(slot, sess, now); ok {
				observations = append(observations, observation)
			}
		case state.StatusPROpen, state.StatusQueued:
			if observation, ok := prGateProgressObservation(slot, sess, now); ok {
				observations = append(observations, observation)
			}
		case state.StatusCodeLanded:
			if observation, ok := postMergeProgressObservation(st, slot, sess, now); ok {
				observations = append(observations, observation)
			}
		}
	}

	observations = append(observations, deliveryProgressObservations(st, now)...)
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].Target.Key() < observations[j].Target.Key()
	})
	return observations
}

func workerProgressObservation(slot string, sess *state.Session, now time.Time) (progress.Observation, bool) {
	if sess == nil || sess.IssueNumber <= 0 || sess.PID <= 0 {
		return progress.Observation{}, false
	}
	sessionID := materialProgressSessionID(slot, sess)
	target := progress.Target{
		Kind:        progress.TargetWorker,
		IssueNumber: sess.IssueNumber,
		Slot:        slot,
		SessionID:   sessionID,
		TmuxSession: strings.TrimSpace(sess.TmuxSession),
		ProcessID:   sess.PID,
		LeaseID:     "spawn:" + sessionID,
	}
	if err := target.Validate(); err != nil {
		return progress.Observation{}, false
	}
	signals := progress.SignalSet{
		materialProgressSignal(progress.SignalIssueSession, now,
			fmt.Sprintf("status=%s", sess.Status), fmt.Sprintf("retry=%d", sess.RetryCount),
			fmt.Sprintf("maintenance_retry=%d", sess.MaintenanceRetryCount), "phase="+string(sess.Phase)),
		materialProgressSignal(progress.SignalProcessTmux, now,
			fmt.Sprintf("pid=%d", sess.PID), "tmux="+strings.TrimSpace(sess.TmuxSession), "session="+sessionID),
	}
	var unavailable []progress.SignalKind
	terminal, terminalComplete := terminalCheckpointProgress(sess)
	if terminal != "" && terminalComplete {
		signals = append(signals, materialProgressSignal(progress.SignalTerminalCheckpoint, now, terminal))
	}
	if !terminalComplete {
		unavailable = append(unavailable, progress.SignalTerminalCheckpoint)
	}
	worktree, worktreeComplete := worktreeProgressProbe(sess.Worktree)
	if worktree != "" {
		signals = append(signals, materialProgressSignal(progress.SignalWorktreeGit, now, worktree))
	}
	if !worktreeComplete {
		unavailable = append(unavailable, progress.SignalWorktreeGit)
	}
	if pr := prReviewFingerprint(sess); pr != "" {
		signals = append(signals, materialProgressSignal(progress.SignalPRReview, now, pr))
	}
	return progress.Observation{
		Target: target, Signals: signals, Phase: progress.PhasePreDelivery,
		Incomplete: len(unavailable) > 0, UnavailableSignals: unavailable,
	}, true
}

func prGateProgressObservation(slot string, sess *state.Session, now time.Time) (progress.Observation, bool) {
	if sess == nil || sess.IssueNumber <= 0 {
		return progress.Observation{}, false
	}
	sessionID := materialProgressSessionID(slot, sess)
	lease := fmt.Sprintf("pr:%d", sess.PRNumber)
	if sess.PRNumber <= 0 {
		lease = "pr-pending:" + sessionID
	}
	target := progress.Target{
		Kind:        progress.TargetPRGate,
		IssueNumber: sess.IssueNumber,
		Slot:        slot,
		SessionID:   sessionID,
		LeaseID:     lease,
		// pr_open/queued is a control-plane gate, not a live process. Never copy stale
		// Session.PID/TmuxSession into this target or its signals (#814/#887).
	}
	if err := target.Validate(); err != nil {
		return progress.Observation{}, false
	}
	signals := progress.SignalSet{
		materialProgressSignal(progress.SignalIssueSession, now,
			fmt.Sprintf("status=%s", sess.Status), fmt.Sprintf("retry=%d", sess.RetryCount),
			fmt.Sprintf("maintenance_retry=%d", sess.MaintenanceRetryCount)),
	}
	if pr := prReviewFingerprint(sess); pr != "" {
		signals = append(signals, materialProgressSignal(progress.SignalPRReview, now, pr))
	}
	return progress.Observation{Target: target, Signals: signals, Phase: progress.PhasePRGate}, true
}

func postMergeProgressObservation(st *state.State, slot string, sess *state.Session, now time.Time) (progress.Observation, bool) {
	if sess == nil || sess.IssueNumber <= 0 {
		return progress.Observation{}, false
	}
	sessionID := materialProgressSessionID(slot, sess)
	lease := fmt.Sprintf("post-merge:pr:%d", sess.PRNumber)
	if sess.PRNumber <= 0 {
		lease = "post-merge:legacy:" + sessionID
	}
	target := progress.Target{
		Kind:        progress.TargetPostMerge,
		IssueNumber: sess.IssueNumber,
		Slot:        slot,
		SessionID:   sessionID,
		LeaseID:     lease,
		// code_landed is a durable control-plane/outcome gate. A stale PID or
		// tmux name from the completed implementation worker is never actionable.
	}
	if err := target.Validate(); err != nil {
		return progress.Observation{}, false
	}
	signals := progress.SignalSet{
		materialProgressSignal(progress.SignalIssueSession, now,
			fmt.Sprintf("status=%s", sess.Status), fmt.Sprintf("pr=%d", sess.PRNumber),
			"landed="+materialProgressOptionalTime(sess.FinishedAt),
			"deployed="+materialProgressOptionalTime(sess.DeploymentFinishedAt),
			fmt.Sprintf("live_verification_notified=%t", sess.LiveVerificationNotified)),
		materialProgressSignal(progress.SignalOutcomeVerification, now,
			postMergeOutcomeFingerprint(st, sess)),
	}
	return progress.Observation{Target: target, Signals: signals, Phase: progress.PhasePostMergeVerification}, true
}

// postMergeOutcomeFingerprint includes semantic merge/deploy/outcome evidence
// but deliberately excludes health-check timestamps and durations. Re-running
// the same failing check is observability, not material progress; a changed
// outcome state or durable deploy/live-verification receipt advances the gate.
func postMergeOutcomeFingerprint(st *state.State, sess *state.Session) string {
	if sess == nil {
		return ""
	}
	parts := []string{
		fmt.Sprintf("pr=%d", sess.PRNumber),
		"landed=" + materialProgressOptionalTime(sess.FinishedAt),
		"deployed=" + materialProgressOptionalTime(sess.DeploymentFinishedAt),
		"visual=" + strings.TrimSpace(sess.VisualEvidence),
		fmt.Sprintf("live_verification_notified=%t", sess.LiveVerificationNotified),
	}
	if st != nil && st.OutcomeHealth != nil {
		parts = append(parts,
			"health_state="+strings.TrimSpace(st.OutcomeHealth.State),
			"health_signal="+strings.TrimSpace(st.OutcomeHealth.Signal),
			fmt.Sprintf("health_exit=%d", st.OutcomeHealth.ExitCode),
		)
	}
	return progress.Fingerprint(parts...)
}

func materialProgressSessionID(slot string, sess *state.Session) string {
	if sess != nil && !sess.StartedAt.IsZero() {
		return sess.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	// Imported legacy state can lack StartedAt. Bind the fallback to stable,
	// non-secret spawn identity; PID remains an independent exact target field.
	issue, pid := 0, 0
	if sess != nil {
		issue, pid = sess.IssueNumber, sess.PID
	}
	return "legacy:" + progress.Fingerprint(slot, fmt.Sprintf("issue=%d", issue), fmt.Sprintf("pid=%d", pid))
}

func materialProgressSignal(kind progress.SignalKind, now time.Time, parts ...string) progress.Signal {
	return progress.Signal{Kind: kind, Fingerprint: progress.Fingerprint(parts...), ObservedAt: now.UTC()}
}

func terminalCheckpointProgress(sess *state.Session) (string, bool) {
	if sess == nil {
		return "", true
	}
	parts := make([]string, 0, 3)
	complete := true
	if strings.TrimSpace(sess.TmuxSession) != "" {
		if live, ok := tmuxProgressFingerprint(sess.TmuxSession); ok {
			parts = append(parts, "tmux="+live)
		} else {
			complete = false
		}
	} else if hash := strings.TrimSpace(sess.LastOutputHash); hash != "" {
		parts = append(parts, "persisted_output="+hash)
	}
	if checkpoint, ok := boundedFileFingerprintProbe(sess.CheckpointFile, 1<<20); checkpoint != "" {
		parts = append(parts, "checkpoint="+checkpoint)
	} else if !ok {
		complete = false
	}
	return progress.Fingerprint(parts...), complete
}

func boundedFileFingerprint(path string, maxBytes int64) string {
	fingerprint, _ := boundedFileFingerprintProbe(path, maxBytes)
	return fingerprint
}

// boundedFileFingerprintProbe distinguishes an optional absent path (complete,
// no signal) from a configured read/stat/size failure (incomplete evidence).
func boundedFileFingerprintProbe(path string, maxBytes int64) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", true
	}
	if maxBytes <= 0 {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) != info.Size() {
		return "", false
	}
	return progress.Fingerprint(string(data)), true
}

const (
	tmuxProgressMaxOutputBytes = 1 << 20
	tmuxProgressHistoryLines   = "-2000"
)

var tmuxProgressTimeout = 2 * time.Second

// tmuxProgressFingerprint captures a bounded tail from the exact recorded tmux
// session. It never persists terminal text: only a non-reversible fingerprint
// leaves this function. A timeout, command failure, or output overflow is
// explicit incomplete evidence so the evaluator suppresses destructive action.
func tmuxProgressFingerprint(session string) (string, bool) {
	session = strings.TrimSpace(session)
	if session == "" {
		return "", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxProgressTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "capture-pane", "-p", "-t", "="+session, "-S", tmuxProgressHistoryLines)
	stdout := &boundedOutput{limit: tmuxProgressMaxOutputBytes}
	stderr := &boundedOutput{limit: 32 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil || stdout.truncated || stderr.truncated || ctx.Err() != nil {
		return "", false
	}
	return progress.Fingerprint(stdout.buf.String()), true
}

func prReviewFingerprint(sess *state.Session) string {
	if sess == nil || (sess.PRNumber == 0 && strings.TrimSpace(sess.ReviewPendingHeadSHA) == "" && strings.TrimSpace(sess.LastNotifiedStatus) == "") {
		return ""
	}
	return progress.Fingerprint(
		fmt.Sprintf("pr=%d", sess.PRNumber),
		"head="+strings.TrimSpace(sess.ReviewPendingHeadSHA),
		"notified="+strings.TrimSpace(sess.LastNotifiedStatus),
		"visual="+strings.TrimSpace(sess.VisualEvidence),
	)
}

func deliveryProgressObservations(st *state.State, now time.Time) []progress.Observation {
	if st == nil {
		return nil
	}
	observations := make([]progress.Observation, 0)
	latestUntrackedTerminal := -1
	for i := range st.Approvals {
		approval := &st.Approvals[i]
		phase, included := deliveryProgressPhase(approval)
		if !included || phase != progress.PhaseDelivered {
			continue
		}
		observation, ok := deliveryProgressObservation(approval, phase, now)
		if !ok || materialProgressTargetKnown(st, observation.Target) {
			continue
		}
		if latestUntrackedTerminal < 0 || st.Approvals[latestUntrackedTerminal].UpdatedAt.Before(approval.UpdatedAt) {
			latestUntrackedTerminal = i
		}
	}

	for i := range st.Approvals {
		approval := &st.Approvals[i]
		phase, included := deliveryProgressPhase(approval)
		if !included {
			continue
		}
		observation, ok := deliveryProgressObservation(approval, phase, now)
		if !ok {
			continue
		}
		if phase == progress.PhaseDelivered {
			existing := materialProgressTarget(st, observation.Target)
			switch {
			case existing != nil && existing.Watermark.Phase == progress.PhaseDelivered:
				// The terminal receipt was already durably observed. Omit it from
				// this complete snapshot so the state layer retires its deadline-
				// free record instead of evaluating historical receipts forever.
				continue
			case existing == nil && i != latestUntrackedTerminal:
				// On first enable, baseline only the newest historical terminal
				// receipt. Active leases and tracked transitions are never sampled.
				continue
			}
		}
		observations = append(observations, observation)
	}
	return observations
}

func deliveryProgressPhase(approval *state.Approval) (progress.Phase, bool) {
	if approval == nil || approval.Action != state.ApprovalActionDeployProject || approval.Delivery == nil {
		return progress.PhaseUnknown, false
	}
	switch approval.Status {
	case state.ApprovalStatusPending, state.ApprovalStatusApproved, state.ApprovalStatusAwaitingDispatch:
		return progress.PhaseDeliveryPending, true
	case state.ApprovalStatusExecuting:
		return progress.PhaseDeliveryExecuting, true
	case state.ApprovalStatusExecuted, state.ApprovalStatusExecutionFailed, state.ApprovalStatusExecutionSkipped:
		return progress.PhaseDelivered, true
	default:
		return progress.PhaseUnknown, false
	}
}

func deliveryProgressObservation(approval *state.Approval, phase progress.Phase, now time.Time) (progress.Observation, bool) {
	if approval == nil || approval.Delivery == nil || strings.TrimSpace(approval.ID) == "" {
		return progress.Observation{}, false
	}
	payload := approval.Delivery
	lease := fmt.Sprintf("%s:g%d", strings.TrimSpace(approval.ID), payload.ApprovalGeneration)
	target := progress.Target{
		Kind:        progress.TargetDelivery,
		IssueNumber: payload.Issue,
		LeaseID:     lease,
	}
	if err := target.Validate(); err != nil {
		return progress.Observation{}, false
	}
	parts := []string{
		"id=" + strings.TrimSpace(approval.ID),
		"status=" + string(approval.Status),
		fmt.Sprintf("generation=%d", payload.ApprovalGeneration),
		"merged_sha=" + strings.TrimSpace(payload.MergedSHA),
		"config_digest=" + strings.TrimSpace(payload.ConfigDigest),
		"expires=" + materialProgressTime(payload.ExpiresAt),
		"started=" + materialProgressTime(payload.StartedAt),
		"finished=" + materialProgressTime(payload.FinishedAt),
		"failure_stage=" + strings.TrimSpace(payload.FailureStage),
		"deploy_exit=" + materialProgressOptionalInt(payload.DeployExitCode),
		"verify_exit=" + materialProgressOptionalInt(payload.VerifyExitCode),
		fmt.Sprintf("timed_out=%t", payload.TimedOut),
		fmt.Sprintf("cleanup_failed=%t", payload.CleanupFailed),
		fmt.Sprintf("verified=%t", payload.Verified),
		"completion_source=" + strings.TrimSpace(payload.CompletionSource),
		"reconcile_outcome=" + strings.TrimSpace(payload.ReconcileOutcome),
		"stale_cause=" + strings.TrimSpace(payload.StaleCause),
		"executed_revision=" + strings.TrimSpace(payload.ExecutedRevision),
	}
	// Audit event codes/times distinguish e.g. an approved→executing claim
	// released back to approved without hashing free-text reason/actor fields.
	start := 0
	if len(approval.Audit) > 16 {
		start = len(approval.Audit) - 16
		parts = append(parts, "audit_truncated=true")
	}
	for _, audit := range approval.Audit[start:] {
		parts = append(parts, "audit="+audit.Event+"@"+materialProgressTime(audit.At)+":"+audit.PayloadHash)
	}
	signal := materialProgressSignal(progress.SignalDelivery, now, parts...)
	return progress.Observation{Target: target, Signals: progress.SignalSet{signal}, Phase: phase}, true
}

func materialProgressTarget(st *state.State, target progress.Target) *state.MaterialProgressTarget {
	if st == nil || st.MaterialProgress == nil || st.MaterialProgress.Targets == nil {
		return nil
	}
	return st.MaterialProgress.Targets[target.Key()]
}

func materialProgressTargetKnown(st *state.State, target progress.Target) bool {
	return materialProgressTarget(st, target) != nil
}

func materialProgressTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func materialProgressOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return materialProgressTime(*value)
}

func materialProgressOptionalInt(value *int) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

// worktreeVolatileDirs are directory names excluded from the bounded worktree
// progress probe: they churn (or are regenerated) independently of the worker's
// material progress, so hashing their contents would mask a real stall and add
// cost. Git metadata is queried explicitly for HEAD/index identity instead.
var worktreeVolatileDirs = map[string]bool{
	".git":          true,
	".cache":        true,
	".generated":    true,
	"node_modules":  true,
	"vendor":        true,
	"target":        true,
	"dist":          true,
	"build":         true,
	"out":           true,
	"generated":     true,
	"gen":           true,
	"tmp":           true,
	"temp":          true,
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

const (
	// worktreeProgressMaxEntries bounds the number of material paths whose Git
	// index and working-tree identities are inspected in one probe. A larger
	// dirty set is reported as unavailable instead of silently sampling it and
	// presenting the sample as authoritative progress evidence.
	worktreeProgressMaxEntries = 4096
	// worktreeProgressMaxPathBytes bounds argv and in-memory status identity.
	worktreeProgressMaxPathBytes = 512 << 10
	// worktreeProgressMaxOutputBytes bounds every Git subprocess independently.
	// The commands used below normally emit only status/raw object identities;
	// crossing this limit means the probe cannot claim a complete fingerprint.
	worktreeProgressMaxOutputBytes = 8 << 20
	// All Git reads for one worker share one deadline. This is deliberately
	// shorter than the watchdog cadence and keeps a pathological repository
	// from serialising the supervisor loop indefinitely.
	worktreeProgressTimeout = 5 * time.Second
)

var worktreeVolatileSuffixes = []string{
	".pyc", ".pyo", ".class", ".o", ".obj", ".a", ".so", ".dylib", ".dll", ".exe",
	".apk", ".ipa", ".map", ".prof", ".trace", ".tmp", ".temp", ".swp", ".swo",
}

var worktreeGeneratedNameFragments = []string{
	".generated.", "_generated.go", ".g.dart", ".freezed.dart", ".pb.go", "_pb2.py",
}

type worktreeChange struct {
	status string
	path   string
}

// worktreeProgressFingerprint returns bounded, material Git evidence for a
// worker worktree. It combines the resolved HEAD commit with filtered porcelain
// status, the staged index/raw diff, the unstaged raw diff, and Git blob hashes
// for relevant working-tree files. Therefore a source edit advances the signal,
// while merely touching an unchanged file (or churning generated/volatile
// output) does not.
//
// Git output, dirty-path count, argv bytes, and wall time are all bounded. The
// probe never persists source, paths, command output, or errors: only a final
// non-reversible digest leaves this function. When any required read is partial
// or fails, it returns an absent signal rather than blessing an incomplete
// sample as truthful material progress.
func worktreeProgressFingerprint(worktree string) string {
	fingerprint, _ := worktreeProgressProbe(worktree)
	return fingerprint
}

// worktreeProgressProbe is the status-aware form used by the watchdog. Empty
// Worktree means the optional signal is not configured; a configured path with
// any stat/Git/timeout/bounds failure is incomplete evidence, never a silently
// absent signal that could authorize recovery.
func worktreeProgressProbe(worktree string) (string, bool) {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return "", true
	}
	info, err := os.Stat(worktree)
	if err != nil || !info.IsDir() {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), worktreeProgressTimeout)
	defer cancel()

	head, ok := boundedGitOutput(ctx, worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if !ok || strings.TrimSpace(string(head)) == "" {
		return "", false
	}
	statusRaw, ok := boundedGitOutput(ctx, worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=dirty")
	if !ok {
		return "", false
	}
	changes, ok := relevantWorktreeChanges(statusRaw)
	if !ok {
		return "", false
	}

	parts := []string{"head=" + strings.TrimSpace(string(head))}
	if len(changes) == 0 {
		return progress.Fingerprint(parts...), true
	}

	paths := make([]string, 0, len(changes))
	statusParts := make([]string, 0, len(changes))
	pathBytes := 0
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		statusParts = append(statusParts, change.status+"\x00"+change.path)
		if _, exists := seen[change.path]; exists {
			continue
		}
		seen[change.path] = struct{}{}
		pathBytes += len(change.path)
		if pathBytes > worktreeProgressMaxPathBytes {
			return "", false
		}
		paths = append(paths, change.path)
	}
	sort.Strings(paths)
	sort.Strings(statusParts)
	parts = append(parts, "status="+progress.Fingerprint(statusParts...))

	pathspec := make([]string, 0, len(paths)+1)
	pathspec = append(pathspec, "--")
	pathspec = append(pathspec, paths...)
	indexRaw, ok := boundedGitOutput(ctx, worktree, append([]string{"diff", "--cached", "--raw", "-z", "--no-renames"}, pathspec...)...)
	if !ok {
		return "", false
	}
	worktreeRaw, ok := boundedGitOutput(ctx, worktree, append([]string{"diff", "--raw", "-z", "--no-renames"}, pathspec...)...)
	if !ok {
		return "", false
	}
	parts = append(parts,
		"index_diff="+progress.Fingerprint(string(indexRaw)),
		"worktree_diff="+progress.Fingerprint(string(worktreeRaw)),
	)

	// Raw worktree diff records intentionally use an all-zero destination OID;
	// hash the relevant existing files through Git to bind their actual content.
	// Deleted files and gitlinks are already fully represented by status/raw diff.
	existing := make([]string, 0, len(paths))
	for _, path := range paths {
		fi, statErr := os.Lstat(filepath.Join(worktree, filepath.FromSlash(path)))
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", false
		}
		if statErr == nil && !fi.IsDir() {
			existing = append(existing, path)
		}
	}
	if len(existing) > 0 {
		hashArgs := []string{"hash-object", "--no-filters", "--"}
		hashArgs = append(hashArgs, existing...)
		workingBlobs, hashOK := boundedGitOutput(ctx, worktree, hashArgs...)
		if !hashOK {
			return "", false
		}
		parts = append(parts, "working_blobs="+progress.Fingerprint(string(workingBlobs)))
	}
	return progress.Fingerprint(parts...), true
}

func relevantWorktreeChanges(raw []byte) ([]worktreeChange, bool) {
	records := bytes.Split(raw, []byte{0})
	changes := make([]worktreeChange, 0, len(records))
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) == 0 {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, false
		}
		status := string(record[:2])
		path := string(record[3:])
		if worktreePathIsMaterial(path) {
			changes = append(changes, worktreeChange{status: status, path: path})
		}
		// Porcelain v1 -z emits a second NUL-delimited path for renames/copies.
		if strings.ContainsAny(status, "RC") {
			i++
			if i >= len(records) || len(records[i]) == 0 {
				return nil, false
			}
			oldPath := string(records[i])
			if worktreePathIsMaterial(oldPath) {
				changes = append(changes, worktreeChange{status: status + "-source", path: oldPath})
			}
		}
		if len(changes) > worktreeProgressMaxEntries {
			return nil, false
		}
	}
	return changes, true
}

func worktreePathIsMaterial(path string) bool {
	path = filepath.ToSlash(path)
	if path == "" || path == "." || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return false
	}
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "." || part == ".." || worktreeVolatileDirs[strings.ToLower(part)] {
			return false
		}
	}
	name := parts[len(parts)-1]
	if name == "." || name == ".." || worktreeVolatileDirs[strings.ToLower(name)] || name == ".DS_Store" || name == "Thumbs.db" || name == ".coverage" || name == "coverage.out" || strings.HasSuffix(name, "~") {
		return false
	}
	lower := strings.ToLower(name)
	for _, suffix := range worktreeVolatileSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	for _, fragment := range worktreeGeneratedNameFragments {
		if strings.Contains(lower, fragment) {
			return false
		}
	}
	return true
}

type boundedOutput struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	written := len(p)
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = w.buf.Write(p[:remaining])
			w.truncated = true
		} else {
			_, _ = w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return written, nil
}

// boundedGitOutput runs one read-only Git query. --literal-pathspecs prevents
// a worker-created filename beginning with pathspec magic from changing query
// scope; GIT_OPTIONAL_LOCKS=0 prevents these observations from taking index
// refresh locks or mutating repository metadata.
func boundedGitOutput(ctx context.Context, worktree string, args ...string) ([]byte, bool) {
	gitArgs := []string{"--literal-pathspecs", "-C", worktree, "-c", "core.fsmonitor=false", "-c", "diff.external="}
	gitArgs = append(gitArgs, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Env = materialProgressGitEnv()
	stdout := &boundedOutput{limit: worktreeProgressMaxOutputBytes}
	stderr := &boundedOutput{limit: 32 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil || stdout.truncated || stderr.truncated || ctx.Err() != nil {
		return nil, false
	}
	return append([]byte(nil), stdout.buf.Bytes()...), true
}

func materialProgressGitEnv() []string {
	blocked := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_OPTIONAL_LOCKS": true, "LC_ALL": true,
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !blocked[key] {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
}
