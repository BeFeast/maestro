package worker

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

func TestIsDockerRunWrapper(t *testing.T) {
	cases := []struct {
		cmdline string
		want    bool
	}{
		{"sudo docker run --name verify-x alpine true", true},
		{"docker run --name verify-x alpine true", true},
		{"/usr/bin/docker run --rm alpine", true},
		{"sudo /usr/bin/docker run --name c img", true},
		{"docker ps -a", false},
		{"docker rm -f c", false},
		{"dockerd --experimental", false},
		{"my-docker-runner run x", false}, // basename is not exactly "docker"
		{"", false},
	}
	for _, tc := range cases {
		if got := isDockerRunWrapper(tc.cmdline); got != tc.want {
			t.Errorf("isDockerRunWrapper(%q) = %v, want %v", tc.cmdline, got, tc.want)
		}
	}
}

func TestParseWrapperContainer(t *testing.T) {
	cases := []struct {
		cmdline  string
		wantName string
		wantOK   bool
	}{
		{"sudo docker run --name verify-826 alpine true", "verify-826", true},
		{"docker run --name=verify-826 alpine true", "verify-826", true},
		{"docker run --rm alpine true", "", false}, // anonymous container
		{"docker run --name", "", false},           // dangling flag
		{"docker ps -a", "", false},                // not a run wrapper
	}
	for _, tc := range cases {
		name, ok := parseWrapperContainer(tc.cmdline)
		if name != tc.wantName || ok != tc.wantOK {
			t.Errorf("parseWrapperContainer(%q) = (%q,%v), want (%q,%v)",
				tc.cmdline, name, ok, tc.wantName, tc.wantOK)
		}
	}
}

