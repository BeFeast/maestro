package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/forge"
)

// fakeForge records every op in order and answers from canned data.
type fakeForge struct {
	pr       forge.PR
	diff     []byte
	statuses []forge.Status

	ops            []string
	posted         []forge.Status
	inlineErr      error
	pendingErrFor  map[string]bool
	finalErrFor    map[string]bool
	comments       []string
	inlineComments []string
}

func (f *fakeForge) GetPR(ctx context.Context, repo string, index int) (forge.PR, error) {
	f.ops = append(f.ops, "get-pr")
	return f.pr, nil
}

func (f *fakeForge) GetPRDiff(ctx context.Context, repo string, index int) ([]byte, error) {
	f.ops = append(f.ops, "get-diff")
	return f.diff, nil
}

func (f *fakeForge) CommitStatuses(ctx context.Context, repo, sha string) ([]forge.Status, error) {
	f.ops = append(f.ops, "statuses")
	return f.statuses, nil
}

func (f *fakeForge) CreateCommitStatus(ctx context.Context, repo, sha string, status forge.Status) error {
	f.ops = append(f.ops, fmt.Sprintf("status:%s=%s", status.Context, status.State))
	if status.State == forge.StatusPending && f.pendingErrFor[status.Context] {
		return errors.New("pending post failed")
	}
	if (status.State == forge.StatusSuccess || status.State == forge.StatusFailure) && f.finalErrFor[status.Context] {
		return errors.New("final post failed")
	}
	f.posted = append(f.posted, status)
	return nil
}

func (f *fakeForge) CreateReviewComment(ctx context.Context, repo string, index int, sha, path string, line int, body string) error {
	f.ops = append(f.ops, "inline:"+path)
	if f.inlineErr != nil {
		return f.inlineErr
	}
	f.inlineComments = append(f.inlineComments, body)
	return nil
}

func (f *fakeForge) CreateComment(ctx context.Context, repo string, index int, body string) error {
	f.ops = append(f.ops, "comment")
	f.comments = append(f.comments, body)
	return nil
}

type fakeLens struct {
	name         string
	availableErr error
	output       string
	runErr       error
	runs         int
	sawPrompt    string
}

func (l *fakeLens) Name() string     { return l.name }
func (l *fakeLens) Available() error { return l.availableErr }
func (l *fakeLens) Run(ctx context.Context, prompt string) (string, error) {
	l.runs++
	l.sawPrompt = prompt
	return l.output, l.runErr
}

func newFakeForge() *fakeForge {
	return &fakeForge{
		pr:   forge.PR{Number: 7, Title: "t", HeadSHA: "abcdef123456789", BaseRef: "main"},
		diff: []byte("diff --git a/a.go b/a.go\n+x\n"),
	}
}

func producer(f *fakeForge, lenses ...Lens) *Producer {
	return &Producer{
		Forge:  f,
		Repo:   "owner/repo",
		Lenses: lenses,
		Now:    func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) },
		Logf:   func(string, ...any) {},
	}
}

func lastStatusFor(t *testing.T, f *fakeForge, context string) forge.Status {
	t.Helper()
	for i := len(f.posted) - 1; i >= 0; i-- {
		if f.posted[i].Context == context {
			return f.posted[i]
		}
	}
	t.Fatalf("no status posted for %s: %v", context, f.posted)
	return forge.Status{}
}

func TestProducePR_PendingFirstAcrossLenses(t *testing.T) {
	f := newFakeForge()
	a := &fakeLens{name: "llm-review-opus", output: "[P2] a.go:1 — minor"}
	b := &fakeLens{name: "llm-review-cursor", output: "NO_FINDINGS"}
	if err := producer(f, a, b).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	// Both pendings must land before any comment or final status: the gate
	// must never observe "comments exist but a stream has no status".
	var sawFirstFinal bool
	pendings := 0
	for _, op := range f.ops {
		if strings.HasPrefix(op, "status:") && strings.HasSuffix(op, "=pending") {
			if sawFirstFinal {
				t.Fatalf("pending posted after phase 2 began: %v", f.ops)
			}
			pendings++
		}
		if strings.HasPrefix(op, "inline:") || op == "comment" ||
			(strings.HasPrefix(op, "status:") && !strings.HasSuffix(op, "=pending")) {
			sawFirstFinal = true
		}
	}
	if pendings != 2 {
		t.Fatalf("pendings = %d, want 2: %v", pendings, f.ops)
	}
	if a.runs != 1 || b.runs != 1 {
		t.Fatalf("runs = %d/%d, want 1/1", a.runs, b.runs)
	}
}

func TestProducePR_SettledSkips(t *testing.T) {
	for _, state := range []forge.StatusState{forge.StatusSuccess, forge.StatusFailure} {
		f := newFakeForge()
		f.statuses = []forge.Status{{Context: "llm-review-opus", State: state}}
		l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
		if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
			t.Fatalf("state %s: %v", state, err)
		}
		if l.runs != 0 {
			t.Fatalf("state %s: a settled lens must not run again (idempotency)", state)
		}
		if len(f.posted) != 0 {
			t.Fatalf("state %s: no statuses expected, got %v", state, f.posted)
		}
	}
}

