package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates every test in this package from the HOST's real emergency
// switch DB (~/.maestro/maestro.db). Since #1150, a daemon started under an
// active emergency KILLS the PIDs recorded in its state files on startup; a
// test that seeds sessions with arbitrary PIDs and boots Daemon.Run on a host
// whose real switch is engaged (e.g. the runtime host during a deliberate
// fleet stop) would otherwise fire real signals and corrupt its own seed
// state. Tests that want an emergency switch set Options.EmergencyDBPath
// explicitly (see emergency_test.go).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "maestro-daemon-test-emergency-")
	if err != nil {
		panic(err)
	}
	defaultEmergencyDBPath = func() string {
		return filepath.Join(dir, "emergency.db")
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
