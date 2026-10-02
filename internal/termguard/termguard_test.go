package termguard

import (
	"errors"
	"testing"
)

func TestCheckWithoutHookAllows(t *testing.T) {
	if err := Check(Attempt{Op: OpKillProcessTree, PID: 4242}); err != nil {
		t.Fatalf("Check without a hook = %v, want nil (production path)", err)
	}
}

func TestSetHookForTestingInstallsAndRestores(t *testing.T) {
	refuse := errors.New("refused")
	var seen []Attempt
	restoreOuter := SetHookForTesting(func(a Attempt) error {
		seen = append(seen, a)
		return refuse
	})
	attempt := Attempt{Op: OpKillTmuxSession, Target: "maestro-x-1"}
	if err := Check(attempt); !errors.Is(err, refuse) {
		t.Fatalf("Check with refusing hook = %v, want %v", err, refuse)
	}
	if len(seen) != 1 || seen[0] != attempt {
		t.Fatalf("hook saw %v, want [%v]", seen, attempt)
	}

	restoreInner := SetHookForTesting(func(Attempt) error { return nil })
	if err := Check(attempt); err != nil {
		t.Fatalf("Check with allowing inner hook = %v, want nil", err)
	}
	restoreInner()
	if err := Check(attempt); !errors.Is(err, refuse) {
		t.Fatalf("Check after inner restore = %v, want the outer hook's %v", err, refuse)
	}
	restoreOuter()
	if err := Check(attempt); err != nil {
		t.Fatalf("Check after outer restore = %v, want nil", err)
	}
}

func TestAttemptString(t *testing.T) {
	for _, tc := range []struct {
		a    Attempt
		want string
	}{
		{Attempt{Op: OpKillProcessTree, PID: 4242}, "kill-process-tree pid=4242"},
		{Attempt{Op: OpKillTmuxSession, Target: "maestro-x-1"}, `kill-tmux-session session="maestro-x-1"`},
	} {
		if got := tc.a.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}
