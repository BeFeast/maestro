package configstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

const Schema = `
CREATE TABLE IF NOT EXISTS global (
	key TEXT PRIMARY KEY,
	value_yaml TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS backends (
	name TEXT PRIMARY KEY,
	definition_yaml TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS project (
	name TEXT PRIMARY KEY,
	source_path TEXT NOT NULL,
	config_yaml TEXT NOT NULL,
	backend_ref TEXT NOT NULL DEFAULT 'global',
	updated_at TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// busy_timeout makes a connection wait (up to 5s) for a lock instead of
	// erroring with "database is locked" immediately: the daemon's watch-loop
	// reads config.db (ProjectsFingerprint every tick, Load on add) while
	// `config-store add/rm/edit` writes it from a SEPARATE process (#757). WAL
	// lets a reader and a writer proceed concurrently across processes. The
	// pragmas go in the DSN so modernc applies them to every pooled connection.
	dsn := path
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Serialize in-process access through a single connection — modernc/sqlite
	// can still report SQLITE_BUSY between two pooled connections of the same
	// process; one connection plus busy_timeout removes that contention while
	// WAL handles the cross-process case.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.Init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, Schema)
	return err
}

func (s *Store) ImportDir(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read config dir %s: %w", dir, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := importProject(ctx, tx, path, data); err != nil {
			return fmt.Errorf("import %s: %w", path, err)
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no config files found in %s", dir)
	}
	return tx.Commit()
}

// SourcePathPrefix marks a config's SourcePath as a config-store row
// reference ("store:<name>") rather than a file path. Rows created through
// the write API (UpsertProject) have no originating file, and an empty
// SourcePath makes ResolvePath fall back to a literal "maestro.yaml" — which
// the fleet snapshot then reports as a vestigial config_path for a file that
// does not exist post-#761 (#801).
const SourcePathPrefix = "store:"

func (s *Store) Load(ctx context.Context, name string) (*config.Config, error) {
	data, sourcePath, err := s.exportProjectYAML(ctx, name)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sourcePath) == "" {
		// No originating file (write-API row): report the store row itself so
		// path consumers surface "store:<name>" instead of inventing a
		// maestro.yaml that does not exist (#801).
		sourcePath = SourcePathPrefix + name
	}
	cfg.SourcePath = sourcePath
	return cfg, nil
}

func (s *Store) LoadAll(ctx context.Context) ([]*config.Config, error) {
	names, err := s.projectNames(ctx)
	if err != nil {
		return nil, err
	}
	cfgs := make([]*config.Config, 0, len(names))
	for _, name := range names {
		cfg, err := s.Load(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("load project %s: %w", name, err)
		}
		cfgs = append(cfgs, cfg)
	}
	if len(cfgs) == 0 {
		return nil, errors.New("no projects in config store")
	}
	return cfgs, nil
}

func (s *Store) ExportDir(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create export dir %s: %w", dir, err)
	}
	names, err := s.projectNames(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		data, _, err := s.exportProjectYAML(ctx, name)
		if err != nil {
			return fmt.Errorf("export project %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), data, 0644); err != nil {
			return fmt.Errorf("write export %s: %w", name, err)
		}
	}
	return nil
}

func importProject(ctx context.Context, tx *sql.Tx, sourcePath string, data []byte) error {
	if _, err := config.Parse(data); err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	projectName := projectNameFromPath(sourcePath)
	if repo := scalarAt(&root, "repo"); repo != "" {
		projectName = safeProjectName(repo)
	}
	backends := detachBackends(&root)
	now := time.Now().UTC().Format(time.RFC3339Nano) // nano, not seconds: see ProjectsFingerprint (#757)
	if err := upsertSharedBackendsTx(ctx, tx, backends, now); err != nil {
		return err
	}
	projectYAML, err := marshalNode(&root)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO project(name, source_path, config_yaml, backend_ref, updated_at)
VALUES(?, ?, ?, 'global', ?)
ON CONFLICT(name) DO UPDATE SET source_path = excluded.source_path, config_yaml = excluded.config_yaml, updated_at = excluded.updated_at
`, projectName, sourcePath, string(projectYAML), now)
	return err
}

