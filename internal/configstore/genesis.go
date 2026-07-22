package configstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
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
// after an explicit project_id confirmation plus required plan-time desired and
// store-baseline fingerprint gates. Both are structured so an external bootstrap adapter can drive them by
// their machine-readable receipts.

// Genesis effect vocabulary shared by PlanProject (predicted) and ApplyProject
// (realized). A plan and an immediately-following apply of the same file report
// the same effect.
const (
	EffectCreate   = "create"
	EffectUpdate   = "update"
	EffectNoOp     = "no-op"
	EffectConflict = "conflict"
	BaselineAbsent = "absent"
)

// ErrIdentityConflict is the sentinel wrapped by ApplyProject when a row cannot
// be written because its identity conflicts with the store. It never overwrites
// on conflict; the caller stops and surfaces the reason.
var ErrIdentityConflict = errors.New("config-store identity conflict")

// PreparedProject is the validated, fingerprinted result of PrepareProject. It
// carries everything plan/apply need without re-parsing, so the fingerprint the
// operator reviewed in `plan` is exactly the one `apply` gates on.
type PreparedProject struct {
	Name           string
	ProjectID      string
	Repo           string
	LocalPath      string
	WorktreeBase   string
	ManagementHome config.ManagementHomeConfig
	Fingerprint    string
	// Canonical is the deterministic project document the fingerprint is taken
	// over (backends normalized, re-marshaled). Raw is the project-only document
	// passed to apply; it preserves the original bytes unless preparation had to
	// detach a matching model.backends block from a config-store export.
	Canonical []byte
	Raw       []byte
	Warnings  []string

	// portableBackends records definitions supplied by an exported portable
	// document. They are never written by genesis: a target-store preparation
	// accepts them only when they exactly match the fleet-shared definitions,
	// then ApplyProject re-checks that equality in its write transaction.
	portableBackends map[string]*yaml.Node
}

