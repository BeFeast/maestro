package review

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const maxErrorBody = 64 << 10

// GatewayTerminal is allowlisted control metadata from the configured HTTP gateway.
// It is never parsed from model output, prompts, harness logs or queue observations.
type GatewayTerminal struct {
	CorrelationID   string     `json:"correlation_id,omitempty"`
	Reason          string     `json:"reason"`
	Attempted       int        `json:"attempted"`
	RetryAt         *time.Time `json:"retry_at,omitempty"`
	RetryScope      string     `json:"retry_scope"`
	StreamCommitted bool       `json:"stream_committed"`
}

type GatewayTerminalError struct {
	Code              string           `json:"code"`
	HTTPStatus        int              `json:"http_status"`
	Terminal          *GatewayTerminal `json:"terminal,omitempty"`
	ResponseSHA256    string           `json:"response_sha256,omitempty"`
	ResponseTruncated bool             `json:"response_truncated,omitempty"`
	// The non-streaming error response does not supply the committed output.
	CheckpointAvailable bool `json:"checkpoint_available"`
}

func (e *GatewayTerminalError) Error() string {
	return fmt.Sprintf("chat completion: HTTP %d: %s", e.HTTPStatus, e.Code)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// uniqueJSON rejects ambiguous duplicate keys (including escaped spellings) before
// typed decoding. Unknown members remain compatible, but cannot shadow controls.
func uniqueJSON(body []byte) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("excessive JSON nesting")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return fmt.Errorf("duplicate JSON key")
					}
					seen[name] = true
					if err := value(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := value(depth + 1); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("unexpected JSON delimiter")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func terminalError(status int, body []byte, truncated bool) *GatewayTerminalError {
	e := &GatewayTerminalError{Code: "terminal_unsupported", HTTPStatus: status, ResponseSHA256: digest(body), ResponseTruncated: truncated}
	if truncated {
		e.Code = "response_too_large"
		return e
	}
	if err := uniqueJSON(body); err != nil {
		e.Code = "terminal_malformed"
		return e
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		e.Code = "terminal_malformed"
		return e
	}
	raw := envelope["terminal"]
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(envelope["error"], &nested); err == nil && nested != nil {
		if len(raw) > 0 {
			e.Code = "terminal_malformed"
			return e
		}
		raw = nested["terminal"]
	}
	if len(raw) == 0 {
		return e
	}
	e.Code = "terminal_malformed"
	var controls map[string]json.RawMessage
	if json.Unmarshal(raw, &controls) != nil || controls == nil {
		return e
	}
	known := map[string]bool{"correlation_id": true, "reason": true, "attempted": true, "retry_at": true, "retry_scope": true, "stream_committed": true}
	for key := range controls {
		// Match encoding/json's Unicode fold (including long s), not just
		// lowercase ASCII. Only exact canonical spellings are protocol keys.
		for canonical := range known {
			if key != canonical && strings.EqualFold(key, canonical) {
				return e
			}
		}
	}
	var wire struct {
		CorrelationID   string          `json:"correlation_id"`
		Reason          string          `json:"reason"`
		Attempted       *int            `json:"attempted"`
		RetryAt         json.RawMessage `json:"retry_at"`
		RetryScope      string          `json:"retry_scope"`
		StreamCommitted *bool           `json:"stream_committed"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Attempted == nil || *wire.Attempted < 0 || wire.StreamCommitted == nil {
		return e
	}
	switch wire.Reason {
	case "quota_cooldown", "credentials_unavailable", "upstream_transient", "request_invalid", "cancelled", "unknown":
	default:
		return e
	}
	switch wire.RetryScope {
	case "same_request", "new_turn_only", "none":
	default:
		return e
	}
	if *wire.StreamCommitted && (wire.RetryScope == "same_request" || *wire.Attempted == 0) {
		return e
	}
	if len(wire.CorrelationID) > 128 || strings.IndexFunc(wire.CorrelationID, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r))
	}) >= 0 {
		return e
	}
	t := &GatewayTerminal{CorrelationID: wire.CorrelationID, Reason: wire.Reason, Attempted: *wire.Attempted, RetryScope: wire.RetryScope, StreamCommitted: *wire.StreamCommitted}
	if len(wire.RetryAt) > 0 {
		var stamp string
		if json.Unmarshal(wire.RetryAt, &stamp) != nil || stamp == "" {
			return e
		}
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			return e
		}
		_, offset := at.Zone()
		if offset != 0 {
			return e
		}
		t.RetryAt = &at
	}
	e.Terminal = t
	e.Code = t.Reason
	return e
}
