package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// NativeOutputCheckpoint locates raw owner-only output without copying it into
// telemetry. Local success/completeness never implies financial settlement.
type NativeOutputCheckpoint struct {
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256"`
	Bytes     int    `json:"bytes"`
	Complete  bool   `json:"complete"`
	Truncated bool   `json:"truncated"`
}

type nativeOutputRecord struct {
	Version         int    `json:"version"`
	RoleRunID       string `json:"role_run_id"`
	InvocationID    string `json:"invocation_id"`
	NativeSessionID string `json:"native_session_id"`
	Status          string `json:"local_status"`
	SHA256          string `json:"sha256"`
	Complete        bool   `json:"complete"`
	Truncated       bool   `json:"truncated"`
	Data            []byte `json:"data"`
}

func (s *consultationStore) saveNativeOutput(identity ConsultationIdentity, inv InvocationReceipt, output []byte) (*NativeOutputCheckpoint, error) {
	if uuid.Validate(identity.ID) != nil || uuid.Validate(inv.ID) != nil || inv.NativeSession == nil || inv.NativeSession.Request.NativeSessionID != inv.ID || len(output) > 4<<20 {
		return nil, fmt.Errorf("native output binding invalid")
	}
	sum := sha256.Sum256(output)
	sha := hex.EncodeToString(sum[:])
	complete := inv.Status == "succeeded"
	record := nativeOutputRecord{Version: 1, RoleRunID: identity.ID, InvocationID: inv.ID, NativeSessionID: inv.NativeSession.Request.NativeSessionID, Status: inv.Status, SHA256: sha, Complete: complete, Truncated: inv.Status == "output_limit", Data: output}
	b, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	filename := inv.ID + ".output.json"
	if err := atomicReceiptWrite(s.dir, filename, b); err != nil {
		return nil, err
	}
	return &NativeOutputCheckpoint{Filename: filename, SHA256: sha, Bytes: len(output), Complete: complete, Truncated: record.Truncated}, nil
}
