// Package tmpfshygiene defines Maestro's conservative, protect-aware tmpfs
// hygiene contract. The sweep implementation is Linux-only, while its summary
// remains portable so Fleet can expose the latest daemon result on every build.
package tmpfshygiene

import (
	"context"
	"time"
)

const (
	// PressureThresholdPct is the post-sweep tmpfs utilization that Fleet
	// promotes to the tmpfs_pressure attention signal.
	PressureThresholdPct = 85

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
	Timestamp        time.Time                `json:"timestamp"`
	Mode             string                   `json:"mode"`
	Root             string                   `json:"root"`
	Tmpfs            bool                     `json:"tmpfs"`
	UsePct           int                      `json:"use_pct"`
	Pressure         bool                     `json:"pressure"`
	AttentionCode    string                   `json:"attention_code,omitempty"`
	ScannedEntries   int                      `json:"scanned_entries"`
	MatchedEntries   int                      `json:"matched_entries"`
	ProtectedEntries int                      `json:"protected_entries"`
	DeletedEntries   int                      `json:"deleted_entries"`
	PartialEntries   int                      `json:"partial_entries,omitempty"`
	ReclaimableBytes int64                    `json:"reclaimable_bytes"`
	FreedBytes       int64                    `json:"freed_bytes"`
	Categories       map[string]CategoryStats `json:"categories"`
	ProtectHits      map[string]int           `json:"protect_hits"`
	ProcScanErrors   int                      `json:"proc_scan_errors,omitempty"`
	Error            string                   `json:"error,omitempty"`
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
	Now            func() time.Time
	InspectMount   func(string) (MountUsage, error)
	EffectiveUID   func() int
	processID      func() int
	beforeApply    func(string) // deterministic isolation/revalidation test hook
	removeEntry    func(context.Context, int, string, uint64) (removalResult, error)
}
