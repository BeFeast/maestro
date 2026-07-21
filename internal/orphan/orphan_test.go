package orphan

import (
	"reflect"
	"testing"
	"time"
)

func exitCode(n int) *int { return &n }

// Acceptance 1: reproduce a container that exits while the client wrapper
// remains blocked/parentless. The named container is Exited (code 127), its
// worktree is gone, the wrappers are a three-process chain with PPID==1, and the
// owning session is not live (superseded by a later authoritative session).
func incidentSnapshot(now time.Time) Snapshot {
	started := now.Add(-11 * time.Hour)
	return Snapshot{
		Now:    now,
		MinAge: 2 * time.Minute,
		Wrappers: []Wrapper{
			{PID: 101, PPID: 1, PGID: 100, ContainerName: "pkg-verify-42", SessionID: "sup-old", StartedAt: started},
			{PID: 102, PPID: 1, PGID: 100, ContainerName: "pkg-verify-42", SessionID: "sup-old", StartedAt: started},
			{PID: 103, PPID: 1, PGID: 100, ContainerName: "pkg-verify-42", SessionID: "sup-old", StartedAt: started},
		},
		Containers: []Container{
			{Name: "pkg-verify-42", State: ContainerExited, ExitCode: exitCode(127), SessionID: "sup-old", IssueNumber: 42, FinishedAt: started.Add(17 * time.Minute)},
		},
		// sup-old is absent from the live set: it was superseded by a later
		// authoritative session that already completed.
		LiveSessions: map[string]bool{"sup-new": true},
	}
}

func TestDecide_ReapsIncidentOrphan(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	plan := Decide(incidentSnapshot(now))

	if plan.Empty() {
		t.Fatal("incident orphan was not reaped")
	}
	if len(plan.TerminateGroups) != 1 {
		t.Fatalf("terminate groups = %d, want 1", len(plan.TerminateGroups))
	}
	g := plan.TerminateGroups[0]
	if g.PGID != 100 {
		t.Errorf("terminated pgid = %d, want 100", g.PGID)
	}
	if !reflect.DeepEqual(g.PIDs, []int{101, 102, 103}) {
		t.Errorf("terminated pids = %v, want the whole wrapper chain [101 102 103]", g.PIDs)
	}
	if g.Reason != ReapTerminalContainerNonLiveSession {
		t.Errorf("reap reason = %q, want terminal_container_nonlive_session", g.Reason)
	}
	if len(plan.RemoveContainers) != 1 || plan.RemoveContainers[0].Name != "pkg-verify-42" {
		t.Fatalf("remove containers = %+v, want the one exited container", plan.RemoveContainers)
	}
}

// Acceptance 2 + 5: an active container is never reaped, but a terminal
// superseded one is. Table drives the whole preserve/reap matrix so the safety
// invariants are pinned as one regression.
func TestDecide_PreservesActivePreservesLiveReapsTerminalSuperseded(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)

	cases := []struct {
		name         string
		wrapper      Wrapper
		container    Container
		live         map[string]bool
		wantReap     bool
		wantReapWhy  ReapReason
		wantPreserve PreserveReason
	}{
		{
			name:         "active running container is preserved",
			wrapper:      Wrapper{PID: 10, PPID: 1, PGID: 10, ContainerName: "c-live", SessionID: "s-live", StartedAt: old},
			container:    Container{Name: "c-live", State: ContainerRunning, SessionID: "s-live"},
			live:         map[string]bool{"s-live": true},
			wantReap:     false,
			wantPreserve: PreserveSessionLive,
		},
		{
			name:         "running container with dead session still preserved (active work)",
			wrapper:      Wrapper{PID: 11, PPID: 1, PGID: 11, ContainerName: "c-run", SessionID: "s-dead", StartedAt: old},
			container:    Container{Name: "c-run", State: ContainerRunning, SessionID: "s-dead"},
			live:         map[string]bool{},
			wantReap:     false,
			wantPreserve: PreserveContainerActive,
		},
		{
			name:         "terminal container but session still live is preserved",
			wrapper:      Wrapper{PID: 12, PPID: 1, PGID: 12, ContainerName: "c-exit-live", SessionID: "s-live", StartedAt: old},
			container:    Container{Name: "c-exit-live", State: ContainerExited, ExitCode: exitCode(0), SessionID: "s-live"},
			live:         map[string]bool{"s-live": true},
			wantReap:     false,
			wantPreserve: PreserveSessionLive,
		},
		{
			name:         "wrapper with a live parent is preserved",
			wrapper:      Wrapper{PID: 13, PPID: 4242, PGID: 13, ContainerName: "c-exit", SessionID: "s-dead", StartedAt: old},
			container:    Container{Name: "c-exit", State: ContainerExited, ExitCode: exitCode(1), SessionID: "s-dead"},
			live:         map[string]bool{},
			wantReap:     false,
			wantPreserve: PreserveWrapperHasLiveParent,
		},
		{
			name:         "unobservable container state is preserved (fail safe)",
			wrapper:      Wrapper{PID: 14, PPID: 1, PGID: 14, ContainerName: "c-unknown", SessionID: "s-dead", StartedAt: old},
			container:    Container{Name: "c-unknown", State: ContainerUnknown, SessionID: "s-dead"},
			live:         map[string]bool{},
			wantReap:     false,
			wantPreserve: PreserveContainerStateUnknown,
		},
		{
			name:        "terminal container + superseded session is reaped",
			wrapper:     Wrapper{PID: 15, PPID: 1, PGID: 15, ContainerName: "c-super", SessionID: "s-old", StartedAt: old},
			container:   Container{Name: "c-super", State: ContainerExited, ExitCode: exitCode(127), SessionID: "s-old"},
			live:        map[string]bool{"s-new": true},
			wantReap:    true,
			wantReapWhy: ReapTerminalContainerNonLiveSession,
		},
		{
			name:        "dead container + no session is reaped",
			wrapper:     Wrapper{PID: 16, PPID: 1, PGID: 16, ContainerName: "c-dead", SessionID: "", StartedAt: old},
			container:   Container{Name: "c-dead", State: ContainerDead, SessionID: ""},
			live:        map[string]bool{},
			wantReap:    true,
			wantReapWhy: ReapTerminalContainerNonLiveSession,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Decide(Snapshot{
				Now:          now,
				MinAge:       time.Minute,
				Wrappers:     []Wrapper{tc.wrapper},
				Containers:   []Container{tc.container},
				LiveSessions: tc.live,
			})
			if tc.wantReap {
				if len(plan.TerminateGroups) != 1 {
					t.Fatalf("terminate groups = %d, want 1 (should reap)", len(plan.TerminateGroups))
				}
				if got := plan.TerminateGroups[0].Reason; got != tc.wantReapWhy {
					t.Errorf("reap reason = %q, want %q", got, tc.wantReapWhy)
				}
				return
			}
			if len(plan.TerminateGroups) != 0 {
				t.Fatalf("terminate groups = %d, want 0 (should preserve)", len(plan.TerminateGroups))
			}
			if len(plan.RemoveContainers) != 0 {
				t.Fatalf("remove containers = %d, want 0 (should preserve)", len(plan.RemoveContainers))
			}
			if len(plan.Preserved) != 1 || plan.Preserved[0].Reason != tc.wantPreserve {
				t.Fatalf("preserved = %+v, want one with reason %q", plan.Preserved, tc.wantPreserve)
			}
		})
	}
}

