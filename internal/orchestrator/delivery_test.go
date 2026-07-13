package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

// deliveryMergeSHA is the fixture merge commit main resolves to after a merge.
const deliveryMergeSHA = "abc123def456abc123def456abc123def456abcd"

func newDeliveryOrch(cfg *config.Config) *Orchestrator {
	return &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		ghPRMergeInfoFn: func(int) (github.PRMergeInfo, error) {
			return github.PRMergeInfo{SHA: deliveryMergeSHA, MergedAt: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)}, nil
		},
	}
}

func deployApprovals(s *state.State) []state.Approval {
	var out []state.Approval
	for _, a := range s.Approvals {
		if a.Action == state.ApprovalActionDeployProject {
			out = append(out, a)
		}
	}
	return out
}

// TestDeliverAfterMerge_ApprovalRequiredMintsPending: a merge in
// approval_required mode enqueues exactly one pending, revision-pinned
// deploy_project approval and runs no delivery command (#872).
func TestDeliverAfterMerge_ApprovalRequiredMintsPending(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{
			Mode:              config.DeliveryModeApprovalRequired,
			Command:           "./scripts/deploy.sh",
			VerifyCommand:     "./scripts/verify-delivery.sh",
			TargetLabel:       "production web",
			VerificationLabel: "public health check",
			RollbackLabel:     "previous release",
			TimeoutMinutes:    10,
		},
		Outcome: outcome.Brief{RequiresDeploy: true},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}
	sess := &state.Session{IssueNumber: 42}

	if o.deliverAfterMerge(s, sess, github.PR{Number: 7}) {
		t.Fatalf("approval_required with RequiresDeploy=true must not report deploy succeeded")
	}

	got := deployApprovals(s)
	if len(got) != 1 {
		t.Fatalf("want 1 deploy_project approval, got %d", len(got))
	}
	a := got[0]
	if a.Status != state.ApprovalStatusPending {
		t.Errorf("status = %q, want pending", a.Status)
	}
	if a.Delivery == nil {
		t.Fatal("approval has no delivery payload")
	}
	if a.Delivery.MergedSHA != deliveryMergeSHA {
		t.Errorf("MergedSHA = %q, want the pinned merge commit %q", a.Delivery.MergedSHA, deliveryMergeSHA)
	}
	if a.Delivery.PR != 7 || a.Delivery.Issue != 42 {
		t.Errorf("PR/Issue = %d/%d, want 7/42", a.Delivery.PR, a.Delivery.Issue)
	}
	if a.Delivery.TargetLabel != "production web" || a.Delivery.VerificationLabel != "public health check" ||
		a.Delivery.RollbackLabel != "previous release" {
		t.Errorf("operator-safe labels missing: %+v", a.Delivery)
	}
}

// TestDeliverAfterMerge_ApprovalRequiredSkipsWhenSHAUnresolvable: without a
// pinnable merge commit the delivery approval is NOT minted — never fall back
// to deploying whatever revision happens to be at local_path (#872 addendum).
func TestDeliverAfterMerge_ApprovalRequiredSkipsWhenSHAUnresolvable(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery:  config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "make deploy"},
	}
	o := newDeliveryOrch(cfg)
	o.ghPRMergeInfoFn = func(int) (github.PRMergeInfo, error) {
		return github.PRMergeInfo{}, fmt.Errorf("no merge commit resolvable")
	}
	s := &state.State{}

	o.deliverAfterMerge(s, &state.Session{IssueNumber: 5}, github.PR{Number: 3})

	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("want 0 delivery approvals when the merge SHA is unresolvable, got %d", n)
	}
}

