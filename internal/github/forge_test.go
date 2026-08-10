package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/forge"
)

// The forge.Client implementation is exercised through the same ghAPIRunner
// seam as every other read in this package: plain JSON bodies pass the
// conditional layer untouched (see stubLLMReviewAPI), so each test dispatches
// canned responses by endpoint and asserts on the recorded args.

func forgeCallWith(calls [][]string, fragment string) []string {
	for _, call := range calls {
		for _, arg := range call {
			if strings.Contains(arg, fragment) {
				return call
			}
		}
	}
	return nil
}

func TestForgeGetPR(t *testing.T) {
	clearETagCache(t)
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{"number":7,"title":"fix: things","head":{"ref":"fix/things","sha":" abc123 "},"base":{"ref":"main"}}`), nil
	})
	pr, err := NewForge().GetPR(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	want := forge.PR{Number: 7, Title: "fix: things", HeadSHA: "abc123", BaseRef: "main"}
	if pr != want {
		t.Fatalf("GetPR = %+v, want %+v", pr, want)
	}
	if forgeCallWith(*calls, "repos/owner/repo/pulls/7") == nil {
		t.Fatalf("expected a repos/owner/repo/pulls/7 read, got %v", *calls)
	}
}

func TestForgeGetPR_EmptyHeadSHA(t *testing.T) {
	clearETagCache(t)
	withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{"number":7,"title":"t","head":{"ref":"b","sha":""},"base":{"ref":"main"}}`), nil
	})
	if _, err := NewForge().GetPR(context.Background(), "owner/repo", 7); err == nil {
		t.Fatal("an empty head sha must be rejected — every downstream op anchors to it")
	}
}

func TestForgeGetPRDiff(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n+not json\n"
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(diff), nil
	})
	out, err := NewForge().GetPRDiff(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatalf("GetPRDiff: %v", err)
	}
	if string(out) != diff {
		t.Fatalf("diff body = %q, want raw passthrough %q", out, diff)
	}
	call := forgeCallWith(*calls, "repos/owner/repo/pulls/7")
	if call == nil {
		t.Fatalf("expected a pulls/7 call, got %v", *calls)
	}
	if !hasArg(call, "Accept: application/vnd.github.diff") {
		t.Fatalf("diff read must override Accept, got %v", call)
	}
}

func TestForgeCommitStatuses(t *testing.T) {
	clearETagCache(t)
	withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{"state":"failure","statuses":[
			{"context":"llm-review-opus","state":"SUCCESS","description":"ok","target_url":"https://x","created_at":"2026-08-10T12:00:00Z"},
			{"context":"llm-review-cursor","state":"pending","created_at":"not-a-time"}]}`), nil
	})
	statuses, err := NewForge().CommitStatuses(context.Background(), "owner/repo", "abc123")
	if err != nil {
		t.Fatalf("CommitStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(statuses))
	}
	first := statuses[0]
	if first.Context != "llm-review-opus" || first.State != forge.StatusSuccess ||
		first.Description != "ok" || first.TargetURL != "https://x" {
		t.Fatalf("first status = %+v", first)
	}
	if want := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC); !first.CreatedAt.Equal(want) {
		t.Fatalf("first CreatedAt = %v, want %v", first.CreatedAt, want)
	}
	second := statuses[1]
	if second.State != forge.StatusPending {
		t.Fatalf("second state = %q, want pending", second.State)
	}
	if !second.CreatedAt.IsZero() {
		t.Fatalf("an unparseable created_at must map to zero (no usable age), got %v", second.CreatedAt)
	}
}

func TestForgeCreateCommitStatus(t *testing.T) {
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{}`), nil
	})
	err := NewForge().CreateCommitStatus(context.Background(), "owner/repo", "abc123", forge.Status{
		Context:     "llm-review-opus",
		State:       forge.StatusPending,
		Description: "review in progress",
	})
	if err != nil {
		t.Fatalf("CreateCommitStatus: %v", err)
	}
	call := forgeCallWith(*calls, "repos/owner/repo/statuses/abc123")
	if call == nil {
		t.Fatalf("expected a statuses/abc123 post, got %v", *calls)
	}
	for _, want := range []string{"state=pending", "context=llm-review-opus", "description=review in progress"} {
		if !hasArg(call, want) {
			t.Fatalf("missing field %q in %v", want, call)
		}
	}
	if arg := argWithPrefix(call, "target_url="); arg != "" {
		t.Fatalf("empty TargetURL must not be posted, got %q", arg)
	}
}

