package configstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"gopkg.in/yaml.v3"
)

// The project plan/apply genesis flow (#871) replaces the retired `maestro init`
// scaffold. It turns a portable project YAML into a single config-store row that
// the one long-lived `maestro daemon --watch-store` observes and starts as one
// flow — no per-project maestro.yaml, systemd unit, or launchd plist. `plan` is a
// pure, zero-write validation + effect preview; `apply` upserts idempotently
// after an explicit project_id confirmation and an optional plan-time fingerprint
// gate. Both are structured so an external bootstrap adapter can drive them by
// their machine-readable receipts.

// Genesis effect vocabulary shared by PlanProject (predicted) and ApplyProject
// (realized). A plan and an immediately-following apply of the same file report
// the same effect.
const (
	EffectCreate   = "create"
	EffectUpdate   = "update"
	EffectNoOp     = "no-op"
	EffectConflict = "conflict"
)

// ErrIdentityConflict is the sentinel wrapped by ApplyProject when a row cannot
// be written because its identity conflicts with the store. It never overwrites
// on conflict; the caller stops and surfaces the reason.
var ErrIdentityConflict = errors.New("config-store identity conflict")

// PreparedProject is the validated, fingerprinted result of PrepareProject. It
// carries everything plan/apply need without re-parsing, so the fingerprint the
// operator reviewed in `plan` is exactly the one `apply` gates on.
type PreparedProject struct {
	Name         string
	ProjectID    string
	Repo         string
	LocalPath    string
	WorktreeBase string
	Fingerprint  string
	// Canonical is the deterministic project document the fingerprint is taken
	// over (backends normalized, re-marshaled). Raw is the original file bytes,
	// stored verbatim on apply so comments/ordering survive the round-trip that
	// UpsertProject itself performs.
	Canonical []byte
	Raw       []byte
	Warnings  []string
}

// ExistingRow is the identity summary of a config-store row that a plan/apply
// would touch, reported so an adapter can see why an effect was chosen.
type ExistingRow struct {
	Name        string `json:"name"`
	ProjectID   string `json:"project_id,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

// GenesisReport is the receipt PlanProject/ApplyProject return: the store-name,
// project id, config fingerprint, effect, and whether a write happened. The CLI
// wraps it with the daemon-reconciliation expectation and next-step commands.
type GenesisReport struct {
	Project     string       `json:"project"`
	ProjectID   string       `json:"project_id"`
	Fingerprint string       `json:"fingerprint"`
	Effect      string       `json:"effect"`
	Conflict    string       `json:"conflict,omitempty"`
	Wrote       bool         `json:"wrote"`
	Existing    *ExistingRow `json:"existing,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
}

// PrepareProject validates a portable project YAML for the genesis flow and
// computes its stable name + fingerprint WITHOUT touching any store. It is the
// strict gate every plan/apply passes through:
//
//   - strict decode (unknown/misspelled keys rejected by name, #869);
//   - repo required (config.Parse) — the derived project name comes from it;
//   - a valid, present project_id (the durable identity the flow is built around);
//   - local_path and worktree_base required (a genesis row must be runnable).
//
// A missing management_home is allowed but warned: an external bootstrap adapter
// cannot correlate the row back to a control-room home without it.
func PrepareProject(sourcePath string, data []byte) (*PreparedProject, error) {
	cfg, err := config.ParseStrict(data)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(cfg.ProjectID)
	if id == "" {
		return nil, fmt.Errorf("project plan/apply requires a stable project_id (a canonical 8-4-4-4-12 UUID); none is set — generate one and add `project_id:` to the config (#869)")
	}
	if strings.TrimSpace(cfg.LocalPath) == "" {
		return nil, fmt.Errorf("local_path is required for project plan/apply (the on-disk clone the daemon works from)")
	}
	if strings.TrimSpace(cfg.WorktreeBase) == "" {
		return nil, fmt.Errorf("worktree_base is required for project plan/apply (the base dir worker worktrees are created under)")
	}
	name, err := ProjectNameFor(sourcePath, data)
	if err != nil {
		return nil, err
	}
	canonical, fp, err := canonicalProjectDoc(data)
	if err != nil {
		return nil, fmt.Errorf("canonicalize config: %w", err)
	}
	var warnings []string
	if !cfg.ManagementHome.Configured() {
		warnings = append(warnings, "no management_home block is set; an external bootstrap adapter cannot correlate this row back to a PM/control-room home (#869)")
	}
	return &PreparedProject{
		Name:         name,
		ProjectID:    id,
		Repo:         cfg.Repo,
		LocalPath:    cfg.LocalPath,
		WorktreeBase: cfg.WorktreeBase,
		Fingerprint:  fp,
		Canonical:    canonical,
		Raw:          data,
		Warnings:     warnings,
	}, nil
}