func TestProducePR_FreshPendingSkips(t *testing.T) {
	f := newFakeForge()
	f.statuses = []forge.Status{{
		Context:   "llm-review-opus",
		State:     forge.StatusPending,
		CreatedAt: time.Date(2026, 8, 10, 11, 55, 0, 0, time.UTC), // 5m old
	}}
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if l.runs != 0 {
		t.Fatal("a fresh pending means another run is mid-flight — must skip")
	}
}

func TestProducePR_StalePendingRetries(t *testing.T) {
	f := newFakeForge()
	f.statuses = []forge.Status{{
		Context:   "llm-review-opus",
		State:     forge.StatusPending,
		CreatedAt: time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC), // 60m old
	}}
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if l.runs != 1 {
		t.Fatal("a stale pending is a crashed run — must retry")
	}
}

func TestProducePR_PendingWithoutAgeRetries(t *testing.T) {
	f := newFakeForge()
	f.statuses = []forge.Status{{Context: "llm-review-opus", State: forge.StatusPending}}
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if l.runs != 1 {
		t.Fatal("a pending with no usable age must be treated as crashed, not wedge")
	}
}

func TestProducePR_ErrorStatusRetries(t *testing.T) {
	f := newFakeForge()
	f.statuses = []forge.Status{{Context: "llm-review-opus", State: forge.StatusError}}
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if l.runs != 1 {
		t.Fatal("an error status is retryable")
	}
}

func TestProducePR_CredsMissingPostsErrorStatus(t *testing.T) {
	f := newFakeForge()
	broken := &fakeLens{name: "llm-review-cursor", availableErr: errors.New("no key")}
	healthy := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	err := producer(f, broken, healthy).ProducePR(context.Background(), 7)
	if err == nil {
		t.Fatal("a creds-missing lens must fail the run so the operator sees the degraded pass")
	}
	st := lastStatusFor(t, f, "llm-review-cursor")
	if st.State != forge.StatusError || st.Description != "skipped: credentials not configured" {
		t.Fatalf("cursor status = %+v", st)
	}
	if broken.runs != 0 {
		t.Fatal("a creds-missing lens must not run")
	}
	if healthy.runs != 1 {
		t.Fatal("the healthy lens must still run — the pair must not wedge on one silent stream")
	}
	if st := lastStatusFor(t, f, "llm-review-opus"); st.State != forge.StatusSuccess {
		t.Fatalf("opus status = %+v", st)
	}
}

func TestProducePR_PendingPostFailureAborts(t *testing.T) {
	f := newFakeForge()
	f.pendingErrFor = map[string]bool{"llm-review-opus": true}
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	err := producer(f, l).ProducePR(context.Background(), 7)
	if err == nil {
		t.Fatal("a failed pending post must fail the run — a green no-op over it hides the protocol violation")
	}
	if l.runs != 0 {
		t.Fatal("the model must not run when phase 1 did not happen")
	}
}

func TestProducePR_UnparseableFailsClosed(t *testing.T) {
	f := newFakeForge()
	l := &fakeLens{name: "llm-review-opus", output: "I'm sorry, I cannot review this diff."}
	err := producer(f, l).ProducePR(context.Background(), 7)
	if err == nil {
		t.Fatal("unparseable output must fail the run")
	}
	st := lastStatusFor(t, f, "llm-review-opus")
	if st.State != forge.StatusError || st.Description != "review output unparseable" {
		t.Fatalf("status = %+v — a refusal must never read as a clean review", st)
	}
}

func TestProducePR_RunFailurePostsErrorStatus(t *testing.T) {
	f := newFakeForge()
	l := &fakeLens{name: "llm-review-opus", runErr: errors.New("model down")}
	if err := producer(f, l).ProducePR(context.Background(), 7); err == nil {
		t.Fatal("a failed run must surface")
	}
	st := lastStatusFor(t, f, "llm-review-opus")
	if st.State != forge.StatusError || st.Description != "review run failed" {
		t.Fatalf("status = %+v", st)
	}
}

