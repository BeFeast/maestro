package workerlease

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testLeaseGeneration atomic.Uint64

func diskTestDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cwd, ".workerlease-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func prepareTestLease(t *testing.T, root, slot string) Lease {
	t.Helper()
	generation := testLeaseGeneration.Add(1)
	lease, err := Prepare(Spec{
		Root: root, ProjectKey: "project-123", Repo: "owner/repo", Slot: slot,
		Attempt: "attempt-1", Unit: fmt.Sprintf("maestro-worker-0123456789abcdef0123456789abcdef-g%d.service", generation), Scope: ScopeSystem,
		Now: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestPrepareCreatesUniquePrivateDiskScratch(t *testing.T) {
	root := filepath.Join(diskTestDir(t), "scratch")
	a := prepareTestLease(t, root, "sup-1")
	b := prepareTestLease(t, root, "sup-1")
	if a.ID == b.ID || a.ScratchDir == b.ScratchDir || a.Unit == b.Unit {
		t.Fatalf("leases are not unique: a=%+v b=%+v", a, b)
	}
	for _, lease := range []Lease{a, b} {
		if !strings.HasPrefix(lease.ScratchDir, root+string(os.PathSeparator)) {
			t.Fatalf("scratch %q escaped root %q", lease.ScratchDir, root)
		}
		for _, path := range []string{lease.TempDir, lease.GoTempDir, lease.CargoTarget, lease.ManifestPath} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("missing private path %s: %v", path, err)
			}
		}
		m, err := ValidateManifest(lease.ManifestPath, lease.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m.ScratchDir != lease.ScratchDir || m.Unit != lease.Unit || m.Slot != "sup-1" {
			t.Fatalf("manifest = %+v, lease = %+v", m, lease)
		}
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch root mode = %v err=%v, want 0700", info.Mode().Perm(), err)
	}
}

func TestPrepareRejectsMemoryBackedScratch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scratch")
	if err := ensureDiskBacked(root); err == nil {
		t.Skip("test temp directory is not memory-backed on this host")
	}
	_, err := Prepare(Spec{
		Root: root, ProjectKey: "project-123", Repo: "owner/repo", Slot: "sup-memory",
		Attempt: "attempt-1", Unit: "maestro-worker-0123456789abcdef0123456789abcdef-g1.service", Scope: ScopeSystem,
	})
	if err == nil || !strings.Contains(err.Error(), "memory-backed") {
		t.Fatalf("Prepare error = %v, want memory-backed rejection", err)
	}
}

func TestEnsureScratchBaseRefusesSharedDirectoryWithoutChmod(t *testing.T) {
	base := filepath.Join(diskTestDir(t), "shared")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureScratchBase(base); err == nil || !strings.Contains(err.Error(), "dedicated private") {
		t.Fatalf("EnsureScratchBase error = %v", err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("shared directory mode was changed to %o", info.Mode().Perm())
	}
}

func TestWorkerSliceControlCommandPreservesControlPlaneHeadroom(t *testing.T) {
	binary, args, err := workerSliceControlCommand(ScopeSystem)
	if err != nil {
		t.Fatal(err)
	}
	if binary != "sudo" {
		t.Fatalf("binary = %q, want sudo", binary)
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"-n\nsystemctl\nset-property\n--runtime\n" + WorkerSlice,
		"MemoryAccounting=yes",
		"MemoryHigh=" + WorkerSliceMemoryHigh,
		"MemoryMax=" + WorkerSliceMemoryMax,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("slice argv missing %q:\n%s", want, joined)
		}
	}
	if WorkerSliceMemoryMax == "100%" {
		t.Fatal("worker slice must preserve memory headroom for the control plane")
	}
	if WorkerSlice == "maestro-workers.slice" {
		t.Fatal("isolated aggregate limits must not capture legacy process scopes during rollback")
	}
}

func TestCleanupManifestIsExactAndIdempotent(t *testing.T) {
	root := filepath.Join(diskTestDir(t), "scratch")
	a := prepareTestLease(t, root, "sup-a")
	b := prepareTestLease(t, root, "sup-b")
	if err := os.WriteFile(filepath.Join(a.TempDir, "large-build-output"), []byte("owned by a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.TempDir, "neighbor"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupManifest(a.ManifestPath, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.ScratchDir); !os.IsNotExist(err) {
		t.Fatalf("cleaned scratch still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.TempDir, "neighbor")); err != nil {
		t.Fatalf("neighboring worker scratch was touched: %v", err)
	}
	if err := CleanupManifest(a.ManifestPath, a.ID); err != nil {
		t.Fatalf("second cleanup was not idempotent: %v", err)
	}
	if err := CleanupManifest(b.ManifestPath, a.ID); err == nil {
		t.Fatal("mismatched lease identity was accepted")
	}
}

func TestListSurfacesAmbiguousOwnershipWithoutDeleting(t *testing.T) {
	root := filepath.Join(diskTestDir(t), "scratch")
	lease := prepareTestLease(t, root, "sup-3")
	ambiguous := filepath.Join(root, "unknown-build-tree")
	if err := os.Mkdir(ambiguous, 0o700); err != nil {
		t.Fatal(err)
	}
	leases, attention, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].ID != lease.ID {
		t.Fatalf("leases = %+v", leases)
	}
	if len(attention) != 1 || attention[0].Entry != "unknown-build-tree" {
		t.Fatalf("attention = %+v", attention)
	}
	if _, err := os.Stat(ambiguous); err != nil {
		t.Fatalf("ambiguous entry was deleted: %v", err)
	}
}

func TestListRefusesSymlinkedScratchRoot(t *testing.T) {
	base := diskTestDir(t)
	realRoot := filepath.Join(base, "real")
	lease := prepareTestLease(t, realRoot, "sup-symlink")
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := List(alias); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("List error = %v, want symlink refusal", err)
	}
	if _, err := os.Stat(lease.ManifestPath); err != nil {
		t.Fatalf("symlinked-root refusal touched lease: %v", err)
	}
}

func TestTwentyWorkerWaveUsesPrivateRootsAndCleansAll(t *testing.T) {
	root := filepath.Join(diskTestDir(t), "scratch")
	seen := map[string]bool{}
	var leases []Lease
	for i := 0; i < 20; i++ {
		lease := prepareTestLease(t, root, "sup-wave")
		if seen[lease.ScratchDir] {
			t.Fatalf("duplicate scratch root %q", lease.ScratchDir)
		}
		seen[lease.ScratchDir] = true
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		if err := CleanupManifest(lease.ManifestPath, lease.ID); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("completed leases left %d runtime directories", len(entries))
	}
}
