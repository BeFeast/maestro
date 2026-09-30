package aiexecution

import (
	"errors"
	"testing"
)

func TestRevisionInvalidatesSnapshotsEvenWhenStrictWasDisabled(t *testing.T) {
	old := new(Revision).Bind(Policy{})
	next := old.BindNext(Policy{RequireVerifiedRoute: true})
	assertExecutionHold(t, old.CheckCurrent(), "project_config_changed")
	if err := next.CheckCurrent(); err != nil {
		t.Fatal(err)
	}
	next.Invalidate()
	assertExecutionHold(t, next.CheckCurrent(), "project_config_changed")
}

func TestLegacyControllerIgnoresUnavailableDurableRevision(t *testing.T) {
	oldWrite := writeControllerRevision
	defer func() { writeControllerRevision = oldWrite }()
	writes := 0
	writeControllerRevision = func(string, []byte) error {
		writes++
		return errors.New("synthetic fsync failure")
	}
	legacy := (Policy{}).BindController(Policy{}, t.TempDir())
	if err := legacy.CheckCurrent(); err != nil || writes != 0 {
		t.Fatalf("legacy controller depends on durable revision: %v (writes=%d)", err, writes)
	}
	strict := legacy.BindController(Policy{RequireVerifiedRoute: true}, t.TempDir())
	assertExecutionHold(t, legacy.CheckCurrent(), "project_config_changed")
	assertExecutionHold(t, strict.CheckCurrent(), "controller_revision_unavailable")
	// Rebinding a copied strict policy must clear private unavailable/pin state.
	copy := strict
	copy.RequireVerifiedRoute = false
	copy.controllerPin = &FileProof{Path: "/nonexistent"}
	next := strict.BindController(copy, "relative-and-unavailable")
	if err := next.CheckCurrent(); err != nil || next.controllerPin != nil || next.controllerLease != nil {
		t.Fatalf("legacy reload retained strict-only pin state: %v", err)
	}
	if err := next.Invalidate(); err != nil {
		t.Fatalf("legacy invalidation depends on disk: %v", err)
	}
	assertExecutionHold(t, next.CheckCurrent(), "project_config_changed")
}

func TestDetachedControllerLeaseRevokedEvenWhenDurableInvalidationFails(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{RequireVerifiedRoute: true}
	policy = policy.BindController(policy, dir)
	pin, err := policy.ControllerPin()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := policy.LiveControllerLease()
	if err != nil {
		t.Fatal(err)
	}
	oldWrite := writeControllerRevision
	defer func() { writeControllerRevision = oldWrite }()
	writeControllerRevision = func(string, []byte) error { return errors.New("synthetic fsync failure") }
	if err := policy.Invalidate(); err == nil {
		t.Fatal("durable failure hidden")
	}
	writeControllerRevision = oldWrite
	if err := VerifyFile(pin); err != nil {
		t.Fatal("fixture should retain old durable pin", err)
	}
	if err := lease.Verify(); err == nil {
		t.Fatal("detached launch accepted stale durable pin after owner revocation")
	}
	next := policy.BindController(Policy{RequireVerifiedRoute: true}, dir)
	defer next.Invalidate()
	if err := next.CheckCurrent(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Verify(); err == nil {
		t.Fatal("fd reuse resurrected old lease")
	}
}
