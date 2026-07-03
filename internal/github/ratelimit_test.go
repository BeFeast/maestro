package github

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubGHRunner installs a runner that returns the supplied (output, err) pairs
// in order — one per call — and records every invocation. The final pair is
// reused if the loop calls more times than provided. It also stubs the sleeper
// (recording each backoff) and the jitter fraction (0, for determinism), and
// returns a cleanup that restores the originals.
func stubGHRunner(t *testing.T, results []struct {
	out []byte
	err error
}) (*int, *[]time.Duration, func()) {
	t.Helper()
	origRunner := ghAPIRunner
	origSleep := ghAPISleep
	origJitter := ghAPIJitterFrac

	// The primary-limit pause gate (#812) is process-wide. Clear it before and
	// after so a test that arms it cannot silently short-circuit the next test's
	// runner calls, and so the etag cache and gate start from a known state.
	resetPrimaryLimitForTest()

	calls := 0
	var sleeps []time.Duration
	ghAPIRunner = func(args ...string) ([]byte, error) {
		i := calls
		if i >= len(results) {
			i = len(results) - 1
		}
		calls++
		return results[i].out, results[i].err
	}
	ghAPISleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	ghAPIJitterFrac = func() float64 { return 0 }

	cleanup := func() {
		ghAPIRunner = origRunner
		ghAPISleep = origSleep
		ghAPIJitterFrac = origJitter
		resetPrimaryLimitForTest()
	}
	return &calls, &sleeps, cleanup
}

func TestGHAPI_RetriesOnSecondaryRateLimitThenSucceeds(t *testing.T) {
	limit403 := []byte("gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)")
	results := []struct {
		out []byte
		err error
	}{
		{limit403, errors.New("exit status 1")},
		{limit403, errors.New("exit status 1")},
		{[]byte(`{"ok":true}`), nil},
	}
	calls, sleeps, cleanup := stubGHRunner(t, results)
	defer cleanup()

	out, err := ghAPIWithArgs("repos/owner/repo/pulls?state=open")
	if err != nil {
		t.Fatalf("ghAPIWithArgs() error = %v, want nil after retry recovery", err)
	}
	if string(out) != `{"ok":true}` {
		t.Fatalf("out = %q, want the recovered body", out)
	}
	if *calls != 3 {
		t.Fatalf("runner calls = %d, want 3 (2 rate-limited + 1 success)", *calls)
	}
	if len(*sleeps) != 2 {
		t.Fatalf("backoff sleeps = %d, want 2", len(*sleeps))
	}
	for i, d := range *sleeps {
		if d <= 0 {
			t.Fatalf("sleep[%d] = %s, want a positive backoff", i, d)
		}
	}
}

func TestGHAPI_HonorsRetryAfterHint(t *testing.T) {
	body := []byte("HTTP 429: API rate limit exceeded\nRetry-After: 7\n")
	results := []struct {
		out []byte
		err error
	}{
		{body, errors.New("exit status 1")},
		{[]byte("[]"), nil},
	}
	_, sleeps, cleanup := stubGHRunner(t, results)
	defer cleanup()

	if _, err := ghAPIWithArgs("repos/owner/repo/issues?state=open"); err != nil {
		t.Fatalf("ghAPIWithArgs() error = %v", err)
	}
	if len(*sleeps) != 1 {
		t.Fatalf("sleeps = %v, want exactly one", *sleeps)
	}
	if (*sleeps)[0] != 7*time.Second {
		t.Fatalf("sleep = %s, want the honored Retry-After of 7s", (*sleeps)[0])
	}
}

func TestGHAPI_SecondaryRateLimitExhaustsRetriesAndSurfacesRealError(t *testing.T) {
	// A SECONDARY (abuse/burst) 403 is the retryable case: when it never clears,
	// the wrapper exhausts its attempts and surfaces the real gh error. Acceptance
	// #3 — secondary behavior is unchanged (#812).
	body := []byte("gh: You have exceeded a secondary rate limit. (HTTP 403)")
	results := []struct {
		out []byte
		err error
	}{
		{body, errors.New("exit status 1")},
	}
	calls, sleeps, cleanup := stubGHRunner(t, results)
	defer cleanup()

	_, err := ghAPIWithArgs("repos/owner/repo/pulls?state=open")
	if err == nil {
		t.Fatal("ghAPIWithArgs() error = nil, want failure after exhausting retries")
	}
	// The real GitHub error text must reach the caller/log (acceptance #2): no
	// more opaque "exit status 1" with the rate-limit message discarded.
	if !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error = %q, want it to surface the real gh rate-limit text", err)
	}
	if *calls != ghAPIMaxAttempts {
		t.Fatalf("runner calls = %d, want %d (all attempts)", *calls, ghAPIMaxAttempts)
	}
	if len(*sleeps) != ghAPIMaxAttempts-1 {
		t.Fatalf("sleeps = %d, want %d backoffs between attempts", len(*sleeps), ghAPIMaxAttempts-1)
	}
}

