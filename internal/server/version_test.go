package server

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// #698: the state response carries the running binary's version so the
// self-deploy health probe can confirm a restart picked up the new build.
func TestStateResponseIncludesBinaryVersion(t *testing.T) {
	SetBinaryVersion("1.4.2+gabc1234")
	t.Cleanup(func() { SetBinaryVersion("") })

	cfg := &config.Config{Repo: "owner/repo"}
	resp := buildStateResponse(cfg, &state.State{})
	if resp.Version != "1.4.2+gabc1234" {
		t.Fatalf("state response version = %q, want 1.4.2+gabc1234", resp.Version)
	}
}

func TestFleetSnapshotIncludesBinaryVersion(t *testing.T) {
	SetBinaryVersion("1.4.2+gabc1234")
	t.Cleanup(func() { SetBinaryVersion("") })

	s := NewFleet(nil, "127.0.0.1", 0, true)
	resp := s.snapshot()
	if resp.Version != "1.4.2+gabc1234" {
		t.Fatalf("fleet snapshot version = %q, want 1.4.2+gabc1234", resp.Version)
	}
}
