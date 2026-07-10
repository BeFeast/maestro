package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexUsageLimitSignature is the exact OpenAI/Codex usage-limit message that
// issue #458 reported slipping past detection. The inserted word "usage" breaks
// the contiguous "you've hit your limit" match, so it must be caught by the
// "(usage )?" group and/or the codex/settings/usage marker.
const codexUsageLimitSignature = "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to see your usage. You can try again at May 30th, 2026 8:13 PM."

// proxyCoolingDownSignature is the exact CLIProxyAPI quota-exhaustion line that
// killed sessions sup-275/276/277 (#859, root cause of #835's day-long stall,
// 2026-07-09). The proxy returns quota exhaustion as a 429 whose "rejected
// (429)" shape sat outside the old http_429 marker list, alongside an "All
// credentials ... are cooling down" phrase no pattern matched — so the deaths
// were recorded rate_limit_hit=false and burned the per-issue retry budget.
const proxyCoolingDownSignature = "API Error: Request rejected (429) · All credentials for model claude-opus-4-8 are cooling down"

func TestDetectRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantHit   bool
		wantLabel string
	}{
		{
			name:      "hit your limit",
			input:     "Error: You've hit your limit for the day.",
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "hit your limit without apostrophe",
			input:     "Youve hit your limit",
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "codex usage limit signature (issue #458)",
			input:     codexUsageLimitSignature,
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "hit your usage limit standalone",
			input:     "You've hit your usage limit.",
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			// #808: the Claude subscription "session limit" phrasing inserts
			// "session" between "your" and "limit"; the qualifier group must
			// admit it just as it admits "usage".
			name:      "claude session limit (#808)",
			input:     "You've hit your session limit · resets 9am (UTC)",
			wantHit:   true,
			wantLabel: "hit_limit",
		},
		{
			name:      "claude fable plan limit (#808)",
			input:     "You've reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model.",
			wantHit:   true,
			wantLabel: "reached_limit",
		},
		{
			name:      "claude out of extra usage (#808)",
			input:     "You're out of extra usage · resets 4:10pm (UTC)",
			wantHit:   true,
			wantLabel: "out_of_usage",
		},
		{
			name:      "HTTP 429 status code",
			input:     "HTTP error 429: Too Many Requests",
			wantHit:   true,
			wantLabel: "http_429",
		},
		{
			name:      "429 in error message",
			input:     "Error: received status 429 from API",
			wantHit:   true,
			wantLabel: "http_429",
		},
		{
			name:      "rate limit exceeded",
			input:     "Rate limit exceeded. Please retry after 60 seconds.",
			wantHit:   true,
			wantLabel: "rate_limit_exceeded",
		},
		{
			name:      "rate limit exceeded case insensitive",
			input:     "RATE LIMIT EXCEEDED",
			wantHit:   true,
			wantLabel: "rate_limit_exceeded",
		},
		{
			name:      "quota exceeded",
			input:     "Error: Quota exceeded for project xyz",
			wantHit:   true,
			wantLabel: "quota_exceeded",
		},
		{
			name:      "too many requests",
			input:     "Too many requests, please slow down",
			wantHit:   true,
			wantLabel: "too_many_requests",
		},
		{
			name:      "resource exhausted gRPC",
			input:     "rpc error: code = ResourceExhausted desc = request limit",
			wantHit:   true,
			wantLabel: "resource_exhausted",
		},
		{
			// #859: the exact CLIProxyAPI line. It matches both the
			// proxy_cooling_down pattern and the widened http_429; the
			// more-specific proxy_cooling_down is listed first, so it wins.
			name:      "CLIProxyAPI credentials cooling down (#859)",
			input:     proxyCoolingDownSignature,
			wantHit:   true,
			wantLabel: "proxy_cooling_down",
		},
		{
			name:      "credentials cooling down bare phrasing (#859)",
			input:     "All credentials for model claude-opus-4-8 are cooling down",
			wantHit:   true,
			wantLabel: "proxy_cooling_down",
		},
		{
			// #859 review: a long, fully-qualified model ID (>40 chars) must
			// still classify. A fixed ".{0,40}" bound missed these, dropping the
			// death to the bare "rejected (429)" and burning the retry budget.
			name:      "cooling down with long model id (#859 review)",
			input:     "API Error: Request rejected (429) · All credentials for model us.anthropic.claude-opus-4-8-20250805-canary-v1:0 are cooling down",
			wantHit:   true,
			wantLabel: "proxy_cooling_down",
		},
		{
			// #859: "rejected (429)" now matches the widened http_429 even
			// without the cooling-down phrase.
			name:      "Request rejected (429) widened http_429 (#859)",
			input:     "API Error: Request rejected (429)",
			wantHit:   true,
			wantLabel: "http_429",
		},
		{
			// #859 negative: "cooling down" in unrelated prose (no "credentials
			// for model" prefix) must not classify.
			name:    "cooling down in unrelated prose no match (#859)",
			input:   "the compressor needs a cooling down period in HVAC docs",
			wantHit: false,
		},
		{
			name:      "multiline with rate limit on later line",
			input:     "Starting task...\nProcessing...\nError: rate limit exceeded\nRetrying...",
			wantHit:   true,
			wantLabel: "rate_limit_exceeded",
		},
		{
			name:    "normal output no rate limit",
			input:   "tokens 50000 (in 10000 / out 40000)\nTask completed successfully.",
			wantHit: false,
		},
		{
			name:    "empty string",
			input:   "",
			wantHit: false,
		},
		{
			name:    "429 as part of larger number — no match",
			input:   "processed 14290 records",
			wantHit: false,
		},
		{
			name:      "429 standalone in error context",
			input:     "status: 429",
			wantHit:   true,
			wantLabel: "http_429",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotHit, gotLabel := DetectRateLimit(tc.input)
			if gotHit != tc.wantHit {
				t.Errorf("DetectRateLimit() hit = %v, want %v", gotHit, tc.wantHit)
			}
			if tc.wantHit && gotLabel != tc.wantLabel {
				t.Errorf("DetectRateLimit() label = %q, want %q", gotLabel, tc.wantLabel)
			}
			if !tc.wantHit && gotLabel != "" {
				t.Errorf("DetectRateLimit() label = %q, want empty when no hit", gotLabel)
			}
		})
	}
}