// TestDeliverAfterMerge_SupersedingMergeInvalidatesStale: a second merge (a new
// revision) supersedes the earlier pending delivery so the stale revision can
// never be approved into a deploy (#872).
func TestDeliverAfterMerge_SupersedingMergeInvalidatesStale(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery:  config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "make deploy"},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	o.deliverAfterMerge(s, &state.Session{IssueNumber: 1}, github.PR{Number: 1})

	// A newer merge advances main to a different commit.
	const newerSHA = "fedcba987654fedcba987654fedcba987654fedc"
	o.ghPRMergeInfoFn = func(int) (github.PRMergeInfo, error) {
		return github.PRMergeInfo{SHA: newerSHA, MergedAt: time.Date(2026, 7, 13, 10, 1, 0, 0, time.UTC)}, nil
	}
	o.deliverAfterMerge(s, &state.Session{IssueNumber: 2}, github.PR{Number: 2})

	var pending, superseded int
	var pendingSHA string
	for _, a := range deployApprovals(s) {
		switch a.Status {
		case state.ApprovalStatusPending:
			pending++
			pendingSHA = a.Delivery.MergedSHA
		case state.ApprovalStatusSuperseded:
			superseded++
		}
	}
	if pending != 1 || superseded != 1 {
		t.Fatalf("want exactly 1 pending + 1 superseded delivery, got pending=%d superseded=%d", pending, superseded)
	}
	if pendingSHA != newerSHA {
		t.Errorf("the live pending delivery pins %q, want the newest merge %q", pendingSHA, newerSHA)
	}
}

// TestDeliverAfterMerge_AutomaticRunsCommand: automatic mode runs the delivery
// command immediately (legacy deploy_cmd parity) and mints no approval.
func TestDeliverAfterMerge_AutomaticRunsCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: dir,
		Delivery: config.DeliveryConfig{
			Mode:           config.DeliveryModeAutomatic,
			Command:        "touch delivered.marker",
			TimeoutMinutes: 1,
		},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	if !o.deliverAfterMerge(s, &state.Session{}, github.PR{Number: 1}) {
		t.Fatal("automatic delivery should report deploy succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "delivered.marker")); err != nil {
		t.Fatalf("automatic delivery command did not run: %v", err)
	}
	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("automatic mode must not mint a deploy_project approval, got %d", n)
	}
}

// TestDeliverAfterMerge_DisabledDoesNothing: disabled delivery runs nothing and
// mints nothing; deploy-success parity with the legacy empty-deploy_cmd path.
func TestDeliverAfterMerge_DisabledDoesNothing(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery:  config.DeliveryConfig{Mode: config.DeliveryModeDisabled},
		Outcome:   outcome.Brief{RequiresDeploy: false},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	if !o.deliverAfterMerge(s, &state.Session{}, github.PR{Number: 1}) {
		t.Fatal("disabled delivery with RequiresDeploy=false must report succeeded (legacy parity)")
	}
	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("disabled mode must mint no approvals, got %d", n)
	}
}

// TestDeliverAfterMerge_LegacyDeployCmdIsAutomatic: a legacy deploy_cmd (no
// delivery block) resolves to automatic and runs immediately — no silent
// behavior change for existing fleet projects (#872).
func TestDeliverAfterMerge_LegacyDeployCmdIsAutomatic(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:                 "owner/repo",
		LocalPath:            dir,
		DeployCmd:            "touch legacy.marker",
		DeployTimeoutMinutes: 1,
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	if !o.deliverAfterMerge(s, &state.Session{}, github.PR{Number: 9}) {
		t.Fatal("legacy deploy_cmd should run automatically and report succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "legacy.marker")); err != nil {
		t.Fatalf("legacy deploy_cmd did not run: %v", err)
	}
	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("legacy deploy_cmd must not mint an approval, got %d", n)
	}
}

