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
