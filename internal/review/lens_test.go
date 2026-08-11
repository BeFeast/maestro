package review

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatLens_Run(t *testing.T) {
	var seenAuth, seenPath string
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"content":"NO_FINDINGS"}}]}`))
	}))
	t.Cleanup(srv.Close)
	l := &ChatLens{Stream: "llm-review-opus", BaseURL: srv.URL, APIKey: "k", Model: "claude-opus-5"}
	out, err := l.Run(context.Background(), "review this")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "NO_FINDINGS" {
		t.Fatalf("out = %q", out)
	}
	if seenPath != "/v1/chat/completions" {
		t.Fatalf("path = %s", seenPath)
	}
	if seenAuth != "Bearer k" {
		t.Fatalf("auth = %q", seenAuth)
	}
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(seenBody, &payload); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if payload.Model != "claude-opus-5" || len(payload.Messages) != 1 ||
		payload.Messages[0].Role != "user" || payload.Messages[0].Content != "review this" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestChatLens_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(`{"error":"unknown provider"}`))
	}))
	t.Cleanup(srv.Close)
	l := &ChatLens{Stream: "s", BaseURL: srv.URL, APIKey: "k", Model: "m"}
	_, err := l.Run(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v — the proxy's error must surface (a disabled credential answers 502)", err)
	}
}

func TestChatLens_NoChoicesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	l := &ChatLens{Stream: "s", BaseURL: srv.URL, APIKey: "k", Model: "m"}
	if _, err := l.Run(context.Background(), "p"); err == nil {
		t.Fatal("an empty choices list must not read as an empty review")
	}
}

func TestChatLens_Available(t *testing.T) {
	if err := (&ChatLens{Stream: "s", BaseURL: "http://x", APIKey: "k", Model: "m"}).Available(); err != nil {
		t.Fatalf("fully configured lens: %v", err)
	}
	for name, l := range map[string]*ChatLens{
		"no base url": {Stream: "s", APIKey: "k", Model: "m"},
		"no key":      {Stream: "s", BaseURL: "http://x", Model: "m"},
		"no model":    {Stream: "s", BaseURL: "http://x", APIKey: "k"},
	} {
		if l.Available() == nil {
			t.Fatalf("%s: must be unavailable", name)
		}
	}
}

// stubCursorAgent writes a fake cursor-agent that asserts its sandbox (empty
// cwd, key in env, prompt on stdin), emits noise on stderr, and prints the
// review on stdout.
func stubCursorAgent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-agent")
	script := `#!/bin/sh
[ -n "$CURSOR_API_KEY" ] || { echo "no key" >&2; exit 3; }
[ -z "$(ls -A .)" ] || { echo "cwd not empty" >&2; exit 4; }
input=$(cat)
[ -n "$input" ] || { echo "no stdin" >&2; exit 5; }
echo "telemetry noise" >&2
echo "NO_FINDINGS"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCursorLens_Run(t *testing.T) {
	l := &CursorLens{Stream: "llm-review-cursor", Model: "composer-2.5", APIKey: "k", Binary: stubCursorAgent(t)}
	out, err := l.Run(context.Background(), "review this")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "NO_FINDINGS" {
		t.Fatalf("out = %q — stderr noise must never reach the parser", out)
	}
}

func TestCursorLens_FailureCarriesStderr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"quota exhausted\" >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &CursorLens{Stream: "s", Model: "m", APIKey: "k", Binary: path}
	_, err := l.Run(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("err = %v — stderr is the diagnosis and must surface", err)
	}
}

func TestCursorLens_TimeoutKillsProcessTree(t *testing.T) {
	// A child that backgrounds an fd-inheriting helper and then hangs: without
	// the process-group kill + WaitDelay, Run stays blocked long past the
	// deadline because the orphan holds the output pipe open.
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor-agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 60 &\nexec sleep 120\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &CursorLens{Stream: "s", Model: "m", APIKey: "k", Binary: path, Timeout: time.Second}
	start := time.Now()
	_, err := l.Run(context.Background(), "p")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a run past the deadline must fail")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Run blocked %s past a 1s deadline — the orphan held the pipe", elapsed)
	}
}

func TestCursorLens_Available(t *testing.T) {
	if err := (&CursorLens{Stream: "s", Model: "m", APIKey: "k"}).Available(); err != nil {
		t.Fatalf("configured lens: %v", err)
	}
	if (&CursorLens{Stream: "s", Model: "m"}).Available() == nil {
		t.Fatal("a missing key must gate the lens (error status, not silence)")
	}
}