func TestDeliverAfterMerge_PersistsApprovalImmediatelyToConfiguredDB(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		Repo:      "owner/repo",
		StateDir:  filepath.Join(root, "state"),
		LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{
			Mode: config.DeliveryModeApprovalRequired, Command: "make deploy", VerifyCommand: "make verify",
		},
		Outcome: outcome.Brief{RequiresDeploy: true},
	}
	o := newDeliveryOrch(cfg)
	dbPath := filepath.Join(root, "custom-maestro.db")
	o.SetApprovalStore("json", dbPath) // delivery is durable even when generic approvals remain JSON
	s := state.NewState()
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatal(err)
	}
	if o.deliverAfterMerge(s, &state.Session{IssueNumber: 42}, github.PR{Number: 7}) {
		t.Fatal("approval-required delivery must remain pending")
	}

	reloaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	approvals := deployApprovals(reloaded)
	if len(approvals) != 1 {
		t.Fatalf("durable JSON approvals = %d, want 1", len(approvals))
	}
	a := approvals[0]
	if a.Delivery.ConfigDigest == "" || a.Delivery.ExpiresAt.IsZero() || a.Delivery.MergedAt.IsZero() {
		t.Fatalf("approval missing digest/expiry/merge order: %+v", a.Delivery)
	}
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stored, err := store.Get(context.Background(), cfg.StateDir, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != state.ApprovalStatusPending || stored.Delivery.MergedSHA != deliveryMergeSHA {
		t.Fatalf("SQLite approval = %+v", stored)
	}
}

func persistentDeliveryTestConfig(root string) *config.Config {
	return &config.Config{
		Repo:      "owner/repo",
		StateDir:  filepath.Join(root, "state"),
		LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{
			Mode:              config.DeliveryModeApprovalRequired,
			Command:           "./scripts/deploy.sh",
			VerifyCommand:     "./scripts/verify-delivery.sh",
			TargetLabel:       "production",
			VerificationLabel: "health check",
			RollbackLabel:     "previous release",
		},
		Outcome: outcome.Brief{RequiresDeploy: true},
	}
}

