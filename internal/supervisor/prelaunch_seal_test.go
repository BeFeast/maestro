package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

type denyingAuxiliaryLimiter struct {
	code  string
	calls atomic.Int64
}

func (d *denyingAuxiliaryLimiter) ReserveAuxiliary(_, _ string) (func(), error) {
	d.calls.Add(1)
	return nil, aiexecution.Held(d.code)
}

func preparedReceipt(id, projectID, role string) ConsultationReceipt {
	return ConsultationReceipt{
		SchemaVersion:    1,
		Identity:         ConsultationIdentity{ID: id, ProjectID: projectID, CycleID: id, Role: role},
		StartedAt:        time.Now().UTC().Add(-time.Hour),
		Status:           "prepared",
		RequestedBackend: "backend", RequestedModel: "model", RequestedModelSource: "supervisor.model",
		PolicyVersion: "legacy-configured-backend-chain/v1", PolicyDigest: strings.Repeat("0", 64),
		InputDigest: strings.Repeat("1", 64),
		Candidates:  []CandidateReceipt{},
		Invocations: []InvocationReceipt{},
	}
}

func writeReceiptFile(t *testing.T, root, name string, value any) string {
	t.Helper()
	dir := filepath.Join(root, "supervisor-consultations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// #1233 review: a reviewer consultation refused by the auxiliary limiter (or
// run without one under a verified-route policy) is a typed hold before any
// launch. The runner must leave a closed receipt — status failed, ended_at set,
// zero candidates and invocations, no launch marker — which is the shape the
// operator-rearm pre-launch proof reads; a "prepared" receipt would spend the
// grant as a possible launch.
func TestNativeReviewerAuxiliaryDenialClosesReceiptBeforeLaunch(t *testing.T) {
	for _, mode := range []string{"capacity_exhausted", "no_controller"} {
		t.Run(mode, func(t *testing.T) {
			cfg, count, _ := nativeConfig(t)
			want := "auxiliary_controller_unavailable"
			aux := &denyingAuxiliaryLimiter{code: "auxiliary_capacity_exhausted"}
			if mode == "capacity_exhausted" {
				cfg.RuntimeAuxiliaryLimiter = aux
				want = aux.code
			}
			reviews := filepath.Join(cfg.StateDir, "native-reviews")
			claim := uuid.NewString()
			_, err := CompleteNativeReview(context.Background(), cfg, "claude-opus-5", claim, "synthetic diff")
			var hold *aiexecution.Hold
			if !errors.As(err, &hold) || hold.Code != want {
				t.Fatalf("error=%v want %s", err, want)
			}
			if nativeCalls(t, count) != 0 {
				t.Fatal("refused reservation launched a process")
			}
			r := loadReceipt(t, &config.Config{StateDir: reviews})
			if r.Identity.ID != claim || r.Identity.Role != "reviewer" || r.Identity.ProjectID != cfg.ProjectID || r.Status != "failed" || r.EndedAt == nil || r.NativeOutcomeComplete || r.PlannedInvocation != nil || len(r.Candidates) != 0 || len(r.Invocations) != 0 {
				t.Fatalf("receipt=%+v", r)
			}
			for _, marker := range []string{"launch.json", "registration.json"} {
				if _, err := os.Lstat(filepath.Join(reviews, "supervisor-consultations", marker)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s survived a pre-launch hold: %v", marker, err)
				}
			}
			if pending, err := PendingAuxiliaryRuns(cfg.StateDir); err != nil || len(pending) != 0 {
				t.Fatalf("pending=%v error=%v", pending, err)
			}
			if abandoned, err := AbandonedPreLaunchReceipt(reviews, claim, cfg.ProjectID, "reviewer"); err != nil || abandoned {
				t.Fatalf("closed receipt reported abandoned=%v error=%v", abandoned, err)
			}
			// The closed receipt holds nothing: the next consultation archives
			// it and is held by the same reason, not by the previous receipt.
			next := uuid.NewString()
			_, err = CompleteNativeReview(context.Background(), cfg, "claude-opus-5", next, "retry")
			if !errors.As(err, &hold) || hold.Code != want {
				t.Fatalf("second consultation error=%v want %s", err, want)
			}
			if _, err := os.Stat(filepath.Join(reviews, "supervisor-consultations", claim+".json")); err != nil {
				t.Fatal("closed receipt not archived by the next consultation", err)
			}
			if r := loadReceipt(t, &config.Config{StateDir: reviews}); r.Identity.ID != next || r.Status != "failed" || r.EndedAt == nil {
				t.Fatalf("second receipt=%+v", r)
			}
			if nativeCalls(t, count) != 0 {
				t.Fatal("process launched without a reservation")
			}
		})
	}
}

// A receipt left "prepared" with nothing launched (a daemon before the
// auxiliary-denial fix, or a crash between the first persist and the first
// candidate) is sealed by the reconcile path as failed+ended_at, once, under
// the store lock; every other shape is left untouched.
func TestReconcileSealsAbandonedPreLaunchReceiptOnce(t *testing.T) {
	for _, mode := range []string{"orphan", "launch_intent_candidate", "launch_marker", "registration_marker", "other_project", "other_role", "closed"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "native-reviews")
			cfg := &config.Config{ProjectID: uuid.NewString(), StateDir: root}
			cfg.Supervisor.NativeSessionRegistration = &config.NativeSessionRegistrationConfig{BudgetRunID: "run"}
			id := uuid.NewString()
			r := preparedReceipt(id, cfg.ProjectID, "reviewer")
			switch mode {
			case "launch_intent_candidate":
				r.Candidates = []CandidateReceipt{{Backend: "backend", Status: "launch_intent", IntentID: uuid.NewString()}}
			case "launch_marker":
				writeReceiptFile(t, root, "launch.json", map[string]any{"identity": r.Identity, "intent_id": uuid.NewString()})
			case "registration_marker":
				writeReceiptFile(t, root, "registration.json", map[string]any{"consultation_id": id, "registration": nil})
			case "other_project":
				r.Identity.ProjectID = uuid.NewString()
			case "other_role":
				r.Identity.Role = "supervisor"
			case "closed":
				end := time.Now().UTC()
				r.EndedAt = &end
				r.Status = "failed"
			}
			path := writeReceiptFile(t, root, "current.json", r)
			before, _ := os.ReadFile(path)
			wantSeal := mode == "orphan"
			if abandoned, err := AbandonedPreLaunchReceipt(root, id, cfg.ProjectID, "reviewer"); err != nil || abandoned != wantSeal {
				t.Fatalf("abandoned=%v error=%v want %v", abandoned, err, wantSeal)
			}
			identity := ConsultationIdentity{ID: uuid.NewString(), ProjectID: cfg.ProjectID, Role: "reviewer"}
			_, err := ReconcileNativeConsultation(cfg, identity, "")
			var hold *aiexecution.Hold
			if !errors.As(err, &hold) {
				t.Fatalf("error=%v", err)
			}
			after, _ := os.ReadFile(path)
			if !wantSeal {
				if hold.Code != "native_receipt_missing" || !bytes.Equal(before, after) {
					t.Fatalf("code=%s receipt changed=%v", hold.Code, !bytes.Equal(before, after))
				}
				return
			}
			if hold.Code != nativePreLaunchReceiptSealed {
				t.Fatalf("code=%s", hold.Code)
			}
			sealed := loadReceipt(t, cfg)
			if sealed.Identity != r.Identity || sealed.Status != "failed" || sealed.EndedAt == nil || sealed.NativeOutcomeComplete || sealed.PlannedInvocation != nil || len(sealed.Candidates) != 0 || len(sealed.Invocations) != 0 || sealed.InputDigest != r.InputDigest || !sealed.StartedAt.Equal(r.StartedAt) {
				t.Fatalf("sealed receipt=%+v", sealed)
			}
			if _, err := os.Lstat(filepath.Join(root, "supervisor-consultations", id+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("seal archived or duplicated the receipt", err)
			}
			if abandoned, err := AbandonedPreLaunchReceipt(root, id, cfg.ProjectID, "reviewer"); err != nil || abandoned {
				t.Fatalf("sealed receipt still abandoned=%v error=%v", abandoned, err)
			}
			_, err = ReconcileNativeConsultation(cfg, ConsultationIdentity{ID: uuid.NewString(), ProjectID: cfg.ProjectID, Role: "reviewer"}, "")
			if !errors.As(err, &hold) || hold.Code != "native_receipt_missing" {
				t.Fatalf("second reconcile error=%v", err)
			}
			if final, _ := os.ReadFile(path); !bytes.Equal(after, final) {
				t.Fatal("second reconcile rewrote the sealed receipt")
			}
			// The next consultation archives the sealed receipt like any closed one.
			_, unlock, err := openConsultationStore(root)
			if err != nil {
				t.Fatal(err)
			}
			unlock()
			if _, err := os.Stat(filepath.Join(root, "supervisor-consultations", id+".json")); err != nil {
				t.Fatal("sealed receipt not archived", err)
			}
		})
	}
}