func TestParseDockerContainers(t *testing.T) {
	out := "verify-826\tExited\tabc123\tsess-old\n" +
		"active-verify\trunning\tdef456\tsess-live\n" +
		"\t\t\t\n" + // blank/garbage line ignored
		"leftover\tdead\tghi789\t\n"
	got := parseDockerContainers(out)
	want := []ContainerState{
		{Name: "verify-826", Running: false, ID: "abc123", Session: "sess-old"},
		{Name: "active-verify", Running: true, ID: "def456", Session: "sess-live"},
		{Name: "leftover", Running: false, ID: "ghi789", Session: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDockerContainers mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestSelectOrphanContainerReaps is the acceptance-5 regression: a genuinely
// active container is never reaped, while a terminal container backing a
// superseded/parentless wrapper is.
func TestSelectOrphanContainerReaps(t *testing.T) {
	containers := []ContainerState{
		{Name: "verify-826", Running: false, Session: "sess-old"}, // terminal, superseded
		{Name: "active-verify", Running: true, Session: "sess-live"},
		{Name: "quiet-live", Running: false, Session: "sess-live2"}, // terminal but session still live
	}
	wrappers := []WrapperProc{
		{PID: 100, PPID: 1, Container: "verify-826"},       // orphan → reap
		{PID: 200, PPID: 4321, Container: "active-verify"}, // live parent + running container → keep
		{PID: 300, PPID: 1, Container: "quiet-live"},       // parentless but session live → keep
	}
	liveSessions := map[string]bool{"sess-live": true, "sess-live2": true}

	plan := selectOrphanContainerReaps(containers, wrappers, liveSessions)

	if !reflect.DeepEqual(plan.KillPIDs, []int{100}) {
		t.Errorf("KillPIDs = %v, want [100]", plan.KillPIDs)
	}
	if !reflect.DeepEqual(plan.RemoveContainers, []string{"verify-826"}) {
		t.Errorf("RemoveContainers = %v, want [verify-826]", plan.RemoveContainers)
	}
}

// TestSelectOrphanContainerReaps_IncidentChain reproduces the incident: a
// three-process `sudo docker run` wrapper chain, all reparented to init, whose
// named container exited 127. No session correlation is available (empty
// Session, nil liveSessions) so the parentless + terminal signal alone reaps it.
func TestSelectOrphanContainerReaps_IncidentChain(t *testing.T) {
	containers := []ContainerState{
		{Name: "pkg-verify", Running: false, Session: ""},
	}
	wrappers := []WrapperProc{
		{PID: 111, PPID: 1, Container: "pkg-verify"},
		{PID: 112, PPID: 1, Container: "pkg-verify"},
		{PID: 113, PPID: 1, Container: "pkg-verify"},
	}
	plan := selectOrphanContainerReaps(containers, wrappers, nil)

	sort.Ints(plan.KillPIDs)
	if !reflect.DeepEqual(plan.KillPIDs, []int{111, 112, 113}) {
		t.Errorf("KillPIDs = %v, want [111 112 113]", plan.KillPIDs)
	}
	if !reflect.DeepEqual(plan.RemoveContainers, []string{"pkg-verify"}) {
		t.Errorf("RemoveContainers = %v, want [pkg-verify]", plan.RemoveContainers)
	}
}

// A terminal container is not removed while any preserved (still-live) wrapper
// references it — even if another parentless wrapper for the same container is
// reaped.
func TestSelectOrphanContainerReaps_PreservedWrapperBlocksRemoval(t *testing.T) {
	containers := []ContainerState{{Name: "shared", Running: false, Session: ""}}
	wrappers := []WrapperProc{
		{PID: 100, PPID: 1, Container: "shared"},   // orphan
		{PID: 200, PPID: 999, Container: "shared"}, // live parent → preserve container
	}
	plan := selectOrphanContainerReaps(containers, wrappers, nil)

	if !reflect.DeepEqual(plan.KillPIDs, []int{100}) {
		t.Errorf("KillPIDs = %v, want [100]", plan.KillPIDs)
	}
	if len(plan.RemoveContainers) != 0 {
		t.Errorf("RemoveContainers = %v, want none (live wrapper still holds it)", plan.RemoveContainers)
	}
}

// A parentless wrapper referencing an unknown/absent container is left alone: we
// cannot positively confirm the container is terminal, so we never guess.
func TestSelectOrphanContainerReaps_UnknownContainerLeftAlone(t *testing.T) {
	wrappers := []WrapperProc{{PID: 100, PPID: 1, Container: "ghost"}}
	plan := selectOrphanContainerReaps(nil, wrappers, nil)
	if len(plan.KillPIDs) != 0 || len(plan.RemoveContainers) != 0 {
		t.Errorf("expected no action for absent container, got %+v", plan)
	}
}

// A wrapper for a container maestro did not launch (no reap) never causes a
// removal — unrelated exited host containers are untouched.
func TestSelectOrphanContainerReaps_UnrelatedExitedContainerUntouched(t *testing.T) {
	containers := []ContainerState{{Name: "someone-elses", Running: false, Session: ""}}
	plan := selectOrphanContainerReaps(containers, nil, nil)
	if len(plan.RemoveContainers) != 0 {
		t.Errorf("RemoveContainers = %v, want none (no maestro wrapper reaped)", plan.RemoveContainers)
	}
}

func TestSweepOrphanContainerWrappers(t *testing.T) {
	containers := []ContainerState{
		{Name: "verify-826", Running: false, Session: ""},
		{Name: "active", Running: true, Session: ""},
	}
	wrappers := []WrapperProc{
		{PID: 100, PPID: 1, Container: "verify-826"},
		{PID: 200, PPID: 5, Container: "active"},
	}
	var killed []int
	var removed []string
	deps := reaperDeps{
		listContainers:  func() ([]ContainerState, error) { return containers, nil },
		killGroup:       func(pid int) { killed = append(killed, pid) },
		removeContainer: func(name string) error { removed = append(removed, name); return nil },
	}

	report, err := sweepOrphanContainerWrappers(wrappers, deps, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !reflect.DeepEqual(killed, []int{100}) {
		t.Errorf("killed = %v, want [100]", killed)
	}
	if !reflect.DeepEqual(removed, []string{"verify-826"}) {
		t.Errorf("removed = %v, want [verify-826]", removed)
	}
	if !reflect.DeepEqual(report.KilledWrappers, []int{100}) || !reflect.DeepEqual(report.RemovedContainers, []string{"verify-826"}) {
		t.Errorf("report = %+v, want killed [100] removed [verify-826]", report)
	}
	if report.Empty() {
		t.Error("report.Empty() = true, want false")
	}
}

// A container-removal failure must not be recorded as removed evidence, and must
// not abort the pass.
func TestSweepOrphanContainerWrappers_RemoveError(t *testing.T) {
	deps := reaperDeps{
		listContainers: func() ([]ContainerState, error) {
			return []ContainerState{{Name: "verify-826", Running: false}}, nil
		},
		killGroup:       func(pid int) {},
		removeContainer: func(name string) error { return errors.New("busy") },
	}
	report, err := sweepOrphanContainerWrappers(
		[]WrapperProc{{PID: 100, PPID: 1, Container: "verify-826"}}, deps, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(report.RemovedContainers) != 0 {
		t.Errorf("RemovedContainers = %v, want none on remove error", report.RemovedContainers)
	}
	if !reflect.DeepEqual(report.KilledWrappers, []int{100}) {
		t.Errorf("KilledWrappers = %v, want [100]", report.KilledWrappers)
	}
}

func TestSweepOrphanContainerWrappers_ListError(t *testing.T) {
	deps := reaperDeps{
		listContainers: func() ([]ContainerState, error) { return nil, errors.New("docker down") },
	}
	if _, err := sweepOrphanContainerWrappers([]WrapperProc{{PID: 1, PPID: 1, Container: "x"}}, deps, nil); err == nil {
		t.Fatal("expected error when container listing fails")
	}
}