// A state.json mirror is never allowed to recreate authorization in a missing
// ledger. Otherwise deleting/changing --approvals-db while the mirror says
// approved would turn that mirror directly into an executable row.
func TestEnqueueDeliveryApproval_RefusesApprovedMirrorWhenLedgerMissing(t *testing.T) {
	root := t.TempDir()
	cfg := persistentDeliveryTestConfig(root)
	o := newDeliveryOrch(cfg)
	dbPath := filepath.Join(root, "wrong-or-empty.db")
	o.SetApprovalStore("json", dbPath)

	s := state.NewState()
	eff := cfg.EffectiveDelivery()
	mergedAt := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	a := s.RecordDeliveryApproval(state.DeliveryPayload{
		Project: cfg.Repo, Repo: cfg.Repo, PR: 7, Issue: 42,
		MergedSHA: deliveryMergeSHA, MergedAt: mergedAt,
		TargetLabel: eff.TargetLabel, VerificationLabel: eff.VerificationLabel, RollbackLabel: eff.RollbackLabel,
		TimeoutMinutes: eff.TimeoutMinutes, ConfigDigest: eff.ApprovalDigest(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}, time.Now().UTC())
	a.Status = state.ApprovalStatusApproved

	if _, err := o.enqueueDeliveryApproval(s, &state.Session{IssueNumber: 42}, github.PR{Number: 7}, eff); err == nil {
		t.Fatal("approved JSON mirror recreated a missing authoritative delivery row")
	}
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Get(context.Background(), cfg.StateDir, a.ID); !errors.Is(err, state.ErrApprovalNotFound) {
		t.Fatalf("wrong/empty ledger row = %v, want absent", err)
	}
}

func TestEnqueueDeliveryApproval_WrongDBCannotReplayApprovedMirror(t *testing.T) {
	root := t.TempDir()
	cfg := persistentDeliveryTestConfig(root)
	primaryDB := filepath.Join(root, "primary.db")
	wrongDB := filepath.Join(root, "wrong.db")
	o := newDeliveryOrch(cfg)
	o.SetApprovalStore("json", primaryDB)
	s := state.NewState()

	pending, err := o.enqueueDeliveryApproval(s, &state.Session{IssueNumber: 42}, github.PR{Number: 7}, cfg.EffectiveDelivery())
	if err != nil {
		t.Fatal(err)
	}
	primary, err := approvalstore.Open(primaryDB)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := primary.Approve(context.Background(), cfg.StateDir, pending.ID, time.Now().UTC(), "operator", "approved")
	if err != nil {
		primary.Close()
		t.Fatal(err)
	}
	primary.Close()
	if current, ok := s.FindApproval(pending.ID); ok {
		*current = *approved
	} else {
		t.Fatal("pending mirror disappeared")
	}

	o.SetApprovalStore("json", wrongDB)
	if _, err := o.enqueueDeliveryApproval(s, &state.Session{IssueNumber: 42}, github.PR{Number: 7}, cfg.EffectiveDelivery()); err == nil {
		t.Fatal("approved mirror was seeded into a wrong ledger")
	}
	wrong, err := approvalstore.Open(wrongDB)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := wrong.Get(context.Background(), cfg.StateDir, pending.ID); !errors.Is(err, state.ErrApprovalNotFound) {
		t.Fatalf("wrong ledger row = %v, want absent", err)
	}
}

// DB-first/state-second crash recovery remains live: if the authoritative
// pending row committed but state.json did not, the next standing reconcile may
// mint the same pending identity in memory and must import the existing DB row.
func TestEnqueueDeliveryApproval_RecoversCommittedPendingMissingFromMirror(t *testing.T) {
	root := t.TempDir()
	cfg := persistentDeliveryTestConfig(root)
	dbPath := filepath.Join(root, "maestro.db")
	o := newDeliveryOrch(cfg)
	o.SetApprovalStore("json", dbPath)

	first, err := o.enqueueDeliveryApproval(state.NewState(), &state.Session{IssueNumber: 42}, github.PR{Number: 7}, cfg.EffectiveDelivery())
	if err != nil {
		t.Fatal(err)
	}
	freshMirror := state.NewState()
	recovered, err := o.enqueueDeliveryApproval(freshMirror, &state.Session{IssueNumber: 42}, github.PR{Number: 7}, cfg.EffectiveDelivery())
	if err != nil {
		t.Fatalf("recover committed pending ledger row: %v", err)
	}
	if recovered.ID != first.ID || recovered.Status != state.ApprovalStatusPending {
		t.Fatalf("recovered = %+v, want authoritative pending %s", recovered, first.ID)
	}
}

func TestEnqueueDeliveryApproval_ConcurrentFreshMintConvergesOnOneLedgerRow(t *testing.T) {
	root := t.TempDir()
	cfg := persistentDeliveryTestConfig(root)
	dbPath := filepath.Join(root, "maestro.db")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := newDeliveryOrch(cfg)
			o.SetApprovalStore("json", dbPath)
			<-start
			_, err := o.enqueueDeliveryApproval(state.NewState(), &state.Session{IssueNumber: 42}, github.PR{Number: 7}, cfg.EffectiveDelivery())
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent mint: %v", err)
		}
	}
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.List(context.Background(), cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != state.ApprovalStatusPending {
		t.Fatalf("ledger rows = %+v, want one pending generation", rows)
	}
}

