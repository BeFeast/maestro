package mirrorstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// fakeGitHub is a GitHubClient that serves a fixed issue and counts calls, so a
// test can assert hydration happens exactly once and later reads are local.
type fakeGitHub struct {
	issue github.Issue
	err   error
	calls int
}

func (f *fakeGitHub) GetIssue(number int) (github.Issue, error) {
	f.calls++
	if f.err != nil {
		return github.Issue{}, f.err
	}
	return f.issue, nil
}

// TestHydrateMissThenLocal covers AC 3: a mirror miss hydrates from the API and
// records the row; the immediately following read is served locally with no
// further API call.
func TestHydrateMissThenLocal(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	fake := &fakeGitHub{issue: github.Issue{
		Number: 100,
		Title:  "hydrated",
		Body:   "from api",
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "needs-triage"}},
	}}
	h := NewHydrator(s, fake, "o/r")

	// Miss → hydrate.
	row, err := h.Issue(ctx, 100)
	if err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	if row.Title != "hydrated" || row.Source != SourceAPI {
		t.Fatalf("hydrated row = %+v", row)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 API call, got %d", fake.calls)
	}

	// The row and its labels are now local.
	if _, ok, _ := s.GetIssue(ctx, "o/r", 100); !ok {
		t.Fatalf("hydrated row not stored")
	}
	labels, _ := s.Labels(ctx, "o/r", SubjectIssue, 100)
	if len(labels) != 1 || labels[0] != "needs-triage" {
		t.Fatalf("hydrated labels = %v", labels)
	}

	// Second read is served from the mirror: no additional API call.
	if _, err := h.Issue(ctx, 100); err != nil {
		t.Fatalf("second Issue: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("second read hit the API: calls=%d", fake.calls)
	}
}

// TestHydrateDoesNotClobberFresherWebhook confirms the guarded upsert keeps a
// fresher webhook row even when a hydration fetch (stamped "now") races it.
func TestHydrateDoesNotClobberFresherWebhook(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A webhook already mirrored the issue with a FUTURE resource timestamp.
	future := time.Now().UTC().Add(24 * time.Hour)
	if _, err := s.UpsertIssue(ctx, Issue{Repo: "o/r", Number: 1, Title: "webhook", State: "open", LastSeenAt: future, Source: SourceWebhook}); err != nil {
		t.Fatal(err)
	}

	fake := &fakeGitHub{issue: github.Issue{Number: 1, Title: "api"}}
	h := NewHydrator(s, fake, "o/r")

	// GetIssue hits (row present) so hydration never calls the API.
	row, err := h.Issue(ctx, 1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("hydration should not call API on a hit: calls=%d", fake.calls)
	}
	if row.Title != "webhook" || row.Source != SourceWebhook {
		t.Fatalf("hit returned wrong row: %+v", row)
	}
}

// TestHydratePreservesClosedState covers the P1 fix: hydrating an already-closed
// issue must record its real state, not hard-code "open". Otherwise a later
// "issues"/"closed" webhook whose issue.updated_at predates the hydration time
// would be guard-rejected as stale and the mirror would stay open indefinitely.
func TestHydratePreservesClosedState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	fake := &fakeGitHub{issue: github.Issue{Number: 5, Title: "done", State: "closed"}}
	h := NewHydrator(s, fake, "o/r")

	row, err := h.Issue(ctx, 5)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if row.State != "closed" {
		t.Fatalf("hydrated state = %q, want closed", row.State)
	}

	// A later close webhook that predates the hydration time is a no-op — but the
	// mirror already shows closed, so it never regressed to open in the first place.
	stored, ok, _ := s.GetIssue(ctx, "o/r", 5)
	if !ok || stored.State != "closed" {
		t.Fatalf("stored issue = %+v, want state closed", stored)
	}
}

// TestHydrateEmptyStateDefaultsOpen confirms the safety fallback: a client that
// does not populate state records the issue as open (the mirror only hydrates
// issues a reader is actively working, which are open by construction).
func TestHydrateEmptyStateDefaultsOpen(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	fake := &fakeGitHub{issue: github.Issue{Number: 6, Title: "no state"}}
	h := NewHydrator(s, fake, "o/r")
	row, err := h.Issue(ctx, 6)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if row.State != "open" {
		t.Fatalf("hydrated state = %q, want open fallback", row.State)
	}
}

func TestHydrateErrorPropagates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	fake := &fakeGitHub{err: errors.New("boom")}
	h := NewHydrator(s, fake, "o/r")
	if _, err := h.Issue(ctx, 7); err == nil {
		t.Fatalf("expected error from failing hydration")
	}
}

func TestHydrateNoClientErrors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	h := NewHydrator(s, nil, "o/r")
	if _, err := h.Issue(ctx, 7); err == nil {
		t.Fatalf("expected error when no client is wired and the row is missing")
	}
}