// The live path treats a seal on the way in as transparent: a new reviewer
// consultation that finds an abandoned pre-launch receipt seals it, archives
// it and proceeds to its own (here limiter-refused) outcome.
func TestNativeReviewerSealsAbandonedPreLaunchReceiptOnTheWayIn(t *testing.T) {
	cfg, count, _ := nativeConfig(t)
	aux := &denyingAuxiliaryLimiter{code: "auxiliary_capacity_exhausted"}
	cfg.RuntimeAuxiliaryLimiter = aux
	reviews := filepath.Join(cfg.StateDir, "native-reviews")
	orphan := uuid.NewString()
	writeReceiptFile(t, reviews, "current.json", preparedReceipt(orphan, cfg.ProjectID, "reviewer"))
	claim := uuid.NewString()
	_, err := CompleteNativeReview(context.Background(), cfg, "claude-opus-5", claim, "synthetic diff")
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != aux.code || aux.calls.Load() != 1 || nativeCalls(t, count) != 0 {
		t.Fatalf("error=%v reservations=%d launches=%d", err, aux.calls.Load(), nativeCalls(t, count))
	}
	b, err := os.ReadFile(filepath.Join(reviews, "supervisor-consultations", orphan+".json"))
	if err != nil {
		t.Fatal("abandoned receipt not sealed and archived", err)
	}
	var archived ConsultationReceipt
	if json.Unmarshal(b, &archived) != nil || archived.Identity.ID != orphan || archived.Status != "failed" || archived.EndedAt == nil || len(archived.Candidates) != 0 || len(archived.Invocations) != 0 {
		t.Fatalf("archived=%+v", archived)
	}
	if r := loadReceipt(t, &config.Config{StateDir: reviews}); r.Identity.ID != claim || r.Status != "failed" || r.EndedAt == nil {
		t.Fatalf("current=%+v", r)
	}
}