// ExistingRow is the identity summary of a config-store row that a plan/apply
// would touch, reported so an adapter can see why an effect was chosen.
type ExistingRow struct {
	Name        string `json:"name"`
	Repo        string `json:"repo,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

// GenesisReport is the receipt PlanProject/ApplyProject return: the store-name,
// project id, config fingerprint, effect, and whether a write happened. The CLI
// wraps it with the daemon-reconciliation expectation and next-step commands.
type GenesisReport struct {
	Project     string `json:"project"`
	ProjectID   string `json:"project_id"`
	Fingerprint string `json:"fingerprint"`
	// BaselineFingerprint is the store state observed before the effect:
	// "absent" for no row, otherwise the existing row's config fingerprint.
	// Apply requires the exact plan-time value to prevent a concurrent store
	// change from being overwritten between plan and apply.
	BaselineFingerprint string       `json:"baseline_fingerprint"`
	Effect              string       `json:"effect"`
	Conflict            string       `json:"conflict,omitempty"`
	Wrote               bool         `json:"wrote"`
	Existing            *ExistingRow `json:"existing,omitempty"`
	Warnings            []string     `json:"warnings,omitempty"`
}

// PrepareProject validates a portable project YAML for the genesis flow and
// computes its stable name + fingerprint WITHOUT touching any store. It is the
// strict gate every plan/apply passes through:
//
//   - strict decode (unknown/misspelled keys rejected by name, #869);
//   - repo required (config.Parse) — the derived project name comes from it;
//   - a valid, present project_id (the durable identity the flow is built around);
//   - local_path and worktree_base required (a genesis row must be runnable).
func PrepareProject(sourcePath string, data []byte) (*PreparedProject, error) {
	return prepareProject(sourcePath, data, nil, false)
}

// PrepareProject validates a portable project document against the shared
// backend definitions already present in this target store. This is the
// store-aware entry point used by project plan/apply for existing stores:
// project-only rows may reference shared backends without embedding them, while
// config-store exports may carry definitions only as an exact, read-only proof
// of what the target store already contains.
func (s *Store) PrepareProject(ctx context.Context, sourcePath string, data []byte) (*PreparedProject, error) {
	backends, err := s.loadBackends(ctx)
	if err != nil {
		return nil, fmt.Errorf("load target shared backends: %w", err)
	}
	return prepareProject(sourcePath, data, backends, true)
}

func prepareProject(sourcePath string, data []byte, sharedBackends map[string]*yaml.Node, storeAware bool) (*PreparedProject, error) {
	var source yaml.Node
	if err := yaml.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	portableBackends := detachBackends(&source)
	if !storeAware && len(portableBackends) > 0 {
		return nil, fmt.Errorf("portable project genesis must not define shared model.backends; configure fleet backends separately before plan/apply")
	}
	if storeAware {
		if err := validatePortableBackends(portableBackends, sharedBackends); err != nil {
			return nil, err
		}
	}

	projectData := data
	if len(portableBackends) > 0 {
		removeEmptyTopLevelMapping(&source, "model")
		var err error
		projectData, err = marshalNode(&source)
		if err != nil {
			return nil, fmt.Errorf("detach portable model.backends: %w", err)
		}
	}

	effective := cloneNode(&source)
	if storeAware {
		attachBackends(effective, sharedBackends)
	}
	effectiveData, err := marshalNode(effective)
	if err != nil {
		return nil, fmt.Errorf("attach target shared backends: %w", err)
	}
	cfg, err := config.ParseStrict(effectiveData)
	if err != nil {
		if storeAware && missingSharedBackendError(err) {
			return nil, fmt.Errorf("target config store is missing a shared backend required by this project; configure it at fleet scope before plan/apply: %w", err)
		}
		return nil, err
	}
	id := strings.TrimSpace(cfg.ProjectID)
	if id == "" {
		return nil, fmt.Errorf("project plan/apply requires a stable project_id (a canonical 8-4-4-4-12 UUID); none is set — generate one and add `project_id:` to the config (#869)")
	}
	if id != strings.ToLower(id) {
		return nil, fmt.Errorf("project_id must use canonical lowercase UUID form; got %q", id)
	}
	repo := strings.TrimSpace(cfg.Repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, "\\ ") {
		return nil, fmt.Errorf("repo must use the canonical owner/repository form; got %q", repo)
	}
	if strings.TrimSpace(cfg.LocalPath) == "" {
		return nil, fmt.Errorf("local_path is required for project plan/apply (the on-disk clone the daemon works from)")
	}
	if strings.TrimSpace(cfg.WorktreeBase) == "" {
		return nil, fmt.Errorf("worktree_base is required for project plan/apply (the base dir worker worktrees are created under)")
	}
	if !filepath.IsAbs(cfg.LocalPath) || !filepath.IsAbs(cfg.WorktreeBase) {
		return nil, fmt.Errorf("local_path and worktree_base must resolve to absolute execution-host paths")
	}
	if !cfg.ManagementHome.Configured() {
		return nil, fmt.Errorf("management_home is required for project genesis so the durable row remains linked to its PM/control-room home (#869)")
	}
	if !filepath.IsAbs(strings.TrimSpace(cfg.ManagementHome.Path)) {
		return nil, fmt.Errorf("management_home.path must be an absolute execution-host path")
	}
	name, err := ProjectNameFor(sourcePath, data)
	if err != nil {
		return nil, err
	}
	canonical, fp, err := canonicalProjectDoc(data)
	if err != nil {
		return nil, fmt.Errorf("canonicalize config: %w", err)
	}
	return &PreparedProject{
		Name:           name,
		ProjectID:      id,
		Repo:           cfg.Repo,
		LocalPath:      cfg.LocalPath,
		WorktreeBase:   cfg.WorktreeBase,
		ManagementHome: cfg.ManagementHome,
		Fingerprint:    fp,
		Canonical:      canonical,
		Raw:            projectData,
		// Surface config load-time warnings on the genesis receipt so an
		// operator sees them at plan/apply. This is where the #872 delivery
		// deprecation nudges a legacy deploy_cmd (or an explicit
		// delivery.mode: automatic) toward the safe approval_required default —
		// new genesis output that names a delivery command defaults to
		// approval_required, and an unattended automatic mode is called out.
		Warnings:         cfg.Warnings(),
		portableBackends: cloneBackendMap(portableBackends),
	}, nil
}

func validatePortableBackends(portable, shared map[string]*yaml.Node) error {
	names := make([]string, 0, len(portable))
	for name := range portable {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		portableDef := portable[name]
		sharedDef, ok := shared[name]
		if !ok {
			return fmt.Errorf("portable model.backends.%s is not defined in the target config store; configure that fleet backend before plan/apply", name)
		}
		equal, err := backendDefinitionsEqual(portableDef, sharedDef)
		if err != nil {
			return fmt.Errorf("compare portable model.backends.%s with target store: %w", name, err)
		}
		if !equal {
			return fmt.Errorf("portable model.backends.%s diverges from the target config store; shared backend values cannot be redefined by project genesis (update the fleet backend separately or export the project again)", name)
		}
	}
	return nil
}

func backendDefinitionsEqual(a, b *yaml.Node) (bool, error) {
	left := cloneNode(a)
	right := cloneNode(b)
	normalizeYAMLPresentation(left)
	normalizeYAMLPresentation(right)
	sortMappingKeys(left)
	sortMappingKeys(right)
	leftYAML, err := marshalNode(left)
	if err != nil {
		return false, err
	}
	rightYAML, err := marshalNode(right)
	if err != nil {
		return false, err
	}
	return string(leftYAML) == string(rightYAML), nil
}

func cloneBackendMap(in map[string]*yaml.Node) map[string]*yaml.Node {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*yaml.Node, len(in))
	for name, def := range in {
		out[name] = cloneNode(def)
	}
	return out
}

func missingSharedBackendError(err error) bool {
	message := err.Error()
	return strings.Contains(message, "model.backends") &&
		(strings.Contains(message, "not defined") || strings.Contains(message, "not declared"))
}

// canonicalProjectDoc returns the deterministic project document and its
// fingerprint. It normalizes so that reformatting-only edits (mapping-key
// reordering, whitespace) do NOT move the fingerprint, while any real change to a
// value or a list DOES:
//
//   - fleet-shared backends are detached, syntax-only presentation is removed,
//     then every mapping's keys are sorted recursively;
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
	// Backends live in the fleet-shared table and ExportProject attaches every
	// shared definition to every row. Genesis forbids defining them above, and
	// excludes attached definitions here, so a second identical apply remains a
	// no-op regardless of unrelated fleet backend changes.
	detachBackends(&root)
	removeEmptyTopLevelMapping(&root, "model")
	normalizeYAMLPresentation(&root)
	sortMappingKeys(&root)
	out, err := marshalNode(&root)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(out)
	return out, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// normalizeYAMLPresentation removes syntax-only details that the config decoder
// does not treat as state. Comments, flow/block layout, and scalar quoting must
// not invalidate an approved plan when the effective values are unchanged.
func normalizeYAMLPresentation(node *yaml.Node) {
	if node == nil {
		return
	}
	node.HeadComment = ""
	node.LineComment = ""
	node.FootComment = ""
	node.Style = 0
	for _, child := range node.Content {
		normalizeYAMLPresentation(child)
	}
}

func removeEmptyTopLevelMapping(root *yaml.Node, key string) {
	doc := documentValue(root)
	if doc == nil || doc.Kind != yaml.MappingNode {
		return
	}
	idx := mappingKeyIndex(doc, key)
	if idx < 0 || doc.Content[idx+1].Kind != yaml.MappingNode || len(doc.Content[idx+1].Content) != 0 {
		return
	}
	doc.Content = append(doc.Content[:idx], doc.Content[idx+2:]...)
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
		Project:             p.Name,
		ProjectID:           p.ProjectID,
		Fingerprint:         p.Fingerprint,
		BaselineFingerprint: BaselineAbsent,
		Effect:              EffectCreate,
		Wrote:               false,
		Warnings:            p.Warnings,
	}
}

// PlanProject previews what applying p would do against the store WITHOUT any
// write. It returns the effect (create/update/no-op/conflict), the existing row
// it compared against, and any validation warnings. It only issues reads, so a
// caller can compare the store fingerprint before and after and see it unchanged.
func (s *Store) PlanProject(ctx context.Context, p *PreparedProject) (*GenesisReport, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	effect, conflict, existing, err := evaluateProjectTx(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	return &GenesisReport{
		Project:             p.Name,
		ProjectID:           p.ProjectID,
		Fingerprint:         p.Fingerprint,
		BaselineFingerprint: baselineFingerprint(existing),
		Effect:              effect,
		Conflict:            conflict,
		Wrote:               false,
		Existing:            existing,
		Warnings:            p.Warnings,
	}, nil
}

// ApplyProject upserts p into the store idempotently, after gating on:
//
//   - confirm: must exactly equal p.ProjectID (an explicit operator ack that this
//     durable identity is the one being written);
//   - wantFingerprint: the desired-config fingerprint the plan reported — a mismatch
//     means the file changed since the plan was reviewed, so the write is refused.
//   - wantBaseline: the row fingerprint (or "absent") observed by plan — a
//     mismatch means the store changed concurrently, so the write is refused.
//
// A create/update performs exactly one UpsertProject; a no-op writes nothing; an
// identity conflict returns the report plus ErrIdentityConflict and never
// overwrites. Removal/rollback is deliberately NOT handled here — it stays a
// separate explicit operator action.
func (s *Store) ApplyProject(ctx context.Context, p *PreparedProject, confirm, wantFingerprint, wantBaseline string) (*GenesisReport, error) {
	if strings.TrimSpace(confirm) == "" {
		return nil, fmt.Errorf("--confirm is required: pass the exact project_id (%s) to apply", p.ProjectID)
	}
	if strings.TrimSpace(confirm) != p.ProjectID {
		return nil, fmt.Errorf("--confirm %q does not match the config project_id %q; refusing to apply", strings.TrimSpace(confirm), p.ProjectID)
	}
	wf := strings.TrimSpace(wantFingerprint)
	if wf == "" {
		return nil, fmt.Errorf("--fingerprint is required: pass the exact fingerprint returned by `maestro project plan`")
	}
	if wf != p.Fingerprint {
		return nil, fmt.Errorf("config changed since plan (approved fingerprint %s, current %s); re-run `maestro project plan` and review before applying", wf, p.Fingerprint)
	}
	wb := strings.TrimSpace(wantBaseline)
	if wb == "" {
		return nil, fmt.Errorf("--baseline is required: pass the exact baseline_fingerprint returned by `maestro project plan`")
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	effect, conflict, existing, err := evaluateProjectTx(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	report := &GenesisReport{
		Project:             p.Name,
		ProjectID:           p.ProjectID,
		Fingerprint:         p.Fingerprint,
		BaselineFingerprint: baselineFingerprint(existing),
		Effect:              effect,
		Conflict:            conflict,
		Existing:            existing,
		Warnings:            p.Warnings,
	}
	switch effect {
	case EffectConflict:
		return report, fmt.Errorf("%w: %s", ErrIdentityConflict, conflict)
	case EffectNoOp:
		// Desired state is already present. Accept even a stale plan baseline so
		// retrying the exact approved command after a lost response is genuinely
		// idempotent; identity and desired-fingerprint gates already matched.
		report.Wrote = false
		return report, nil
	case EffectCreate, EffectUpdate:
		if wb != report.BaselineFingerprint {
			return report, fmt.Errorf("config store changed since plan (approved baseline %s, current %s); re-run `maestro project plan` and review before applying", wb, report.BaselineFingerprint)
		}
		if err := upsertPreparedProjectTx(ctx, tx, p.Name, string(p.Raw)); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		report.Wrote = true
		return report, nil
	default:
		return report, fmt.Errorf("internal: unknown genesis effect %q", effect)
	}
}

func evaluateProjectTx(ctx context.Context, tx *sql.Tx, p *PreparedProject) (effect, conflict string, existing *ExistingRow, err error) {
	if err := validatePreparedProjectTx(ctx, tx, p); err != nil {
		return "", "", nil, err
	}
	row, err := existingRowTx(ctx, tx, p.Name)
	if err != nil {
		return "", "", nil, err
	}
	other, err := projectNameByIDTx(ctx, tx, p.ProjectID, p.Name)
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
	if !strings.EqualFold(strings.TrimSpace(row.Repo), strings.TrimSpace(p.Repo)) {
		return EffectConflict,
			fmt.Sprintf("project %q already exists for repo %q, not incoming repo %q; refusing basename adoption", p.Name, row.Repo, p.Repo),
			row, nil
	}
	if row.ProjectID != "" && !strings.EqualFold(row.ProjectID, p.ProjectID) {
		return EffectConflict,
			fmt.Sprintf("project %q already exists with a different project_id (stored %s, incoming %s); refusing to overwrite by name", p.Name, row.ProjectID, p.ProjectID),
			row, nil
	}
	if row.Fingerprint == p.Fingerprint {
		return EffectNoOp, "", row, nil
	}
	return EffectUpdate, "", row, nil
}

func validatePreparedProjectTx(ctx context.Context, tx *sql.Tx, p *PreparedProject) error {
	backends, err := loadBackendsTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("load target shared backends: %w", err)
	}
	if err := validatePortableBackends(p.portableBackends, backends); err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(p.Raw, &root); err != nil {
		return err
	}
	attachBackends(&root, backends)
	effective, err := marshalNode(&root)
	if err != nil {
		return err
	}
	if _, err := config.ParseStrict(effective); err != nil {
		if missingSharedBackendError(err) {
			return fmt.Errorf("target config store is missing a shared backend required by this project; configure it at fleet scope before plan/apply: %w", err)
		}
		return err
	}
	return nil
}

func loadBackendsTx(ctx context.Context, tx *sql.Tx) (map[string]*yaml.Node, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, definition_yaml FROM backends ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*yaml.Node{}
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(data), &doc); err != nil {
			return nil, fmt.Errorf("parse backend %s: %w", name, err)
		}
		out[name] = documentValue(&doc)
	}
	return out, rows.Err()
}

func existingRowTx(ctx context.Context, tx *sql.Tx, name string) (*ExistingRow, error) {
	var data string
	err := tx.QueryRowContext(ctx, `SELECT config_yaml FROM project WHERE name = ?`, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, fp, err := canonicalProjectDoc([]byte(data))
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(data), &root); err != nil {
		return nil, err
	}
	return &ExistingRow{Name: name, Repo: scalarAt(&root, "repo"), ProjectID: scalarAt(&root, "project_id"), Fingerprint: fp}, nil
}

func projectNameByIDTx(ctx context.Context, tx *sql.Tx, id, exclude string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, config_yaml FROM project WHERE name <> ? ORDER BY name`, exclude)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			return "", err
		}
		var root yaml.Node
		if err := yaml.Unmarshal([]byte(data), &root); err != nil {
			return "", err
		}
		if strings.EqualFold(strings.TrimSpace(scalarAt(&root, "project_id")), strings.TrimSpace(id)) {
			return name, nil
		}
	}
	return "", rows.Err()
}

func baselineFingerprint(existing *ExistingRow) string {
	if existing == nil {
		return BaselineAbsent
	}
	return existing.Fingerprint
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
	if !strings.EqualFold(strings.TrimSpace(row.Repo), strings.TrimSpace(p.Repo)) {
		return EffectConflict,
			fmt.Sprintf("project %q already exists for repo %q, not incoming repo %q; refusing basename adoption", p.Name, row.Repo, p.Repo),
			row, nil
	}
	if row.ProjectID != "" && !strings.EqualFold(row.ProjectID, p.ProjectID) {
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
	return &ExistingRow{Name: name, Repo: scalarAt(&root, "repo"), ProjectID: scalarAt(&root, "project_id"), Fingerprint: fp}, nil
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
		if strings.EqualFold(strings.TrimSpace(scalarAt(&root, "project_id")), id) {
			return n, nil
		}
	}
	return "", nil
}
