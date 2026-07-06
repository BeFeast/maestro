package mirrorstore

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// delivery is one recorded webhook from the stream fixture: its event type
// (derived from the "NN.eventtype.json" filename) and its raw payload.
type delivery struct {
	name      string
	eventType string
	payload   []byte
}

// loadStream reads testdata/stream/*.json in filename order. The numeric prefix
// gives the recorded delivery order; the middle dotted segment is the
// X-GitHub-Event type (e.g. "03.pull_request.json" → "pull_request").
func loadStream(t *testing.T) []delivery {
	t.Helper()
	dir := filepath.Join("testdata", "stream")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read stream dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]delivery, 0, len(names))
	for _, name := range names {
		parts := strings.Split(name, ".")
		if len(parts) < 3 {
			t.Fatalf("unexpected fixture name %q (want NN.eventtype.json)", name)
		}
		eventType := strings.Join(parts[1:len(parts)-1], ".")
		payload, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out = append(out, delivery{name: name, eventType: eventType, payload: payload})
	}
	return out
}

func replay(t *testing.T, s *Store, stream []delivery, recv time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, d := range stream {
		if err := s.ProjectWebhook(ctx, d.eventType, d.payload, recv); err != nil {
			t.Fatalf("project %s: %v", d.name, err)
		}
	}
}

// assertSnapshot asserts the mirror holds exactly the normalised state the
// recorded stream describes for BeFeast/maestro — the "direct API snapshot" the
// stream is expected to reproduce (#825 AC 1).
func assertSnapshot(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	const repo = "BeFeast/maestro"

	issue, ok, err := s.GetIssue(ctx, repo, 824)
	if err != nil || !ok {
		t.Fatalf("issue 824 ok=%v err=%v", ok, err)
	}
	if issue.Title != "Webhook ingestion into the unified SQLite store" || issue.State != "open" {
		t.Fatalf("issue snapshot mismatch: %+v", issue)
	}
	if labels, _ := s.Labels(ctx, repo, SubjectIssue, 824); !equalStrings(labels, []string{"maestro-ready", "phase-b"}) {
		t.Fatalf("issue labels snapshot mismatch: %v", labels)
	}

	pr, ok, err := s.GetPullRequest(ctx, repo, 828)
	if err != nil || !ok {
		t.Fatalf("pr 828 ok=%v err=%v", ok, err)
	}
	if pr.State != "open" || pr.Merged || pr.HeadSHA != "0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e" || pr.BaseRef != "main" {
		t.Fatalf("pr snapshot mismatch: %+v", pr)
	}

	checks, err := s.Checks(ctx, repo, "0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e")
	if err != nil {
		t.Fatalf("checks: %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "go test ./..." || checks[0].Conclusion != "success" {
		t.Fatalf("checks snapshot mismatch: %+v", checks)
	}

	rev, ok, err := s.GetReview(ctx, repo, 555111222)
	if err != nil || !ok {
		t.Fatalf("review ok=%v err=%v", ok, err)
	}
	if rev.PRNumber != 828 || rev.State != "approved" {
		t.Fatalf("review snapshot mismatch: %+v", rev)
	}

	c, ok, err := s.GetComment(ctx, repo, 700800900)
	if err != nil || !ok {
		t.Fatalf("comment ok=%v err=%v", ok, err)
	}
	if c.SubjectNumber != 824 || c.Author != "octocat" {
		t.Fatalf("comment snapshot mismatch: %+v", c)
	}
}

// TestReplayMatchesSnapshot covers AC 1: replaying a recorded webhook stream
// produces the mirror an operator would get by reading the repo directly.
func TestReplayMatchesSnapshot(t *testing.T) {
	s := openTestStore(t)
	stream := loadStream(t)
	replay(t, s, stream, mustTime(t, "2026-07-02T12:00:00Z"))
	assertSnapshot(t, s)
}

// TestReplayOutOfOrderConverges covers AC 2 at the stream level: replaying the
// SAME deliveries in reverse (and then forward again) converges to the identical
// snapshot, because every upsert is guarded by the resource timestamp.
func TestReplayOutOfOrderConverges(t *testing.T) {
	s := openTestStore(t)
	stream := loadStream(t)

	reversed := make([]delivery, len(stream))
	for i, d := range stream {
		reversed[len(stream)-1-i] = d
	}
	recv := mustTime(t, "2026-07-02T12:00:00Z")
	replay(t, s, reversed, recv) // out of order
	replay(t, s, stream, recv)   // duplicate, in order
	replay(t, s, reversed, recv) // out of order again
	assertSnapshot(t, s)
}

// TestReplayIssueMatchesAPIHydration is the concrete "mirror matches a direct API
// snapshot" equivalence: the issue row + labels produced by replaying the webhook
// stream equal those produced by hydrating the same issue straight from the API,
// ignoring only the bookkeeping fields (source, last_seen_at).
func TestReplayIssueMatchesAPIHydration(t *testing.T) {
	ctx := context.Background()

	// (a) webhook-projected mirror.
	webhookMirror := openTestStore(t)
	replay(t, webhookMirror, loadStream(t), mustTime(t, "2026-07-02T12:00:00Z"))
	wIssue, _, _ := webhookMirror.GetIssue(ctx, "BeFeast/maestro", 824)
	wLabels, _ := webhookMirror.Labels(ctx, "BeFeast/maestro", SubjectIssue, 824)

	// (b) API-hydrated mirror: the fake client returns the same final issue state a
	// direct GET would, and the hydrator records it on the miss.
	apiMirror := openTestStore(t)
	fake := &fakeGitHub{issue: github.Issue{
		Number: 824,
		Title:  "Webhook ingestion into the unified SQLite store",
		Body:   "Land signed webhook deliveries durably.",
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "maestro-ready"}, {Name: "phase-b"}},
	}}
	h := NewHydrator(apiMirror, fake, "BeFeast/maestro")
	if _, err := h.Issue(ctx, 824); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	aIssue, _, _ := apiMirror.GetIssue(ctx, "BeFeast/maestro", 824)
	aLabels, _ := apiMirror.Labels(ctx, "BeFeast/maestro", SubjectIssue, 824)

	if wIssue.Number != aIssue.Number || wIssue.Title != aIssue.Title || wIssue.Body != aIssue.Body || wIssue.State != aIssue.State {
		t.Fatalf("webhook vs API issue mismatch:\n webhook=%+v\n api=%+v", wIssue, aIssue)
	}
	if !equalStrings(wLabels, aLabels) {
		t.Fatalf("webhook vs API labels mismatch: webhook=%v api=%v", wLabels, aLabels)
	}
	// The bookkeeping differs, as it should: one was pushed, one was pulled.
	if wIssue.Source != SourceWebhook || aIssue.Source != SourceAPI {
		t.Fatalf("source bookkeeping wrong: webhook=%q api=%q", wIssue.Source, aIssue.Source)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
