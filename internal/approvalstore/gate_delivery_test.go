package approvalstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func TestApplyDecisionJSONModeUsesSQLiteForDelivery(t *testing.T) {
	stateDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "custom-approvals.db")
	approval := savePendingDelivery(t, stateDir, "delivery-json")
	seedDeliveryStore(t, dbPath, stateDir, approval)

	st, got, err := ApplyDecision(Binding{
		Mode:     ModeJSON,
		DBPath:   dbPath,
		StateDir: stateDir,
		Repo:     "owner/app",
		Project:  "owner/app",
	}, "approve", approval.ID, time.Now().UTC(), "operator", "ship it")
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if got == nil || got.Status != state.ApprovalStatusApproved {
		t.Fatalf("returned approval = %+v, want approved", got)
	}
	mirrored, ok := st.FindApproval(approval.ID)
	if !ok || mirrored.Status != state.ApprovalStatusApproved {
		t.Fatalf("JSON mirror = %+v, want approved", mirrored)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	stored, err := store.Get(context.Background(), stateDir, approval.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != state.ApprovalStatusApproved {
		t.Fatalf("SQLite status = %q, want approved", stored.Status)
	}
}

func TestApplyDecisionJSONModeDeliveryClaimOnce(t *testing.T) {
	stateDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "custom-approvals.db")
	approval := savePendingDelivery(t, stateDir, "delivery-race")
	seedDeliveryStore(t, dbPath, stateDir, approval)
	binding := Binding{
		Mode:     ModeJSON,
		DBPath:   dbPath,
		StateDir: stateDir,
		Repo:     "owner/app",
		Project:  "owner/app",
	}

	const racers = 6
	var wins, lost int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := ApplyDecision(binding, "approve", approval.ID, time.Now().UTC(), "operator", "ship it")
			switch {
			case err == nil:
				atomic.AddInt32(&wins, 1)
			case errors.Is(err, state.ErrApprovalNotPending):
				atomic.AddInt32(&lost, 1)
			default:
				t.Errorf("ApplyDecision error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 || lost != racers-1 {
		t.Fatalf("wins=%d lost=%d, want 1/%d", wins, lost, racers-1)
	}
}

func TestApplyDecisionDeliveryWrongDBFailsWithoutSeeding(t *testing.T) {
	stateDir := t.TempDir()
	authoritativeDB := filepath.Join(t.TempDir(), "authoritative.db")
	wrongDB := filepath.Join(t.TempDir(), "wrong.db")
	approval := savePendingDelivery(t, stateDir, "delivery-ledger-binding")
	seedDeliveryStore(t, authoritativeDB, stateDir, approval)

	_, got, err := ApplyDecision(Binding{
		Mode: ModeJSON, DBPath: wrongDB, StateDir: stateDir,
		Repo: "owner/app", Project: "owner/app",
	}, "approve", approval.ID, time.Now().UTC(), "operator", "ship it")
	if !errors.Is(err, state.ErrApprovalNotFound) {
		t.Fatalf("wrong-ledger approval err = %v, want ErrApprovalNotFound", err)
	}
	if got != nil {
		t.Fatalf("wrong-ledger approval returned %+v, want nil", got)
	}

	wrong, openErr := Open(wrongDB)
	if openErr != nil {
		t.Fatalf("open wrong db: %v", openErr)
	}
	defer wrong.Close()
	if _, getErr := wrong.Get(context.Background(), stateDir, approval.ID); !errors.Is(getErr, state.ErrApprovalNotFound) {
		t.Fatalf("wrong DB was seeded: get err = %v", getErr)
	}
	authoritative, openErr := Open(authoritativeDB)
	if openErr != nil {
		t.Fatalf("open authoritative db: %v", openErr)
	}
	defer authoritative.Close()
	row, getErr := authoritative.Get(context.Background(), stateDir, approval.ID)
	if getErr != nil {
		t.Fatalf("authoritative row: %v", getErr)
	}
	if row.Status != state.ApprovalStatusPending {
		t.Fatalf("authoritative row status = %q, want pending", row.Status)
	}
}

func TestApplyDecisionJSONModeGenericApprovalStaysJSONOnly(t *testing.T) {
	stateDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "must-not-exist.db")
	now := time.Now().UTC()
	st := state.NewState()
	approval := makeApproval("generic-json", "merge_pr", &state.SupervisorTarget{PR: 7}, RowBinding{
		Project: "owner/app", Repo: "owner/app", StateDir: stateDir,
	})
	st.Approvals = append(st.Approvals, *approval)
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, got, err := ApplyDecision(Binding{
		Mode: ModeJSON, DBPath: dbPath, StateDir: stateDir, Repo: "owner/app", Project: "owner/app",
	}, "approve", approval.ID, now, "operator", "merge")
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if got.Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %q, want approved", got.Status)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("generic JSON approval unexpectedly opened SQLite: err=%v", err)
	}
}

func savePendingDelivery(t *testing.T, stateDir, id string) *state.Approval {
	t.Helper()
	now := time.Now().UTC()
	st := state.NewState()
	approval := &state.Approval{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		Action:    state.ApprovalActionDeployProject,
		Status:    state.ApprovalStatusPending,
		Repo:      "owner/app",
		Project:   "owner/app",
		Delivery: &state.DeliveryPayload{
			Project:      "owner/app",
			Repo:         "owner/app",
			MergedSHA:    "0123456789abcdef0123456789abcdef01234567",
			ConfigDigest: "approved-digest",
			ExpiresAt:    now.Add(time.Hour),
		},
		Audit: []state.ApprovalAudit{{At: now, Event: state.ApprovalAuditCreated}},
	}
	approval.PayloadHash = approval.ComputePayloadHash()
	st.Approvals = append(st.Approvals, *approval)
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return approval
}

func seedDeliveryStore(t *testing.T, dbPath, stateDir string, approval *state.Approval) {
	t.Helper()
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if _, err := store.PutDelivery(context.Background(), approval, RowBinding{
		Project: "owner/app", Repo: "owner/app", StateDir: stateDir,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("PutDelivery: %v", err)
	}
}
