// Package tmpfshygiene defines Maestro's conservative, protect-aware tmpfs
// hygiene contract. The sweep implementation is Linux-only, while its summary
// remains portable so Fleet can expose the latest daemon result on every build.
package tmpfshygiene

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultPressureFreeBytes is the absolute free-byte budget below which the
	// tmpfs root counts as under pressure. It is deliberately NOT a percentage
	// of the mount (#1128): /tmp on the fleet host is a RAM-backed ~16GiB tmpfs
	// on a 24GiB machine with zero swap, so the old "85% of the tmpfs" rule only
	// fired once 13.2GiB — 55% of host RAM — was already gone. The 2026-07-25
	// event peaked at 8.5GB used and never tripped it. Remaining bytes, not the
	// utilization ratio, decide whether the next allocation becomes an outage.
	DefaultPressureFreeBytes int64 = 8 << 30

	// DefaultSpawnFreeBytes is the free-byte budget below which new worker
	// dispatch pauses. It sits below DefaultPressureFreeBytes on purpose: the
	// operator is paged first, and throughput is only given up once the
	// remaining headroom would not comfortably hold another worker's scratch.
	DefaultSpawnFreeBytes int64 = 4 << 30

	ModeDryRun = "dry-run"
	ModeApply  = "apply"
)

// MountUsage is the filesystem identity and capacity snapshot used before and
// after a sweep. InspectMount is injectable so tests never inspect or mutate the
// live host /tmp.
type MountUsage struct {
	Tmpfs          bool  `json:"tmpfs"`
	UsePct         int   `json:"use_pct"`
	TotalBytes     int64 `json:"total_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

// CategoryStats reports what each allowlisted category matched and reclaimed.
type CategoryStats struct {
	Candidates       int   `json:"candidates"`
	Protected        int   `json:"protected"`
	Deleted          int   `json:"deleted"`
	Partial          int   `json:"partial,omitempty"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	FreedBytes       int64 `json:"freed_bytes"`
}

// Summary is emitted as one JSON line by every CLI or scheduled sweep.
type Summary struct {
	Timestamp time.Time `json:"timestamp"`
	Mode      string    `json:"mode"`
	Root      string    `json:"root"`
	Tmpfs     bool      `json:"tmpfs"`
	UsePct    int       `json:"use_pct"`
	// TotalBytes/AvailableBytes carry the post-sweep capacity so the JSONL
	// metric and the Fleet snapshot show the same absolute budget the pressure
	// decision was made on, rather than only the ratio (#1128).
	TotalBytes         int64                    `json:"total_bytes"`
	AvailableBytes     int64                    `json:"available_bytes"`
	PressureFloorBytes int64                    `json:"pressure_floor_bytes,omitempty"`
	Pressure           bool                     `json:"pressure"`
	AttentionCode      string                   `json:"attention_code,omitempty"`
	ScannedEntries     int                      `json:"scanned_entries"`
	MatchedEntries     int                      `json:"matched_entries"`
	ProtectedEntries   int                      `json:"protected_entries"`
	DeletedEntries     int                      `json:"deleted_entries"`
	PartialEntries     int                      `json:"partial_entries,omitempty"`
	ReclaimableBytes   int64                    `json:"reclaimable_bytes"`
	FreedBytes         int64                    `json:"freed_bytes"`
	Categories         map[string]CategoryStats `json:"categories"`
	ProtectHits        map[string]int           `json:"protect_hits"`
	ProcScanErrors     int                      `json:"proc_scan_errors,omitempty"`
	// ProcPermissionSkips counts /proc entries the sweeper could not inspect
	// because they belong to other users (EACCES). These are expected on any
	// multi-user host and never blanket-protect candidates; they are surfaced
	// for observability only.
	ProcPermissionSkips int    `json:"proc_permission_skips,omitempty"`
	Error               string `json:"error,omitempty"`
}

