package review

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/forge"
	"github.com/befeast/maestro/internal/state"
)

func newAttemptStore(t *testing.T) *AttemptStore {
	t.Helper()
	dir := t.TempDir()
	if err := state.Save(dir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	return &AttemptStore{StateDir: dir}
}
func scopeFor(f *fakeForge) AttemptScope {
	return AttemptScope{"owner/repo", 7, f.pr.HeadSHA, "llm-review-opus"}
}
func httpProducer(t *testing.T, s *AttemptStore, url string, now *time.Time, max int) (*Producer, *fakeForge) {
	t.Helper()
	f := newFakeForge()
	p := producer(f, &ChatLens{Stream: "llm-review-opus", BaseURL: url, APIKey: "secret", Model: "fixture"})
	p.Attempts = s
	p.MaxAttempts = max
	p.Now = func() time.Time { return *now }
	return p, f
}
func latestAttempt(t *testing.T, s *AttemptStore, scope AttemptScope) state.ReviewAttempt {
	t.Helper()
	st, err := state.Load(s.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	track := st.ReviewAttempts[scope.key()]
	if len(track.Attempts) == 0 {
		t.Fatal("no durable attempt")
	}
	return track.Attempts[len(track.Attempts)-1]
}

func TestHTTPReviewDurableQuotaRetryReopenAndLimit(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(429)
		fmt.Fprint(w, terminalBody("quota_cooldown", "same_request", false, `,"retry_at":"2030-01-01T00:01:00Z"`))
	}))
	defer srv.Close()
	s := newAttemptStore(t)
	p, f := httpProducer(t, s, srv.URL, &now, 2)
	if p.ProducePR(context.Background(), 7) == nil {
		t.Fatal("quota became successful review")
	}
	a := latestAttempt(t, s, scopeFor(f))
	if a.Outcome != "retry_wait" || a.RetryAt == nil {
		t.Fatalf("%+v", a)
	}
	artifact, err := os.ReadFile(filepath.Join(s.StateDir, "review-evidence", a.EvidenceFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(artifact), "private-token") || strings.Contains(string(artifact), "secret") {
		t.Fatal("raw response or key leaked into artifact")
	}
	if !s.evidenceIntact(a) {
		t.Fatal("artifact digest or permissions invalid")
	}
	p.Attempts = &AttemptStore{StateDir: s.StateDir}
	f.statuses = []forge.Status{{Context: "llm-review-opus", State: forge.StatusError}}
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 1 {
		t.Fatal("retry before eligibility")
	}
	now = now.Add(time.Minute)
	if !s.Due(scopeFor(f), now, 2) {
		t.Fatal("known quota retry not due")
	}
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	b := latestAttempt(t, s, scopeFor(f))
	if b.ID == a.ID || b.Outcome != "held" {
		t.Fatalf("retry not separate/bounded: %+v", b)
	}
	now = now.Add(time.Hour)
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 2 {
		t.Fatal("exhausted retry allowance ignored")
	}
	if len(f.comments)+len(f.inlineComments) != 0 {
		t.Fatal("provider failure published as product findings")
	}
}