// TestGHAPI_PrimaryRateLimitFailsFast covers acceptance #1: a simulated primary
// "API rate limit exceeded for user" 403 causes AT MOST ONE request (no 4×
// retry amplification), fails fast, and arms the shared pause gate.
func TestGHAPI_PrimaryRateLimitFailsFast(t *testing.T) {
	reset := time.Now().Add(37 * time.Minute).Truncate(time.Second)
	body := []byte("HTTP/2.0 403 Forbidden\n" +
		"x-ratelimit-limit: 5000\n" +
		"x-ratelimit-remaining: 0\n" +
		"x-ratelimit-reset: " + strconv.FormatInt(reset.Unix(), 10) + "\n\n" +
		`{"message":"API rate limit exceeded for user ID 123."}`)
	results := []struct {
		out []byte
		err error
	}{
		{body, errors.New("exit status 1")},
	}
	calls, sleeps, cleanup := stubGHRunner(t, results)
	defer cleanup()

	_, err := ghAPIWithArgs("repos/owner/repo/pulls?state=open")
	if err == nil {
		t.Fatal("ghAPIWithArgs() error = nil, want a fail-fast error on primary exhaustion")
	}
	if *calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (fail fast, no retry amplification)", *calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("sleeps = %d, want 0 (a primary exhaustion is not retried)", len(*sleeps))
	}
	// The gate must be armed at the parsed X-RateLimit-Reset so sibling flows
	// skip their REST cycle until then.
	st := PrimaryRateLimitState()
	if !st.Paused {
		t.Fatal("PrimaryRateLimitState().Paused = false, want the pause gate armed")
	}
	if !st.ResetAt.Equal(reset) {
		t.Fatalf("gate ResetAt = %s, want the parsed X-RateLimit-Reset %s", st.ResetAt, reset)
	}
	if st.Remaining != 0 {
		t.Fatalf("gate Remaining = %d, want 0 (last-seen X-RateLimit-Remaining)", st.Remaining)
	}
	if st.Hits != 1 {
		t.Fatalf("gate Hits = %d, want 1", st.Hits)
	}
}

// TestGHAPI_SkipsCallsWhilePrimaryPauseArmed covers acceptance #2: while the
// primary bucket is exhausted, core-REST calls are short-circuited (never
// issued) until the reset — but rate_limit stays exempt so the budget can be
// re-probed.
func TestGHAPI_SkipsCallsWhilePrimaryPauseArmed(t *testing.T) {
	calls, _, cleanup := stubGHRunner(t, []struct {
		out []byte
		err error
	}{
		{[]byte(`{"ok":true}`), nil},
	})
	defer cleanup()

	reset := time.Now().Add(20 * time.Minute)
	notePrimaryRateLimit(reset, 0)

	_, err := ghAPIWithArgs("repos/owner/repo/pulls?state=open")
	if err == nil {
		t.Fatal("ghAPIWithArgs() error = nil, want a skip error while the gate is armed")
	}
	var primaryErr *PrimaryRateLimitError
	if !errors.As(err, &primaryErr) {
		t.Fatalf("error = %v, want a *PrimaryRateLimitError", err)
	}
	if *calls != 0 {
		t.Fatalf("runner calls = %d, want 0 (the doomed call must never be issued)", *calls)
	}

	// rate_limit is exempt: it does not consume the core quota and is the probe
	// used to confirm the bucket refilled.
	if _, err := ghAPIWithArgs("rate_limit"); err != nil {
		t.Fatalf("rate_limit while paused: err = %v, want it exempt from the gate", err)
	}
	if *calls != 1 {
		t.Fatalf("runner calls after rate_limit = %d, want 1 (rate_limit issued)", *calls)
	}
	if skipped := PrimaryRateLimitState().Skipped; skipped != 1 {
		t.Fatalf("gate Skipped = %d, want 1 (only the pulls call skipped)", skipped)
	}
}

