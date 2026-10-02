package admissioncontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// LocalTerminationAttestation records an operator's verified OS observation.
// The authority validates its identity and digest, not the remote OS itself.
type LocalTerminationAttestation struct {
	SchemaVersion   int    `json:"schema_version"`
	NativeSessionID string `json:"native_session_id"`
	Unit            string `json:"unit"`
	Cgroup          string `json:"cgroup"`
	BootID          string `json:"boot_id"`
	InvocationID    string `json:"invocation_id"`
	ProofDigest     string `json:"proof_digest"`
}

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func (a LocalTerminationAttestation) Valid(nativeID string) bool {
	invocation, err := hex.DecodeString(a.InvocationID)
	return a.SchemaVersion == 1 && a.NativeSessionID == nativeID && canonicalUUID(nativeID) &&
		canonicalUUID(a.BootID) && Identifier(a.Unit) && strings.HasPrefix(a.Unit, "maestro-") &&
		strings.HasSuffix(a.Unit, ".service") && !strings.Contains(a.Unit, "/") &&
		filepath.IsAbs(a.Cgroup) && filepath.Clean(a.Cgroup) == a.Cgroup && filepath.Base(a.Cgroup) == a.Unit &&
		err == nil && len(invocation) == 16 && hex.EncodeToString(invocation) == a.InvocationID && digestHex(a.ProofDigest)
}

type AttemptObservation struct {
	PhysicalAttemptID string `json:"physical_attempt_id"`
	ObservationDigest string `json:"observation_digest"`
}

func validAttemptObservations(observations []AttemptObservation) bool {
	if len(observations) == 0 || len(observations) > 64 {
		return false
	}
	previous := ""
	for _, observation := range observations {
		if !canonicalUUID(observation.PhysicalAttemptID) || observation.PhysicalAttemptID <= previous || !digestHex(observation.ObservationDigest) {
			return false
		}
		previous = observation.PhysicalAttemptID
	}
	return true
}

// RetireRequest is an explicit, separately authorized operator operation. It
// never refunds requests, resolves unknown usage, or changes run/daily caps.
type RetireRequest struct {
	SealRequest
	ExpectedEvidenceID          string                      `json:"expected_evidence_id"`
	ExpectedSnapshotDigest      string                      `json:"expected_snapshot_digest"`
	ExpectedAttemptsDigest      string                      `json:"expected_attempts_digest"`
	Reason                      string                      `json:"reason"`
	AttemptObservations         []AttemptObservation        `json:"attempt_observations"`
	LocalTerminationAttestation LocalTerminationAttestation `json:"local_termination_attestation"`
}

func (r RetireRequest) Valid() bool {
	return r.SealRequest.Valid() && r.AdmissionBasis == "requests" && digestHex(r.ExpectedSnapshotDigest) &&
		r.ExpectedEvidenceID == "native-outcome-v2:"+r.ExpectedSnapshotDigest && digestHex(r.ExpectedAttemptsDigest) &&
		Identifier(r.Reason) && validAttemptObservations(r.AttemptObservations) && r.LocalTerminationAttestation.Valid(r.NativeSessionID)
}

type OperatorRetirement struct {
	SchemaVersion               int                         `json:"schema_version"`
	OperatorUID                 uint32                      `json:"operator_uid"`
	Reason                      string                      `json:"reason"`
	PriorOutcome                NativeOutcome               `json:"prior_outcome"`
	AttemptObservations         []AttemptObservation        `json:"attempt_observations"`
	LocalTerminationAttestation LocalTerminationAttestation `json:"local_termination_attestation"`
	RequestDigest               string                      `json:"request_digest"`
	RetirementDigest            string                      `json:"retirement_digest"`
}

func (p OperatorRetirement) Request() RetireRequest {
	old := p.PriorOutcome
	return RetireRequest{SealRequest: SealRequest{Binding: old.Binding, RegistrationVersion: old.RegistrationVersion},
		ExpectedEvidenceID: old.EvidenceID, ExpectedSnapshotDigest: old.SnapshotDigest, ExpectedAttemptsDigest: old.AttemptsDigest,
		Reason: p.Reason, AttemptObservations: p.AttemptObservations, LocalTerminationAttestation: p.LocalTerminationAttestation}
}

