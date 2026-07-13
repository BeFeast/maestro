// Package selfcheck implements `maestro selfcheck` (#842): a bundled, held-out
// behavioral smoke gate that exercises the maestro binary's core invariants
// with no external side effects.
//
// It is the evaluator that sits OUTSIDE the self-editing loop. Self-deploy
// (internal/selfdeploy, scripts/self-deploy.sh) builds, installs, and restarts
// a freshly-merged maestro binary; its only pre-#842 post-restart gate was a
// version-string health check, which a binary that boots and reports the right
// version but behaves worse can satisfy. selfcheck closes that gap: the deploy
// script runs `<new-binary> selfcheck` after the version check and before the
// deploy is finalized (before `.prev` is used for rollback), so a regression in
// config parsing, backend resolution, prompt assembly, or state persistence
// rolls the binary back instead of shipping.
//
// Every check runs against a FIXED fixture embedded in the binary (fixture.yaml)
// or a throwaway temp dir — never live fleet state — so the gate asserts
// behavior independent of current production data, is deterministic, and makes
// no GitHub writes, config-store mutations, or network calls beyond localhost.
package selfcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// fixtureConfig is the held-out fixture config the gate runs against: the input
// the config-parse, backend-resolution, and prompt-assembly invariants use, so
// the gate never depends on a live config store or production data. It ships
// inside the binary as a Go constant — not a *.yaml file — because the repo's
// .gitignore blocks *.yaml (to prevent committing real config with secrets),
// and this fixture must always travel with the binary.
//
// It is NOT a runnable project config: `repo` is a placeholder and the backends
// are never executed. Keep it self-contained and deterministic — no env
// interpolation, no on-disk sidecar files, no metered/auto-routing paths (which
// would make the gate depend on the network). model.default deliberately
// differs from the policy default_tier's backend (codex) so the
// backend-resolution invariant catches a router regression that bypasses the
// routing policy and falls straight through to model.default.
const fixtureConfig = `
repo: maestro-selfcheck/fixture
model:
  default: gemini
  backends:
    gemini:
      cmd: gemini
    codex:
      cmd: codex
    claude:
      cmd: claude
routing:
  mode: policy
  tiers:
    cheap:
      backend: gemini
      rank: 0
    standard:
      backend: codex
      effort: medium
      rank: 1
    strong:
      backend: claude
      effort: high
      rank: 2
  policy:
    default_tier: standard
    rules:
      - when: { labels: ["migration", "security"] }
        tier: strong
      - when: { size: small, dependency: leaf }
        tier: cheap
`

// Check names. These are the stable identifiers self-deploy names in the
// rollback reason and self-deploy-result.json when a check fails, so keep them
// short and script-greppable.
const (
	CheckConfig   = "config"
	CheckBackend  = "backend"
	CheckPrompt   = "prompt"
	CheckState    = "state"
	CheckDelivery = "delivery"
)

// Check is one invariant's outcome.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Report is the full selfcheck outcome.
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// FailedNames returns the names of the checks that failed, in check order.
func (r Report) FailedNames() []string {
	var names []string
	for _, c := range r.Checks {
		if !c.OK {
			names = append(names, c.Name)
		}
	}
	return names
}

// JSON renders the report as a single indented JSON object.
func (r Report) JSON() string {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		// A Report is all plain fields; marshal cannot realistically fail, but
		// never panic inside the deploy gate.
		return fmt.Sprintf(`{"ok":false,"checks":[{"name":"selfcheck","ok":false,"detail":%q}]}`, err.Error())
	}
	return string(data)
}

func pass(name, detail string) Check { return Check{Name: name, OK: true, Detail: detail} }
func fail(name, detail string) Check { return Check{Name: name, OK: false, Detail: detail} }

// Run executes every invariant against the embedded held-out fixture and
// returns the aggregate report. It is deterministic and side-effect-free (state
// round-trip uses a throwaway temp dir it removes before returning).
func Run() Report {
	return RunWithConfig([]byte(fixtureConfig))
}

