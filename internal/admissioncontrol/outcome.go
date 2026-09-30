package admissioncontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"path/filepath"
	"time"
	"unicode/utf8"
)

type SealRequest struct {
	Binding
	RegistrationVersion int64 `json:"registration_version"`
}

func (r SealRequest) Valid() bool {
	return (RegistrationRequest{Binding: r.Binding, ExpectedVersion: r.RegistrationVersion}).Valid()
}

// NativeOutcome is an immutable authority snapshot. Only Sealed plus a strictly
// validated allow result permits recovery; registration and local exit cannot.
type NativeOutcome struct {
	Binding               Binding `json:"binding"`
	RegistrationVersion   int64   `json:"registration_version"`
	Sealed                bool    `json:"sealed"`
	Outcome               string  `json:"outcome"`
	NextGenerationAllowed bool    `json:"next_generation_allowed"`
	PhysicalAttempts      int64   `json:"physical_attempts"`
	TerminalAttempts      int64   `json:"terminal_attempts"`
	UnresolvedAttempts    int64   `json:"unresolved_attempts"`
	BoundViolations       int64   `json:"bound_violations"`
	HoldCode              *string `json:"hold_code"`
	AttemptsDigest        string  `json:"attempts_digest"`
	SnapshotDigest        string  `json:"snapshot_digest"`
	EvidenceID            string  `json:"evidence_id"`
}

// SealNative irrevocably revokes this exact registration, then reconciles its
// physical attempts. Repeating the exact request is safe after a lost reply or
// late ledger evidence. A held result is still a committed seal, never a grant.
func (c Client) SealNative(request SealRequest) (NativeOutcome, error) {
	if !request.Valid() || !filepath.IsAbs(c.SocketPath) || c.Timeout <= 0 || c.Timeout > 30*time.Second {
		return NativeOutcome{}, &Hold{Code: "registration_invalid"}
	}
	body, err := c.call(request.NativeSessionID, "seal_native", request)
	if err != nil {
		return NativeOutcome{}, err
	}
	return decodeNativeOutcome(body, request)
}

func digestHex(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == value
}

func outcomeDigest(outcome NativeOutcome) (string, error) {
	body, err := json.Marshal(outcome)
	if err != nil {
		return "", err
	}
	// Encoding the map sorts keys; decode raw values so nested binding keys are
	// sorted too. UseNumber preserves every int64 exactly, without float64 loss.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var canonical map[string]any
	if err = decoder.Decode(&canonical); err != nil {
		return "", err
	}
	delete(canonical, "snapshot_digest")
	delete(canonical, "evidence_id")
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(canonical); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:]), nil
}

// ValidateNativeOutcome also validates saved receipts after a process restart.
// Expiry and current policy do not invalidate an already sealed old registration.
func ValidateNativeOutcome(outcome NativeOutcome, request SealRequest) error {
	invalid := &Hold{Code: "invalid_response"}
	if !request.Valid() || outcome.Binding != request.Binding || outcome.RegistrationVersion != request.RegistrationVersion ||
		!outcome.Sealed || outcome.PhysicalAttempts < 0 || outcome.TerminalAttempts < 0 || outcome.UnresolvedAttempts < 0 ||
		outcome.BoundViolations < 0 || outcome.TerminalAttempts > math.MaxInt64-outcome.UnresolvedAttempts ||
		outcome.TerminalAttempts+outcome.UnresolvedAttempts != outcome.PhysicalAttempts || outcome.BoundViolations > outcome.PhysicalAttempts ||
		!digestHex(outcome.AttemptsDigest) || !digestHex(outcome.SnapshotDigest) {
		return invalid
	}
	code := ""
	switch {
	case outcome.PhysicalAttempts > 4096:
		code = "attempt_limit_exceeded"
	case outcome.BoundViolations > 0:
		code = "bound_violation"
	case outcome.UnresolvedAttempts > 0:
		code = "outcome_unknown"
	}
	disposition := "no_dispatch"
	if code != "" {
		disposition = "held"
	} else if outcome.PhysicalAttempts > 0 {
		disposition = "settled"
	}
	if outcome.Outcome != disposition || outcome.NextGenerationAllowed != (code == "") ||
		(code == "" && outcome.HoldCode != nil) || (code != "" && (outcome.HoldCode == nil || *outcome.HoldCode != code)) {
		return invalid
	}
	computed, err := outcomeDigest(outcome)
	if err != nil || computed != outcome.SnapshotDigest || outcome.EvidenceID != "native-outcome-v1:"+computed {
		return invalid
	}
	return nil
}

func decodeNativeOutcome(data []byte, request SealRequest) (NativeOutcome, error) {
	invalid := func() (NativeOutcome, error) { return NativeOutcome{}, &Hold{Code: "invalid_response"} }
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !utf8.Valid(data) || !uniqueJSON(decoder, 0) {
		return invalid()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid()
	}
	var envelope struct {
		Version int             `json:"version"`
		ID      *string         `json:"id"`
		OK      *bool           `json:"ok"`
		Result  json.RawMessage `json:"result"`
		Hold    json.RawMessage `json:"hold"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 || envelope.OK == nil {
		return invalid()
	}
	if !*envelope.OK {
		if !fields(data, "version", "id", "ok", "hold") || (envelope.ID != nil && *envelope.ID != request.NativeSessionID) || !fields(envelope.Hold, "code") {
			return invalid()
		}
		var hold Hold
		if json.Unmarshal(envelope.Hold, &hold) != nil {
			return invalid()
		}
		switch hold.Code {
		case "registration_invalid", "registration_missing", "registration_conflict", "identity_conflict", "authority_unavailable", "invalid_frame", "invalid_request", "unsupported_version", "operation_forbidden", "caller_forbidden", "outcome_conflict":
			return NativeOutcome{}, &hold
		default:
			return invalid()
		}
	}
	if !fields(data, "version", "id", "ok", "result") || envelope.ID == nil || *envelope.ID != request.NativeSessionID ||
		!fields(envelope.Result, "binding", "registration_version", "sealed", "outcome", "next_generation_allowed", "physical_attempts", "terminal_attempts", "unresolved_attempts", "bound_violations", "hold_code", "attempts_digest", "snapshot_digest", "evidence_id") {
		return invalid()
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(envelope.Result, &object) != nil || !fields(object["binding"], "gateway_scope", "native_session_id", "fleet_id", "project_id", "run_id", "role", "expires_at") {
		return invalid()
	}
	for key, value := range object {
		if key != "hold_code" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid()
		}
	}
	var outcome NativeOutcome
	if json.Unmarshal(envelope.Result, &outcome) != nil || ValidateNativeOutcome(outcome, request) != nil {
		return invalid()
	}
	return outcome, nil
}