func TestReconcileCodeLandedSessions_RecoversExternalMergeAndWaitsForDelivery(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{
			Mode: config.DeliveryModeApprovalRequired, Command: "make deploy", VerifyCommand: "make verify",
		},
	}
	o := newDeliveryOrch(cfg)
	o.isPRMergedFn = func(pr int) (bool, error) { return pr == 7, nil }
	s := state.NewState()
	sess := &state.Session{IssueNumber: 42, PRNumber: 7, Status: state.StatusCodeLanded}
	s.Sessions["slot"] = sess

	o.reconcileCodeLandedSessions(s)
	if sess.Status != state.StatusCodeLanded || sess.DeploymentFinishedAt != nil {
		t.Fatalf("session advanced before delivery: status=%q deployed=%v", sess.Status, sess.DeploymentFinishedAt)
	}
	approvals := deployApprovals(s)
	if len(approvals) != 1 || approvals[0].Status != state.ApprovalStatusPending {
		t.Fatalf("standing reconcile did not recover pending delivery: %+v", approvals)
	}

	// Simulate the shared executor's verified terminal row mirrored into state.
	a, _ := s.FindApproval(approvals[0].ID)
	a.Status = state.ApprovalStatusExecuted
	a.Delivery.Verified = true
	a.Delivery.FinishedAt = time.Now().UTC()
	o.isIssueClosedFn = func(int) (bool, error) { return true, nil }
	o.reconcileCodeLandedSessions(s)
	if sess.DeploymentFinishedAt == nil {
		t.Fatal("verified matching delivery did not stamp DeploymentFinishedAt")
	}
	if sess.Status != state.StatusDone {
		t.Fatalf("status = %q, want done after verified delivery", sess.Status)
	}
}

func TestDeliverAfterMerge_UsesPRMergeInfoNotMainHead(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo", LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "deploy", VerifyCommand: "verify"},
	}
	o := newDeliveryOrch(cfg)
	o.mainHeadSHAFn = func() (string, error) {
		t.Fatal("delivery mint must not infer revision from main")
		return "", nil
	}
	s := state.NewState()
	o.deliverAfterMerge(s, &state.Session{}, github.PR{Number: 7})
	if got := deployApprovals(s); len(got) != 1 || got[0].Delivery.MergedSHA != deliveryMergeSHA {
		t.Fatalf("approval merge identity = %+v", got)
	}
}

func TestReconcileCodeLandedDelivery_NewerVerifiedDescendantCoversSupersededMerge(t *testing.T) {
	now := time.Now().UTC()
	const (
		olderSHA = "1111111111111111111111111111111111111111"
		newerSHA = "2222222222222222222222222222222222222222"
	)
	cfg := &config.Config{
		Repo: "owner/repo", LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "deploy", VerifyCommand: "verify"},
	}
	o := newDeliveryOrch(cfg)
	o.ghPRMergeInfoFn = func(pr int) (github.PRMergeInfo, error) {
		if pr == 1 {
			return github.PRMergeInfo{SHA: olderSHA, MergedAt: now.Add(-time.Hour)}, nil
		}
		return github.PRMergeInfo{SHA: newerSHA, MergedAt: now}, nil
	}
	o.deliveryRevisionContainsFn = func(ancestor, descendant string) (bool, error) {
		return ancestor == olderSHA && descendant == newerSHA, nil
	}
	s := state.NewState()
	o.deliverAfterMerge(s, &state.Session{IssueNumber: 1}, github.PR{Number: 1})
	o.deliverAfterMerge(s, &state.Session{IssueNumber: 2}, github.PR{Number: 2})
	var newest *state.Approval
	for i := range s.Approvals {
		if s.Approvals[i].Delivery != nil && s.Approvals[i].Delivery.MergedSHA == newerSHA {
			newest = &s.Approvals[i]
		}
	}
	if newest == nil {
		t.Fatal("newer delivery approval not found")
	}
	newest.Status = state.ApprovalStatusExecuted
	newest.Delivery.Verified = true
	newest.Delivery.FinishedAt = now.Add(time.Minute)
	sess := &state.Session{IssueNumber: 1, PRNumber: 1, Status: state.StatusCodeLanded}
	if !o.reconcileCodeLandedDelivery(s, sess) {
		t.Fatal("verified descendant delivery should cover the superseded older merge")
	}
	if sess.DeploymentFinishedAt == nil || !sess.DeploymentFinishedAt.Equal(newest.Delivery.FinishedAt) {
		t.Fatalf("DeploymentFinishedAt = %v, want %v", sess.DeploymentFinishedAt, newest.Delivery.FinishedAt)
	}
}
