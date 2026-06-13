package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #713: the live failure signature from the claude CLI when Fable was pulled
// early ("It may not exist or you may not have access to it") and
// provider-equivalent phrasings must be detected so the orchestrator can
// classify the death as a backend failure (model unavailable) instead of
// burning the per-issue retry budget on a doomed re-spawn.
func TestDetectModelUnavailable_KnownSignatures(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		wantLabel string
	}{
		{
			name:      "claude CLI live signature (#713)",
			output:    "There's an issue with the selected model (claude-fable-5). It may not exist or you may not have access to it. Run --model to pick a different model.",
			wantLabel: "model_no_access",
		},
		{
			name:      "openai model does not exist + no access",
			output:    "The model `gpt-5-mini` does not exist or you do not have access to it.",
			wantLabel: "model_no_access",
		},
		{
			name:      "gemini model does not exist",
			output:    "The model gemini-3-ultra does not exist.",
			wantLabel: "model_does_not_exist",
		},
		{
			name:      "anthropic not_found_error JSON",
			output:    `API Error: 404 {"type":"error","error":{"type":"not_found_error","message":"model: claude-fable-5"}}`,
			wantLabel: "not_found_error",
		},
		{
			name:      "model not found phrasing",
			output:    "model: claude-fable-5 not found",
			wantLabel: "model_not_found",
		},
		{
			name:      "openai model_not_found code",
			output:    `{"error":{"code":"model_not_found","message":"unknown model"}}`,
			wantLabel: "model_not_found",
		},
		{
			name:      "http 404 with context word",
			output:    "request failed with status: 404",
			wantLabel: "http_404",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, label := DetectModelUnavailable(tc.output)
			if !hit {
				t.Fatalf("DetectModelUnavailable(%q) = false, want true", tc.output)
			}
			if label != tc.wantLabel {
				t.Fatalf("DetectModelUnavailable(%q) label = %q, want %q", tc.output, label, tc.wantLabel)
			}
		})
	}
}

// Bare digits, issue references, generic model talk, and auth/rate-limit
// phrasings must not be classified as model-unavailable — the patterns are
// precision-first, mirroring the #663 rule (acceptance criterion #4).
func TestDetectModelUnavailable_NoFalsePositives(t *testing.T) {
	cases := []string{
		"",
		"see issue #404 for details",
		"processed 404 records in 12ms",
		"the model trained on our data is the best fit",
		"the model file does not compile",
		"renamed the model_config struct in this PR",
		"HTTP 429 Too Many Requests",
		"Failed to authenticate. API Error: 401 Invalid authentication credentials",
		"the checkpoint was not found in cache",
		"all 404 assertions passed",
	}
	for _, output := range cases {
		if hit, label := DetectModelUnavailable(output); hit {
			t.Errorf("DetectModelUnavailable(%q) = true (label=%q), want false", output, label)
		}
	}
}

// An auth-failure signature must NOT be reported as model-unavailable: the two
// classes are distinct so the operator gets the right remediation (swap the
// model id vs fix credentials).
func TestDetectModelUnavailable_AuthSignatureNotMisclassified(t *testing.T) {
	for _, output := range []string{
		"Failed to authenticate. API Error: 401 Invalid authentication credentials",
		"Invalid API key · Please run /login",
		"OAuth token has expired.",
	} {
		if hit, label := DetectModelUnavailable(output); hit {
			t.Errorf("DetectModelUnavailable(%q) = true (label=%q), want false — auth belongs to DetectAuthFailure", output, label)
		}
	}
}

func TestIsModelUnavailable_ReadsLogTail(t *testing.T) {
	dir := t.TempDir()

	logFile := filepath.Join(dir, "worker.log")
	content := "starting worker\nsome output\nThere's an issue with the selected model (claude-fable-5). It may not exist or you may not have access to it.\n"
	if err := os.WriteFile(logFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hit, label := IsModelUnavailable(logFile)
	if !hit || label != "model_no_access" {
		t.Fatalf("IsModelUnavailable = (%v, %q), want (true, model_no_access)", hit, label)
	}

	if hit, _ := IsModelUnavailable(filepath.Join(dir, "missing.log")); hit {
		t.Fatal("IsModelUnavailable on missing file = true, want false")
	}
	if hit, _ := IsModelUnavailable(""); hit {
		t.Fatal("IsModelUnavailable on empty path = true, want false")
	}
}

// A prompt echo at the top of a long log (e.g. a worker assigned #713 whose
// body quotes the model-unavailable error) must not classify the session as a
// backend failure: only the log tail — where the CLI's terminal error actually
// lands — is scanned.
func TestIsModelUnavailable_PromptEchoOutsideTailIgnored(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")

	var b strings.Builder
	b.WriteString("Issue #713: classify model unavailable / no access as backend failure\n")
	b.WriteString("It may not exist or you may not have access to it.\n")
	for i := 0; i < authFailureTailLines+10; i++ {
		b.WriteString("normal worker output line\n")
	}
	if err := os.WriteFile(logFile, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if hit, label := IsModelUnavailable(logFile); hit {
		t.Fatalf("IsModelUnavailable = (true, %q), want false — a model phrase outside the log tail is prompt echo, not a backend outage", label)
	}
}
