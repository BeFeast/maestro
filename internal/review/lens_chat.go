package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ChatLens runs a review through an OpenAI-compatible chat-completions
// endpoint. For the fleet's Anthropic/OpenAI lenses (opus, terra, ...) the
// endpoint MUST be CLIProxy — the accounting and credential-rotation layer —
// never a direct provider URL or host login; that invariant is the reason the
// bash producer's direct `claude -p` opus branch is being retired (#1162).
// The caller injects the base URL and key from config (*_env indirection).
type ChatLens struct {
	// Stream is the status context / stream name, e.g. "llm-review-opus".
	Stream string
	// BaseURL is the API root, e.g. "http://127.0.0.1:23020"; the lens posts
	// to {BaseURL}/v1/chat/completions.
	BaseURL string
	// APIKey is sent as a Bearer token.
	APIKey string
	// Model is the endpoint's model id, e.g. "claude-opus-5".
	Model string
	// Timeout bounds one model run. Zero means 10 minutes.
	Timeout time.Duration
	// HTTPClient overrides the transport (tests). Nil means a fresh client;
	// the run deadline comes from the context either way.
	HTTPClient *http.Client
}

const defaultLensTimeout = 10 * time.Minute

func (l *ChatLens) Name() string { return l.Stream }

// Available reports whether the lens is configured. Base URL, key, and model
// are all required — a missing one becomes the producer's explicit
// "skipped: credentials not configured" error status, never a silent stream.
func (l *ChatLens) Available() error {
	switch {
	case strings.TrimSpace(l.BaseURL) == "":
		return fmt.Errorf("chat lens %s: base URL not configured", l.Stream)
	case strings.TrimSpace(l.APIKey) == "":
		return fmt.Errorf("chat lens %s: API key not configured", l.Stream)
	case strings.TrimSpace(l.Model) == "":
		return fmt.Errorf("chat lens %s: model not configured", l.Stream)
	}
	return nil
}

func (l *ChatLens) timeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return defaultLensTimeout
}

// Run posts one single-shot chat completion and returns the model's text.
func (l *ChatLens) Run(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()

	payload, err := json.Marshal(map[string]any{
		"model": l.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	url := strings.TrimRight(l.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+l.APIKey)
	req.Header.Set("Content-Type", "application/json")

	httpc := l.HTTPClient
	if httpc == nil {
		httpc = &http.Client{}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat completion: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt := strings.TrimSpace(string(body))
		if len(excerpt) > 512 {
			excerpt = excerpt[:512]
		}
		return "", fmt.Errorf("chat completion: HTTP %d: %s", resp.StatusCode, excerpt)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("chat completion: response carries no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
