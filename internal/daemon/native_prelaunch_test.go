package daemon

import (
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/orchestrator"
	"testing"
)

func TestNativePrelaunchDecisionsConsumedOnlyByExactProjectOnce(t *testing.T) {
	requests := []orchestrator.NativePrelaunchRecovery{{ProjectID: "a", Slot: "a-1"}, {ProjectID: "b", Slot: "b-1"}}
	d := New(nil, Options{NativePrelaunchRecoveries: requests})
	requests[0].ProjectID = "foreign"
	if got := d.takeNativePrelaunchRecoveries("foreign"); len(got) != 0 {
		t.Fatal("caller widened startup decisions")
	}
	if got := d.takeNativePrelaunchRecoveries("a"); len(got) != 1 || got[0].Slot != "a-1" {
		t.Fatal(got)
	}
	if got := d.takeNativePrelaunchRecoveries("a"); len(got) != 0 {
		t.Fatal("flow restart replayed startup decision")
	}
	if got := d.takeNativePrelaunchRecoveries("b"); len(got) != 1 || got[0].Slot != "b-1" {
		t.Fatal(got)
	}
}

func TestNativePrelaunchProjectMustBeSelectedUnambiguously(t *testing.T) {
	r := []orchestrator.NativePrelaunchRecovery{{ProjectID: "a"}}
	one := namedConfig{cfg: &config.Config{ProjectID: "a"}}
	if err := validateNativePrelaunchProjects([]namedConfig{one}, r); err != nil {
		t.Fatal(err)
	}
	for _, projects := range [][]namedConfig{nil, {{cfg: &config.Config{ProjectID: "b"}}}, {one, one}} {
		if err := validateNativePrelaunchProjects(projects, r); err == nil {
			t.Fatal("unselected/ambiguous project accepted")
		}
	}
}