// canonicalProjectDoc returns the deterministic project document and its
// fingerprint. It normalizes so that reformatting-only edits (mapping-key
// reordering, whitespace) do NOT move the fingerprint, while any real change to a
// value or a list DOES:
//
//   - backends are detached and re-attached in sorted order (matching a
//     stored+exported row), then every mapping's keys are sorted recursively;
//   - sequences are left in place (their order is semantically meaningful, e.g.
//     issue_labels / ordered_queue.issues).
//
// The fingerprint is a sha256 over the canonical bytes, prefixed "sha256:", so an
// incoming file and the same config after a round-trip through the store compare
// equal.
func canonicalProjectDoc(data []byte) ([]byte, string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, "", err
	}
	backends := detachBackends(&root)
	attachBackends(&root, backends)
	sortMappingKeys(&root)
	out, err := marshalNode(&root)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(out)
	return out, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// sortMappingKeys recursively sorts the key/value pairs of every mapping node by
// key so the canonical form is independent of mapping-key order. Sequence order
// is preserved. Mapping key order carries no semantics for the config decoder,
// but list order does, so only mappings are reordered.
func sortMappingKeys(node *yaml.Node) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			sortMappingKeys(c)
		}
	case yaml.MappingNode:
		type pair struct{ k, v *yaml.Node }
		pairs := make([]pair, 0, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			sortMappingKeys(node.Content[i+1])
			pairs = append(pairs, pair{node.Content[i], node.Content[i+1]})
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].k.Value < pairs[j].k.Value })
		node.Content = node.Content[:0]
		for _, p := range pairs {
			node.Content = append(node.Content, p.k, p.v)
		}
	case yaml.SequenceNode:
		for _, c := range node.Content {
			sortMappingKeys(c)
		}
	}
}

// PlanEmpty previews p against a store that does not exist yet. A missing store
// holds no rows, so the effect is unconditionally create with no existing row
// and no possible identity conflict. The CLI uses this to keep `project plan`
// from creating the store file at all (the zero-write guarantee): validating a
// config must not leave a schema-only DB behind.
func PlanEmpty(p *PreparedProject) *GenesisReport {
	return &GenesisReport{
		Project:     p.Name,
		ProjectID:   p.ProjectID,
		Fingerprint: p.Fingerprint,
		Effect:      EffectCreate,
		Wrote:       false,
		Warnings:    p.Warnings,
	}
}

// PlanProject previews what applying p would do against the store WITHOUT any
// write. It returns the effect (create/update/no-op/conflict), the existing row
// it compared against, and any validation warnings. It only issues reads, so a
// caller can compare the store fingerprint before and after and see it unchanged.
func (s *Store) PlanProject(ctx context.Context, p *PreparedProject) (*GenesisReport, error) {
	effect, conflict, existing, err := s.evaluate(ctx, p)
	if err != nil {
		return nil, err
	}
	return &GenesisReport{
		Project:     p.Name,
		ProjectID:   p.ProjectID,
		Fingerprint: p.Fingerprint,
		Effect:      effect,
		Conflict:    conflict,
		Wrote:       false,
		Existing:    existing,
		Warnings:    p.Warnings,
	}, nil
}

