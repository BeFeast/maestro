package approvalstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func stateDirAliasFixture(t *testing.T) (realState, aliasState string) {
	t.Helper()
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := mkdirAll(realParent); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(root, "alias")
	if err := symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	realState = filepath.Join(realParent, "state")
	if err := mkdirAll(realState); err != nil {
		t.Fatal(err)
	}
	return realState, filepath.Join(aliasParent, "state", ".")
}

func aliasDelivery(now time.Time) *state.Approval {
	a := &state.Approval{
		ID: "delivery-state-dir-alias", CreatedAt: now, UpdatedAt: now,
		Action: state.ApprovalActionDeployProject, Status: state.ApprovalStatusPending,
		Project: "owner/app", Repo: "owner/app",
		Delivery: &state.DeliveryPayload{
			Project: "owner/app", Repo: "owner/app",
			MergedSHA:    "0123456789abcdef0123456789abcdef01234567",
			ConfigDigest: "sha256:config", ExpiresAt: now.Add(time.Hour),
		},
	}
	a.PayloadHash = a.ComputePayloadHash()
	return a
}

func TestStoreCanonicalStateDirAliasesShareOneClaimNamespace(t *testing.T) {
	realState, aliasState := stateDirAliasFixture(t)
	store := openTestStore(t)
	now := time.Now().UTC()
	a := aliasDelivery(now)
	if _, err := store.PutDelivery(context.Background(), a, RowBinding{
		Project: "owner/app", Repo: "owner/app", StateDir: aliasState,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(context.Background(), realState, a.ID, now.Add(time.Second), "operator", "approve"); err != nil {
		t.Fatalf("approve via canonical path: %v", err)
	}
	if _, err := store.Approve(context.Background(), aliasState, a.ID, now.Add(2*time.Second), "operator", "duplicate"); !errors.Is(err, state.ErrApprovalNotPending) {
		t.Fatalf("second alias approval err = %v, want ErrApprovalNotPending", err)
	}
	rows, err := store.List(context.Background(), realState)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("canonical namespace rows = %d, want 1", len(rows))
	}
}

func TestOpenMigratesCollisionFreeLegacyStateDirAlias(t *testing.T) {
	realState, aliasState := stateDirAliasFixture(t)
	dbPath := filepath.Join(t.TempDir(), "legacy-alias.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a := aliasDelivery(now)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := putApprovalTx(context.Background(), tx, state.CanonicalDeliveryApproval(a), RowBinding{
		Project: "owner/app", Repo: "owner/app", StateDir: aliasState,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen with alias migration: %v", err)
	}
	defer store.Close()
	if _, err := store.Get(context.Background(), realState, a.ID); err != nil {
		t.Fatalf("canonical row after migration: %v", err)
	}
}

// Local wrappers keep filesystem mutations explicit in this test file while
// avoiding shell helpers.
var mkdirAll = func(path string) error { return os.MkdirAll(path, 0o755) }
var symlink = os.Symlink