func TestOutputContainsRateLimit_ClaudeMessage(t *testing.T) {
	output := "Error: You've hit your limit for Claude. Please wait before trying again."
	if !OutputContainsRateLimit(output) {
		t.Error("should detect Claude rate limit message")
	}
}

func TestOutputContainsRateLimit_CaseInsensitive(t *testing.T) {
	output := "ERROR: YOU'VE HIT YOUR LIMIT"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect rate limit case-insensitively")
	}
}

func TestOutputContainsRateLimit_TooManyRequests(t *testing.T) {
	output := "HTTP 429 Too Many Requests"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'too many requests'")
	}
}

func TestOutputContainsRateLimit_QuotaExceeded(t *testing.T) {
	output := "API error: quota exceeded for this billing period"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'quota exceeded'")
	}
}

func TestOutputContainsRateLimit_RateLimitUnderscore(t *testing.T) {
	output := `{"error": {"type": "rate_limit_error", "message": "rate limited"}}`
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'rate_limit'")
	}
}

func TestOutputContainsRateLimit_NoMatch(t *testing.T) {
	output := "Worker completed successfully. All tests passing."
	if OutputContainsRateLimit(output) {
		t.Error("should not detect rate limit in normal output")
	}
}

func TestOutputContainsRateLimit_EmptyString(t *testing.T) {
	if OutputContainsRateLimit("") {
		t.Error("should not detect rate limit in empty string")
	}
}

func TestIsRateLimited_FromFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	content := "Starting worker...\nProcessing issue #42\nError: You've hit your limit for Claude.\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if !IsRateLimited(logFile) {
		t.Error("should detect rate limit from log file")
	}
}

