package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/tmpfshygiene"
)

func TestRunTmpfsHygieneRequiresExactlyOneMode(t *testing.T) {
	deps := tmpfsHygieneDependencies{
		defaultStore: "unused",
		loadProtectedPaths: func(context.Context, string) ([]string, error) {
			t.Fatal("protected paths must not load for invalid flags")
			return nil, nil
		},
	}
	for _, args := range [][]string{nil, {"--dry-run", "--apply"}} {
		if err := runTmpfsHygiene(context.Background(), args, &bytes.Buffer{}, deps); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("args %v error = %v, want exactly-one error", args, err)
		}
	}
}

func TestRunTmpfsHygieneEmitsJSONLFromFakeTree(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tmp")
	proc := filepath.Join(base, "proc")
	if err := os.MkdirAll(filepath.Join(root, "tmp.old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "tmp.old", "payload")
	if err := os.WriteFile(payload, []byte("reclaim"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(payload, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Dir(payload), old, old); err != nil {
		t.Fatal(err)
	}

	deps := tmpfsHygieneDependencies{
		defaultStore: "fake.db",
		options: tmpfshygiene.Options{
			Root:     root,
			ProcRoot: proc,
			Now:      func() time.Time { return now },
			InspectMount: func(string) (tmpfshygiene.MountUsage, error) {
				return tmpfshygiene.MountUsage{Tmpfs: true, UsePct: 60}, nil
			},
			EffectiveUID: os.Geteuid,
		},
		loadProtectedPaths: func(_ context.Context, store string) ([]string, error) {
			if store != "fake.db" {
				t.Fatalf("store = %q", store)
			}
			return nil, nil
		},
	}
	var out bytes.Buffer
	if err := runTmpfsHygiene(context.Background(), []string{"--dry-run"}, &out, deps); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 1 {
		t.Fatalf("output is not one JSONL record: %q", out.String())
	}
	var summary tmpfshygiene.Summary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out.String())
	}
	if summary.Mode != tmpfshygiene.ModeDryRun || summary.ReclaimableBytes != int64(len("reclaim")) || summary.FreedBytes != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(root, "tmp.old")); err != nil {
		t.Fatalf("dry-run mutated fake tree: %v", err)
	}
}

func TestRunTmpfsHygieneEmitsJSONLErrorWhenProtectionStoreFails(t *testing.T) {
	deps := tmpfsHygieneDependencies{
		defaultStore: "missing.db",
		options: tmpfshygiene.Options{
			Now: func() time.Time { return time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC) },
		},
		loadProtectedPaths: func(context.Context, string) ([]string, error) {
			return nil, errors.New("store unavailable")
		},
	}
	var out bytes.Buffer
	err := runTmpfsHygiene(context.Background(), []string{"--apply"}, &out, deps)
	if err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("error = %v", err)
	}
	var summary tmpfshygiene.Summary
	if decodeErr := json.Unmarshal(out.Bytes(), &summary); decodeErr != nil {
		t.Fatalf("decode summary: %v", decodeErr)
	}
	if summary.Mode != tmpfshygiene.ModeApply || !strings.Contains(summary.Error, "store unavailable") {
		t.Fatalf("summary = %+v", summary)
	}
}
