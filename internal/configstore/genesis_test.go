package configstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const genesisYAML = `repo: BeFeast/maestro
local_path: ~/src/maestro
worktree_base: ~/.worktrees/maestro
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /home/god/Obsidian/Dev
  vault: Obsidian Vault
  vault_path: Dev/Areas/maestro
`

func TestPrepareProjectValid(t *testing.T) {
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	if p.Name != "befeast-maestro" {
		t.Fatalf("derived name = %q, want befeast-maestro", p.Name)
	}
	if p.ProjectID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("project_id = %q", p.ProjectID)
	}
	if !strings.HasPrefix(p.Fingerprint, "sha256:") {
		t.Fatalf("fingerprint = %q, want sha256: prefix", p.Fingerprint)
	}
}

// The fingerprint is deterministic and insensitive to key ordering / backend
// map ordering, so plan and apply of the same config always agree.
func TestPrepareProjectFingerprintStable(t *testing.T) {
	reordered := `local_path: ~/src/maestro
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
worktree_base: ~/.worktrees/maestro
repo: BeFeast/maestro
management_home:
  vault_path: Dev/Areas/maestro
  kind: obsidian
  vault: Obsidian Vault
  path: /home/god/Obsidian/Dev
`
	a, err := PrepareProject("a.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject a: %v", err)
	}
	b, err := PrepareProject("b.yaml", []byte(reordered))
	if err != nil {
		t.Fatalf("PrepareProject b: %v", err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("fingerprint not stable across key order: %s vs %s", a.Fingerprint, b.Fingerprint)
	}
	// A real config change must move the fingerprint.
	changed := strings.Replace(genesisYAML, "~/src/maestro", "/srv/maestro", 1)
	c, err := PrepareProject("c.yaml", []byte(changed))
	if err != nil {
		t.Fatalf("PrepareProject c: %v", err)
	}
	if a.Fingerprint == c.Fingerprint {
		t.Fatalf("fingerprint did not change for a changed config")
	}
}

func TestPrepareProjectRejections(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown field", genesisYAML + "worktree_basee: typo\n", "worktree_basee"},
		{"missing project_id", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nworktree_base: ~/.wt/x\n", "project_id"},
		{"invalid project_id", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nworktree_base: ~/.wt/x\nproject_id: not-a-uuid\n", "not a valid UUID"},
		{"missing repo", "local_path: ~/src/x\nworktree_base: ~/.wt/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "repo is required"},
		{"missing local_path", "repo: BeFeast/maestro\nworktree_base: ~/.wt/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "local_path is required"},
		{"missing worktree_base", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "worktree_base is required"},
		{"invalid management_home", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nworktree_base: ~/.wt/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\nmanagement_home:\n  kind: notion\n", "not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PrepareProject("x.yaml", []byte(tc.yaml))
			if err == nil {
				t.Fatalf("want error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want to contain %q", err, tc.want)
			}
		})
	}
}

// PlanProject issues only reads: the store fingerprint (name->updated_at) and
// the row count are unchanged across a plan.
func TestPlanProjectIsZeroWrite(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}

	before, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint before: %v", err)
	}
	report, err := store.PlanProject(ctx, p)
	if err != nil {
		t.Fatalf("PlanProject: %v", err)
	}
	if report.Effect != EffectCreate {
		t.Fatalf("effect = %q, want create", report.Effect)
	}
	if report.Wrote {
		t.Fatalf("plan reported a write")
	}
	after, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatalf("fingerprint after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("plan changed the store fingerprint: %v -> %v", before, after)
	}
	names, err := store.projectNames(ctx)
	if err != nil {
		t.Fatalf("projectNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("plan created rows: %v", names)
	}
}

// First apply creates exactly one row; a second identical apply is a reported
// no-op that writes nothing.
func TestApplyProjectIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	confirm := p.ProjectID

	first, err := store.ApplyProject(ctx, p, confirm, p.Fingerprint)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if first.Effect != EffectCreate || !first.Wrote {
		t.Fatalf("first apply = %+v, want create+wrote", first)
	}
	names, err := store.projectNames(ctx)
	if err != nil {
		t.Fatalf("projectNames: %v", err)
	}
	if len(names) != 1 || names[0] != "befeast-maestro" {
		t.Fatalf("after first apply names = %v, want [befeast-maestro]", names)
	}

	// A fresh prepare of the same bytes (the adapter re-reads the file) is a no-op.
	p2, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	second, err := store.ApplyProject(ctx, p2, confirm, p2.Fingerprint)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second.Effect != EffectNoOp || second.Wrote {
		t.Fatalf("second apply = %+v, want no-op+!wrote", second)
	}
	names, _ = store.projectNames(ctx)
	if len(names) != 1 {
		t.Fatalf("second apply changed row count: %v", names)
	}
}