func TestIsRateLimited_NoRateLimit(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	content := "Starting worker...\nDone.\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if IsRateLimited(logFile) {
		t.Error("should not detect rate limit in normal log file")
	}
}

func TestIsRateLimited_EmptyPath(t *testing.T) {
	if IsRateLimited("") {
		t.Error("should return false for empty path")
	}
}

func TestIsRateLimited_NonexistentFile(t *testing.T) {
	if IsRateLimited("/nonexistent/path/worker.log") {
		t.Error("should return false for nonexistent file")
	}
}

func TestOutputContainsRateLimit_HTTP429(t *testing.T) {
	output := "Request failed with status 429"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect HTTP 429 status code")
	}
}

func TestOutputContainsRateLimit_ResourceExhausted(t *testing.T) {
	output := "gRPC error: RESOURCE_EXHAUSTED: quota exceeded"
	if !OutputContainsRateLimit(output) {
		t.Error("should detect 'resource_exhausted'")
	}
}

// TestRateLimit_CodexSignature exercises every detection entry point against
// the exact Codex/OpenAI usage-limit string from issue #458, ensuring the
// "usage" variant is recognised end to end.
func TestRateLimit_CodexSignature(t *testing.T) {
	if hit, label := DetectRateLimit(codexUsageLimitSignature); !hit {
		t.Errorf("DetectRateLimit should recognise the codex usage-limit signature, got hit=false")
	} else if label != "hit_limit" {
		t.Errorf("DetectRateLimit label = %q, want %q", label, "hit_limit")
	}

	if !OutputContainsRateLimit(codexUsageLimitSignature) {
		t.Error("OutputContainsRateLimit should recognise the codex usage-limit signature")
	}

	dir := t.TempDir()
	logFile := filepath.Join(dir, "codex.log")
	content := "Starting worker...\nProcessing issue #247\n" + codexUsageLimitSignature + "\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if !IsRateLimited(logFile) {
		t.Error("IsRateLimited should recognise the codex usage-limit signature from a log file")
	}
}

// TestRateLimit_CodexUsageURLOnly verifies the codex/settings/usage marker is
// detected even when the "you've hit your ... limit" phrasing is absent (e.g.
// only the help URL is logged).
func TestRateLimit_CodexUsageURLOnly(t *testing.T) {
	output := "See https://chatgpt.com/codex/settings/usage for details."
	if hit, label := DetectRateLimit(output); !hit {
		t.Error("DetectRateLimit should match the codex/settings/usage marker")
	} else if label != "codex_usage_limit" {
		t.Errorf("DetectRateLimit label = %q, want %q", label, "codex_usage_limit")
	}
	if !OutputContainsRateLimit(output) {
		t.Error("OutputContainsRateLimit should match the codex/settings/usage marker")
	}
}

// TestRateLimit_ClaudeStillMatches guards against regression: the original
// Claude "You've hit your limit" phrasing (no "usage" word) must still match.
func TestRateLimit_ClaudeStillMatches(t *testing.T) {
	output := "You've hit your limit"
	if hit, label := DetectRateLimit(output); !hit {
		t.Error("DetectRateLimit should still match the Claude 'You've hit your limit' phrasing")
	} else if label != "hit_limit" {
		t.Errorf("DetectRateLimit label = %q, want %q", label, "hit_limit")
	}
	if !OutputContainsRateLimit(output) {
		t.Error("OutputContainsRateLimit should still match the Claude phrasing")
	}
}

