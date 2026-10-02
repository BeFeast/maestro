// Package termguardtest installs the termguard hook for a test binary.
//
// Packages whose tests build sessions with literal worker PIDs or tmux session
// names call Main from TestMain. Under the guard a test may only terminate a
// process it spawned itself; any other real termination attempt is refused
// before a signal is sent, panics in the offending test, and fails the binary
// even if production code recovered the panic (#1252).
package termguardtest

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/befeast/maestro/internal/termguard"
)

// maxAncestry bounds the parent walk so a corrupt /proc read cannot loop.
const maxAncestry = 128

// refused counts unexpected refusals in this test binary, including ones whose
// panic production code recovered.
var refused atomic.Int64

// Refusal is the panic value of a refused termination attempt.
type Refusal struct {
	Err error
}

func (r *Refusal) Error() string { return r.Err.Error() }

func (r *Refusal) Unwrap() error { return r.Err }

// Main runs m under the guard and exits with its result. Use it as the whole
// body of a package's TestMain, after any package-level seam replacements.
func Main(m *testing.M) {
	os.Exit(Run(m))
}

// Run runs m under the guard and returns the exit code. A run that refused any
// unexpected termination attempt never reports success.
func Run(m *testing.M) int {
	g := newGuard(os.Getpid(), parentPID)
	restore := termguard.SetHookForTesting(g.check)
	code := m.Run()
	restore()
	if n := refused.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "termguard: %d real process-termination attempt(s) were refused; see the panic output above (#1252)\n", n)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Refuse records a refused termination and panics with a *Refusal wrapping err
// so the offending test fails at the call site. Package-level seams that must
// never reach their production default under test call it from the
// replacement their TestMain installs.
func Refuse(err error) {
	refused.Add(1)
	panic(&Refusal{Err: err})
}

// ExpectRefusal runs fn, which must be refused by the guard, and returns the
// refusal. An expected refusal does not fail the binary; any other panic is
// propagated and a fn that completes without a refusal fails t.
func ExpectRefusal(t testing.TB, fn func()) (err error) {
	t.Helper()
	completed := false
	defer func() {
		if completed {
			return
		}
		r := recover()
		if r == nil {
			return // fn called t.FailNow; let the Goexit continue
		}
		var refusal *Refusal
		if e, ok := r.(error); ok && errors.As(e, &refusal) {
			refused.Add(-1)
			err = refusal.Err
			return
		}
		panic(r)
	}()
	fn()
	completed = true
	t.Fatalf("expected the termination guard to refuse, but the call completed")
	return nil
}

type guard struct {
	self     int
	parentOf func(pid int) (int, bool)
}

func newGuard(self int, parentOf func(int) (int, bool)) *guard {
	return &guard{self: self, parentOf: parentOf}
}

// check is the installed hook: anything decide rejects is refused before a
// signal is sent.
func (g *guard) check(a termguard.Attempt) error {
	if err := g.decide(a); err != nil {
		Refuse(err)
	}
	return nil
}

// decide allows only a process-tree kill rooted at a descendant of this test
// binary. Every other attempt targets something the test does not own: a
// literal fixture PID that may belong to an unrelated process, or a tmux
// session on a server shared with the rest of the host.
func (g *guard) decide(a termguard.Attempt) error {
	if a.Op == termguard.OpKillProcessTree && g.isDescendant(a.PID) {
		return nil
	}
	return fmt.Errorf("termguard: test reached a real %s it does not own; refused before signalling. Inject a fake process-control seam instead (#1252)", a)
}

func (g *guard) isDescendant(pid int) bool {
	if pid <= 0 || pid == g.self {
		return false
	}
	cur := pid
	for i := 0; i < maxAncestry; i++ {
		ppid, ok := g.parentOf(cur)
		if !ok || ppid <= 0 {
			return false
		}
		if ppid == g.self {
			return true
		}
		if ppid == 1 || ppid == cur {
			return false
		}
		cur = ppid
	}
	return false
}

// parentPID reads the parent of pid from /proc/<pid>/stat. The command name
// field may contain spaces or parentheses, so fields are split after the last
// ')'. Off Linux, or for a missing process, it reports false.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