func TestForgeCreateCommitStatus_TargetURL(t *testing.T) {
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{}`), nil
	})
	err := NewForge().CreateCommitStatus(context.Background(), "owner/repo", "abc123", forge.Status{
		Context:   "llm-review-opus",
		State:     forge.StatusSuccess,
		TargetURL: "https://example.test/run/1",
	})
	if err != nil {
		t.Fatalf("CreateCommitStatus: %v", err)
	}
	call := forgeCallWith(*calls, "repos/owner/repo/statuses/abc123")
	if !hasArg(call, "target_url=https://example.test/run/1") {
		t.Fatalf("TargetURL must be posted when set, got %v", call)
	}
}

func TestForgeCreateCommitStatus_ErrorPropagates(t *testing.T) {
	withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte("HTTP 502"), errors.New("boom")
	})
	err := NewForge().CreateCommitStatus(context.Background(), "owner/repo", "abc123", forge.Status{
		Context: "llm-review-opus",
		State:   forge.StatusPending,
	})
	if err == nil {
		t.Fatal("a failed status post must surface — a swallowed error here is the silent-no-op bug the bash glue was hardened against")
	}
}

func TestForgeCreateCommitStatus_RequiresContextAndState(t *testing.T) {
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{}`), nil
	})
	err := NewForge().CreateCommitStatus(context.Background(), "owner/repo", "abc123", forge.Status{State: forge.StatusPending})
	if err == nil {
		t.Fatal("a status without a context must be rejected")
	}
	err = NewForge().CreateCommitStatus(context.Background(), "owner/repo", "abc123", forge.Status{Context: "llm-review-opus"})
	if err == nil {
		t.Fatal("a status without a state must be rejected")
	}
	if len(*calls) != 0 {
		t.Fatalf("validation failures must not reach the API, got %v", *calls)
	}
}

func TestForgeCreateReviewComment(t *testing.T) {
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{}`), nil
	})
	err := NewForge().CreateReviewComment(context.Background(), "owner/repo", 7, "abc123", "a.go", 42, "[P1] boom")
	if err != nil {
		t.Fatalf("CreateReviewComment: %v", err)
	}
	call := forgeCallWith(*calls, "repos/owner/repo/pulls/7/comments")
	if call == nil {
		t.Fatalf("expected a pulls/7/comments post, got %v", *calls)
	}
	for _, want := range []string{"body=[P1] boom", "commit_id=abc123", "path=a.go", "line=42", "side=RIGHT"} {
		if !hasArg(call, want) {
			t.Fatalf("missing field %q in %v", want, call)
		}
	}
}

func TestForgeCreateReviewComment_ErrorPropagates(t *testing.T) {
	withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte("HTTP 422 Validation Failed"), errors.New("boom")
	})
	err := NewForge().CreateReviewComment(context.Background(), "owner/repo", 7, "abc123", "a.go", 9999, "[P1] boom")
	if err == nil {
		t.Fatal("a rejected inline anchor must surface as an error — it is the caller's CreateComment fallback trigger, and swallowing it drops findings")
	}
}

func TestForgeCreateComment(t *testing.T) {
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{}`), nil
	})
	err := NewForge().CreateComment(context.Background(), "owner/repo", 7, "[P1] boom (at `a.go:42`)")
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	call := forgeCallWith(*calls, "repos/owner/repo/issues/7/comments")
	if call == nil {
		t.Fatalf("expected an issues/7/comments post, got %v", *calls)
	}
	if !hasArg(call, "body=[P1] boom (at `a.go:42`)") {
		t.Fatalf("missing body field in %v", call)
	}
}

func TestForgeCreateComment_ErrorPropagates(t *testing.T) {
	withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte("HTTP 502"), errors.New("boom")
	})
	if err := NewForge().CreateComment(context.Background(), "owner/repo", 7, "b"); err == nil {
		t.Fatal("a failed fallback comment must not read as posted — it is the last resort before a finding is lost")
	}
}

func TestForge_ContextCanceled(t *testing.T) {
	calls := withGHRunner(t, func(args []string) ([]byte, error) {
		return []byte(`{}`), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := NewForge()
	if _, err := f.GetPR(ctx, "owner/repo", 7); err == nil {
		t.Fatal("GetPR must fail on a canceled context")
	}
	if _, err := f.GetPRDiff(ctx, "owner/repo", 7); err == nil {
		t.Fatal("GetPRDiff must fail on a canceled context")
	}
	if _, err := f.CommitStatuses(ctx, "owner/repo", "abc123"); err == nil {
		t.Fatal("CommitStatuses must fail on a canceled context")
	}
	if err := f.CreateCommitStatus(ctx, "owner/repo", "abc123", forge.Status{Context: "c", State: forge.StatusPending}); err == nil {
		t.Fatal("CreateCommitStatus must fail on a canceled context")
	}
	if err := f.CreateReviewComment(ctx, "owner/repo", 7, "abc123", "a.go", 1, "b"); err == nil {
		t.Fatal("CreateReviewComment must fail on a canceled context")
	}
	if err := f.CreateComment(ctx, "owner/repo", 7, "b"); err == nil {
		t.Fatal("CreateComment must fail on a canceled context")
	}
	if len(*calls) != 0 {
		t.Fatalf("a canceled context must not reach the API, got %v", *calls)
	}
}
