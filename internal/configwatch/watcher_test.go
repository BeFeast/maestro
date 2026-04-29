package configwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatch_DetectsFileChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.yaml")
	if err := os.WriteFile(path, []byte("repo: owner/repo\nmax_parallel: 3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := Watch(ctx, path, 100*time.Millisecond)

	// Wait for initial stat
	time.Sleep(200 * time.Millisecond)

	// Modify file
	if err := os.WriteFile(path, []byte("repo: owner/repo\nmax_parallel: 10\n"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case cfg := <-ch:
		if cfg.MaxParallel != 10 {
			t.Errorf("MaxParallel = %d, want 10", cfg.MaxParallel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for config reload")
	}
}

func TestWatch_InvalidYAMLKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.yaml")
	if err := os.WriteFile(path, []byte("repo: owner/repo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := Watch(ctx, path, 100*time.Millisecond)

	time.Sleep(200 * time.Millisecond)

	// Write invalid YAML
	if err := os.WriteFile(path, []byte("invalid: [yaml: {broken\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Should not receive anything (invalid config is skipped)
	select {
	case cfg := <-ch:
		t.Fatalf("should not receive config for invalid YAML, got %+v", cfg)
	case <-time.After(1 * time.Second):
		// Expected — no event for invalid YAML
	}
}

func TestWatch_NoChangeNoEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.yaml")
	if err := os.WriteFile(path, []byte("repo: owner/repo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := Watch(ctx, path, 100*time.Millisecond)

	// No modification — should not receive anything
	select {
	case cfg := <-ch:
		t.Fatalf("should not receive config when file is unchanged, got %+v", cfg)
	case <-time.After(500 * time.Millisecond):
		// Expected
	}
}

func TestWatch_ContextCancelStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.yaml")
	if err := os.WriteFile(path, []byte("repo: owner/repo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := Watch(ctx, path, 100*time.Millisecond)

	cancel()

	// Channel should be closed soon after context cancel
	select {
	case _, ok := <-ch:
		if ok {
			// Got a value before close, that's fine, drain it
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("channel should be closed after context cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("channel not closed after context cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after context cancel")
	}
}

func TestWatch_MissingConfigOnReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.yaml")
	if err := os.WriteFile(path, []byte("repo: owner/repo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := Watch(ctx, path, 100*time.Millisecond)

	time.Sleep(200 * time.Millisecond)

	// Remove the file
	os.Remove(path)

	// Should not receive anything (missing file is skipped)
	select {
	case cfg := <-ch:
		t.Fatalf("should not receive config for missing file, got %+v", cfg)
	case <-time.After(500 * time.Millisecond):
		// Expected
	}
}

func TestWatch_DetectsSupervisorPolicyChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "maestro.yaml")
	policyDir := filepath.Join(dir, ".maestro")
	if err := os.Mkdir(policyDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("repo: owner/repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(policyDir, "supervisor.yaml")
	if err := os.WriteFile(policyPath, []byte("ready_label: old-ready\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := Watch(ctx, path, 100*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(policyPath, []byte("ready_label: maestro-ready\n"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case cfg := <-ch:
		if cfg.Supervisor.ReadyLabel != "maestro-ready" {
			t.Errorf("Supervisor.ReadyLabel = %q, want maestro-ready", cfg.Supervisor.ReadyLabel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for supervisor policy reload")
	}
}
