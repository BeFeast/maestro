package aiexecution

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NativeProcessTermination proves the exact launched incarnation's OS outcome.
// LocalStatus=local_output_unknown is a terminal OS observation, never proof of
// complete stdout or a financially settled provider invocation.
type NativeProcessTermination struct {
	Version         int       `json:"version"`
	Profile         FileProof `json:"profile"`
	NativeSessionID string    `json:"native_session_id"`
	Unit            string    `json:"unit"`
	Cgroup          string    `json:"cgroup"`
	BootID          string    `json:"boot_id"`
	InvocationID    string    `json:"invocation_id"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	LocalStatus     string    `json:"local_status"`
	ExitCode        int       `json:"exit_code"`
	Digest          string    `json:"digest"`
}

// ValidateNativeProcessLaunch validates the durable pre-exec claim shape;
// ObserveNativeProcessLaunch additionally binds it to the live kernel/service.
func ValidateNativeProcessLaunch(proof NativeProcessTermination, profile FileProof, nativeID, unit string) error {
	if proof.Version != 1 || proof.Profile != profile || !filepath.IsAbs(profile.Path) || !validDigest(profile.SHA256) || proof.NativeSessionID != nativeID || uuid.Validate(nativeID) != nil || proof.Unit != unit || filepath.Base(unit) != unit || !strings.HasSuffix(unit, ".service") || !strings.HasPrefix(unit, "maestro-") || proof.Cgroup == "/" || !filepath.IsAbs(proof.Cgroup) || filepath.Clean(proof.Cgroup) != proof.Cgroup || filepath.Base(proof.Cgroup) != unit || uuid.Validate(proof.BootID) != nil || proof.StartedAt.IsZero() || !proof.EndedAt.IsZero() || proof.LocalStatus != "launch_intent" || proof.ExitCode != -1 || proof.Digest != nativeTerminationDigest(proof) {
		return Held("containment_launch_proof_invalid")
	}
	inv, err := hex.DecodeString(proof.InvocationID)
	if err != nil || len(inv) != 16 || strings.ToLower(proof.InvocationID) != proof.InvocationID {
		return Held("containment_launch_proof_invalid")
	}
	return nil
}

// ValidateNativeProcessTermination validates a previously trusted persisted
// terminal observation without depending on rotated live configuration or a
// collected systemd unit. It does not turn an arbitrary local claim into a
// trusted observation; only VerifyNativeProcessTermination creates that proof.
func ValidateNativeProcessTermination(proof NativeProcessTermination, profile FileProof, nativeID, unit string) error {
	if proof.Version != 1 || proof.Profile != profile || !filepath.IsAbs(profile.Path) || !validDigest(profile.SHA256) || proof.NativeSessionID != nativeID || uuid.Validate(nativeID) != nil || proof.Unit != unit || filepath.Base(unit) != unit || !strings.HasSuffix(unit, ".service") || !strings.HasPrefix(unit, "maestro-") || proof.Cgroup == "/" || !filepath.IsAbs(proof.Cgroup) || filepath.Clean(proof.Cgroup) != proof.Cgroup || filepath.Base(proof.Cgroup) != unit || uuid.Validate(proof.BootID) != nil || proof.StartedAt.IsZero() || proof.EndedAt.IsZero() || proof.EndedAt.Before(proof.StartedAt) || proof.Digest != nativeTerminationDigest(proof) {
		return Held("containment_termination_proof_invalid")
	}
	inv, err := hex.DecodeString(proof.InvocationID)
	if err != nil || len(inv) != 16 || strings.ToLower(proof.InvocationID) != proof.InvocationID {
		return Held("containment_termination_proof_invalid")
	}
	switch proof.LocalStatus {
	case "succeeded":
		if proof.ExitCode != 0 {
			return Held("containment_termination_proof_invalid")
		}
	case "failed":
		if proof.ExitCode == 0 || proof.ExitCode < -1 {
			return Held("containment_termination_proof_invalid")
		}
	case "local_output_unknown":
		if proof.ExitCode != -1 {
			return Held("containment_termination_proof_invalid")
		}
	default:
		return Held("containment_termination_proof_invalid")
	}
	return nil
}

func nativeTerminationDigest(proof NativeProcessTermination) string {
	proof.Digest = ""
	b, _ := json.Marshal(proof)
	return digest(b)
}
