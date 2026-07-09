package configstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"gopkg.in/yaml.v3"
)

// scopeFleet labels a settings_audit row for a fleet-level default; a
// per-project override is scoped "project:<name>". Only fleet-scope rows feed
// ProjectsFingerprint (they change every project's effective config); a project
// override already advances that project's own row.
const scopeFleet = "fleet"

func scopeProject(name string) string { return "project:" + name }

// SettingAudit is one row of the settings-change journal (#839): who changed
// which key, in which scope, from what to what, and when (RFC3339Nano).
type SettingAudit struct {
	Key       string
	Scope     string
	OldValue  string
	NewValue  string
	Actor     string
	ChangedAt string
}

// FleetSettings returns the fleet-level default layer: dotted key -> stored
// value. Empty when no fleet default has been set.
func (s *Store) FleetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

// SetFleetSetting writes (or replaces) a fleet-level default for key and
// journals the change. key must be a known fleet setting and value must match
// its kind (config.NormalizeSettingValue). actor is the RFC3339-journaled
// identity of who made the change. The settings row and its audit entry are
// written in one transaction.
func (s *Store) SetFleetSetting(ctx context.Context, key, value, actor string) error {
	norm, err := config.NormalizeSettingValue(key, value)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	old, _, err := fleetSettingTx(ctx, tx, key)
	if err != nil {
		return err
	}
	changedAt, err := writeSettingsAuditTx(ctx, tx, key, scopeFleet, old, norm, actor)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings(key, value, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, key, norm, changedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFleetSetting clears a fleet-level default (reverting affected projects
// to the built-in value) and journals the change. Clearing an unset key is a
// no-op — no row and no audit entry — so it does not spuriously advance the
// fingerprint.
func (s *Store) DeleteFleetSetting(ctx context.Context, key, actor string) error {
	if _, ok := config.FleetSettingSpecByKey(key); !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	old, present, err := fleetSettingTx(ctx, tx, key)
	if err != nil {
		return err
	}
	if !present {
		return tx.Commit()
	}
	if _, err := writeSettingsAuditTx(ctx, tx, key, scopeFleet, old, "", actor); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return err
	}
	return tx.Commit()
}

// SetProjectSetting writes a per-project override for key by editing the
// project's stored YAML (project value wins over fleet default) and journals
// the change under scope project:<name>. The project row upsert and the audit
// entry share one transaction, and the row's updated_at advances so
// --watch-store reloads just that project.
func (s *Store) SetProjectSetting(ctx context.Context, project, key, value, actor string) error {
	spec, ok := config.FleetSettingSpecByKey(key)
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	norm, err := config.NormalizeSettingValue(key, value)
	if err != nil {
		return err
	}
	// Start from the project's portable YAML (backends re-attached) so
	// upsertProjectTx re-validates exactly what it stores.
	exported, err := s.ExportProject(ctx, project)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(exported, &root); err != nil {
		return err
	}
	old := scalarValueAtPath(&root, spec.YAMLPath)
	setNodeAtPath(&root, spec.YAMLPath, scalarNodeForKind(spec.Kind, norm))
	edited, err := marshalNode(&root)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertProjectTx(ctx, tx, project, string(edited)); err != nil {
		return err
	}
	if _, err := writeSettingsAuditTx(ctx, tx, key, scopeProject(project), old, norm, actor); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteProjectSetting removes a per-project override for key (reverting that
// project to the fleet default, else the built-in) and journals the change.
func (s *Store) DeleteProjectSetting(ctx context.Context, project, key, actor string) error {
	spec, ok := config.FleetSettingSpecByKey(key)
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	exported, err := s.ExportProject(ctx, project)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(exported, &root); err != nil {
		return err
	}
	old := scalarValueAtPath(&root, spec.YAMLPath)
	if old == "" && nodeAtPath(&root, spec.YAMLPath) == nil {
		return nil // nothing to clear
	}
	deleteNodeAtPath(&root, spec.YAMLPath)
	edited, err := marshalNode(&root)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertProjectTx(ctx, tx, project, string(edited)); err != nil {
		return err
	}
	if _, err := writeSettingsAuditTx(ctx, tx, key, scopeProject(project), old, "", actor); err != nil {
		return err
	}
	return tx.Commit()
}

// SettingsAudit returns the most-recent settings changes, newest first. A
// non-positive limit returns the whole journal.
func (s *Store) SettingsAudit(ctx context.Context, limit int) ([]SettingAudit, error) {
	query := `SELECT key, scope, old_value, new_value, actor, changed_at FROM settings_audit ORDER BY id DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettingAudit
	for rows.Next() {
		var a SettingAudit
		if err := rows.Scan(&a.Key, &a.Scope, &a.OldValue, &a.NewValue, &a.Actor, &a.ChangedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// latestFleetSettingsChange returns the changed_at of the newest fleet-scope
// audit row, or the zero time if there are none. It orders by the autoincrement
// id (insert order), not by MAX(changed_at): RFC3339Nano strings do not sort
// lexically once fractional-second widths differ, so ORDER BY id is the only
// reliable "most recent".
func (s *Store) latestFleetSettingsChange(ctx context.Context) (time.Time, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT changed_at FROM settings_audit WHERE scope = ? ORDER BY id DESC LIMIT 1`, scopeFleet).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse settings_audit changed_at %q: %w", raw, err)
	}
	return ts, nil
}

// writeSettingsAuditTx appends one journal row and returns its changed_at. The
// timestamp is strictly greater than any prior audit row's, so the fleet
// settings-change clock is monotonic even for two writes within the same
// nanosecond tick — the property ProjectsFingerprint relies on to guarantee a
// reload after a delete/revert.
func writeSettingsAuditTx(ctx context.Context, tx *sql.Tx, key, scope, oldValue, newValue, actor string) (string, error) {
	now := time.Now().UTC()
	var lastRaw string
	err := tx.QueryRowContext(ctx, `SELECT changed_at FROM settings_audit ORDER BY id DESC LIMIT 1`).Scan(&lastRaw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// first audit row
	case err != nil:
		return "", err
	default:
		if last, perr := time.Parse(time.RFC3339Nano, lastRaw); perr == nil && !now.After(last) {
			now = last.Add(time.Nanosecond)
		}
	}
	changedAt := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings_audit(key, scope, old_value, new_value, actor, changed_at)
VALUES(?, ?, ?, ?, ?, ?)
`, key, scope, oldValue, newValue, actor, changedAt); err != nil {
		return "", err
	}
	return changedAt, nil
}

func fleetSettingTx(ctx context.Context, tx *sql.Tx, key string) (value string, present bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// applyFleetDefaults folds the fleet-level defaults into a project's YAML and
// returns the merged document plus the per-key source map. Precedence per key:
// a value already present in the project YAML wins (source "project"); else a
// fleet default is injected (source "fleet"); else the built-in default is left
// to config.Parse (source "builtin"). Only registered fleet keys are touched,
// so an unknown settings row can never inject arbitrary YAML.
func applyFleetDefaults(projectYAML []byte, fleet map[string]string) ([]byte, map[string]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(projectYAML, &root); err != nil {
		return nil, nil, err
	}
	sources := make(map[string]string, len(config.FleetSettingSpecs()))
	changed := false
	for _, spec := range config.FleetSettingSpecs() {
		if nodeAtPath(&root, spec.YAMLPath) != nil {
			sources[spec.Key] = config.SettingSourceProject
			continue
		}
		if v, ok := fleet[spec.Key]; ok {
			setNodeAtPath(&root, spec.YAMLPath, scalarNodeForKind(spec.Kind, v))
			sources[spec.Key] = config.SettingSourceFleet
			changed = true
			continue
		}
		sources[spec.Key] = config.SettingSourceBuiltin
	}
	if !changed {
		return projectYAML, sources, nil
	}
	merged, err := marshalNode(&root)
	if err != nil {
		return nil, nil, err
	}
	return merged, sources, nil
}

// scalarNodeForKind builds a YAML scalar carrying the right tag for a fleet
// value so config.Parse decodes it as the correct Go type (bool/int/string).
func scalarNodeForKind(kind, value string) *yaml.Node {
	tag := "!!str"
	switch kind {
	case config.SettingKindBool:
		tag = "!!bool"
	case config.SettingKindInt:
		tag = "!!int"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

// nodeAtPath walks the mapping value at path within root, returning the node or
// nil if any segment is absent.
func nodeAtPath(root *yaml.Node, path []string) *yaml.Node {
	node := documentValue(root)
	for _, key := range path {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		idx := mappingKeyIndex(node, key)
		if idx < 0 {
			return nil
		}
		node = node.Content[idx+1]
	}
	return node
}

// scalarValueAtPath returns the trimmed scalar value at path, or "" if the path
// is absent or not a scalar.
func scalarValueAtPath(root *yaml.Node, path []string) string {
	n := nodeAtPath(root, path)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(n.Value)
}

// setNodeAtPath sets value at path, creating intermediate mapping nodes as
// needed. A non-mapping intermediate is replaced with a mapping.
func setNodeAtPath(root *yaml.Node, path []string, value *yaml.Node) {
	if len(path) == 0 {
		return
	}
	node := documentValue(root)
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i, key := range path {
		if i == len(path)-1 {
			if idx := mappingKeyIndex(node, key); idx >= 0 {
				node.Content[idx+1] = value
				return
			}
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
			return
		}
		idx := mappingKeyIndex(node, key)
		if idx < 0 {
			child := &yaml.Node{Kind: yaml.MappingNode}
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
			node = child
			continue
		}
		child := node.Content[idx+1]
		if child.Kind != yaml.MappingNode {
			child = &yaml.Node{Kind: yaml.MappingNode}
			node.Content[idx+1] = child
		}
		node = child
	}
}

// deleteNodeAtPath removes the key at path (leaving now-empty parent mappings in
// place; they marshal to `{}` which config.Parse ignores).
func deleteNodeAtPath(root *yaml.Node, path []string) {
	if len(path) == 0 {
		return
	}
	node := documentValue(root)
	for i, key := range path {
		if node == nil || node.Kind != yaml.MappingNode {
			return
		}
		idx := mappingKeyIndex(node, key)
		if idx < 0 {
			return
		}
		if i == len(path)-1 {
			node.Content = append(node.Content[:idx], node.Content[idx+2:]...)
			return
		}
		node = node.Content[idx+1]
	}
}

// FleetSettingsFileName is the reserved export/import filename for the fleet
// settings layer (#839). config-store export writes it alongside the project
// YAMLs; ImportDir recognizes it and loads it into the settings table instead
// of treating it as a project.
const FleetSettingsFileName = "_fleet-settings.yaml"

// fleetSettingsDocKey is the top-level key under which FleetSettingsFileName
// stores the dotted key -> value map.
const fleetSettingsDocKey = "fleet_settings"

// ExportFleetSettings serializes the fleet settings layer to portable YAML
// (empty map -> nil so export omits the file when there are no defaults).
func (s *Store) ExportFleetSettings(ctx context.Context) ([]byte, error) {
	settings, err := s.FleetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if len(settings) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	doc := &yaml.Node{Kind: yaml.MappingNode}
	inner := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		spec, ok := config.FleetSettingSpecByKey(k)
		kind := config.SettingKindString
		if ok {
			kind = spec.Kind
		}
		inner.Content = append(inner.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
			scalarNodeForKind(kind, settings[k]))
	}
	doc.Content = append(doc.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fleetSettingsDocKey}, inner)
	return marshalNode(doc)
}

// importFleetSettingsTx loads an exported fleet settings document into the
// settings table (replacing existing rows for the keys it names). Unknown or
// ill-typed keys are rejected so a hand-edited export cannot poison the layer.
func importFleetSettingsTx(ctx context.Context, tx *sql.Tx, data []byte, actor string) error {
	var doc struct {
		FleetSettings map[string]string `yaml:"fleet_settings"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	keys := make([]string, 0, len(doc.FleetSettings))
	for k := range doc.FleetSettings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		norm, err := config.NormalizeSettingValue(key, doc.FleetSettings[key])
		if err != nil {
			return fmt.Errorf("%s: %w", FleetSettingsFileName, err)
		}
		old, _, err := fleetSettingTx(ctx, tx, key)
		if err != nil {
			return err
		}
		changedAt, err := writeSettingsAuditTx(ctx, tx, key, scopeFleet, old, norm, actor)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO settings(key, value, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, key, norm, changedAt); err != nil {
			return err
		}
	}
	return nil
}
