package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
)

func TestCollectMaterialProgressObservations_ExactWorkersCannotMaskEachOther(t *testing.T) {
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot-a"] = &state.Session{
		IssueNumber: 11, Status: state.StatusRunning, PID: 101, TmuxSession: "mae-a",
		StartedAt: t0.Add(-time.Hour), LastOutputHash: "frozen-a",
	}
	st.Sessions["slot-b"] = &state.Session{
		IssueNumber: 12, Status: state.StatusRunning, PID: 202, TmuxSession: "mae-b",
		StartedAt: t0.Add(-30 * time.Minute), LastOutputHash: "output-b-1",
	}

	first := collectMaterialProgressObservations(st, t0)
	if len(first) != 2 {
		t.Fatalf("observations = %d, want two exact workers", len(first))
	}
	if first[0].Target.Key() == first[1].Target.Key() {
		t.Fatalf("distinct workers collapsed to one target: %+v %+v", first[0].Target, first[1].Target)
	}
	for _, observation := range first {
		if observation.Target.Kind != progress.TargetWorker || observation.Phase != progress.PhasePreDelivery {
			t.Fatalf("worker observation = %+v, phase=%q", observation.Target, observation.Phase)
		}
		if observation.Target.ProcessID <= 0 || observation.Target.SessionID == "" || observation.Target.LeaseID == "" {
			t.Fatalf("worker target is not exact: %+v", observation.Target)
		}
	}

	budget := 20 * time.Minute
	if _, err := st.RecordMaterialProgress(first, budget, time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	// Only worker B progresses after the shared silence deadline. Its progress
	// must not reset worker A's independently persisted watermark.
	st.Sessions["slot-b"].LastOutputHash = "output-b-2"
	later := t0.Add(budget + time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, later), budget, time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	bySlot := decisionsBySlot(decisions)
	if got := bySlot["slot-a"].Action; got != progress.ActionStopAndRetry {
		t.Fatalf("hung worker A action = %q, want stop_and_retry", got)
	}
	if got := bySlot["slot-b"].Action; got != progress.ActionNone {
		t.Fatalf("progressing worker B action = %q, want none", got)
	}
}

func TestCollectMaterialProgressObservations_WorkerReplacementHasNewLease(t *testing.T) {
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	sess := &state.Session{IssueNumber: 9, Status: state.StatusRunning, PID: 100, TmuxSession: "mae-9", StartedAt: t0}
	st.Sessions["slot-9"] = sess
	first := collectMaterialProgressObservations(st, t0)

	sess.PID = 200
	sess.TmuxSession = "mae-9-retry"
	sess.StartedAt = t0.Add(10 * time.Minute)
	second := collectMaterialProgressObservations(st, t0.Add(10*time.Minute))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("observations first=%d second=%d, want one each", len(first), len(second))
	}
	if first[0].Target.Key() == second[0].Target.Key() || first[0].Target.LeaseID == second[0].Target.LeaseID {
		t.Fatalf("respawn reused old exact lease: before=%+v after=%+v", first[0].Target, second[0].Target)
	}
}

func TestCollectMaterialProgressObservations_PROpenNeverCarriesProcessIdentity(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	// PID/tmux are deliberately stale legacy fields. pr_open is a gate, not a
	// running worker, so the collector must not attach either to its target.
	st.Sessions["slot-pr"] = &state.Session{
		IssueNumber: 44, Status: state.StatusPROpen, PRNumber: 893,
		PID: 999, TmuxSession: "stale-tmux", StartedAt: now.Add(-time.Hour),
		ReviewPendingHeadSHA: "abc123", LastNotifiedStatus: "ci_pending",
	}
	observations := collectMaterialProgressObservations(st, now)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want one PR gate", len(observations))
	}
	got := observations[0]
	if got.Target.Kind != progress.TargetPRGate || got.Phase != progress.PhasePRGate {
		t.Fatalf("target=%+v phase=%q, want pr_gate", got.Target, got.Phase)
	}
	if got.Target.ProcessID != 0 || got.Target.TmuxSession != "" {
		t.Fatalf("pr_open leaked process identity: %+v", got.Target)
	}
	if signalKindPresent(got.Signals, progress.SignalProcessTmux) || signalKindPresent(got.Signals, progress.SignalWorktreeGit) {
		t.Fatalf("pr_open collected live-worker signals: %+v", got.Signals)
	}

	if _, err := st.RecordMaterialProgress(observations, 20*time.Minute, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(21 * time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, later), 20*time.Minute, time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionSurfaceReconciliation {
		t.Fatalf("overdue pr_gate decisions = %+v, want surface_reconciliation", decisions)
	}
}

