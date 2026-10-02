// Package termguard is the choke point every real process-termination
// primitive consults before it acts: the signal sweep of a worker process tree
// and the exact kill of a tmux session.
//
// Production code never installs a hook, so Check is one atomic load that
// returns nil and every primitive behaves exactly as it did before the guard
// existed. Test binaries install a hook from TestMain (see package
// termguardtest) so a fixture that reaches a real kill path with a literal PID
// or session name is refused and fails loudly instead of terminating an
// unrelated process on the host (#1252).
package termguard

import (
	"fmt"
	"sync/atomic"
)

// Op names a termination primitive.
type Op string

const (
	// OpKillProcessTree signals a pid and every visible descendant.
	OpKillProcessTree Op = "kill-process-tree"
	// OpKillTmuxSession kills one tmux session, hanging up every process in
	// its panes.
	OpKillTmuxSession Op = "kill-tmux-session"
)

// Attempt describes one termination a primitive is about to perform.
type Attempt struct {
	Op Op
	// PID is the root process of an OpKillProcessTree attempt.
	PID int
	// Target is the session name of an OpKillTmuxSession attempt.
	Target string
}

func (a Attempt) String() string {
	switch a.Op {
	case OpKillProcessTree:
		return fmt.Sprintf("%s pid=%d", a.Op, a.PID)
	case OpKillTmuxSession:
		return fmt.Sprintf("%s session=%q", a.Op, a.Target)
	default:
		return fmt.Sprintf("%s pid=%d target=%q", a.Op, a.PID, a.Target)
	}
}

// Hook decides an attempt. A nil error lets the real primitive run; a non-nil
// error suppresses it, and primitives that return errors surface it.
type Hook func(Attempt) error

var hook atomic.Pointer[Hook]

// Check consults the installed hook. Without one, which is always the case in
// production, it returns nil and the caller proceeds unchanged.
func Check(a Attempt) error {
	h := hook.Load()
	if h == nil {
		return nil
	}
	return (*h)(a)
}

// SetHookForTesting installs h for the whole process and returns a function
// that restores the previous hook. It exists for test binaries only; production
// code must never call it.
func SetHookForTesting(h Hook) (restore func()) {
	var next *Hook
	if h != nil {
		next = &h
	}
	prev := hook.Swap(next)
	return func() { hook.Store(prev) }
}