// TestRateLimit_ProxyCoolingDownSignature is the #859 regression guard. The
// exact CLIProxyAPI quota-exhaustion line that killed sup-275/276/277 must be
// classified as a rate-limit across every entry point. Because it carries no
// "try again at/resets" clause, the reset extractor must tolerate it — yield no
// reset (ok=false), not a parse error — which leaves ClassifyRateLimit
// low-confidence. The escalation to RateLimitHit=true then happens on the
// usage-limit path (usagelimit.go), not the provider-limit path.
func TestRateLimit_ProxyCoolingDownSignature(t *testing.T) {
	if hit, label := DetectRateLimit(proxyCoolingDownSignature); !hit {
		t.Fatal("DetectRateLimit should classify the CLIProxyAPI cooling-down signature")
	} else if label != "proxy_cooling_down" {
		t.Errorf("label = %q, want proxy_cooling_down", label)
	}
	if !OutputContainsRateLimit(proxyCoolingDownSignature) {
		t.Error("OutputContainsRateLimit should classify the CLIProxyAPI cooling-down signature")
	}

	// No "try again at/resets" clause: the extractor must return ok=false
	// (no reset time) rather than erroring or panicking (#859 requirement 2).
	if reset, ok := ParseRateLimitReset(proxyCoolingDownSignature); ok {
		t.Errorf("ParseRateLimitReset = %v, want no reset — the proxy signature states no reset window", reset)
	}

	// Classification is therefore low-confidence (no parseable reset).
	hit, label, confidence, resetAt := ClassifyRateLimit(proxyCoolingDownSignature)
	if !hit || label != "proxy_cooling_down" {
		t.Errorf("ClassifyRateLimit hit/label = %v/%q, want true/proxy_cooling_down", hit, label)
	}
	if confidence != "low" {
		t.Errorf("confidence = %q, want low (no reset hint)", confidence)
	}
	if !resetAt.IsZero() {
		t.Errorf("resetAt = %v, want zero on low confidence", resetAt)
	}

	// The terminal error in a dead worker's log tail must still be recognised.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "sup-277.log")
	content := "Starting worker sup-277 on issue #835\nProcessing...\n" + proxyCoolingDownSignature + "\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if !IsRateLimited(logFile) {
		t.Error("IsRateLimited should recognise the CLIProxyAPI cooling-down signature from a log file")
	}
}

func TestParseRateLimitReset_CodexSignature(t *testing.T) {
	got, ok := ParseRateLimitReset(codexUsageLimitSignature)
	if !ok {
		t.Fatal("ParseRateLimitReset should parse the reset hint from the codex signature")
	}
	want := time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseRateLimitReset = %v, want %v", got, want)
	}
}

// codexWriteStdinClosed is the exact codex tools-router error from issue #663
// (apertune session apt-2, 2026-06-04). Codex emits this when the worker's
// stdin gets closed mid-session; the worker recovers and continues. It MUST
// NOT be classified as a backend rate-limit, otherwise the orchestrator
// triggers a false fallback to a more expensive backend and can loop the
// session to the retry cap.
const codexWriteStdinClosed = "2026-06-04T19:41:32Z ERROR codex_core::tools::router: error=write_stdin failed: stdin is closed\nfor this session; rerun exec_command with tty=true to keep stdin open"

// TestRateLimit_CodexWriteStdinError_NotClassified is the #663 regression
// guard. The codex tools-router `write_stdin failed: stdin is closed` line
// is a transient tool error, not a provider rate-limit, so all three entry
// points must report rate_limited=false.
func TestRateLimit_CodexWriteStdinError_NotClassified(t *testing.T) {
	if hit, label := DetectRateLimit(codexWriteStdinClosed); hit {
		t.Errorf("DetectRateLimit incorrectly classified codex write_stdin error as rate-limit (label=%q)", label)
	}
	if OutputContainsRateLimit(codexWriteStdinClosed) {
		t.Error("OutputContainsRateLimit incorrectly classified codex write_stdin error as rate-limit")
	}
	dir := t.TempDir()
	logFile := filepath.Join(dir, "apt-2.log")
	content := "Starting worker apt-2 on issue #471\nProcessing...\n" + codexWriteStdinClosed + "\nworker continued, opened PR.\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	if IsRateLimited(logFile) {
		t.Error("IsRateLimited incorrectly classified codex write_stdin error log as rate-limit")
	}
}