func TestGHAPI_NonRateLimitErrorDoesNotRetry(t *testing.T) {
	body := []byte("gh: Not Found (HTTP 404)")
	results := []struct {
		out []byte
		err error
	}{
		{body, errors.New("exit status 1")},
	}
	calls, sleeps, cleanup := stubGHRunner(t, results)
	defer cleanup()

	_, err := ghAPIWithArgs("repos/owner/repo/pulls/999")
	if err == nil {
		t.Fatal("ghAPIWithArgs() error = nil, want the 404 surfaced")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("error = %q, want the 404 detail surfaced", err)
	}
	if *calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (no retry on a non-rate-limit error)", *calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("sleeps = %d, want 0 (no backoff on a non-rate-limit error)", len(*sleeps))
	}
}

func TestParseGHRateLimit(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		wantLimit  bool
		wantAfter  time.Duration
		wantAfterY bool
	}{
		{"secondary", "You have exceeded a secondary rate limit (HTTP 403)", true, 0, false},
		{"primary", "gh: API rate limit exceeded (HTTP 403)", true, 0, false},
		{"http429", "HTTP 429: too many requests", true, 0, false},
		{"abuse403", "gh: You have triggered an abuse detection mechanism (HTTP 403)", true, 0, false},
		{"retry-after", "HTTP 429\nRetry-After: 12", true, 12 * time.Second, true},
		{"plain-403", "gh: Forbidden (HTTP 403)", false, 0, false},
		{"404", "gh: Not Found (HTTP 404)", false, 0, false},
		{"success", `{"ok":true}`, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after, limited := parseGHRateLimit([]byte(tc.out))
			if limited != tc.wantLimit {
				t.Fatalf("rateLimited = %v, want %v", limited, tc.wantLimit)
			}
			if tc.wantAfterY && after != tc.wantAfter {
				t.Fatalf("retryAfter = %s, want %s", after, tc.wantAfter)
			}
			if !tc.wantAfterY && after != 0 {
				t.Fatalf("retryAfter = %s, want 0 (no explicit hint)", after)
			}
		})
	}
}

func TestGHBackoffDelay_IsBoundedAndGrows(t *testing.T) {
	orig := ghAPIJitterFrac
	ghAPIJitterFrac = func() float64 { return 0 }
	defer func() { ghAPIJitterFrac = orig }()

	prev := time.Duration(0)
	for attempt := 0; attempt < 6; attempt++ {
		d := ghBackoffDelay(attempt)
		if d <= 0 {
			t.Fatalf("attempt %d: delay = %s, want positive", attempt, d)
		}
		if d > ghAPIMaxBackoff {
			t.Fatalf("attempt %d: delay = %s, want <= cap %s", attempt, d, ghAPIMaxBackoff)
		}
		if attempt > 0 && d < prev {
			t.Fatalf("attempt %d: delay %s < previous %s, want monotonic until cap", attempt, d, prev)
		}
		prev = d
	}
}

func TestGHErrorDetail_CollapsesAndBounds(t *testing.T) {
	if got := ghErrorDetail([]byte("  \n\t ")); got != "" {
		t.Fatalf("blank detail = %q, want empty", got)
	}
	multiline := ghErrorDetail([]byte("gh: error\n  on two lines"))
	if strings.Contains(multiline, "\n") {
		t.Fatalf("detail = %q, want newlines collapsed", multiline)
	}
	long := ghErrorDetail([]byte(strings.Repeat("x", 1000)))
	if len([]rune(long)) > 401 {
		t.Fatalf("detail rune length = %d, want bounded (~400 + ellipsis)", len([]rune(long)))
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatalf("long detail = %q, want truncation ellipsis", long[len(long)-10:])
	}
}

// Guard: the secondary-rate-limit fixture the incident reproduced from is still
// classified as retryable.
func TestParseGHRateLimit_IncidentFixture(t *testing.T) {
	fixture := "gh: You have exceeded a secondary rate limit. (HTTP 403)"
	if _, limited := parseGHRateLimit([]byte(fixture)); !limited {
		t.Fatal("the 2026-06-28 secondary 403 fixture must be retryable")
	}
}

