package configstore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const genesisYAML = `repo: BeFeast/maestro
local_path: /srv/example-src/maestro
worktree_base: /srv/example-worktrees/maestro
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /srv/example-vault/Dev/Areas/maestro
  vault: Example Vault
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
	reordered := `local_path: /srv/example-src/maestro
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
worktree_base: /srv/example-worktrees/maestro
repo: BeFeast/maestro
management_home:
  vault_path: Dev/Areas/maestro
  kind: obsidian
  vault: Example Vault
  path: /srv/example-vault/Dev/Areas/maestro
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
	presentationOnly := strings.Replace(genesisYAML, "repo: BeFeast/maestro", "# owner/repo\nrepo: \"BeFeast/maestro\" # canonical", 1)
	d, err := PrepareProject("d.yaml", []byte(presentationOnly))
	if err != nil {
		t.Fatalf("PrepareProject presentation-only: %v", err)
	}
	if a.Fingerprint != d.Fingerprint {
		t.Fatalf("fingerprint moved for comments/scalar quoting: %s vs %s", a.Fingerprint, d.Fingerprint)
	}
	// A real config change must move the fingerprint.
	changed := strings.Replace(genesisYAML, "/srv/example-src/maestro", "/srv/changed-src/maestro", 1)
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
		{"uppercase project_id", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nworktree_base: ~/.wt/x\nproject_id: 3F2504E0-4F89-41D3-9A0C-0305E82C3301\n", "canonical lowercase"},
		{"missing repo", "local_path: ~/src/x\nworktree_base: ~/.wt/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "repo is required"},
		{"missing local_path", "repo: BeFeast/maestro\nworktree_base: ~/.wt/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "local_path is required"},
		{"missing worktree_base", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "worktree_base is required"},
		{"invalid repo shape", strings.Replace(genesisYAML, "repo: BeFeast/maestro", "repo: maestro", 1), "owner/repository"},
		{"relative local path", strings.Replace(genesisYAML, "local_path: /srv/example-src/maestro", "local_path: relative/src", 1), "absolute execution-host paths"},
		{"missing management_home", "repo: BeFeast/maestro\nlocal_path: /srv/example-src/x\nworktree_base: /srv/example-worktrees/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "management_home is required"},
		{"relative management_home", strings.Replace(genesisYAML, "path: /srv/example-vault/Dev/Areas/maestro", "path: relative/vault", 1), "management_home.path must be an absolute"},
		{"invalid management_home", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nworktree_base: ~/.wt/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\nmanagement_home:\n  kind: notion\n", "not supported"},
		{"shared backend definition", "repo: BeFeast/maestro\nlocal_path: ~/src/x\nworktree_base: ~/.wt/x\nproject_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\nmodel:\n  default: claude\n  backends:\n    claude:\n      provider: claude\n", "must not define shared model.backends"},
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

// TestPrepareProjectSurfacesDeliveryWarnings: the genesis receipt carries the
// #872 delivery deprecation so an operator sees, at plan/apply time, that a
// legacy deploy_cmd runs an unattended deploy and should migrate to the
// approval_required default — and a config that already uses the safe default
// stays quiet.
func TestPrepareProjectSurfacesDeliveryWarnings(t *testing.T) {
	legacy := genesisYAML + "deploy_cmd: /srv/example-src/maestro/scripts/deploy.sh\n"
	p, err := PrepareProject("legacy.yaml", []byte(legacy))
	if err != nil {
		t.Fatalf("PrepareProject legacy: %v", err)
	}
	if !containsSubstr(p.Warnings, "deploy_cmd is deprecated") {
		t.Fatalf("legacy deploy_cmd genesis must warn about the deprecation; warnings=%v", p.Warnings)
	}

	safe := genesisYAML + "delivery:\n  mode: approval_required\n  command: ./scripts/deploy.sh\n  verify_command: ./scripts/status.sh\n  target_label: production\n  verification_label: status check\n  rollback_label: previous release\n"
	q, err := PrepareProject("safe.yaml", []byte(safe))
	if err != nil {
		t.Fatalf("PrepareProject safe: %v", err)
	}
	if containsSubstr(q.Warnings, "deploy_cmd is deprecated") || containsSubstr(q.Warnings, "delivery.mode: automatic") {
		t.Fatalf("approval_required delivery must not emit a delivery deprecation/automatic warning; warnings=%v", q.Warnings)
	}
}

func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
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

	first, err := store.ApplyProject(ctx, p, confirm, p.Fingerprint, BaselineAbsent)
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

	// Retry the exact same approved command (including its now-stale `absent`
	// baseline), as an adapter would after losing the first response. It must be
	// a no-op rather than forcing a new plan.
	p2, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	second, err := store.ApplyProject(ctx, p2, confirm, p2.Fingerprint, BaselineAbsent)
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

func TestApplyProjectIdempotentWithUnrelatedSharedBackends(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertProject(ctx, "backend-seed", writeTestYAML); err != nil {
		t.Fatalf("seed shared backends: %v", err)
	}
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	plan, err := store.PlanProject(ctx, p)
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	if plan.Effect != EffectCreate {
		t.Fatalf("first effect = %q, want create", plan.Effect)
	}
	if _, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint, plan.BaselineFingerprint); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after, err := store.PlanProject(ctx, p)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if after.Effect != EffectNoOp {
		t.Fatalf("second effect = %q, want no-op despite shared backends (existing=%+v)", after.Effect, after.Existing)
	}
}

func TestApplyProjectConfirmGate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}

	if _, err := store.ApplyProject(ctx, p, "", p.Fingerprint, BaselineAbsent); err == nil {
		t.Fatal("missing --confirm should be refused")
	}
	if _, err := store.ApplyProject(ctx, p, "11111111-2222-3333-4444-555555555555", p.Fingerprint, BaselineAbsent); err == nil {
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
	if _, err := store.ApplyProject(ctx, p, p.ProjectID, "", BaselineAbsent); err == nil {
		t.Fatal("missing fingerprint should be refused")
	}
	_, err = store.ApplyProject(ctx, p, p.ProjectID, "sha256:deadbeef", BaselineAbsent)
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

func TestApplyProjectRefusesStoreDriftSincePlan(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	plan, err := store.PlanProject(ctx, p)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.BaselineFingerprint != BaselineAbsent {
		t.Fatalf("baseline = %q, want absent", plan.BaselineFingerprint)
	}

	// Another process creates the same identity with different config after the
	// plan. Apply must not reinterpret the approved create as an update.
	drifted := strings.Replace(genesisYAML, "/srv/example-src/maestro", "/srv/concurrent-change", 1)
	if err := store.UpsertProject(ctx, p.Name, drifted); err != nil {
		t.Fatalf("seed concurrent change: %v", err)
	}
	report, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint, plan.BaselineFingerprint)
	if err == nil || !strings.Contains(err.Error(), "store changed since plan") {
		t.Fatalf("apply err = %v, want store-drift refusal", err)
	}
	if report == nil || report.BaselineFingerprint == BaselineAbsent {
		t.Fatalf("drift report = %+v, want observed current baseline", report)
	}
	cfg, err := store.Load(ctx, p.Name)
	if err != nil {
		t.Fatalf("load drifted row: %v", err)
	}
	if cfg.LocalPath != "/srv/concurrent-change" {
		t.Fatalf("apply overwrote concurrent config: local_path=%q", cfg.LocalPath)
	}
}

func TestApplyProjectConcurrentSameIdentityCreatesAtMostOneRow(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "config.db")
	storeA, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}
	defer storeA.Close()
	storeB, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	defer storeB.Close()

	pA, err := PrepareProject("a.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("prepare A: %v", err)
	}
	otherYAML := strings.Replace(genesisYAML, "repo: BeFeast/maestro", "repo: BeFeast/other", 1)
	pB, err := PrepareProject("b.yaml", []byte(otherYAML))
	if err != nil {
		t.Fatalf("prepare B: %v", err)
	}
	planA, err := storeA.PlanProject(ctx, pA)
	if err != nil {
		t.Fatalf("plan A: %v", err)
	}
	planB, err := storeB.PlanProject(ctx, pB)
	if err != nil {
		t.Fatalf("plan B: %v", err)
	}
	if planA.BaselineFingerprint != BaselineAbsent || planB.BaselineFingerprint != BaselineAbsent {
		t.Fatalf("initial baselines = (%q, %q), want absent", planA.BaselineFingerprint, planB.BaselineFingerprint)
	}

	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	results := make(chan error, 2)
	apply := func(store *Store, p *PreparedProject, baseline string) {
		ready <- struct{}{}
		<-start
		_, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint, baseline)
		results <- err
	}
	go apply(storeA, pA, planA.BaselineFingerprint)
	go apply(storeB, pB, planB.BaselineFingerprint)
	<-ready
	<-ready
	close(start)

	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent applies succeeded %d times, want exactly one", successes)
	}

	names, err := storeA.projectNames(ctx)
	if err != nil {
		t.Fatalf("list final projects: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("concurrent applies created rows %v, want exactly one", names)
	}
	cfg, err := storeA.Load(ctx, names[0])
	if err != nil {
		t.Fatalf("load winning project: %v", err)
	}
	if cfg.ProjectID != pA.ProjectID {
		t.Fatalf("winning project_id = %q, want %q", cfg.ProjectID, pA.ProjectID)
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

	report, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint, plan.BaselineFingerprint)
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
	report, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint, BaselineAbsent)
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

func TestApplyProjectRejectsIdlessSameNameForDifferentRepo(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	legacy := strings.Replace(genesisYAML, "repo: BeFeast/maestro", "repo: Other/project", 1)
	legacy = strings.Replace(legacy, "project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n", "", 1)
	if err := store.UpsertProject(ctx, "befeast-maestro", legacy); err != nil {
		t.Fatalf("seed id-less legacy row: %v", err)
	}
	p, err := PrepareProject("portable.yaml", []byte(genesisYAML))
	if err != nil {
		t.Fatalf("PrepareProject: %v", err)
	}
	plan, err := store.PlanProject(ctx, p)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Effect != EffectConflict || !strings.Contains(plan.Conflict, "basename adoption") {
		t.Fatalf("plan = %+v, want repo conflict", plan)
	}
	if _, err := store.ApplyProject(ctx, p, p.ProjectID, p.Fingerprint, plan.BaselineFingerprint); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("apply err = %v, want identity conflict", err)
	}
}