// RunWithConfig runs the gate against a caller-supplied fixture config instead
// of the embedded one. It exists so tests can prove the gate has teeth — that a
// deliberately-broken fixture (unparseable config, or a config whose backend
// resolution regressed) fails the corresponding check — without shipping a
// second binary.
func RunWithConfig(configYAML []byte) Report {
	var checks []Check

	cfg, cfgCheck := checkConfig(configYAML)
	checks = append(checks, cfgCheck)

	// Backend resolution and prompt assembly need a parsed config; when config
	// parsing failed there is nothing to resolve against, so record dependent
	// failures rather than dereferencing a nil config.
	if cfg != nil {
		checks = append(checks, checkBackend(cfg), checkPrompt(cfg))
	} else {
		checks = append(checks,
			fail(CheckBackend, "skipped: fixture config did not parse"),
			fail(CheckPrompt, "skipped: fixture config did not parse"),
		)
	}

	// State persistence is independent of the fixture config.
	checks = append(checks, checkState())

	// Delivery gate canary (#872): pending → approve → exactly-once → verified
	// against a harmless fixture command/verifier, no live service touched.
	checks = append(checks, checkDelivery())

	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	return Report{OK: ok, Checks: checks}
}

// checkConfig asserts the config parses (defaults + full validation, including
// the routing-policy validator) and has the shape the other invariants rely on.
func checkConfig(configYAML []byte) (*config.Config, Check) {
	cfg, err := config.Parse(configYAML)
	if err != nil {
		return nil, fail(CheckConfig, "parse fixture config: "+err.Error())
	}
	if strings.TrimSpace(cfg.Repo) == "" {
		return cfg, fail(CheckConfig, "parsed config has empty repo")
	}
	if len(cfg.Model.Backends) == 0 {
		return cfg, fail(CheckConfig, "parsed config declares no backends")
	}
	if !cfg.Routing.IsPolicyMode() || cfg.Routing.Policy == nil {
		return cfg, fail(CheckConfig, "fixture routing is not in validated policy mode")
	}
	return cfg, pass(CheckConfig, fmt.Sprintf("%d backends, %d routing tiers, policy mode",
		len(cfg.Model.Backends), len(cfg.Routing.Tiers)))
}

// checkBackend asserts the router resolves backends deterministically from the
// config: a model: label overrides everything, and an unlabelled issue routes
// through the policy to its default tier's backend (NOT model.default — the
// fixture makes those differ so a policy-bypass regression is caught).
func checkBackend(cfg *config.Config) Check {
	r := router.New(cfg)

	// 1. Label override wins and is a real, validated backend.
	labelBackend := pinnableBackend(cfg)
	if labelBackend == "" {
		return fail(CheckBackend, "fixture declares no backends to pin")
	}
	labelled := github.Issue{Number: 1, Title: "labelled", State: "open"}
	labelled.Labels = append(labelled.Labels, struct {
		Name string `json:"name"`
	}{Name: "model:" + labelBackend})
	if d := r.ResolveBackendDecision(labelled); d.Backend != labelBackend || d.Reason != router.ReasonLabel {
		return fail(CheckBackend, fmt.Sprintf("model:%s label resolved to backend=%q reason=%q, want backend=%q reason=%q",
			labelBackend, d.Backend, d.Reason, labelBackend, router.ReasonLabel))
	}

	// 2. Unlabelled issue routes via policy to the default tier's backend.
	defaultTier := strings.TrimSpace(cfg.Routing.Policy.DefaultTier)
	wantBackend := strings.TrimSpace(cfg.Routing.Tiers[defaultTier].Backend)
	if wantBackend == "" {
		return fail(CheckBackend, "policy default_tier has no backend")
	}
	unlabelled := github.Issue{Number: 2, Title: "unlabelled", State: "open"}
	if d := r.ResolveBackendDecision(unlabelled); d.Backend != wantBackend || d.Tier != defaultTier {
		return fail(CheckBackend, fmt.Sprintf("unlabelled issue resolved to backend=%q tier=%q, want backend=%q tier=%q (policy default_tier)",
			d.Backend, d.Tier, wantBackend, defaultTier))
	}

	return pass(CheckBackend, fmt.Sprintf("label override → %s; unlabelled → %s (tier %s)", labelBackend, wantBackend, defaultTier))
}