// TestClassifyGHRateLimit pins the primary-vs-secondary split (#812): only a
// clear primary signal (X-RateLimit-Remaining: 0 or the "api rate limit
// exceeded for user" signature) is non-retryable; every other rate-limit-ish
// response stays secondary (retryable).
func TestClassifyGHRateLimit(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want ghRateLimitKind
	}{
		{"primary-user-signature", "gh: API rate limit exceeded for user ID 123. (HTTP 403)", ghRateLimitPrimary},
		{"primary-header-remaining-0", "HTTP/2.0 403 Forbidden\nx-ratelimit-remaining: 0\nx-ratelimit-reset: 1893456000\n\n{}", ghRateLimitPrimary},
		{"primary-header-wins-over-text", "HTTP/2.0 403\nx-ratelimit-remaining: 0\n\nYou have exceeded a secondary rate limit", ghRateLimitPrimary},
		{"secondary", "You have exceeded a secondary rate limit (HTTP 403)", ghRateLimitSecondary},
		{"abuse", "gh: You have triggered an abuse detection mechanism (HTTP 403)", ghRateLimitSecondary},
		{"retry-after-hint", "HTTP 429\nRetry-After: 12", ghRateLimitSecondary},
		{"bare-429", "HTTP 429: too many requests", ghRateLimitSecondary},
		{"remaining-positive-not-primary", "HTTP/2.0 403\nx-ratelimit-remaining: 42\n\nYou have exceeded a secondary rate limit", ghRateLimitSecondary},
		{"plain-403", "gh: Forbidden (HTTP 403)", ghRateLimitNone},
		{"404", "gh: Not Found (HTTP 404)", ghRateLimitNone},
		{"success", `{"ok":true}`, ghRateLimitNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyGHRateLimit([]byte(tc.out)).kind; got != tc.want {
				t.Fatalf("kind = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClassifyGHRateLimit_ParsesResetAndRemaining(t *testing.T) {
	out := []byte("HTTP/2.0 403 Forbidden\nx-ratelimit-remaining: 0\nx-ratelimit-reset: 1893456000\n\n{}")
	info := classifyGHRateLimit(out)
	if info.remaining != 0 {
		t.Fatalf("remaining = %d, want 0", info.remaining)
	}
	if want := time.Unix(1893456000, 0); !info.resetAt.Equal(want) {
		t.Fatalf("resetAt = %s, want %s", info.resetAt, want)
	}
}

// TestPrimaryRateLimitGate exercises the shared pause gate directly: arming,
// auto-expiry at reset, refresh-only-extends, and the fallback pause when no
// reset was parsed. Injects primaryLimitNow so no wall-clock sleeping is needed.
func TestPrimaryRateLimitGate(t *testing.T) {
	resetPrimaryLimitForTest()
	origNow := primaryLimitNow
	defer func() { primaryLimitNow = origNow; resetPrimaryLimitForTest() }()

	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	now := base
	primaryLimitNow = func() time.Time { return now }

	if paused, _ := primaryRateLimitActive(); paused {
		t.Fatal("gate armed before any exhaustion")
	}

	reset := base.Add(40 * time.Minute)
	notePrimaryRateLimit(reset, 0)
	paused, gotReset := primaryRateLimitActive()
	if !paused || !gotReset.Equal(reset) {
		t.Fatalf("after arm: paused=%v reset=%s, want true %s", paused, gotReset, reset)
	}

	// A refresh carrying an EARLIER reset must not shorten the window.
	notePrimaryRateLimit(base.Add(5*time.Minute), 0)
	if _, gotReset = primaryRateLimitActive(); !gotReset.Equal(reset) {
		t.Fatalf("after early refresh: reset=%s, want unchanged %s", gotReset, reset)
	}

	// Advancing past the reset auto-clears the gate (no explicit clear needed).
	now = reset.Add(time.Second)
	if paused, _ = primaryRateLimitActive(); paused {
		t.Fatal("gate still armed after the reset instant passed")
	}

	// A zero reset (no header parsed) falls back to a bounded fixed pause.
	notePrimaryRateLimit(time.Time{}, -1)
	paused, gotReset = primaryRateLimitActive()
	if !paused {
		t.Fatal("fallback pause not armed for a zero reset")
	}
	if want := now.Add(primaryLimitFallbackPause); !gotReset.Equal(want) {
		t.Fatalf("fallback reset = %s, want %s", gotReset, want)
	}
}
