package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/emergencystore"
)

func openEmergency(t *testing.T) (*emergencystore.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maestro.db")
	store, err := emergencystore.Open(path)
	if err != nil {
		t.Fatalf("open emergency store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

// TestConfigureEmergencyStop_SeedsFromExistingSwitch covers the daemon-down
// path (#840 AC: "flag written to DB directly, honored on next daemon start"):
// a switch set while the daemon was down must seed the gate cache on startup so
// the very first cycle already halts.
func TestConfigureEmergencyStop_SeedsFromExistingSwitch(t *testing.T) {
	store, path := openEmergency(t)
	if err := store.Set(context.Background(), emergencystore.LevelLLMStopped, "oleg", "burn", time.Now().UTC()); err != nil {
		t.Fatalf("pre-set switch: %v", err)
	}

	d := New(fakeLoader{cfgs: nil}, Options{EmergencyDBPath: path})
	opened := d.configureEmergencyStop(context.Background(), nil)
	if opened == nil {
		t.Fatal("configureEmergencyStop returned nil for a valid db")
	}
	defer opened.Close()

	if !d.emergencyLLMHalt() {
		t.Fatal("LLM halt not seeded from an existing switch on startup")
	}
	if !d.emergencySpawnHalt() {
		t.Fatal("spawn halt not seeded from an existing switch on startup")
	}
}

// TestWatchEmergencyLoop_PicksUpSetAndResume covers the live-pickup path (#840
// AC: "takes effect within one cycle WITHOUT daemon restart" — the CLI writes
// the switch directly and the daemon picks it up via the watch loop). The gate
// closures flip within one poll interval of a direct DB write and flip back on
// resume.
func TestWatchEmergencyLoop_PicksUpSetAndResume(t *testing.T) {
	store, _ := openEmergency(t)
	d := New(fakeLoader{cfgs: nil}, Options{})
	d.emergency = store

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.watchEmergencyLoop(ctx, 5*time.Millisecond)

	// Not halted before any switch is written.
	if d.emergencyLLMHalt() {
		t.Fatal("halted before any switch write")
	}

	if err := store.Set(context.Background(), emergencystore.LevelAllStopped, "oleg", "incident", time.Now().UTC()); err != nil {
		t.Fatalf("set switch: %v", err)
	}
	waitFor(t, func() bool { return d.emergencySpawnHalt() && d.emergencyLLMHalt() })

	if err := store.Resume(context.Background(), "operator", time.Now().UTC()); err != nil {
		t.Fatalf("resume switch: %v", err)
	}
	waitFor(t, func() bool { return !d.emergencySpawnHalt() && !d.emergencyLLMHalt() })
}