// TestRateLimit_BareDigitsNotClassified guards against the previous bare-429
// substring match. Stray "429" sequences in IDs, version strings, or token
// counts must not classify as HTTP 429 — only a 429 paired with an HTTP /
// status / error context word counts.
func TestRateLimit_BareDigitsNotClassified(t *testing.T) {
	cases := []string{
		"processed 14290 records",
		"build artifact v0.4.29 ready",
		"tokens 4290 (in 1000 / out 3290)",
		"commit sha 429abcd",
	}
	for _, in := range cases {
		if hit, label := DetectRateLimit(in); hit {
			t.Errorf("DetectRateLimit(%q) incorrectly hit (label=%q) — bare 429 must not classify", in, label)
		}
		if OutputContainsRateLimit(in) {
			t.Errorf("OutputContainsRateLimit(%q) incorrectly matched — bare 429 must not classify", in)
		}
	}
}

// TestRateLimit_BarePhrasesNotClassified guards against the previous bare
// "rate limit" / "rate_limit" substring match. Generic mentions in prompt
// context, runbook docs, or config field names must not classify — only a
// concrete "rate limit exceeded/reached/hit/error" phrasing counts.
func TestRateLimit_BarePhrasesNotClassified(t *testing.T) {
	cases := []string{
		"see the rate limit section in the runbook",
		`{"config": {"rate_limit": 100}}`,
		"the API has a rate limit of 60 req/min",
	}
	for _, in := range cases {
		if hit, label := DetectRateLimit(in); hit {
			t.Errorf("DetectRateLimit(%q) incorrectly hit (label=%q) — bare 'rate limit' must not classify", in, label)
		}
		if OutputContainsRateLimit(in) {
			t.Errorf("OutputContainsRateLimit(%q) incorrectly matched — bare 'rate limit' must not classify", in)
		}
	}
}

// TestClassifyRateLimit_Confidence verifies that ClassifyRateLimit returns
// "high" only when a parseable reset hint is present, and "low" otherwise.
// Per #663, the orchestrator must use this confidence signal to gate backend
// fallback — a low-confidence detection (reset=unknown) does NOT justify
// switching to a more-expensive backend.
func TestClassifyRateLimit_Confidence(t *testing.T) {
	t.Run("high confidence with reset", func(t *testing.T) {
		hit, label, confidence, resetAt := ClassifyRateLimit(codexUsageLimitSignature)
		if !hit {
			t.Fatal("expected hit=true on codex usage-limit signature")
		}
		if label != "hit_limit" {
			t.Errorf("label = %q, want hit_limit", label)
		}
		if confidence != "high" {
			t.Errorf("confidence = %q, want high (parseable reset hint)", confidence)
		}
		want := time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC)
		if !resetAt.Equal(want) {
			t.Errorf("resetAt = %v, want %v", resetAt, want)
		}
	})

	t.Run("low confidence without reset", func(t *testing.T) {
		hit, _, confidence, resetAt := ClassifyRateLimit("Error: rate limit exceeded.")
		if !hit {
			t.Fatal("expected hit=true on 'rate limit exceeded'")
		}
		if confidence != "low" {
			t.Errorf("confidence = %q, want low (no reset hint)", confidence)
		}
		if !resetAt.IsZero() {
			t.Errorf("resetAt = %v, want zero on low confidence", resetAt)
		}
	})

	t.Run("no match", func(t *testing.T) {
		hit, label, confidence, _ := ClassifyRateLimit(codexWriteStdinClosed)
		if hit {
			t.Errorf("expected hit=false on write_stdin error, got label=%q confidence=%q", label, confidence)
		}
		if confidence != "" {
			t.Errorf("confidence = %q, want empty when no hit", confidence)
		}
	})
}