// checkPrompt asserts the supervisor prompt — the highest-stakes text the
// fleet-orchestrating binary produces — assembles deterministically from the
// config, with the state packet substituted into the template.
func checkPrompt(cfg *config.Config) Check {
	prompt, err := supervisor.BuildSelfCheckPrompt(cfg)
	if err != nil {
		return fail(CheckPrompt, "assemble supervisor prompt: "+err.Error())
	}
	if strings.TrimSpace(prompt) == "" {
		return fail(CheckPrompt, "assembled supervisor prompt is empty")
	}
	if strings.Contains(prompt, "{{STATE_PACKET}}") {
		return fail(CheckPrompt, "assembled prompt still contains the {{STATE_PACKET}} placeholder (substitution failed)")
	}
	// The substituted packet must carry the project identity, proving the
	// packet was rendered into the template rather than dropped.
	if cfg.Repo != "" && !strings.Contains(prompt, cfg.Repo) {
		return fail(CheckPrompt, "assembled prompt does not contain the project repo from the state packet")
	}
	return pass(CheckPrompt, fmt.Sprintf("%d-byte supervisor prompt with substituted state packet", len(prompt)))
}

// checkState round-trips a State through the JSON store in a throwaway temp dir:
// an empty dir must cold-start to a fresh State, and a populated State must
// survive Save→Load unchanged. This exercises the on-disk state contract the
// whole fleet reads and writes, without touching any real state dir.
func checkState() Check {
	dir, err := os.MkdirTemp("", "maestro-selfcheck-state-")
	if err != nil {
		return fail(CheckState, "create temp state dir: "+err.Error())
	}
	defer os.RemoveAll(dir)

	store := state.NewJSONStore(dir)

	// Cold start: an empty state dir must load a fresh, initialized State and
	// must NOT create a file (the fleet's first-run contract).
	fresh, err := store.Load()
	if err != nil {
		return fail(CheckState, "cold-start load: "+err.Error())
	}
	if fresh == nil || fresh.Sessions == nil {
		return fail(CheckState, "cold-start load did not return an initialized State")
	}

	want := state.NewState()
	want.Sessions["slot-1"] = &state.Session{
		IssueNumber: 842,
		Branch:      "feat/selfcheck",
		Status:      state.StatusPROpen,
		PRNumber:    842,
	}
	want.SupervisorDecisions = []state.SupervisorDecision{{
		ID:      "selfcheck-fixture",
		Status:  "succeeded",
		Summary: "selfcheck state round-trip",
	}}

	if err := store.Save(want); err != nil {
		return fail(CheckState, "save state: "+err.Error())
	}
	got, err := store.Load()
	if err != nil {
		return fail(CheckState, "load state: "+err.Error())
	}

	sess := got.Sessions["slot-1"]
	if sess == nil || sess.IssueNumber != 842 || sess.PRNumber != 842 || sess.Status != state.StatusPROpen {
		return fail(CheckState, "session did not survive a save/load round-trip")
	}
	if len(got.SupervisorDecisions) != 1 || got.SupervisorDecisions[0].ID != "selfcheck-fixture" {
		return fail(CheckState, "supervisor decision did not survive a save/load round-trip")
	}
	return pass(CheckState, fmt.Sprintf("round-tripped %d session(s) and %d decision(s)",
		len(got.Sessions), len(got.SupervisorDecisions)))
}