// PressureSnapshot is one capacity reading of the tmpfs root, taken
// independently of any sweep. Decoupling the sample from Sweep is the point
// (#1128): the sweeper can refuse a non-tmpfs root or reclaim nothing at all
// (#1125), and the operator alert plus the spawn precondition must still have a
// current signal to act on.
type PressureSnapshot struct {
	Timestamp          time.Time `json:"timestamp"`
	Root               string    `json:"root"`
	Tmpfs              bool      `json:"tmpfs"`
	TotalBytes         int64     `json:"total_bytes"`
	AvailableBytes     int64     `json:"available_bytes"`
	UsePct             int       `json:"use_pct"`
	PressureFloorBytes int64     `json:"pressure_floor_bytes"`
	SpawnFloorBytes    int64     `json:"spawn_floor_bytes"`
	// Pressure is the alert condition: free bytes below PressureFloorBytes.
	Pressure bool `json:"pressure"`
	// SpawnHold is the dispatch condition: free bytes below SpawnFloorBytes.
	SpawnHold bool `json:"spawn_hold"`
	// HeldSpawns counts dispatches the precondition has paused since the daemon
	// started. It is how an operator tells a deliberate throughput pause apart
	// from a silently idle fleet.
	HeldSpawns uint64 `json:"held_spawns"`
	Error      string `json:"error,omitempty"`
}

// SampleOptions configures one sweep-independent capacity reading.
type SampleOptions struct {
	Root               string
	PressureFloorBytes int64
	SpawnFloorBytes    int64
	Now                func() time.Time
	InspectMount       func(string) (MountUsage, error)
}

// Sample takes one capacity reading of the tmpfs root and evaluates both
// absolute floors against it.
//
// A failed reading yields Pressure=false and SpawnHold=false with Error set:
// the measurement failing must never page the operator, and it must never park
// the fleet. Both signals fail open.
func Sample(opts SampleOptions) PressureSnapshot {
	if strings.TrimSpace(opts.Root) == "" {
		opts.Root = "/tmp"
	}
	opts.Root = filepath.Clean(opts.Root)
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.InspectMount == nil {
		opts.InspectMount = InspectLinuxMount
	}
	snapshot := PressureSnapshot{
		Timestamp:          opts.Now().UTC(),
		Root:               opts.Root,
		PressureFloorBytes: opts.PressureFloorBytes,
		SpawnFloorBytes:    opts.SpawnFloorBytes,
	}
	usage, err := opts.InspectMount(opts.Root)
	if err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}
	snapshot.Tmpfs = usage.Tmpfs
	snapshot.TotalBytes = usage.TotalBytes
	snapshot.AvailableBytes = usage.AvailableBytes
	snapshot.UsePct = usage.UsePct
	snapshot.Pressure = BelowFloor(usage.AvailableBytes, usage.TotalBytes, opts.PressureFloorBytes)
	snapshot.SpawnHold = BelowFloor(usage.AvailableBytes, usage.TotalBytes, opts.SpawnFloorBytes)
	return snapshot
}

// BelowFloor reports whether availableBytes sits under an absolute floor.
//
// A non-positive floor disables the signal. A floor at or above the mount's own
// size is a misconfiguration that could only ever report "always breached", so
// it is ignored rather than turned into a permanent page or a permanent freeze;
// callers log it once. totalBytes==0 means the caller did not supply a size and
// the floor is applied as given.
func BelowFloor(availableBytes, totalBytes, floorBytes int64) bool {
	if floorBytes <= 0 {
		return false
	}
	if totalBytes > 0 && floorBytes >= totalBytes {
		return false
	}
	return availableBytes < floorBytes
}

type removalResult struct {
	freedBytes     int64
	removedObjects int
}

// Options supplies the sweep roots and safety dependencies. Production uses
// /tmp, /proc, InspectLinuxMount, and the process effective uid.
type Options struct {
	Root           string
	ProcRoot       string
	Mode           string
	ProtectedPaths []string
	// PressureFloorBytes is the absolute free-byte budget the post-sweep
	// pressure verdict is measured against. Zero takes
	// DefaultPressureFreeBytes; negative disables the verdict.
	PressureFloorBytes int64
	Now                func() time.Time
	InspectMount       func(string) (MountUsage, error)
	EffectiveUID       func() int
	processID          func() int
	beforeApply        func(string) // deterministic isolation/revalidation test hook
	removeEntry        func(context.Context, int, string, uint64) (removalResult, error)
}