func TestParseRateLimitReset_Variants(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
		ok    bool
	}{
		{
			name:  "ordinal with PM",
			input: "try again at May 30th, 2026 8:13 PM.",
			want:  time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "ordinal AM",
			input: "Please try again at January 1st, 2027 9:05 AM",
			want:  time.Date(2027, time.January, 1, 9, 5, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "try again after wording",
			input: "Rate limited; try again after June 3rd, 2026 12:00 PM.",
			want:  time.Date(2026, time.June, 3, 12, 0, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "iso 8601",
			input: "you've hit your limit; try again at 2026-05-30T20:13:00Z",
			want:  time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC),
			ok:    true,
		},
		{
			// Parenthesised timezone followed by a sentence period: the
			// period-after-")" tail must be trimmed before the "$"-anchored
			// timezone strip runs, or the date-bearing layouts see a trailing
			// "(UTC)" and reject the hint (#808 review).
			name:  "parenthesised timezone with trailing period",
			input: "try again at May 30th, 2026 8:13 PM (UTC).",
			want:  time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "no reset hint",
			input: "You've hit your usage limit.",
			ok:    false,
		},
		{
			name:  "unparseable timestamp",
			input: "try again at some point soon.",
			ok:    false,
		},
		{
			name:  "empty",
			input: "",
			ok:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRateLimitReset(tc.input)
			if ok != tc.ok {
				t.Fatalf("ParseRateLimitReset ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if tc.ok && !got.Equal(tc.want) {
				t.Errorf("ParseRateLimitReset = %v, want %v", got, tc.want)
			}
		})
	}
}

// #808 live Claude/Fable subscription signatures (BeFeast ok-player fleet,
// 2026-07-03). Both state the reset as "resets <clock> (UTC)", a shape the
// pre-#808 "try again at <when>" reset parser did not recognise, so the death
// stayed low-confidence and no failover fired.
const (
	claudeSessionLimitSignature = "You've hit your session limit · resets 9am (UTC)"
	claudeExtraUsageSignature   = "You're out of extra usage · resets 4:10pm (UTC)"
)