func TestApplyProjectConfirmGate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}

	if _, err := store.ApplyProject(ctx, p, "", p.Fingerprint); err == nil {
		t.Fatal("missing --confirm should be refused")
	}
	if _, err := store.ApplyProject(ctx, p, "11111111-2222-3333-4444-555555555555", p.Fingerprint); err == nil {
		t.Fatal("wrong --confirm should be refused")
	}
	// Nothing was written by the refused applies.
	if names, _ := store.projectNames(ctx); len(names) != 0 {
		t.Fatalf("refused apply wrote a row: %v", names)
	}
}

func TestApplyProjectFingerprintGate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	// A stale plan-time fingerprint (file changed since plan) is refused.
	_, err = store.ApplyProject(ctx, p, p.ProjectID, "sha256:deadbeef")
	if err == nil {
		t.Fatal("stale fingerprint should be refused")
	}
	if !strings.Contains(err.Error(), "changed since plan") {
		t.Fatalf("error = %v, want to mention change since plan", err)
	}
	if names, _ := store.projectNames(ctx); len(names) != 0 {
		t.Fatalf("refused apply wrote a row: %v", names)
	}
}

// A row with the same derived name but a different project_id must never be
// overwritten by a plan/apply — the identity conflict is an explicit stop.
func TestApplyProjectIdentityConflictSameName(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	// Seed a row under the same name carrying a DIFFERENT project_id, bypassing
	// the genesis path so we exercise the conflict guard directly.
	seed := strings.Replace(genesisYAML,
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"99999999-8888-7777-6666-555555555555", 1)
	if err := store.UpsertProject(ctx, "befeast-maestro", seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	plan, err := store.PlanProject(ctx, p)
	if err != nil {
		t.Fatalf("PlanProject: %v", err)
	}
	if plan.Effect != EffectConflict {
		t.Fatalf("plan effect = %q, want conflict", plan.Effect)
	}

	report, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint)
	if !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("apply err = %v, want ErrIdentityConflict", err)
	}
	if report == nil || report.Effect != EffectConflict || report.Wrote {
		t.Fatalf("conflict report = %+v", report)
	}
	// The seeded row's identity is untouched.
	cfg, err := store.Load(ctx, "befeast-maestro")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProjectID != "99999999-8888-7777-6666-555555555555" {
		t.Fatalf("conflict overwrote the stored identity: %q", cfg.ProjectID)
	}
}

// The same project_id already registered under a DIFFERENT name is a conflict:
// one identity must not fan out to two rows.
func TestApplyProjectIdentityConflictOtherName(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	// Existing row "other" already owns the identity.
	other := strings.Replace(genesisYAML, "repo: BeFeast/maestro", "repo: BeFeast/other", 1)
	if err := store.UpsertProject(ctx, "other", other); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	report, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint)
	if !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("apply err = %v, want ErrIdentityConflict", err)
	}
	if report == nil || !strings.Contains(report.Conflict, "other") {
		t.Fatalf("conflict report = %+v, want to name the other row", report)
	}
	// No new row was created for befeast-maestro.
	if names, _ := store.projectNames(ctx); len(names) != 1 {
		t.Fatalf("conflict created an extra row: %v", names)
	}
}
