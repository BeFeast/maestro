package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/mirrorstore"
)

// TestNewReadSourceNilWithoutMirror: with no mirror opened, a flow gets no read
// source and keeps reading GitHub directly (pre-#826 behavior).
func TestNewReadSourceNilWithoutMirror(t *testing.T) {
	d := &Daemon{}
	getCfg := func() *config.Config { return &config.Config{Repo: "o/r"} }
	if src := d.newReadSource("o/r", getCfg); src != nil {
		t.Fatal("newReadSource should be nil when the daemon has no mirror")
	}
}

// TestNewReadSourceServesWarmMirror wires the real daemon helper against a warm
// mirror and a mirror-first config, and asserts the read is served locally and
// shows up in the journal digest fragment — the end-to-end wiring for AC 3/6/5.
func TestNewReadSourceServesWarmMirror(t *testing.T) {
	mirror, err := mirrorstore.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}
	defer mirror.Close()

	now := time.Now().UTC()
	if _, err := mirror.UpsertIssueWithLabels(context.Background(), mirrorstore.Issue{
		Repo: "o/r", Number: 5, Title: "warm", State: "open",
		LastSeenAt: now, Source: mirrorstore.SourceWebhook,
	}, []string{"maestro-ready"}); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	d := &Daemon{mirror: mirror, mirrorHorizon: mirrorstore.DefaultStaleHorizon}
	getCfg := func() *config.Config {
		return &config.Config{Repo: "o/r", GitHubMirror: config.GitHubMirrorConfig{Source: config.GitHubSourceMirrorFirst}}
	}
	src := d.newReadSource("o/r", getCfg)
	if src == nil {
		t.Fatal("newReadSource returned nil with a mirror present")
	}

	// A warm mirror hit is served locally — no gh exec, so this is safe in a test
	// environment without a real gh binary.
	issues, err := src.ListOpenIssues(nil)
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 5 {
		t.Fatalf("open issues = %+v; want just #5", issues)
	}

	line := mirrorReadDigestLine()
	if !strings.Contains(line, "served locally") {
		t.Fatalf("digest line missing local-serve count: %q", line)
	}
}

// TestReconcileIntervalReadsConfig: the reconcile cadence comes from the flow's
// live config, defaulting when it is momentarily unavailable (#827).
func TestReconcileIntervalReadsConfig(t *testing.T) {
	if got := reconcileInterval(func() *config.Config { return nil }); got != config.DefaultMirrorReconcileInterval {
		t.Fatalf("nil config cadence = %s, want default %s", got, config.DefaultMirrorReconcileInterval)
	}
	cfg := &config.Config{GitHubMirror: config.GitHubMirrorConfig{ReconcileSeconds: 120}}
	if got := reconcileInterval(func() *config.Config { return cfg }); got != 2*time.Minute {
		t.Fatalf("configured cadence = %s, want 2m", got)
	}
}

// TestRunMirrorReconcileNoopWithoutMirror: the loop returns immediately when the
// daemon has no open mirror, so a flow without webhook ingestion spawns no
// reconcile work.
func TestRunMirrorReconcileNoopWithoutMirror(t *testing.T) {
	d := &Daemon{}
	done := make(chan struct{})
	go func() {
		d.runMirrorReconcile(context.Background(), "flow", "o/r", func() *config.Config { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runMirrorReconcile should return immediately without a mirror")
	}
}