func exactNonNullFields(data json.RawMessage, names ...string) bool {
	if !fields(data, names...) {
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	for _, value := range object {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

func retirementFields(data json.RawMessage) bool {
	if !exactNonNullFields(data, "schema_version", "operator_uid", "reason", "prior_outcome", "attempt_observations", "local_termination_attestation", "request_digest", "retirement_digest") {
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	var prior map[string]json.RawMessage
	if json.Unmarshal(object["prior_outcome"], &prior) != nil || prior == nil {
		return false
	}
	if _, recursive := prior["operator_retirement"]; recursive {
		return false
	}
	if !exactNonNullFields(object["local_termination_attestation"], "schema_version", "native_session_id", "unit", "cgroup", "boot_id", "invocation_id", "proof_digest") {
		return false
	}
	var observations []json.RawMessage
	if json.Unmarshal(object["attempt_observations"], &observations) != nil || len(observations) == 0 || len(observations) > 64 {
		return false
	}
	for _, observation := range observations {
		if !exactNonNullFields(observation, "physical_attempt_id", "observation_digest") {
			return false
		}
	}
	return true
}

func canonicalObjectDigest(value any, omit ...string) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	if err = decoder.Decode(&object); err != nil {
		return "", err
	}
	for _, key := range omit {
		delete(object, key)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(object); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:]), nil
}

func validateOperatorRetirement(outcome NativeOutcome, request SealRequest) error {
	invalid := &Hold{Code: "invalid_response"}
	p := outcome.OperatorRetirement
	if p == nil || request.AdmissionBasis != "requests" || p.SchemaVersion != 1 || p.PriorOutcome.OperatorRetirement != nil {
		return invalid
	}
	old := p.PriorOutcome
	if ValidateNativeOutcome(old, request) != nil || old.Outcome != "held" || old.HoldCode == nil || *old.HoldCode != "outcome_unknown" ||
		old.UnresolvedAttempts <= 0 || old.PhysicalAttempts > 64 || old.CapViolations != 0 ||
		!p.Request().Valid() || int64(len(p.AttemptObservations)) != old.PhysicalAttempts {
		return invalid
	}
	digest, err := canonicalObjectDigest(p.Request())
	if err != nil || digest != p.RequestDigest {
		return invalid
	}
	digest, err = canonicalObjectDigest(p, "retirement_digest")
	if err != nil || digest != p.RetirementDigest {
		return invalid
	}
	expected := old
	expected.Outcome, expected.NextGenerationAllowed, expected.HoldCode, expected.OperatorRetirement = "operator_retired_unknown", true, nil, p
	expected.SnapshotDigest, err = outcomeDigest(expected)
	expected.EvidenceID = "native-outcome-v2:" + expected.SnapshotDigest
	if err != nil || expected != outcome {
		return invalid
	}
	return nil
}

// RetireNative performs exactly one control RPC. Ordinary reconciliation never
// calls it, and an uncertain reply must be recovered with the exact same request.
func (c Client) RetireNative(request RetireRequest) (NativeOutcome, error) {
	if !request.Valid() || !filepath.IsAbs(c.SocketPath) || c.Timeout <= 0 || c.Timeout > 30*time.Second {
		return NativeOutcome{}, &Hold{Code: "retirement_invalid"}
	}
	body, err := c.call(request.NativeSessionID, "retire_native", request)
	if err != nil {
		return NativeOutcome{}, err
	}
	outcome, err := decodeNativeOutcome(body, request.SealRequest)
	if err != nil {
		return NativeOutcome{}, err
	}
	digest, err := canonicalObjectDigest(request)
	if err != nil || outcome.OperatorRetirement == nil || outcome.OperatorRetirement.RequestDigest != digest || outcome.OperatorRetirement.OperatorUID != uint32(os.Geteuid()) {
		return NativeOutcome{}, &Hold{Code: "invalid_response"}
	}
	return outcome, nil
}
