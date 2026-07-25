package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/tmpfshygiene"
)

func TestTmpfsHygieneLoopAppliesFakeTreeAndPublishesPressure(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tmp")
	proc := filepath.Join(base, "proc")
	protected := filepath.Join(root, "tmp.worktrees")
	abandoned := filepath.Join(root, "tmp.abandoned")
	for _, dir := range []string{root, proc, protected, abandoned} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for dir, data := range map[string]string{protected: "worktree", abandoned: "residue"} {
		path := filepath.Join(dir, "payload")
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	for _, dir := range []string{protected, abandoned} {
		if err := filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		}); err != nil {
			t.Fatal(err)
		}
	}

	d := New(fakeLoader{cfgs: []*config.Config{{
		Repo:         "owner/repo",
		LocalPath:    filepath.Join(base, "repo"),
		WorktreeBase: protected,
	}}}, Options{RunInterval: time.Minute, SuperviseInterval: time.Minute, TmpfsHygieneInterval: 5 * time.Millisecond})
	d.tmpfsHygiene.options = tmpfshygiene.Options{
		Root:     root,
		ProcRoot: proc,
		Mode:     tmpfshygiene.ModeApply,
		Now:      func() time.Time { return now },
		InspectMount: func(string) (tmpfshygiene.MountUsage, error) {
			return tmpfshygiene.MountUsage{Tmpfs: true, UsePct: 90}, nil
		},
		EffectiveUID: os.Geteuid,
	}
	var metrics bytes.Buffer
	d.tmpfsHygiene.metricWriter = &metrics

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.tmpfsHygieneLoop(ctx, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if summary, ok := d.tmpfsHygieneSummary(); ok {
			if summary.AttentionCode != "tmpfs_pressure" || summary.DeletedEntries != 1 {
				t.Fatalf("summary = %+v", summary)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for scheduled tmpfs sweep")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned outcome dir was not swept: %v", err)
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("configured worktree base was removed: %v", err)
	}
	var emitted tmpfshygiene.Summary
	if err := json.NewDecoder(&metrics).Decode(&emitted); err != nil {
		t.Fatalf("scheduled metric is not JSONL: %v\n%s", err, metrics.String())
	}
	if emitted.AttentionCode != "tmpfs_pressure" {
		t.Fatalf("emitted summary = %+v", emitted)
	}
}
