package tmpfshygiene

import (
	"errors"
	"testing"
	"time"
)

// Sample must stand on its own: the sweeper refuses non-tmpfs roots and can
// reclaim nothing at all (#1125), so the pressure signal cannot be a by-product
// of a completed sweep (#1128).
func TestSampleEvaluatesBothFloorsWithoutRunningASweep(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	const total = int64(16) << 30

	cases := []struct {
		name          string
		available     int64
		wantPressure  bool
		wantSpawnHold bool
	}{
		{name: "healthy", available: 12 << 30},
		{name: "below pressure floor only", available: 6 << 30, wantPressure: true},
		{name: "below both floors", available: 3 << 30, wantPressure: true, wantSpawnHold: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			available := tc.available
			snapshot := Sample(SampleOptions{
				Root:               "/fake/tmp",
				PressureFloorBytes: DefaultPressureFreeBytes,
				SpawnFloorBytes:    DefaultSpawnFreeBytes,
				Now:                func() time.Time { return now },
				InspectMount: func(string) (MountUsage, error) {
					return MountUsage{Tmpfs: true, TotalBytes: total, UsedBytes: total - available, AvailableBytes: available}, nil
				},
			})
			if snapshot.Error != "" {
				t.Fatalf("snapshot error = %q", snapshot.Error)
			}
			if snapshot.Pressure != tc.wantPressure || snapshot.SpawnHold != tc.wantSpawnHold {
				t.Fatalf("snapshot = %+v, want pressure=%v spawn_hold=%v", snapshot, tc.wantPressure, tc.wantSpawnHold)
			}
			if snapshot.AvailableBytes != available || snapshot.PressureFloorBytes != DefaultPressureFreeBytes {
				t.Fatalf("snapshot budget = %+v, want the absolute bytes it decided on", snapshot)
			}
		})
	}
}

// A measurement failure must page nobody and pause nobody: an unreadable mount
// is a bug in the sample, not evidence that the host is out of memory.
func TestSampleFailsOpenWhenTheMountCannotBeRead(t *testing.T) {
	snapshot := Sample(SampleOptions{
		Root:               "/fake/tmp",
		PressureFloorBytes: DefaultPressureFreeBytes,
		SpawnFloorBytes:    DefaultSpawnFreeBytes,
		InspectMount:       func(string) (MountUsage, error) { return MountUsage{}, errors.New("statfs boom") },
	})
	if snapshot.Error == "" {
		t.Fatal("snapshot did not record the sample failure")
	}
	if snapshot.Pressure || snapshot.SpawnHold {
		t.Fatalf("snapshot = %+v, want both signals off on a failed sample", snapshot)
	}
}

func TestBelowFloorRejectsUnusableFloors(t *testing.T) {
	if BelowFloor(0, 16<<30, 0) {
		t.Fatal("a zero floor must disable the signal")
	}
	if BelowFloor(0, 16<<30, -1) {
		t.Fatal("a negative floor must disable the signal")
	}
	// A floor at or above the mount could only ever report "always breached",
	// which is a permanent page and a permanent pause. Ignore it instead.
	if BelowFloor(1<<30, 2<<30, 4<<30) {
		t.Fatal("a floor larger than the mount must be ignored, not always true")
	}
	if !BelowFloor(1<<30, 16<<30, 4<<30) {
		t.Fatal("a usable floor must still fire")
	}
	// An unknown mount size leaves the floor applied as given.
	if !BelowFloor(1<<30, 0, 4<<30) {
		t.Fatal("an unknown total must not disable the floor")
	}
}