func TestProducePR_FindingsPostAndBlock(t *testing.T) {
	f := newFakeForge()
	l := &fakeLens{name: "llm-review-opus", output: "[P0] a.go:3 — data loss\n[P2] b.go:9 — nit"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if len(f.inlineComments) != 2 {
		t.Fatalf("inline comments = %d, want 2", len(f.inlineComments))
	}
	if !strings.Contains(f.inlineComments[0], "[P0] data loss") ||
		!strings.Contains(f.inlineComments[0], "<sub>llm-review-opus @ abcdef123456</sub>") {
		t.Fatalf("first comment = %q — needs the [Px] severity marker and the stream marker", f.inlineComments[0])
	}
	st := lastStatusFor(t, f, "llm-review-opus")
	if st.State != forge.StatusFailure || st.Description != "1 blocking (P0/P1) of 2 findings" {
		t.Fatalf("status = %+v", st)
	}
}

func TestProducePR_AdvisoryOnlySucceeds(t *testing.T) {
	f := newFakeForge()
	l := &fakeLens{name: "llm-review-opus", output: "```\n[P3] a.go:1 — nit\n```"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	st := lastStatusFor(t, f, "llm-review-opus")
	if st.State != forge.StatusSuccess || st.Description != "1 advisory findings, none blocking" {
		t.Fatalf("status = %+v", st)
	}
}

func TestProducePR_InlineRejectionFallsBack(t *testing.T) {
	f := newFakeForge()
	f.inlineErr = errors.New("422 position not in diff")
	l := &fakeLens{name: "llm-review-opus", output: "[P1] a.go:9999 — boom"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "(at `a.go:9999`)") {
		t.Fatalf("fallback comments = %v — a rejected anchor must not drop the finding", f.comments)
	}
	if st := lastStatusFor(t, f, "llm-review-opus"); st.State != forge.StatusFailure {
		t.Fatalf("status = %+v — the P1 must still block", st)
	}
}

func TestProducePR_AnchorlessFindingFallsBack(t *testing.T) {
	f := newFakeForge()
	l := &fakeLens{name: "llm-review-opus", output: "[P1] the whole change is wrong"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if len(f.inlineComments) != 0 {
		t.Fatalf("no inline expected without an anchor, got %v", f.inlineComments)
	}
	if len(f.comments) != 1 {
		t.Fatalf("fallback comments = %v", f.comments)
	}
	if st := lastStatusFor(t, f, "llm-review-opus"); st.State != forge.StatusFailure {
		t.Fatalf("status = %+v — a P1 without an anchor still blocks", st)
	}
}

func TestProducePR_TruncationSurfacedInStatus(t *testing.T) {
	f := newFakeForge()
	f.diff = []byte("diff --git a/a.go b/a.go\n+" + strings.Repeat("x", 500) + "\n")
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	p := producer(f, l)
	p.MaxDiffBytes = 100
	if err := p.ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	st := lastStatusFor(t, f, "llm-review-opus")
	if !strings.Contains(st.Description, "warning: diff truncated to 100 bytes") {
		t.Fatalf("status = %+v — truncation must be operator-visible", st)
	}
	if wantMax := len(fmt.Sprintf(promptTemplate, "t")) + 100; len(l.sawPrompt) != wantMax {
		t.Fatalf("prompt length = %d, want template+100 = %d", len(l.sawPrompt), wantMax)
	}
}

func TestProducePR_EmptyDiffNoOps(t *testing.T) {
	f := newFakeForge()
	f.diff = []byte("  \n")
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err != nil {
		t.Fatalf("ProducePR: %v", err)
	}
	if l.runs != 0 || len(f.posted) != 0 {
		t.Fatal("an empty diff must produce nothing")
	}
}

func TestProducePR_FinalStatusPostFailureSurfaces(t *testing.T) {
	f := newFakeForge()
	f.finalErrFor = map[string]bool{"llm-review-opus": true}
	l := &fakeLens{name: "llm-review-opus", output: "NO_FINDINGS"}
	if err := producer(f, l).ProducePR(context.Background(), 7); err == nil {
		t.Fatal("a failed final status post must surface — a swallowed one reads as a green no-op run")
	}
}

func TestParseOutput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantN   int
		wantOK  bool
		anchor0 string
	}{
		{"no findings sentinel", "NO_FINDINGS", 0, true, ""},
		{"fenced sentinel", "```\nNO_FINDINGS\n```", 0, true, ""},
		{"crlf sentinel", "NO_FINDINGS\r\n", 0, true, ""},
		{"indented finding", "  [P1] a.go:3 — boom", 1, true, "a.go:3"},
		{"hyphen separator", "[P2] b.go:4 - meh", 1, true, "b.go:4"},
		{"en-dash separator", "[P2] c.go:5 – meh", 1, true, "c.go:5"},
		{"dash-less anchor stays inline (bash parity)", "[P1] foo.go:12 fix the check", 1, true, "foo.go:12"},
		{"double-colon token splits first/last (bash parity)", "[P0] foo.go:42:13 — msg", 1, true, "foo.go:13"},
		{"crlf finding", "[P1] a.go:3 — boom\r\n", 1, true, "a.go:3"},
		{"loose finding keeps severity", "[P0] everything is broken", 1, true, ":0"},
		{"refusal fails closed", "I cannot help with that.", 0, false, ""},
		{"empty fails closed", "", 0, false, ""},
		{"prose with embedded sentinel mid-line stays closed", "maybe NO_FINDINGS later", 0, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, ok := parseOutput(tc.in)
			if ok != tc.wantOK || len(findings) != tc.wantN {
				t.Fatalf("parseOutput(%q) = %d findings, ok=%v; want %d, ok=%v", tc.in, len(findings), ok, tc.wantN, tc.wantOK)
			}
			if tc.wantN > 0 {
				got := fmt.Sprintf("%s:%d", findings[0].Path, findings[0].Line)
				if got != tc.anchor0 {
					t.Fatalf("anchor = %s, want %s", got, tc.anchor0)
				}
			}
		})
	}
}
