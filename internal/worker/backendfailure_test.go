package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #713: the live failure signature from the claude CLI when Fable was pulled
// from Pro/Max subscriptions ("There's an issue with the selected model
// (claude-fable-5). It may not exist or you may not have access to it.") and
// provider-equivalent phrasings must be detected so the orchestrator can
// classify the death as a backend failure instead of burning the per-issue
// retry budget.
func TestDetectModelUnavailable_KnownSignatures(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		wantLabel string
	}{
		{
			name:      "claude CLI live signature (#713)",
			output:    "There's an issue with the selected model (claude-fable-5). It may not exist or you may not have access to it. Run --model to pick a different model.",
			wantLabel: "model_access_denied",
		},
		{
			name:      "openai model does not exist",
			output:    "The model `gpt-5-turbo` does not exist or you do not have access to it.",
			wantLabel: "model_does_not_exist",
		},
		{
			name:      "openai model doesn't exist contraction",
			output:    "Error: The model gpt-5-turbo doesn't exist.",
			wantLabel: "model_does_not_exist",
		},
		{
			name:      "anthropic not_found_error with model context",
			output:    `API Error: 404 {"type":"error","error":{"type":"not_found_error","message":"model: claude-fable-5"}}`,
			wantLabel: "not_found_error",
		},
		{
			name:      "model not found phrasing",
			output:    "gemini: model not found for the configured project",
			wantLabel: "model_not_found",
		},
		{
			name:      "model_not_found token",
			output:    `{"error":{"code":"model_not_found"}}`,
			wantLabel: "model_not_found",
		},
		{
			name:      "model id colon not found",
			output:    "request rejected: model: claude-fable-5 not found",
			wantLabel: "model_not_found",
		},
		{
			name:      "CLIProxyAPI model overloaded",
			output:    `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantLabel: "model_overloaded",
		},
		{
			name:      "HTTP 529 overloaded",
			output:    "HTTP 529: Overloaded",
			wantLabel: "model_overloaded",
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

// #713 acceptance 4: a worker log echoing the words "model", "not exist", or
// "404" from a prompt/issue body must not be classified as model-unavailable.
// Precision-first, mirroring the #663 rule for rate-limit detection.
func TestDetectModelUnavailable_NoFalsePositives(t *testing.T) {
	cases := []string{
		"",
		"processed 404 records in 401ms",
		"see issue #404 for details",
		"the 404 page template rendered correctly",
		"the model card documents the architecture and limits",
		"model accuracy did not regress on the eval set",
		"not_found_error in the users table lookup",
		"the existing model works fine in production",
		"HTTP 200 OK — model loaded",
		"all tests passed: 404 assertions",
		"processed 529 overloaded fixtures successfully",
		"issue #529 tracks upstream overload handling",
		"HTTP 529 without an overload marker",
		"overloaded queue recovered without an HTTP status",
		"the issue body says the model may not exist, but that is the prompt",
		`assert_eq!(state.last_load_error.as_deref(), Some("libmpv error 404"));`,
		"HTTP 404/retry",
		"stream error: received 404 from upstream",
		"tool response status: 404 while loading album art",
		"API Error: 404 from the application's media endpoint",
	}
	for _, output := range cases {
		if hit, label := DetectModelUnavailable(output); hit {
			t.Errorf("DetectModelUnavailable(%q) = true (label=%q), want false", output, label)
		}
	}
}

// #918: domain-level 404 output in the terminal log tail is ordinary worker
// output, not evidence that the configured AI model is unavailable.
func TestIsModelUnavailable_DomainError404InLogTailIgnored(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")

	var b strings.Builder
	for i := 0; i < authFailureTailLines; i++ {
		b.WriteString("normal worker output line\n")
	}
	b.WriteString(`assert_eq!(state.last_load_error.as_deref(), Some("libmpv error 404"));`)
	b.WriteString("\nHTTP 404/retry\n")
	if err := os.WriteFile(logFile, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if hit, label := IsModelUnavailable(logFile); hit {
		t.Fatalf("IsModelUnavailable = (true, %q), want false for domain 404 output in the log tail", label)
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
	if !hit || label != "model_access_denied" {
		t.Fatalf("IsModelUnavailable = (%v, %q), want (true, model_access_denied)", hit, label)
	}

	if hit, _ := IsModelUnavailable(filepath.Join(dir, "missing.log")); hit {
		t.Fatal("IsModelUnavailable on missing file = true, want false")
	}
	if hit, _ := IsModelUnavailable(""); hit {
		t.Fatal("IsModelUnavailable on empty path = true, want false")
	}
}

// A prompt echo at the top of a long log (a worker assigned issue #713 whose
// body quotes the exact claude model-unavailable message) must not classify
// the session as a backend failure: only the log tail — where the CLI's
// terminal error actually lands — is scanned.
func TestIsModelUnavailable_PromptEchoOutsideTailIgnored(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")

	var b strings.Builder
	b.WriteString("Issue #713: classify model unavailable as backend failure\n")
	b.WriteString("There's an issue with the selected model (claude-fable-5). It may not exist or you may not have access to it.\n")
	for i := 0; i < authFailureTailLines+10; i++ {
		b.WriteString("normal worker output line\n")
	}
	if err := os.WriteFile(logFile, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if hit, label := IsModelUnavailable(logFile); hit {
		t.Fatalf("IsModelUnavailable = (true, %q), want false — model signature outside the log tail is prompt echo, not a backend outage", label)
	}
}
