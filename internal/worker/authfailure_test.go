package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #693: the live failure signature from the claude CLI on workshop
// ("Failed to authenticate. API Error: 401 Invalid authentication
// credentials") and provider-equivalent phrasings must be detected so the
// orchestrator can classify the death as a backend failure instead of
// burning the per-issue retry budget.
func TestDetectAuthFailure_KnownSignatures(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		wantLabel string
	}{
		{
			name:      "claude CLI live signature (#693)",
			output:    "Failed to authenticate. API Error: 401 Invalid authentication credentials",
			wantLabel: "failed_to_authenticate",
		},
		{
			name:      "anthropic API error JSON",
			output:    `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`,
			wantLabel: "authentication_error",
		},
		{
			name:      "http 401 with context word",
			output:    "request failed with status: 401",
			wantLabel: "http_401",
		},
		{
			name:      "401 unauthorized reason phrase",
			output:    "stream error: 401 Unauthorized",
			wantLabel: "http_401",
		},
		{
			name:      "oauth token expired",
			output:    "OAuth token has expired. Please obtain a new token or refresh your existing token.",
			wantLabel: "oauth_token_expired",
		},
		{
			name:      "claude CLI logged out",
			output:    "Invalid API key · Please run /login",
			wantLabel: "login_required",
		},
		{
			name:      "openai incorrect api key",
			output:    "Incorrect API key provided: sk-xxxx. You can find your API key at platform.openai.com.",
			wantLabel: "invalid_api_key",
		},
		{
			name:      "gemini api key not valid",
			output:    "API key not valid. Please pass a valid API key.",
			wantLabel: "invalid_api_key",
		},
		{
			name:      "codex cliproxy missing env var",
			output:    "ERROR: Missing environment variable: `CLIPROXY_API_KEY`.",
			wantLabel: "missing_api_key_env_var",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, label := DetectAuthFailure(tc.output)
			if !hit {
				t.Fatalf("DetectAuthFailure(%q) = false, want true", tc.output)
			}
			if label != tc.wantLabel {
				t.Fatalf("DetectAuthFailure(%q) label = %q, want %q", tc.output, label, tc.wantLabel)
			}
		})
	}
}

// Bare digits, issue references, and rate-limit phrasings must not be
// classified as auth failures — the patterns are precision-first, mirroring
// the #663 rule for rate-limit detection.
func TestDetectAuthFailure_NoFalsePositives(t *testing.T) {
	cases := []string{
		"",
		"processed 4012 records in 401ms",
		"see issue #401 for details",
		"You've hit your usage limit. Try again at May 30th, 2026 8:13 PM.",
		"HTTP 429 Too Many Requests",
		"the authentication flow redirects to the login page",
		"unauthorized users are shown a banner",
		"all tests passed: 401 assertions",
	}
	for _, output := range cases {
		if hit, label := DetectAuthFailure(output); hit {
			t.Errorf("DetectAuthFailure(%q) = true (label=%q), want false", output, label)
		}
	}
}

func TestIsAuthFailure_ReadsLogTail(t *testing.T) {
	dir := t.TempDir()

	logFile := filepath.Join(dir, "worker.log")
	content := "starting worker\nsome output\nFailed to authenticate. API Error: 401 Invalid authentication credentials\n"
	if err := os.WriteFile(logFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hit, label := IsAuthFailure(logFile)
	if !hit || label != "failed_to_authenticate" {
		t.Fatalf("IsAuthFailure = (%v, %q), want (true, failed_to_authenticate)", hit, label)
	}

	if hit, _ := IsAuthFailure(filepath.Join(dir, "missing.log")); hit {
		t.Fatal("IsAuthFailure on missing file = true, want false")
	}
	if hit, _ := IsAuthFailure(""); hit {
		t.Fatal("IsAuthFailure on empty path = true, want false")
	}
}

// A prompt echo at the top of a long log (e.g. a worker assigned an
// auth-related issue whose body quotes the 401 error) must not classify the
// session as a backend auth failure: only the log tail — where the CLI's
// terminal error actually lands — is scanned.
func TestIsAuthFailure_PromptEchoOutsideTailIgnored(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")

	var b strings.Builder
	b.WriteString("Issue #693: worker spawn: classify backend auth failures (401)\n")
	b.WriteString("Failed to authenticate. API Error: 401 Invalid authentication credentials\n")
	for i := 0; i < authFailureTailLines+10; i++ {
		b.WriteString("normal worker output line\n")
	}
	if err := os.WriteFile(logFile, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if hit, label := IsAuthFailure(logFile); hit {
		t.Fatalf("IsAuthFailure = (true, %q), want false — auth signature outside the log tail is prompt echo, not a backend outage", label)
	}
}