// Acceptance 3 grace window: a just-launched parentless wrapper on a terminal
// container is preserved until it ages past MinAge, so the reaper never races a
// command that started moments ago.
func TestDecide_GraceWindowPreservesFreshWrapper(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{
		Now:          now,
		MinAge:       5 * time.Minute,
		Wrappers:     []Wrapper{{PID: 20, PPID: 1, PGID: 20, ContainerName: "c", SessionID: "s-old", StartedAt: now.Add(-30 * time.Second)}},
		Containers:   []Container{{Name: "c", State: ContainerExited, SessionID: "s-old"}},
		LiveSessions: map[string]bool{},
	}
	plan := Decide(snap)
	if !plan.Empty() {
		t.Fatalf("fresh wrapper reaped inside grace window: %s", plan)
	}
	if len(plan.Preserved) != 1 || plan.Preserved[0].Reason != PreserveWithinGrace {
		t.Fatalf("preserved = %+v, want within_grace_period", plan.Preserved)
	}

	// Past the grace window it is reaped.
	snap.Wrappers[0].StartedAt = now.Add(-10 * time.Minute)
	if plan := Decide(snap); plan.Empty() {
		t.Fatal("aged wrapper past grace window was not reaped")
	}
}

// A container is not removed while any wrapper naming it is still preserved
// (e.g. one wrapper is inside the grace window): removal waits until all its
// wrappers are safe to reap.
func TestDecide_ContainerRemovalWaitsForPreservedWrapper(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{
		Now:    now,
		MinAge: 5 * time.Minute,
		Wrappers: []Wrapper{
			{PID: 30, PPID: 1, PGID: 30, ContainerName: "c", SessionID: "s-old", StartedAt: now.Add(-time.Hour)},
			{PID: 31, PPID: 1, PGID: 31, ContainerName: "c", SessionID: "s-old", StartedAt: now.Add(-10 * time.Second)}, // fresh
		},
		Containers:   []Container{{Name: "c", State: ContainerExited, SessionID: "s-old"}},
		LiveSessions: map[string]bool{},
	}
	plan := Decide(snap)
	if len(plan.RemoveContainers) != 0 {
		t.Fatalf("container removed while a wrapper is still preserved: %+v", plan.RemoveContainers)
	}
	if len(plan.TerminateGroups) != 1 || plan.TerminateGroups[0].PGID != 30 {
		t.Fatalf("aged wrapper group not scheduled independently: %+v", plan.TerminateGroups)
	}
}

// Acceptance 2: an idle host with no orphans yields an empty plan and never a
// synthetic reap.
func TestDecide_IdleHostReapsNothing(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	plan := Decide(Snapshot{Now: now, LiveSessions: map[string]bool{"s": true}})
	if !plan.Empty() {
		t.Fatalf("idle host produced a reap: %s", plan)
	}
}
