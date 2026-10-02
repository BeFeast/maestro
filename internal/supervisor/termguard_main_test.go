package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/termguard/termguardtest"
)

// TestMain refuses every real process termination this test binary does not
// own, so a fixture with a literal PID or tmux session name fails loudly
// instead of signalling an unrelated process on the host (#1252).
func TestMain(m *testing.M) {
	termguardtest.Main(m)
}