// checkDelivery proves the #872 approval-gated delivery loop end to end against
// a harmless real fixture: a pending deploy_project approval runs ZERO
// commands; after approval the executor fetches a commit from a local bare
// origin, creates a clean detached temporary worktree at the exact approved
// SHA, runs a marker command + verifier, cleans the worktree, and records the
// terminal result. It then reopens SQLite and proves the command cannot replay.
// The authoritative fixture checkout is deliberately left on another revision
// with an untracked file and must remain byte-for-byte unchanged. No live repo,
// service, device, or network endpoint is touched.
func checkDelivery() Check {
	dir, err := os.MkdirTemp("", "maestro-selfcheck-delivery-")
	if err != nil {
		return fail(CheckDelivery, "create temp dir: "+err.Error())
	}
	defer os.RemoveAll(dir)

	origin := filepath.Join(dir, "origin.git")
	source := filepath.Join(dir, "source")
	marker := filepath.Join(dir, "delivery-runs")
	if err := initDeliveryFixture(dir, origin, source, marker); err != nil {
		return fail(CheckDelivery, "initialize git fixture: "+err.Error())
	}
	baseline, err := selfcheckGit(source, "rev-parse", "HEAD~1")
	if err != nil {
		return fail(CheckDelivery, "read baseline revision: "+err.Error())
	}
	approved, err := selfcheckGit(source, "rev-parse", "HEAD")
	if err != nil {
		return fail(CheckDelivery, "read approved revision: "+err.Error())
	}
	if _, err := selfcheckGit(source, "push", "-u", "origin", "main"); err != nil {
		return fail(CheckDelivery, "publish fixture revision: "+err.Error())
	}
	if _, err := selfcheckGit(source, "checkout", "--detach", baseline); err != nil {
		return fail(CheckDelivery, "set authoritative fixture to baseline: "+err.Error())
	}
	if err := os.WriteFile(filepath.Join(source, "operator-local.txt"), []byte("must survive unchanged\n"), 0o644); err != nil {
		return fail(CheckDelivery, "create authoritative-checkout sentinel")
	}
	beforeHead, err := selfcheckGit(source, "rev-parse", "HEAD")
	if err != nil {
		return fail(CheckDelivery, "read authoritative HEAD: "+err.Error())
	}
	beforeStatus, err := selfcheckGit(source, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fail(CheckDelivery, "read authoritative status: "+err.Error())
	}

	delivery := config.DeliveryConfig{
		Mode:          config.DeliveryModeApprovalRequired,
		LocalPath:     source,
		Command:       "./deploy.sh",
		VerifyCommand: "./verify.sh",
	}
	dbPath := filepath.Join(dir, "maestro.db")
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		return fail(CheckDelivery, "open approvals store: "+err.Error())
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()

	ctx := context.Background()
	const (
		stateDir = "selfcheck-delivery"
		repo     = "maestro-selfcheck/fixture"
		id       = "approval-deploy-selfcheck"
	)
	rb := approvalstore.RowBinding{Project: repo, Repo: repo, StateDir: stateDir}

	// Seed a PENDING delivery and assert it is NOT claimable — approval-required
	// runs zero delivery before approval.
	pending := &state.Approval{
		ID: id, CreatedAt: fixedNow, UpdatedAt: fixedNow, Action: state.ApprovalActionDeployProject,
		Status: state.ApprovalStatusPending, Repo: repo, Project: repo,
		Delivery: &state.DeliveryPayload{
			Project: repo, Repo: repo, MergedSHA: approved,
			ConfigDigest: delivery.ApprovalDigest(),
			MergedAt:     fixedNow,
			ExpiresAt:    fixedNow.Add(time.Hour),
		},
	}
	pending.PayloadHash = pending.ComputePayloadHash()
	if _, err := store.Put(ctx, pending, rb); err != nil {
		return fail(CheckDelivery, "seed pending: "+err.Error())
	}
	if _, err := store.ClaimExecuting(ctx, stateDir, id, fixedNow, "selfcheck", "claim"); err != state.ErrApprovalNotApproved {
		return fail(CheckDelivery, fmt.Sprintf("pending delivery was claimable (err=%v) — approval gate leaks", err))
	}

	// Approve it (claim-once), then run the real delivery executor.
	if _, err := store.Approve(ctx, stateDir, id, fixedNow, "selfcheck", "approve"); err != nil {
		return fail(CheckDelivery, "approve: "+err.Error())
	}
	ex := &approver.DeliveryExecutor{
		Store:     store,
		StateDir:  stateDir,
		Repo:      repo,
		Delivery:  delivery,
		Checkout:  approver.NewLocalFixtureCheckoutPreparer(repo, origin),
		Actor:     "selfcheck",
		Freshness: approver.DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error { return nil }),
		Now:       func() time.Time { return fixedNow },
	}
	first := ex.Deliver(ctx, id)
	if first.Status != state.ApprovalStatusExecuted {
		return fail(CheckDelivery, fmt.Sprintf("approve did not execute delivery: status=%q err=%v", first.Status, first.Err))
	}
	if first.Approval == nil || first.Approval.Delivery == nil || !first.Approval.Delivery.Verified {
		return fail(CheckDelivery, "delivery ran but was not verified")
	}
	if first.Approval.Delivery.ExecutedRevision != approved {
		return fail(CheckDelivery, "delivery executed a revision other than the approved SHA")
	}
	afterHead, err := selfcheckGit(source, "rev-parse", "HEAD")
	if err != nil || afterHead != beforeHead {
		return fail(CheckDelivery, "delivery changed the authoritative checkout HEAD")
	}
	afterStatus, err := selfcheckGit(source, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || afterStatus != beforeStatus {
		return fail(CheckDelivery, "delivery changed the authoritative checkout files")
	}
	worktrees, err := selfcheckGit(source, "worktree", "list", "--porcelain")
	if err != nil || strings.Count(worktrees, "worktree ") != 1 {
		return fail(CheckDelivery, "temporary delivery worktree was not cleaned up")
	}
	markerBefore, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(markerBefore)) != approved {
		return fail(CheckDelivery, "delivery marker does not prove one execution at the approved SHA")
	}

	// Reopen the durable store before the retry: this proves the no-replay
	// fence survives a daemon/CLI process boundary, not just one Go object.
	if err := store.Close(); err != nil {
		return fail(CheckDelivery, "close approvals store before replay check: "+err.Error())
	}
	store = nil
	reopened, err := approvalstore.Open(dbPath)
	if err != nil {
		return fail(CheckDelivery, "reopen approvals store: "+err.Error())
	}
	store = reopened
	ex.Store = reopened
	second := ex.Deliver(ctx, id)
	if !second.Skipped {
		return fail(CheckDelivery, fmt.Sprintf("second delivery re-ran (status=%q) — not exactly-once", second.Status))
	}
	markerAfter, err := os.ReadFile(marker)
	if err != nil || string(markerAfter) != string(markerBefore) {
		return fail(CheckDelivery, "delivery replayed after reopening SQLite")
	}
	return pass(CheckDelivery, "pending→approve→exact detached SHA→verified→SQLite reopen no-replay")
}