// upsertSharedBackendsTx inserts/updates the shared backend definitions
// detached from a project's YAML. Two projects may define the same backend
// with an identical cmd but different optional metadata (provider / model /
// pricing / effort) — that is not a real conflict, so the definitions are
// merged (mergeBackendDefinitions, richest wins). Only a genuinely different
// cmd is rejected, since the cmd is what dispatches the worker.
func upsertSharedBackendsTx(ctx context.Context, tx *sql.Tx, backends map[string]*yaml.Node, now string) error {
	for name, def := range backends {
		defYAML, err := marshalNode(def)
		if err != nil {
			return err
		}
		toStore := string(defYAML)
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT definition_yaml FROM backends WHERE name = ?`, name).Scan(&existing)
		switch {
		case err == nil:
			merged, mErr := mergeBackendDefinitions(name, existing, toStore)
			if mErr != nil {
				return mErr
			}
			toStore = merged
		case errors.Is(err, sql.ErrNoRows):
			// first definition for this backend name — store as-is
		default:
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO backends(name, definition_yaml, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(name) DO UPDATE SET definition_yaml = excluded.definition_yaml, updated_at = excluded.updated_at
`, name, toStore, now); err != nil {
			return err
		}
	}
	return nil
}

// descriptiveBackendFields are the BackendDef fields that are attribution / cost
// display metadata only (#513 provider/model/variant/effort, #619 pricing) — they
// do not affect how the worker is dispatched, so two projects sharing a backend
// name may carry different values and they get merged. EVERY other field (cmd,
// extra_args, prompt_mode, enabled, mcp, usage_stream, non_agentic, quota,
// subagent_hint) is execution-affecting: a difference there is a real conflict,
// because the store keeps ONE global backend per name and re-attaches it to every
// project, so a silent overlay would make one project run with another's runtime
// settings.
var descriptiveBackendFields = map[string]bool{
	"provider": true,
	"model":    true,
	"variant":  true,
	"effort":   true,
	"pricing":  true,
}

// backendEffectiveCmd resolves the dispatch command: an empty cmd defaults to the
// backend name for built-in backends (claude/codex/… BuildCmd), so `claude: {}`
// and `claude: {cmd: claude}` are the same command, while `claude: {cmd: x}` is
// not.
func backendEffectiveCmd(def map[string]any, name string) string {
	if c, _ := def["cmd"].(string); strings.TrimSpace(c) != "" {
		return strings.TrimSpace(c)
	}
	return name
}

// behavioralCore returns the execution-affecting subset of a backend definition
// (everything except descriptive metadata and cmd, which is compared separately
// via its effective value).
func behavioralCore(def map[string]any) map[string]any {
	core := make(map[string]any, len(def))
	for k, v := range def {
		if k == "cmd" || descriptiveBackendFields[k] {
			continue
		}
		core[k] = v
	}
	return core
}

// mergeBackendDefinitions reconciles two definitions of the same shared backend.
// The store keeps backends in one global table, but per-project configs often
// annotate the same backend with different DESCRIPTIVE metadata (provider / model
// / variant / effort / pricing) while keeping identical execution settings — not
// a real conflict. So when the effective cmd and every other execution-affecting
// field agree, the definitions are merged into their union (descriptive metadata
// deep-merged, so e.g. complementary pricing fields converge to the superset).
// A differing cmd, or any differing behavioral field, is rejected. The merge is
// order-independent and idempotent, so a bulk `config-store migrate` converges
// regardless of file order.
func mergeBackendDefinitions(name, existingYAML, incomingYAML string) (string, error) {
	if strings.TrimSpace(existingYAML) == strings.TrimSpace(incomingYAML) {
		return existingYAML, nil
	}
	var existing, incoming map[string]any
	if err := yaml.Unmarshal([]byte(existingYAML), &existing); err != nil {
		return "", fmt.Errorf("backend %q: parse stored definition: %w", name, err)
	}
	if err := yaml.Unmarshal([]byte(incomingYAML), &incoming); err != nil {
		return "", fmt.Errorf("backend %q: parse incoming definition: %w", name, err)
	}
	existingCmd := backendEffectiveCmd(existing, name)
	incomingCmd := backendEffectiveCmd(incoming, name)
	if existingCmd != incomingCmd {
		return "", fmt.Errorf("backend %q has a conflicting cmd (%q vs %q) — projects cannot share a backend name with different commands", name, existingCmd, incomingCmd)
	}
	if !reflect.DeepEqual(behavioralCore(existing), behavioralCore(incoming)) {
		return "", fmt.Errorf("backend %q has conflicting execution settings (extra_args/prompt_mode/enabled/mcp/usage_stream/non_agentic/quota/subagent_hint) — only descriptive metadata (provider/model/variant/effort/pricing) may differ between projects sharing a backend", name)
	}
	merged := deepMergeMaps(existing, incoming)
	// Keep an explicit cmd if either side had one (the effective values already
	// match), so an omitted-cmd side never erases the spelled-out command.
	if ec, _ := existing["cmd"].(string); strings.TrimSpace(ec) != "" {
		merged["cmd"] = ec
	} else if ic, _ := incoming["cmd"].(string); strings.TrimSpace(ic) != "" {
		merged["cmd"] = ic
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("backend %q: marshal merged definition: %w", name, err)
	}
	return string(out), nil
}

