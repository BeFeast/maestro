package aiexecution

import (
	"encoding/json"
	"time"
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

func nativeTerminationDigest(proof NativeProcessTermination) string {
	proof.Digest = ""
	b, _ := json.Marshal(proof)
	return digest(b)
}
