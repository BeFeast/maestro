// Package admissioncontrol registers native sessions with the existing shared
// authority. Registration is attribution, never permission to generate tokens.
package admissioncontrol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const MaxFrame = 65536

type Binding struct {
	GatewayScope    string `json:"gateway_scope"`
	NativeSessionID string `json:"native_session_id"`
	FleetID         string `json:"fleet_id"`
	ProjectID       string `json:"project_id"`
	RunID           string `json:"run_id"`
	Role            string `json:"role"`
	ExpiresAt       int64  `json:"expires_at"`
}

type RegistrationRequest struct {
	Binding
	ExpectedVersion int64 `json:"expected_version"`
}

type Acknowledgement struct {
	Binding             Binding `json:"binding"`
	Revoked             bool    `json:"revoked"`
	RegistrationVersion int64   `json:"registration_version"`
}

// Hold carries only an allowlisted protocol code, never socket paths or payloads.
type Hold struct{ Code string }

func (h *Hold) Error() string { return "native registration held: " + h.Code }

type Client struct {
	SocketPath   string
	AuthorityUID uint32
	Timeout      time.Duration
}

func Identifier(value string) bool {
	if value == "" || len(value) > 200 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 33 || r == 127 {
			return false
		}
	}
	return true
}

func (r RegistrationRequest) Valid() bool {
	id, err := uuid.Parse(r.NativeSessionID)
	return err == nil && id.String() == r.NativeSessionID && r.ExpectedVersion > 0 && r.ExpiresAt > 0 &&
		Identifier(r.GatewayScope) && Identifier(r.FleetID) && Identifier(r.ProjectID) && Identifier(r.RunID) && Identifier(r.Role)
}

// Register performs exactly one bounded request; no transport error is retried.
// Repeating this operation is safe only with the same persisted request.
func (c Client) Register(request RegistrationRequest) (Acknowledgement, error) {
	var ack Acknowledgement
	if !filepath.IsAbs(c.SocketPath) || c.Timeout <= 0 || c.Timeout > 30*time.Second || !request.Valid() {
		return ack, &Hold{Code: "registration_invalid"}
	}
	response, err := c.call(request.NativeSessionID, "register", request)
	if err != nil {
		return ack, err
	}
	return decodeResponse(response, request)
}

// call sends one bounded framed request to the pinned Unix peer; no retries.
func (c Client) call(id, op string, args any) ([]byte, error) {
	deadline := time.Now().Add(c.Timeout)
	conn, err := net.DialTimeout("unix", c.SocketPath, c.Timeout)
	if err != nil {
		return nil, &Hold{Code: "authority_unavailable"}
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, &Hold{Code: "authority_unavailable"}
	}
	if err := verifyPeer(conn, c.AuthorityUID); err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Version int    `json:"version"`
		ID      string `json:"id"`
		Op      string `json:"op"`
		Args    any    `json:"args"`
	}{1, id, op, args})
	if err != nil || len(body) == 0 || len(body) > MaxFrame {
		return nil, &Hold{Code: "registration_invalid"}
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if _, err := io.Copy(conn, bytes.NewReader(frame)); err != nil {
		return nil, &Hold{Code: "authority_unavailable"}
	}
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, &Hold{Code: "authority_unavailable"}
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxFrame {
		return nil, &Hold{Code: "invalid_response"}
	}
	response := make([]byte, int(n))
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, &Hold{Code: "authority_unavailable"}
	}
	return response, nil
}

// Strict response decoding rejects duplicate keys, extra/missing fields,
// coerced numbers/booleans, conflicting binding echoes and revoked identities.
func decodeResponse(data []byte, request RegistrationRequest) (Acknowledgement, error) {
	var ack Acknowledgement
	invalid := func() (Acknowledgement, error) { return ack, &Hold{Code: "invalid_response"} }
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !uniqueJSON(decoder, 0) {
		return invalid()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid()
	}
	var envelope struct {
		Version int             `json:"version"`
		ID      *string         `json:"id"`
		OK      bool            `json:"ok"`
		Result  json.RawMessage `json:"result"`
		Hold    json.RawMessage `json:"hold"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 {
		return invalid()
	}
	if !envelope.OK {
		if !fields(data, "version", "id", "ok", "hold") || (envelope.ID != nil && *envelope.ID != request.NativeSessionID) || !fields(envelope.Hold, "code") {
			return invalid()
		}
		var hold Hold
		if json.Unmarshal(envelope.Hold, &hold) != nil {
			return invalid()
		}
		switch hold.Code {
		case "policy_conflict", "scope_missing", "identity_conflict", "registration_invalid", "registration_conflict", "registration_expired", "registration_revoked", "caller_forbidden", "authority_unavailable", "invalid_frame", "invalid_request", "unsupported_version", "operation_forbidden", "clock_unknown":
			return ack, &hold
		default:
			return invalid()
		}
	}
	if !fields(data, "version", "id", "ok", "result") || envelope.ID == nil || *envelope.ID != request.NativeSessionID || !fields(envelope.Result, "binding", "revoked", "registration_version") {
		return invalid()
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(envelope.Result, &result) != nil || !fields(result["binding"], "gateway_scope", "native_session_id", "fleet_id", "project_id", "run_id", "role", "expires_at") {
		return invalid()
	}
	// JSON null must not silently become false or zero in a successful response.
	if bytes.Equal(bytes.TrimSpace(result["revoked"]), []byte("null")) || json.Unmarshal(envelope.Result, &ack) != nil || ack.RegistrationVersion <= 0 || ack.RegistrationVersion > request.ExpectedVersion || ack.Binding != request.Binding {
		return invalid()
	}
	if ack.Revoked {
		return Acknowledgement{}, &Hold{Code: "registration_revoked"}
	}
	if ack.Binding.ExpiresAt <= time.Now().Unix() {
		return Acknowledgement{}, &Hold{Code: "registration_expired"}
	}
	return ack, nil
}

func fields(data []byte, names ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || len(object) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := object[name]; !ok {
			return false
		}
	}
	return true
}

func uniqueJSON(d *json.Decoder, depth int) bool {
	if depth > 16 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return false
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return false
			}
			seen[key] = true
			if !uniqueJSON(d, depth+1) {
				return false
			}
		}
	case '[':
		for d.More() {
			if !uniqueJSON(d, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	end, err := d.Token()
	return err == nil && ((delimiter == '{' && end == json.Delim('}')) || (delimiter == '[' && end == json.Delim(']')))
}
