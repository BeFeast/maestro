package aiexecution

import "testing"

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