// ApplyProject upserts p into the store idempotently, after gating on:
//
//   - confirm: must exactly equal p.ProjectID (an explicit operator ack that this
//     durable identity is the one being written);
//   - wantFingerprint (optional): the fingerprint the plan reported — a mismatch
//     means the file changed since the plan was reviewed, so the write is refused.
//
// A create/update performs exactly one UpsertProject; a no-op writes nothing; an
// identity conflict returns the report plus ErrIdentityConflict and never
// overwrites. Removal/rollback is deliberately NOT handled here — it stays a
// separate explicit operator action.
func (s *Store) ApplyProject(ctx context.Context, p *PreparedProject, confirm, wantFingerprint string) (*GenesisReport, error) {
	if strings.TrimSpace(confirm) == "" {
		return nil, fmt.Errorf("--confirm is required: pass the exact project_id (%s) to apply", p.ProjectID)
	}
	if strings.TrimSpace(confirm) != p.ProjectID {
		return nil, fmt.Errorf("--confirm %q does not match the config project_id %q; refusing to apply", strings.TrimSpace(confirm), p.ProjectID)
	}
	if wf := strings.TrimSpace(wantFingerprint); wf != "" && wf != p.Fingerprint {
		return nil, fmt.Errorf("config changed since plan (approved fingerprint %s, current %s); re-run `maestro project plan` and review before applying", wf, p.Fingerprint)
	}

	effect, conflict, existing, err := s.evaluate(ctx, p)
	if err != nil {
		return nil, err
	}
	report := &GenesisReport{
		Project:     p.Name,
		ProjectID:   p.ProjectID,
		Fingerprint: p.Fingerprint,
		Effect:      effect,
		Conflict:    conflict,
		Existing:    existing,
		Warnings:    p.Warnings,
	}
	switch effect {
	case EffectConflict:
		return report, fmt.Errorf("%w: %s", ErrIdentityConflict, conflict)
	case EffectNoOp:
		// Second identical apply: the matching row already exists, so this is a
		// reported no-op and nothing is written.
		report.Wrote = false
		return report, nil
	case EffectCreate, EffectUpdate:
		if err := s.UpsertProject(ctx, p.Name, string(p.Raw)); err != nil {
			return nil, err
		}
		report.Wrote = true
		return report, nil
	default:
		return report, fmt.Errorf("internal: unknown genesis effect %q", effect)
	}
}

// evaluate decides the effect of applying p by comparing it to the current
// store contents. It is read-only and shared by plan and apply so the two never
// disagree. Conflict rules (never overwrite by basename accident):
//
//   - the incoming project_id is already registered under a DIFFERENT row name
//     (same identity, two rows) — conflict;
//   - a row with the same name exists but carries a DIFFERENT non-empty
//     project_id — conflict.
//
// Otherwise: no row -> create; identical fingerprint -> no-op; else update.
func (s *Store) evaluate(ctx context.Context, p *PreparedProject) (effect, conflict string, existing *ExistingRow, err error) {
	row, err := s.existingRow(ctx, p.Name)
	if err != nil {
		return "", "", nil, err
	}
	other, err := s.projectNameByID(ctx, p.ProjectID, p.Name)
	if err != nil {
		return "", "", nil, err
	}
	if other != "" {
		return EffectConflict,
			fmt.Sprintf("project_id %s is already registered under project %q; refusing to create a second row for the same identity", p.ProjectID, other),
			row, nil
	}
	if row == nil {
		return EffectCreate, "", nil, nil
	}
	if row.ProjectID != "" && row.ProjectID != p.ProjectID {
		return EffectConflict,
			fmt.Sprintf("project %q already exists with a different project_id (stored %s, incoming %s); refusing to overwrite by name", p.Name, row.ProjectID, p.ProjectID),
			row, nil
	}
	if row.Fingerprint == p.Fingerprint {
		return EffectNoOp, "", row, nil
	}
	return EffectUpdate, "", row, nil
}

// existingRow summarizes the config-store row named name, or nil if there is no
// such row. Its fingerprint is computed over the exported (backends re-attached)
// document through the same canonicalization as the incoming file, so equal
// configs compare equal.
func (s *Store) existingRow(ctx context.Context, name string) (*ExistingRow, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project WHERE name = ?`, name).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	exported, err := s.ExportProject(ctx, name)
	if err != nil {
		return nil, err
	}
	_, fp, err := canonicalProjectDoc(exported)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(exported, &root); err != nil {
		return nil, err
	}
	return &ExistingRow{Name: name, ProjectID: scalarAt(&root, "project_id"), Fingerprint: fp}, nil
}

// projectNameByID returns the name of a stored project whose project_id equals
// id, skipping the exclude name (the row we would write ourselves). Empty when
// no other row claims that identity.
func (s *Store) projectNameByID(ctx context.Context, id, exclude string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil
	}
	names, err := s.projectNames(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if n == exclude {
			continue
		}
		var stored string
		err := s.db.QueryRowContext(ctx, `SELECT config_yaml FROM project WHERE name = ?`, n).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		var root yaml.Node
		if err := yaml.Unmarshal([]byte(stored), &root); err != nil {
			return "", err
		}
		if scalarAt(&root, "project_id") == id {
			return n, nil
		}
	}
	return "", nil
}