func TestHTTPReviewHoldsUnsafeOutcomesAndDefaultOneAttempt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		max        int
	}{
		{"default one", terminalBody("upstream_transient", "same_request", false, ""), 0},
		{"unknown retry time", terminalBody("quota_cooldown", "same_request", false, ""), 2},
		{"past retry time", terminalBody("quota_cooldown", "same_request", false, `,"retry_at":"2000-01-01T00:00:00Z"`), 2},
		{"committed", terminalBody("upstream_transient", "new_turn_only", true, ""), 2},
		{"credentials", terminalBody("credentials_unavailable", "same_request", false, ""), 2},
		{"unknown", terminalBody("unknown", "same_request", false, ""), 2},
		{"invalid", terminalBody("request_invalid", "none", false, ""), 2},
		{"cancelled", terminalBody("cancelled", "same_request", false, ""), 2},
		{"unsupported", `{"error":"quota maybe"}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			s := newAttemptStore(t)
			p, f := httpProducer(t, s, srv.URL, &now, tc.max)
			_ = p.ProducePR(context.Background(), 7)
			now = now.Add(24 * time.Hour)
			_ = p.ProducePR(context.Background(), 7)
			if calls.Load() != 1 || latestAttempt(t, s, scopeFor(f)).Outcome != "held" {
				t.Fatal("unsafe outcome replayed")
			}
		})
	}
}

func TestHTTPReviewPersistenceFailuresKeepIntent(t *testing.T) {
	for _, when := range []string{"claim", "outcome"} {
		t.Run(when, func(t *testing.T) {
			now := time.Now()
			s := newAttemptStore(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if when == "outcome" {
					if err := os.WriteFile(filepath.Join(s.StateDir, "review-evidence"), []byte("fixture"), 0600); err != nil {
						t.Error(err)
					}
				}
				w.WriteHeader(503)
				fmt.Fprint(w, terminalBody("upstream_transient", "same_request", false, ""))
			}))
			defer srv.Close()
			p, f := httpProducer(t, s, srv.URL, &now, 2)
			if when == "claim" {
				if err := os.Mkdir(state.StatePath(s.StateDir)+".tmp", 0700); err != nil {
					t.Fatal(err)
				}
			}
			_ = p.ProducePR(context.Background(), 7)
			now = now.Add(time.Hour)
			_ = p.ProducePR(context.Background(), 7)
			if when == "claim" && calls.Load() != 0 {
				t.Fatal("HTTP before successful claim persistence")
			}
			if when == "outcome" && (calls.Load() != 1 || latestAttempt(t, s, scopeFor(f)).Outcome != "launch_intent") {
				t.Fatal("outcome failure erased intent")
			}
		})
	}
}

func TestHTTPReviewTimeoutRetainsUncertainty(t *testing.T) {
	now := time.Now()
	s := newAttemptStore(t)
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	p, f := httpProducer(t, s, srv.URL, &now, 2)
	p.Lenses[0].(*ChatLens).Timeout = 20 * time.Millisecond
	_ = p.ProducePR(context.Background(), 7)
	now = now.Add(time.Hour)
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 1 || latestAttempt(t, s, scopeFor(f)).Reason != "cancelled" {
		t.Fatal("timeout replayed")
	}
}

func TestHTTPReviewExistingUnclassifiedErrorAndStalePendingHold(t *testing.T) {
	now := time.Now()
	for _, status := range []forge.StatusState{forge.StatusError, forge.StatusPending} {
		s := newAttemptStore(t)
		p, f := httpProducer(t, s, "http://never-called.invalid", &now, 2)
		f.statuses = []forge.Status{{Context: "llm-review-opus", State: status, CreatedAt: now.Add(-time.Hour)}}
		if err := p.ProducePR(context.Background(), 7); err == nil {
			t.Fatal("legacy error treated as permission")
		}
		st, _ := state.Load(s.StateDir)
		if len(st.ReviewAttempts) != 0 {
			t.Fatal("unclassified history claimed a new request")
		}
	}
}

func TestHTTPReviewProcessHelper(t *testing.T) {
	dir := os.Getenv("MAESTRO_R5_TEST_STATE")
	if dir == "" {
		return
	}
	now := time.Now()
	p, _ := httpProducer(t, &AttemptStore{StateDir: dir}, os.Getenv("MAESTRO_R5_TEST_URL"), &now, 1)
	_ = p.ProducePR(context.Background(), 7)
}
func TestHTTPReviewTwoProcessesCannotDoubleLaunch(t *testing.T) {
	s := newAttemptStore(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		fmt.Fprint(w, terminalBody("unknown", "none", false, ""))
	}))
	defer srv.Close()
	commands := []*exec.Cmd{}
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPReviewProcessHelper$")
		cmd.Env = append(os.Environ(), "MAESTRO_R5_TEST_STATE="+s.StateDir, "MAESTRO_R5_TEST_URL="+srv.URL)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestReviewStateMergePreservesConcurrentReceipts(t *testing.T) {
	s := newAttemptStore(t)
	stale, err := state.Load(s.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	scope := AttemptScope{"owner/repo", 7, "head", "llm-review-opus"}
	id, err := s.Claim(scope, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	staleAfterClaim, err := state.Load(s.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(scope, id, now, terminalError(503, []byte(terminalBody("upstream_transient", "same_request", false, "")), false)); err != nil {
		t.Fatal(err)
	}
	stale.NextSlot = 10
	if err := state.Save(s.StateDir, stale); err != nil {
		t.Fatal(err)
	}
	staleAfterClaim.NextSlot = 11
	if err := state.Save(s.StateDir, staleAfterClaim); err != nil {
		t.Fatal(err)
	}
	if a := latestAttempt(t, s, scope); a.ID != id || a.Outcome != "retry_wait" {
		t.Fatalf("stale save erased receipt: %+v", a)
	}
	if err := os.Remove(filepath.Join(s.StateDir, "review-evidence", id+".json")); err != nil {
		t.Fatal(err)
	}
	if s.Due(scope, now.Add(time.Hour), 2) {
		t.Fatal("lost evidence allowed retry")
	}
}

type advancingForge struct {
	*fakeForge
	reads int
}

func (f *advancingForge) GetPR(ctx context.Context, repo string, number int) (forge.PR, error) {
	f.reads++
	pr := f.pr
	if f.reads > 1 {
		pr.HeadSHA = "new-head"
	}
	return pr, nil
}
func TestHTTPReviewHeadChangeAndOtherLensIsolation(t *testing.T) {
	now := time.Now()
	s := newAttemptStore(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		fmt.Fprint(w, terminalBody("unknown", "none", false, ""))
	}))
	defer srv.Close()
	p, f := httpProducer(t, s, srv.URL, &now, 1)
	p.Forge = &advancingForge{fakeForge: f}
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 0 {
		t.Fatal("old head was executed")
	}
	p.Forge = f
	other := &fakeLens{name: "other-lens", output: "NO_FINDINGS"}
	p.Lenses = append(p.Lenses, other)
	_ = p.ProducePR(context.Background(), 7)
	if other.runs != 1 || calls.Load() != 0 {
		t.Fatal("one held scope blocked unrelated lens")
	}
	f.pr.HeadSHA = "new-head"
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 1 {
		t.Fatal("new head did not get independent identity")
	}
}

func TestHTTPReviewCaseShadowNeverAuthorizesReplay(t *testing.T) {
	now := time.Now()
	s := newAttemptStore(t)
	var calls atomic.Int32
	body := `{"error":{"terminal":{"reason":"unknown","Reason":"upstream_transient","attempted":1,"retry_scope":"same_request","stream_committed":true,"Stream_Committed":false}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503); fmt.Fprint(w, body) }))
	defer srv.Close()
	p, f := httpProducer(t, s, srv.URL, &now, 2)
	_ = p.ProducePR(context.Background(), 7)
	now = now.Add(time.Hour)
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 1 || latestAttempt(t, s, scopeFor(f)).Reason != "terminal_malformed" {
		t.Fatal("case shadow created retry permission")
	}
}