func TestCollectMaterialProgressObservations_IdleProjectHasNoTarget(t *testing.T) {
	st := state.NewState()
	st.Sessions["done"] = &state.Session{IssueNumber: 1, Status: state.StatusDone, PID: 123}
	st.Sessions["failed"] = &state.Session{IssueNumber: 2, Status: state.StatusFailed}
	if got := collectMaterialProgressObservations(st, time.Now().UTC()); len(got) != 0 {
		t.Fatalf("idle observations = %+v, want none", got)
	}
}

func TestCollectMaterialProgressObservations_DeliveryLifecycleOneStableLease(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	approval := state.Approval{
		ID: "delivery-lease", Action: state.ApprovalActionDeployProject,
		Status: state.ApprovalStatusPending, CreatedAt: now, UpdatedAt: now,
		Delivery: &state.DeliveryPayload{
			Issue: 887, PR: 893, MergedSHA: "merge-a", ConfigDigest: "cfg-a", ApprovalGeneration: 2,
		},
	}
	st.Approvals = append(st.Approvals, approval)

	pending := onlyDeliveryObservation(t, st, now)
	key := pending.Target.Key()
	if pending.Phase != progress.PhaseDeliveryPending || pending.Target.ProcessID != 0 || pending.Target.TmuxSession != "" {
		t.Fatalf("pending delivery observation = %+v phase=%q", pending.Target, pending.Phase)
	}
	pendingID := pending.Signals.Combined()

	st.Approvals[0].Status = state.ApprovalStatusApproved
	approved := onlyDeliveryObservation(t, st, now.Add(time.Minute))
	if approved.Target.Key() != key || approved.Signals.Combined() == pendingID {
		t.Fatalf("approval transition changed lease or not signal: pending=%+v approved=%+v", pending, approved)
	}

	st.Approvals[0].Status = state.ApprovalStatusExecuting
	st.Approvals[0].Delivery.StartedAt = now.Add(2 * time.Minute)
	executing := onlyDeliveryObservation(t, st, now.Add(2*time.Minute))
	if executing.Target.Key() != key || executing.Phase != progress.PhaseDeliveryExecuting {
		t.Fatalf("executing delivery changed target/phase: %+v", executing)
	}

	zero := 0
	st.Approvals[0].Status = state.ApprovalStatusExecuted
	st.Approvals[0].Delivery.FinishedAt = now.Add(3 * time.Minute)
	st.Approvals[0].Delivery.DeployExitCode = &zero
	st.Approvals[0].Delivery.VerifyExitCode = &zero
	st.Approvals[0].Delivery.Verified = true
	st.Approvals[0].Delivery.ExecutedRevision = "merge-a"
	delivered := onlyDeliveryObservation(t, st, now.Add(3*time.Minute))
	if delivered.Target.Key() != key || delivered.Phase != progress.PhaseDelivered || delivered.Signals.Combined() == executing.Signals.Combined() {
		t.Fatalf("terminal receipt missing or changed lease: %+v", delivered)
	}
	if _, err := st.RecordMaterialProgress([]progress.Observation{delivered}, 20*time.Minute, time.Minute, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Once the exact terminal receipt is durable, the next complete snapshot
	// retires it instead of arming/evaluating historical deliveries forever.
	if got := collectMaterialProgressObservations(st, now.Add(4*time.Minute)); len(got) != 0 {
		t.Fatalf("already-recorded terminal delivery remained active: %+v", got)
	}
}

func TestCollectMaterialProgressObservations_IgnoresNonActionableDeliveryRows(t *testing.T) {
	for _, status := range []state.ApprovalStatus{
		state.ApprovalStatusRejected, state.ApprovalStatusStale, state.ApprovalStatusSuperseded,
	} {
		st := state.NewState()
		st.Approvals = append(st.Approvals, state.Approval{
			ID: "delivery-" + string(status), Action: state.ApprovalActionDeployProject, Status: status,
			Delivery: &state.DeliveryPayload{MergedSHA: "a", ConfigDigest: "b"},
		})
		if got := collectMaterialProgressObservations(st, time.Now().UTC()); len(got) != 0 {
			t.Fatalf("status %q produced watchdog target: %+v", status, got)
		}
	}
}

func TestRecordMaterialProgress_DisableTransitionRetiresTargets(t *testing.T) {
	st := state.NewState()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot"] = &state.Session{IssueNumber: 1, Status: state.StatusRunning, PID: 1, StartedAt: now}
	enabled := true
	cfg := &config.Config{StalledProgressWatchdog: config.StalledProgressWatchdogConfig{Enabled: &enabled}}
	if _, err := recordMaterialProgress(cfg, st, now); err != nil {
		t.Fatal(err)
	}
	if len(st.MaterialProgress.Targets) != 1 {
		t.Fatalf("enabled targets = %d, want 1", len(st.MaterialProgress.Targets))
	}

	disabled := false
	cfg.StalledProgressWatchdog.Enabled = &disabled
	if _, err := recordMaterialProgress(cfg, st, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for key, target := range st.MaterialProgress.Targets {
		if target.Active {
			t.Fatalf("disabled watchdog left target %s active: %+v", key, target)
		}
		if deadline := target.Deadline(st.MaterialProgress.BudgetSeconds); !deadline.IsZero() {
			t.Fatalf("disabled target deadline = %s, want zero", deadline)
		}
	}
}

func TestWorktreeProgressFingerprint_MaterialGitIdentityNotMtime(t *testing.T) {
	wt := initMaterialProgressGitRepo(t)
	src := filepath.Join(wt, "main.go")
	base := worktreeProgressFingerprint(wt)
	if base == "" {
		t.Fatal("clean Git worktree produced no fingerprint")
	}

	// A timestamp-only touch is not material progress.
	touched := time.Now().UTC().Add(2 * time.Hour)
	if err := os.Chtimes(src, touched, touched); err != nil {
		t.Fatal(err)
	}
	if got := worktreeProgressFingerprint(wt); got != base {
		t.Fatalf("mtime-only touch advanced fingerprint: got %q, want %q", got, base)
	}

	// Actual working-tree content and then the staged index are distinct
	// material states even though HEAD has not changed.
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edited := worktreeProgressFingerprint(wt)
	if edited == "" || edited == base {
		t.Fatalf("source edit did not advance fingerprint: base=%q edited=%q", base, edited)
	}
	runMaterialProgressGit(t, wt, "add", "main.go")
	staged := worktreeProgressFingerprint(wt)
	if staged == "" || staged == edited {
		t.Fatalf("index transition did not advance fingerprint: edited=%q staged=%q", edited, staged)
	}

	// A commit moves the resolved HEAD even if it leaves a clean tree.
	runMaterialProgressGit(t, wt, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-q", "-m", "edit")
	committed := worktreeProgressFingerprint(wt)
	if committed == "" || committed == staged || committed == base {
		t.Fatalf("HEAD commit did not produce a distinct identity: base=%q staged=%q committed=%q", base, staged, committed)
	}
}

func TestWorktreeProgressFingerprint_ExcludesGeneratedAndVolatileChurn(t *testing.T) {
	wt := initMaterialProgressGitRepo(t)
	base := worktreeProgressFingerprint(wt)

	volatile := []struct {
		path string
		body string
	}{
		{"build/cache.bin", "one"},
		{"node_modules/pkg/index.js", "one"},
		{"generated/schema.pb.go", "package generated"},
		{"coverage.out", "mode: set"},
	}
	for _, file := range volatile {
		path := filepath.Join(wt, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := worktreeProgressFingerprint(wt); got != base {
		t.Fatalf("generated/volatile churn advanced fingerprint: got %q, want %q", got, base)
	}

	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("material docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := worktreeProgressFingerprint(wt); got == "" || got == base {
		t.Fatalf("relevant untracked source/docs did not advance fingerprint: got %q, base %q", got, base)
	}
}

func TestCollectMaterialProgressObservations_SourceEditsAdvanceButGeneratedChurnDoesNot(t *testing.T) {
	wt := initMaterialProgressGitRepo(t)
	st := state.NewState()
	t0 := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	st.Sessions["slot"] = &state.Session{
		IssueNumber: 7, Status: state.StatusRunning, PID: 77, TmuxSession: "mae-7",
		StartedAt: t0.Add(-time.Hour), LastOutputHash: "frozen", Worktree: wt,
	}
	budget := 20 * time.Minute
	if _, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, t0), budget, time.Minute, t0); err != nil {
		t.Fatal(err)
	}

	// Quiet terminal, real source edit after the old deadline: the exact
	// worktree signal advances and prevents a false recovery recommendation.
	if err := os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(21 * time.Minute)
	decisions, err := st.RecordMaterialProgress(collectMaterialProgressObservations(st, t1), budget, time.Minute, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionNone {
		t.Fatalf("source edit decisions = %+v, want material progress", decisions)
	}

	// Churn only generated output for another full budget. It is intentionally
	// excluded, so it cannot keep a genuinely idle worker alive forever.
	generated := filepath.Join(wt, "build", "heartbeat.log")
	if err := os.MkdirAll(filepath.Dir(generated), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generated, []byte("churn"), 0o644); err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(21 * time.Minute)
	decisions, err = st.RecordMaterialProgress(collectMaterialProgressObservations(st, t2), budget, time.Minute, t2)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != progress.ActionStopAndRetry {
		t.Fatalf("generated-only churn decisions = %+v, want stop_and_retry", decisions)
	}
}

func TestWorktreeProgressFingerprint_NonRepositoryIsAbsent(t *testing.T) {
	if got := worktreeProgressFingerprint(t.TempDir()); got != "" {
		t.Fatalf("non-Git directory fingerprint = %q, want absent", got)
	}
}

func decisionsBySlot(decisions []progress.Decision) map[string]progress.Decision {
	out := make(map[string]progress.Decision, len(decisions))
	for _, decision := range decisions {
		out[decision.Target.Slot] = decision
	}
	return out
}

func signalKindPresent(signals progress.SignalSet, kind progress.SignalKind) bool {
	for _, signal := range signals.Present() {
		if signal.Kind == kind {
			return true
		}
	}
	return false
}

func onlyDeliveryObservation(t *testing.T, st *state.State, now time.Time) progress.Observation {
	t.Helper()
	observations := collectMaterialProgressObservations(st, now)
	if len(observations) != 1 || observations[0].Target.Kind != progress.TargetDelivery {
		t.Fatalf("delivery observations = %+v, want exactly one delivery", observations)
	}
	return observations[0]
}

func initMaterialProgressGitRepo(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	runMaterialProgressGit(t, wt, "init", "-q")
	if err := os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runMaterialProgressGit(t, wt, "add", "main.go")
	runMaterialProgressGit(t, wt, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-q", "-m", "initial")
	return wt
}

func runMaterialProgressGit(t *testing.T, worktree string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", worktree}, args...)
	if out, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