// TestParseRateLimitResetAt_ClaudeResetsTZ verifies the "resets <clock> (TZ)"
// hint parses to the next occurrence of that wall-clock time in UTC, with the
// hour-only ("9am") and minute-bearing ("4:10pm") shapes both handled and the
// trailing "(UTC)" stripped (#808).
func TestParseRateLimitResetAt_ClaudeResetsTZ(t *testing.T) {
	tests := []struct {
		name  string
		input string
		now   time.Time
		want  time.Time
		ok    bool
	}{
		{
			name:  "hour-only am resolves same day",
			input: claudeSessionLimitSignature,
			now:   time.Date(2026, time.July, 3, 6, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 3, 9, 0, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "hour-only am already passed rolls next day",
			input: claudeSessionLimitSignature,
			now:   time.Date(2026, time.July, 3, 10, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 4, 9, 0, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "minute-bearing pm resolves same day",
			input: claudeExtraUsageSignature,
			now:   time.Date(2026, time.July, 3, 9, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 3, 16, 10, 0, 0, time.UTC),
			ok:    true,
		},
		{
			// Sentence-terminated reset: the period lands AFTER the "(UTC)"
			// paren, so the "$"-anchored timezone strip only fires once the
			// period is trimmed first. Regression for the trim-order defect
			// that left an unparseable "9am (UTC)" and downgraded an otherwise
			// high-confidence reset to low (#808 review).
			name:  "hour-only am with trailing sentence period",
			input: "You've hit your session limit · resets 9am (UTC).",
			now:   time.Date(2026, time.July, 3, 6, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 3, 9, 0, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "minute-bearing pm with trailing sentence period",
			input: "You're out of extra usage · resets 4:10pm (UTC).",
			now:   time.Date(2026, time.July, 3, 9, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 3, 16, 10, 0, 0, time.UTC),
			ok:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRateLimitResetAt(tc.input, tc.now)
			if ok != tc.ok {
				t.Fatalf("ParseRateLimitResetAt ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if tc.ok && !got.Equal(tc.want) {
				t.Errorf("ParseRateLimitResetAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyRateLimitAt_ClaudeResetsTZ_HighConfidence: the Claude signatures
// must classify HIGH-confidence so the orchestrator's provider-limit fallover
// fires with the provider-stated reset as the backend cooldown, rather than a
// blind 30-minute re-probe (#808).
func TestClassifyRateLimitAt_ClaudeResetsTZ_HighConfidence(t *testing.T) {
	now := time.Date(2026, time.July, 3, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		input string
		label string
		want  time.Time
	}{
		{"session limit", claudeSessionLimitSignature, "hit_limit", time.Date(2026, time.July, 3, 9, 0, 0, 0, time.UTC)},
		{"out of extra usage", claudeExtraUsageSignature, "out_of_usage", time.Date(2026, time.July, 3, 16, 10, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit, label, confidence, resetAt := ClassifyRateLimitAt(tc.input, now)
			if !hit {
				t.Fatalf("expected hit=true on %q", tc.input)
			}
			if label != tc.label {
				t.Errorf("label = %q, want %q", label, tc.label)
			}
			if confidence != "high" {
				t.Errorf("confidence = %q, want high (parseable reset)", confidence)
			}
			if !resetAt.Equal(tc.want) {
				t.Errorf("resetAt = %v, want %v", resetAt, tc.want)
			}
		})
	}
}

// codexTimeOnlySignature is the live #805 signature (BeFeast/ok-folio fleet,
// 2026-07-02): codex states only a wall-clock reset with no date. The
// date-bearing layouts reject it, so before #805 the reset went unparsed, the
// death stayed low-confidence, and no failover fired.
const codexTimeOnlySignature = "You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/codex/settings/usage) or try again at 12:30 PM."

// TestParseRateLimitResetAt_TimeOnly verifies clock-only reset hints resolve
// against the reference time: same day when the clock time is still ahead,
// next day when it has already passed (#805).
func TestParseRateLimitResetAt_TimeOnly(t *testing.T) {
	tests := []struct {
		name  string
		input string
		now   time.Time
		want  time.Time
		ok    bool
	}{
		{
			name:  "clock ahead of now resolves same day",
			input: codexTimeOnlySignature,
			now:   time.Date(2026, time.July, 2, 1, 17, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 2, 12, 30, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "clock already passed rolls to next day",
			input: codexTimeOnlySignature,
			now:   time.Date(2026, time.July, 2, 22, 45, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 3, 12, 30, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "clock equal to now rolls to next day",
			input: codexTimeOnlySignature,
			now:   time.Date(2026, time.July, 2, 12, 30, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 3, 12, 30, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "lowercase meridiem",
			input: "You've hit your usage limit. try again at 8:13pm.",
			now:   time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 2, 20, 13, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "24h clock",
			input: "quota exceeded; try again at 09:15",
			now:   time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.July, 3, 9, 15, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "date-bearing hint keeps absolute parse",
			input: codexUsageLimitSignature,
			now:   time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.May, 30, 20, 13, 0, 0, time.UTC),
			ok:    true,
		},
		{
			name:  "unparseable hint still fails",
			input: "try again at some point soon.",
			now:   time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC),
			ok:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRateLimitResetAt(tc.input, tc.now)
			if ok != tc.ok {
				t.Fatalf("ParseRateLimitResetAt ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if tc.ok && !got.Equal(tc.want) {
				t.Errorf("ParseRateLimitResetAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyRateLimitAt_TimeOnlyIsHighConfidence: the #805 signature must
// classify as a HIGH-confidence rate limit so the orchestrator's provider-limit
// fallover fires instead of burning the per-issue retry budget on retries
// against a quota-dead backend.
func TestClassifyRateLimitAt_TimeOnlyIsHighConfidence(t *testing.T) {
	now := time.Date(2026, time.July, 2, 1, 17, 0, 0, time.UTC)
	hit, label, confidence, resetAt := ClassifyRateLimitAt(codexTimeOnlySignature, now)
	if !hit {
		t.Fatal("expected hit=true on the codex time-only usage-limit signature")
	}
	if label != "hit_limit" {
		t.Errorf("label = %q, want hit_limit", label)
	}
	if confidence != "high" {
		t.Errorf("confidence = %q, want high — a resolvable time-only reset is a positive signal (#805)", confidence)
	}
	want := time.Date(2026, time.July, 2, 12, 30, 0, 0, time.UTC)
	if !resetAt.Equal(want) {
		t.Errorf("resetAt = %v, want %v", resetAt, want)
	}
}

// #805 review regression: a long-running worker can echo the live quota text
// early in its log — a prompt quoting the provider message, work output
// touching this very classifier — and later die for an unrelated reason.
// Post-mortem detection must scan only the log tail, so an echo buried above
// it neither classifies the death as rate-limited nor lends it a parseable
// reset window.
func TestIsRateLimited_QuotaEchoBeyondTail_NotDetected(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	var b strings.Builder
	b.WriteString("## Issue body\n")
	b.WriteString(codexTimeOnlySignature + "\n")
	for i := 0; i < authFailureTailLines+20; i++ {
		b.WriteString("normal work output\n")
	}
	b.WriteString("worker finished normally\n")
	if err := os.WriteFile(logFile, []byte(b.String()), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if IsRateLimited(logFile) {
		t.Error("IsRateLimited must not match a quota echo above the log tail")
	}
	now := time.Date(2026, time.July, 2, 11, 0, 0, 0, time.UTC)
	if reset, ok := ParseRateLimitResetFromLog(logFile, now); ok {
		t.Errorf("ParseRateLimitResetFromLog = %v, want no parse — the hint is above the tail", reset)
	}
}

// The terminal quota error that actually killed the CLI is always within the
// tail, however long the log: tail-bounding must not lose the true positive.
func TestIsRateLimited_TerminalQuotaErrorOnLongLog_StillDetected(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	var b strings.Builder
	for i := 0; i < authFailureTailLines+200; i++ {
		b.WriteString("normal work output\n")
	}
	b.WriteString(codexTimeOnlySignature + "\n")
	if err := os.WriteFile(logFile, []byte(b.String()), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if !IsRateLimited(logFile) {
		t.Error("IsRateLimited must still match the terminal quota error on a long log")
	}
	now := time.Date(2026, time.July, 2, 11, 0, 0, 0, time.UTC)
	reset, ok := ParseRateLimitResetFromLog(logFile, now)
	if !ok {
		t.Fatal("ParseRateLimitResetFromLog should parse the terminal time-only hint")
	}
	want := time.Date(2026, time.July, 2, 12, 30, 0, 0, time.UTC)
	if !reset.Equal(want) {
		t.Errorf("reset = %v, want %v", reset, want)
	}
}

// #805 review: when several "try again at" hints survive into the scanned
// output, the LAST parseable one wins — the terminal error beats an earlier
// echo — and an unparseable trailing capture falls back to the previous
// parseable hint instead of failing the whole parse.
func TestParseRateLimitResetAt_LastParseableHintWins(t *testing.T) {
	now := time.Date(2026, time.July, 2, 8, 0, 0, 0, time.UTC)

	echoThenTerminal := "the issue body quotes the provider: try again at 9:00 AM.\n" +
		codexTimeOnlySignature + "\n"
	got, ok := ParseRateLimitResetAt(echoThenTerminal, now)
	if !ok {
		t.Fatal("expected the terminal hint to parse")
	}
	want := time.Date(2026, time.July, 2, 12, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("reset = %v, want %v — the terminal hint must beat the earlier echo", got, want)
	}

	terminalThenGarbage := codexTimeOnlySignature + "\n" +
		"and the docs say to try again at some point soon.\n"
	got, ok = ParseRateLimitResetAt(terminalThenGarbage, now)
	if !ok {
		t.Fatal("an unparseable trailing capture must fall back to the earlier parseable hint")
	}
	if !got.Equal(want) {
		t.Errorf("reset = %v, want %v", got, want)
	}
}