func TestHTTPReviewUnicodeShadowNeverAuthorizesReplay(t *testing.T) {
	now := time.Now()
	s := newAttemptStore(t)
	var calls atomic.Int32
	body := `{"error":{"terminal":{"reason":"unknown","reaſon":"upstream_transient","attempted":1,"retry_scope":"same_request","stream_committed":true,"ſtream_committed":false}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503); fmt.Fprint(w, body) }))
	defer srv.Close()
	p, f := httpProducer(t, s, srv.URL, &now, 2)
	_ = p.ProducePR(context.Background(), 7)
	now = now.Add(time.Hour)
	_ = p.ProducePR(context.Background(), 7)
	if calls.Load() != 1 || latestAttempt(t, s, scopeFor(f)).Reason != "terminal_malformed" {
		t.Fatal("case shadow created retry permission")
	}
}

func TestHTTPReviewDurabilityFailureCannotGrantOrRetry(t *testing.T) {
	for _, phase := range []string{"claim", "finish"} {
		t.Run(phase, func(t *testing.T) {
			now := time.Now()
			s := newAttemptStore(t)
			var calls atomic.Int32
			writes := 0
			s.persist = func(dir string, fn func(*state.State) error) error {
				writes++
				if err := state.Update(dir, fn); err != nil {
					return err
				}
				if phase == "claim" && writes == 1 || phase == "finish" && writes == 2 {
					return fmt.Errorf("fixture state fsync failure after rename")
				}
				return nil
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(503)
				fmt.Fprint(w, terminalBody("upstream_transient", "same_request", false, ""))
			}))
			defer srv.Close()
			p, _ := httpProducer(t, s, srv.URL, &now, 2)
			_ = p.ProducePR(context.Background(), 7)
			if phase == "claim" && calls.Load() != 0 {
				t.Fatal("HTTP issued after failed durable claim")
			}
			if phase == "finish" {
				// A failed final sync must not leave a retryable receipt reachable.
				now = now.Add(time.Hour)
				_ = p.ProducePR(context.Background(), 7)
				if calls.Load() != 1 {
					t.Fatal("failed outcome sync authorized a retry")
				}
			}
		})
	}
}
