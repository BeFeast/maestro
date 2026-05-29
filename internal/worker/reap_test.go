package worker

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestSelectVisualQAKills_MatchesByCmdlineAndCwd covers the pure kill-selection
// decision used by the startup sweep. Full process killing touches syscalls and
// /proc and is not unit-tested here; this validates which PIDs the sweep WOULD
// target given a snapshot of processes.
func TestSelectVisualQAKills_MatchesByCmdlineAndCwd(t *testing.T) {
	procs := []procInfo{
		// Matches: cmdline references the visual-QA temp dir.
		{PID: 101, Cmdline: "chrome --user-data-dir=/tmp/scribe-visual-qa-abc123/profile"},
		// Matches: cwd is under the visual-QA temp dir (crashpad handler).
		{PID: 102, Cmdline: "chrome_crashpad_handler", Cwd: "/tmp/scribe-visual-qa-abc123"},
		// No match: generic chrome with an unrelated profile — must NOT be killed.
		{PID: 103, Cmdline: "chrome --user-data-dir=/home/god/.config/google-chrome"},
		// No match: unrelated process.
		{PID: 104, Cmdline: "node server.js", Cwd: "/srv/app"},
		// Ignored: non-positive PID is never selected.
		{PID: 0, Cmdline: "chrome /tmp/scribe-visual-qa-zzz"},
	}

	got := selectVisualQAKills(procs, VisualQATempPrefix)
	sort.Ints(got)
	want := []int{101, 102}
	if len(got) != len(want) {
		t.Fatalf("selectVisualQAKills = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selectVisualQAKills = %v, want %v", got, want)
		}
	}
}

// TestSelectVisualQAKills_EmptyPrefixSelectsNothing guards the conservative
// contract: an empty prefix must never select any process (so a misconfigured
// caller cannot accidentally kill every chrome).
func TestSelectVisualQAKills_EmptyPrefixSelectsNothing(t *testing.T) {
	procs := []procInfo{
		{PID: 1, Cmdline: "chrome /tmp/scribe-visual-qa-abc"},
		{PID: 2, Cmdline: "chrome", Cwd: "/tmp/scribe-visual-qa-abc"},
	}
	if got := selectVisualQAKills(procs, ""); got != nil {
		t.Fatalf("empty prefix should select nothing, got %v", got)
	}
}

// TestSelectVisualQAKills_OnlyExactPrefixCwd ensures a cwd is matched by prefix,
// not by mere substring, so a path that merely contains the marker elsewhere is
// not matched by the cwd rule (cmdline still uses Contains by design).
func TestSelectVisualQAKills_CwdMatchedByPrefixNotSubstring(t *testing.T) {
	procs := []procInfo{
		// cwd has the prefix elsewhere in the path, not at the start — the cwd
		// rule (HasPrefix) must not match this on cwd alone.
		{PID: 5, Cmdline: "chrome", Cwd: "/var/lib/tmp/scribe-visual-qa-abc"},
	}
	if got := selectVisualQAKills(procs, VisualQATempPrefix); len(got) != 0 {
		t.Fatalf("cwd substring (not prefix) should not match, got %v", got)
	}
}

// TestRemoveStaleVisualQADirs_RemovesOnlyMatchingSiblings verifies that the
// temp-dir cleanup removes directories sharing the prefix base name and leaves
// unrelated siblings untouched.
func TestRemoveStaleVisualQADirs_RemovesOnlyMatchingSiblings(t *testing.T) {
	tmp := t.TempDir()
	prefix := filepath.Join(tmp, "scribe-visual-qa-")

	stale1 := filepath.Join(tmp, "scribe-visual-qa-abc123")
	stale2 := filepath.Join(tmp, "scribe-visual-qa-def456")
	unrelated := filepath.Join(tmp, "some-other-dir")
	for _, d := range []string{stale1, stale2, unrelated} {
		if err := os.MkdirAll(filepath.Join(d, "nested"), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	removeStaleVisualQADirs(prefix)

	if _, err := os.Stat(stale1); !os.IsNotExist(err) {
		t.Errorf("stale dir %s should be removed (err=%v)", stale1, err)
	}
	if _, err := os.Stat(stale2); !os.IsNotExist(err) {
		t.Errorf("stale dir %s should be removed (err=%v)", stale2, err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated dir %s should survive (err=%v)", unrelated, err)
	}
}

// TestRemoveStaleVisualQADirs_NoMatchingDirsIsNoop ensures cleanup is a no-op
// when no matching dirs exist and does not error on a missing parent.
func TestRemoveStaleVisualQADirs_NoMatchingDirsIsNoop(t *testing.T) {
	tmp := t.TempDir()
	keep := filepath.Join(tmp, "unrelated")
	if err := os.MkdirAll(keep, 0755); err != nil {
		t.Fatal(err)
	}

	removeStaleVisualQADirs(filepath.Join(tmp, "scribe-visual-qa-"))
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated dir should survive, got %v", err)
	}

	// Non-existent parent must not panic or error.
	removeStaleVisualQADirs(filepath.Join(tmp, "no-such-parent", "scribe-visual-qa-"))

	// Empty prefix is a no-op.
	removeStaleVisualQADirs("")
}

// TestKillProcessTree_NonPositivePIDIsNoop guards the cheap defensive path so a
// zero/negative PID never reaches syscall.Kill.
func TestKillProcessTree_NonPositivePIDIsNoop(t *testing.T) {
	// Must return without panicking and without signalling anything.
	KillProcessTree(0)
	KillProcessTree(-1)
}

// TestParsePPIDFromStat covers the /proc/<pid>/stat PPID parser, including the
// awkward case where comm contains spaces and parentheses. The parser anchors
// on the final ')', so a comm like "(chrome (test))" must not confuse it.
func TestParsePPIDFromStat(t *testing.T) {
	cases := []struct {
		name     string
		stat     string
		wantPPID int
		wantOK   bool
	}{
		{
			name:     "simple comm",
			stat:     "1234 (chrome) S 1000 1234 1234 0 -1 ...",
			wantPPID: 1000,
			wantOK:   true,
		},
		{
			name:     "comm with spaces and parens",
			stat:     "4242 (chrome (test) thread) S 999 4242 ...",
			wantPPID: 999,
			wantOK:   true,
		},
		{
			name:     "ppid is one",
			stat:     "5 (init helper) S 1 5 5 0 -1",
			wantPPID: 1,
			wantOK:   true,
		},
		{
			name:   "no closing paren",
			stat:   "5 init S 1 5",
			wantOK: false,
		},
		{
			name:   "truncated after comm",
			stat:   "5 (init)",
			wantOK: false,
		},
		{
			name:   "non-numeric ppid",
			stat:   "5 (init) S notanumber 5",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ppid, ok := parsePPIDFromStat(tc.stat)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && ppid != tc.wantPPID {
				t.Fatalf("ppid = %d, want %d", ppid, tc.wantPPID)
			}
		})
	}
}
