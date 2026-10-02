package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

// #1233 review: the start-up/`maestro auxiliary reconcile` pass closes a
// reviewer receipt a previous daemon left "prepared" with nothing launched
// (no candidates, invocations or markers) and reports it as "sealed" rather
// than "clear", so the operator sees the repair in the journal. No capacity
// is released because none was held; a second pass is a no-op.
func TestReconcileAuxiliaryRootSealsAbandonedPreLaunchReceipt(t *testing.T) {
	// Receipt roots must be private (0700); t.TempDir honours the umask.
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectID: uuid.NewString(), StateDir: stateDir}
	cfg.Supervisor.NativeSessionRegistration = &config.NativeSessionRegistrationConfig{BudgetRunID: "run"}
	reviews := filepath.Join(stateDir, "native-reviews")
	dir := filepath.Join(reviews, "supervisor-consultations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	receipt := supervisor.ConsultationReceipt{
		SchemaVersion:    1,
		Identity:         supervisor.ConsultationIdentity{ID: id, ProjectID: cfg.ProjectID, CycleID: id, Role: "reviewer"},
		StartedAt:        time.Now().UTC().Add(-time.Hour),
		Status:           "prepared",
		RequestedBackend: "backend", RequestedModel: "model", RequestedModelSource: "supervisor.model",
		PolicyVersion: "legacy-configured-backend-chain/v1", PolicyDigest: strings.Repeat("0", 64),
		InputDigest: strings.Repeat("1", 64),
		Candidates:  []supervisor.CandidateReceipt{},
		Invocations: []supervisor.InvocationReceipt{},
	}
	b, _ := json.MarshalIndent(receipt, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "current.json"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	report := reconcileAuxiliaryRoot(context.Background(), cfg, stateDir, reviews, "reviewer", nil, nil)
	if report.Result != "sealed" || report.Hold != "" || len(report.Occupants) != 0 {
		t.Fatalf("report=%+v", report)
	}
	data, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sealed supervisor.ConsultationReceipt
	if json.Unmarshal(data, &sealed) != nil || sealed.Identity != receipt.Identity || sealed.Status != "failed" || sealed.EndedAt == nil || len(sealed.Candidates) != 0 || len(sealed.Invocations) != 0 {
		t.Fatalf("sealed=%+v", sealed)
	}
	if again := reconcileAuxiliaryRoot(context.Background(), cfg, stateDir, reviews, "reviewer", nil, nil); again.Result != "clear" {
		t.Fatalf("second pass=%+v", again)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "current.json")); string(after) != string(data) {
		t.Fatal("second pass rewrote the sealed receipt")
	}
}