func initDeliveryFixture(root, origin, source, marker string) error {
	if _, err := selfcheckGit(root, "init", "--bare", origin); err != nil {
		return err
	}
	if _, err := selfcheckGit(root, "init", "--initial-branch=main", source); err != nil {
		return err
	}
	for _, setting := range [][2]string{{"user.name", "Maestro Selfcheck"}, {"user.email", "maestro-selfcheck@example.invalid"}} {
		if _, err := selfcheckGit(source, "config", setting[0], setting[1]); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(source, "artifact.txt"), []byte("baseline\n"), 0o644); err != nil {
		return errors.New("write baseline fixture file failed")
	}
	if _, err := selfcheckGit(source, "add", "artifact.txt"); err != nil {
		return err
	}
	if _, err := selfcheckGit(source, "commit", "-m", "baseline"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(source, "artifact.txt"), []byte("approved\n"), 0o644); err != nil {
		return errors.New("write approved fixture file failed")
	}
	deployScript := "#!/bin/sh\nset -eu\ntest \"$(cat artifact.txt)\" = approved\ntest -z \"$(git symbolic-ref -q HEAD || true)\"\ngit rev-parse HEAD >> " + shellQuote(marker) + "\n"
	verifyScript := "#!/bin/sh\nset -eu\ntest \"$(cat " + shellQuote(marker) + ")\" = \"$(git rev-parse HEAD)\"\n"
	if err := os.WriteFile(filepath.Join(source, "deploy.sh"), []byte(deployScript), 0o700); err != nil {
		return errors.New("write delivery fixture entrypoint failed")
	}
	if err := os.WriteFile(filepath.Join(source, "verify.sh"), []byte(verifyScript), 0o700); err != nil {
		return errors.New("write delivery fixture verifier failed")
	}
	if _, err := selfcheckGit(source, "add", "artifact.txt", "deploy.sh", "verify.sh"); err != nil {
		return err
	}
	if _, err := selfcheckGit(source, "commit", "-m", "approved delivery"); err != nil {
		return err
	}
	if _, err := selfcheckGit(source, "remote", "add", "origin", origin); err != nil {
		return err
	}
	return nil
}

func selfcheckGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Fixed commit timestamps keep object IDs reproducible while the report
	// itself remains free of fixture paths and hashes.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-07-12T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-07-12T00:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// fixedNow is a deterministic clock for the delivery canary so the gate stays
// reproducible.
var fixedNow = time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)

// pinnableBackend returns a declared backend name to pin via a model: label.
// It prefers a backend that is NOT model.default so the label-override
// invariant is falsifiable: a router regression that ignored the label and fell
// through to model.default would resolve to a different backend and fail the
// check. Falls back to the default (or lexically-first) backend when the config
// declares only one.
func pinnableBackend(cfg *config.Config) string {
	def := strings.TrimSpace(cfg.Model.Default)
	best := ""
	for name := range cfg.Model.Backends {
		if name == def {
			continue
		}
		if best == "" || name < best {
			best = name
		}
	}
	if best != "" {
		return best
	}
	if _, ok := cfg.Model.Backends[def]; ok {
		return def
	}
	return ""
}