// deepMergeMaps returns the recursive union of a and b. On a scalar-vs-scalar
// overlap b wins; on a map-vs-map overlap the maps are merged recursively (so a
// nested pricing map keeps both sides' fields instead of one overwriting the
// other).
func deepMergeMaps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if existing, ok := out[k]; ok {
			em, eok := existing.(map[string]any)
			vm, vok := v.(map[string]any)
			if eok && vok {
				out[k] = deepMergeMaps(em, vm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// UpsertProject writes a single project's config under an explicit name,
// mirroring importProject: the YAML is validated through config.Parse, any
// model.backends block is detached into the shared backends table, and the
// remaining project document is stored INSERT ... ON CONFLICT. The whole
// write runs in one transaction so a divergent shared backend rolls the
// project write back too. source_path is left empty for write-API projects
// (they have no originating file) and is preserved on conflict.
func (s *Store) UpsertProject(ctx context.Context, name, configYAML string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("project name is required")
	}
	if _, err := config.Parse([]byte(configYAML)); err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(configYAML), &root); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	backends := detachBackends(&root)
	now := time.Now().UTC().Format(time.RFC3339Nano) // nano, not seconds: see ProjectsFingerprint (#757)
	if err := upsertSharedBackendsTx(ctx, tx, backends, now); err != nil {
		return err
	}
	projectYAML, err := marshalNode(&root)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO project(name, source_path, config_yaml, backend_ref, updated_at)
VALUES(?, '', ?, 'global', ?)
ON CONFLICT(name) DO UPDATE SET config_yaml = excluded.config_yaml, updated_at = excluded.updated_at
`, name, string(projectYAML), now); err != nil {
		return err
	}
	return tx.Commit()
}

// ProjectNameFor derives the canonical project name for a config document the
// same way ImportDir does: the sanitised `repo` field when present, else the
// source file's basename. Exposed so `config-store add --file` names a single
// file identically to a bulk `config-store migrate` (#757).
func ProjectNameFor(sourcePath string, data []byte) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", err
	}
	name := projectNameFromPath(sourcePath)
	if repo := scalarAt(&root, "repo"); repo != "" {
		name = safeProjectName(repo)
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("cannot derive a project name from %q (set repo or pass --name)", sourcePath)
	}
	return name, nil
}

// ExportProject returns one project's portable YAML — the stored project
// document with its shared backend definitions re-attached, identical to a
// single file written by ExportDir. Exposed for `config-store edit`, which
// round-trips a project through $EDITOR (#757).
func (s *Store) ExportProject(ctx context.Context, name string) ([]byte, error) {
	data, _, err := s.exportProjectYAML(ctx, name)
	return data, err
}

// DeleteProject removes a project row. Deleting a missing project is a no-op
// (no error), matching idempotent delete semantics. Shared backend rows are
// left intact: they may still be referenced by other projects.
func (s *Store) DeleteProject(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("project name is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM project WHERE name = ?`, name)
	return err
}

// UpsertBackend writes a shared backend definition directly (write-API).
// Unlike the import path it overwrites an existing definition rather than
// rejecting divergence — the caller is making an explicit edit. definitionYAML
// is validated only as well-formed YAML; backend-shape validation happens when
// a project that references it is parsed.
func (s *Store) UpsertBackend(ctx context.Context, name, definitionYAML string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("backend name is required")
	}
	var probe yaml.Node
	if err := yaml.Unmarshal([]byte(definitionYAML), &probe); err != nil {
		return fmt.Errorf("parse backend %q: %w", name, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano) // nano, not seconds: see ProjectsFingerprint (#757)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO backends(name, definition_yaml, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(name) DO UPDATE SET definition_yaml = excluded.definition_yaml, updated_at = excluded.updated_at
`, name, definitionYAML, now)
	return err
}

// SetGlobal writes a global key/value (write-API). valueYAML is validated only
// as well-formed YAML.
func (s *Store) SetGlobal(ctx context.Context, key, valueYAML string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("global key is required")
	}
	var probe yaml.Node
	if err := yaml.Unmarshal([]byte(valueYAML), &probe); err != nil {
		return fmt.Errorf("parse global %q: %w", key, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano) // nano, not seconds: see ProjectsFingerprint (#757)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO global(key, value_yaml, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value_yaml = excluded.value_yaml, updated_at = excluded.updated_at
`, key, valueYAML, now)
	return err
}

