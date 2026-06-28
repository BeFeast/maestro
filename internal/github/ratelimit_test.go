package github

import (
	"errors"
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

func TestGHAPI_RateLimitExhaustsRetriesAndSurfacesRealError(t *testing.T) {
	body := []byte("gh: API rate limit exceeded for user ID 123. (HTTP 403)")
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