// ProjectsFingerprint returns each project's name mapped to its updated_at
// timestamp. Callers (the daemon diff-loop and configwatch.WatchStore) use it
// to detect which projects changed since a prior snapshot without loading every
// config. updated_at is written at RFC3339Nano precision so two writes in the
// same second do not compare equal — otherwise a config-store edit immediately
// after add/migrate would never advance the fingerprint and a --watch-store
// daemon would never hot-reload that project (#757). Parsing with RFC3339Nano
// also accepts older seconds-precision rows.
func (s *Store) ProjectsFingerprint(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, updated_at FROM project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var name, updatedAt string
		if err := rows.Scan(&name, &updatedAt); err != nil {
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse updated_at for project %q: %w", name, err)
		}
		out[name] = ts
	}
	return out, rows.Err()
}

func (s *Store) exportProjectYAML(ctx context.Context, name string) ([]byte, string, error) {
	var projectYAML, sourcePath string
	err := s.db.QueryRowContext(ctx, `SELECT config_yaml, source_path FROM project WHERE name = ?`, name).Scan(&projectYAML, &sourcePath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("project %q not found in config store", name)
	}
	if err != nil {
		return nil, "", err
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(projectYAML), &root); err != nil {
		return nil, "", err
	}
	backends, err := s.loadBackends(ctx)
	if err != nil {
		return nil, "", err
	}
	attachBackends(&root, backends)
	data, err := marshalNode(&root)
	return data, sourcePath, err
}

func (s *Store) loadBackends(ctx context.Context) (map[string]*yaml.Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, definition_yaml FROM backends ORDER BY name`)
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

func (s *Store) projectNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM project ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func detachBackends(root *yaml.Node) map[string]*yaml.Node {
	model := mappingChild(root, "model", true)
	if model == nil {
		return nil
	}
	idx := mappingKeyIndex(model, "backends")
	if idx < 0 {
		return nil
	}
	backendsNode := model.Content[idx+1]
	out := map[string]*yaml.Node{}
	if backendsNode.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(backendsNode.Content); i += 2 {
			out[backendsNode.Content[i].Value] = backendsNode.Content[i+1]
		}
	}
	model.Content = append(model.Content[:idx], model.Content[idx+2:]...)
	return out
}

func attachBackends(root *yaml.Node, backends map[string]*yaml.Node) {
	if len(backends) == 0 {
		return
	}
	model := mappingChild(root, "model", true)
	if model == nil {
		model = &yaml.Node{Kind: yaml.MappingNode}
		setMappingChild(root, "model", model)
	}
	backendsNode := &yaml.Node{Kind: yaml.MappingNode}
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		backendsNode.Content = append(backendsNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, cloneNode(backends[name]))
	}
	setMappingChild(model, "backends", backendsNode)
}

func mappingChild(root *yaml.Node, key string, unwrap bool) *yaml.Node {
	node := root
	if unwrap {
		node = documentValue(root)
	}
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	idx := mappingKeyIndex(node, key)
	if idx < 0 {
		return nil
	}
	return node.Content[idx+1]
}

func setMappingChild(root *yaml.Node, key string, value *yaml.Node) {
	node := documentValue(root)
	if node == nil {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		node = root.Content[0]
	}
	idx := mappingKeyIndex(node, key)
	if idx >= 0 {
		node.Content[idx+1] = value
		return
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func mappingKeyIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func scalarAt(root *yaml.Node, key string) string {
	n := mappingChild(root, key, true)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(n.Value)
}

func documentValue(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

func marshalNode(node *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func cloneNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	if len(node.Content) > 0 {
		clone.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			clone.Content[i] = cloneNode(child)
		}
	}
	return &clone
}

func projectNameFromPath(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return safeProjectName(base)
}

func safeProjectName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "maestro-")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.ToLower(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
