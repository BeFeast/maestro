package configstore

// Fleet-level settings layer (#839).
//
// Cost-control knobs (supervisor.enabled, supervisor.backend/model/effort,
// poll_interval_seconds, worker_max_tokens, …) live per-project in each row's
// config YAML. Before this layer, flipping one fleet-wide meant exporting,
// editing and re-importing every project row (the 2026-07-09 P0 mitigation did
// exactly that for 5 rows). This file adds a fleet-level DEFAULT layer plus an
// audit trail, resolved with a strict precedence:
//
//	per-project value  >  fleet default  >  built-in (config.parse default)
//
// A fleet default is stored in the `settings` table and OVERLAID onto a
// project's YAML at Load time only for keys the project does not set itself, so
// a project override always wins. Every change (fleet or per-project) is
// journaled in `settings_audit` with actor + RFC3339 timestamp and old→new
// values, so a backend flip is attributable without DB-backup archaeology.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"gopkg.in/yaml.v3"
)

// SettingsSchema creates the fleet-settings tables. Kept separate from the
// core Schema string only for readability; Init runs both so a store opened
// against an older config.db migrates forward automatically (IF NOT EXISTS).
const SettingsSchema = `
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS settings_audit (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	key        TEXT NOT NULL,
	scope      TEXT NOT NULL,
	old_value  TEXT NOT NULL,
	new_value  TEXT NOT NULL,
	actor      TEXT NOT NULL,
	at         TEXT NOT NULL
);
`

// FleetScope is the audit scope recorded for a fleet-wide default change. A
// per-project override records the project name as its scope.
const FleetScope = "fleet"

// Setting sources reported in effective_config (#839 acceptance criterion 2).
const (
	SourceBuiltin = "builtin"
	SourceFleet   = "fleet"
	SourceProject = "project"
)

type settingType int

const (
	settingBool settingType = iota
	settingInt
	settingString
)

// SettingSpec is one supported cost-control knob: its dotted CLI name, the YAML
// path it maps to in a project config, and its value type (used to validate a
// `settings set` and to build the overlay scalar node with the right tag).
type SettingSpec struct {
	Name string
	Path []string
	Type settingType
	Doc  string
}

// settingSpecs is the canonical registry of fleet-settable cost-control knobs.
// Keep this in sync with the config fields the paths reference; a path that no
// longer exists in config.Config would silently overlay a dead key.
var settingSpecs = []SettingSpec{
	{"supervisor.enabled", []string{"supervisor", "enabled"}, settingBool, "LLM supervisor on/off"},
	{"supervisor.backend", []string{"supervisor", "backend"}, settingString, "supervisor LLM backend name"},
	{"supervisor.model", []string{"supervisor", "model"}, settingString, "supervisor LLM model"},
	{"supervisor.effort", []string{"supervisor", "effort"}, settingString, "supervisor LLM effort (low|medium|high|xhigh)"},
	{"supervisor.allow_metered_backend", []string{"supervisor", "allow_metered_backend"}, settingBool, "opt the supervisor LLM loop into a metered (per-token) backend (#838)"},
	{"supervisor.always_consult_llm", []string{"supervisor", "always_consult_llm"}, settingBool, "always call the LLM even on safe deterministic decisions (pre-#837)"},
	{"poll_interval_seconds", []string{"poll_interval_seconds"}, settingInt, "supervisor/orchestrator poll interval override (0 = use CLI flag)"},
	{"worker_max_tokens", []string{"worker_max_tokens"}, settingInt, "kill a worker over this token threshold (0 = unlimited)"},
}

// SettingSpecs returns the supported-key registry (name + doc), sorted by name.
// Exposed so the CLI can print an accurate usage/list of valid keys.
func SettingSpecs() []SettingSpec {
	out := append([]SettingSpec(nil), settingSpecs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TypeName returns a human-readable type label for the key ("bool"/"int"/
// "string"), used by `maestro settings list --keys`.
func (spec SettingSpec) TypeName() string {
	switch spec.Type {
	case settingBool:
		return "bool"
	case settingInt:
		return "int"
	default:
		return "string"
	}
}

func lookupSpec(name string) (SettingSpec, bool) {
	for _, spec := range settingSpecs {
		if spec.Name == name {
			return spec, true
		}
	}
	return SettingSpec{}, false
}

// canonicalizeSettingValue validates raw against the key's type and returns the
// canonical stored form ("true"/"false", a base-10 int, or the trimmed string).
// A per-type parse keeps a `settings set poll_interval_seconds=banana` from
// landing an un-parseable value that would later break config.parse for every
// project.
func canonicalizeSettingValue(spec SettingSpec, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch spec.Type {
	case settingBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return "", fmt.Errorf("setting %q wants a boolean (true|false), got %q", spec.Name, raw)
		}
		return strconv.FormatBool(b), nil
	case settingInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return "", fmt.Errorf("setting %q wants an integer, got %q", spec.Name, raw)
		}
		return strconv.Itoa(n), nil
	default:
		if raw == "" {
			return "", fmt.Errorf("setting %q wants a non-empty string value", spec.Name)
		}
		return raw, nil
	}
}

// scalarNode builds the YAML scalar for a canonical value with the tag matching
// the key type, so an overlaid `worker_max_tokens: 400000` re-encodes as an int
// (not a quoted string) and parses back cleanly.
func (spec SettingSpec) scalarNode(value string) *yaml.Node {
	tag := "!!str"
	switch spec.Type {
	case settingBool:
		tag = "!!bool"
	case settingInt:
		tag = "!!int"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

// SetFleetSetting writes (or clears, when value is empty and the key already
// exists) a fleet-level default and journals the change. actor is recorded
// verbatim. The write advances the settings fingerprint so every store-backed
// flow hot-reloads on its next poll without a restart (#839 AC1).
func (s *Store) SetFleetSetting(ctx context.Context, key, rawValue, actor string) error {
	spec, ok := lookupSpec(key)
	if !ok {
		return fmt.Errorf("unknown setting %q (see `maestro settings list --keys`)", key)
	}
	canon, err := canonicalizeSettingValue(spec, rawValue)
	if err != nil {
		return err
	}
	old, _, err := s.FleetSetting(ctx, key)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano) // nano: see ProjectsFingerprint (#757)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settings(key, value, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, key, canon, nowStr); err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, key, FleetScope, old, canon, actor, now); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFleetSetting removes a fleet-level default (reverting affected projects
// to the built-in value) and journals the removal.
func (s *Store) DeleteFleetSetting(ctx context.Context, key, actor string) error {
	if _, ok := lookupSpec(key); !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	old, present, err := s.FleetSetting(ctx, key)
	if err != nil {
		return err
	}
	if !present {
		return nil // idempotent: nothing to delete
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, key, FleetScope, old, "", actor, now); err != nil {
		return err
	}
	return tx.Commit()
}

// SetProjectSetting writes a per-project override into the project's config YAML
// (the value that beats the fleet default) and journals it under the project's
// scope. Advancing the project row's updated_at hot-reloads that flow.
func (s *Store) SetProjectSetting(ctx context.Context, project, key, rawValue, actor string) error {
	spec, ok := lookupSpec(key)
	if !ok {
		return fmt.Errorf("unknown setting %q (see `maestro settings list --keys`)", key)
	}
	canon, err := canonicalizeSettingValue(spec, rawValue)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var projectYAML string
	err = tx.QueryRowContext(ctx, `SELECT config_yaml FROM project WHERE name = ?`, project).Scan(&projectYAML)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("project %q not found in config store", project)
	}
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(projectYAML), &root); err != nil {
		return err
	}
	old := scalarAtPath(&root, spec.Path)
	setSettingPath(&root, spec.Path, spec.scalarNode(canon))
	updated, err := marshalNode(&root)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE project SET config_yaml = ?, updated_at = ? WHERE name = ?`, string(updated), nowStr, project); err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, key, project, old, canon, actor, now); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAuditTx(ctx context.Context, tx *sql.Tx, key, scope, old, next, actor string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO settings_audit(key, scope, old_value, new_value, actor, at)
VALUES(?, ?, ?, ?, ?, ?)
`, key, scope, old, next, strings.TrimSpace(actor), at.Format(time.RFC3339))
	return err
}

// FleetSetting returns the fleet-level value for key and whether it is set.
func (s *Store) FleetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// FleetSettings returns every fleet-level default (key → canonical value).
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

// settingsFingerprint returns the newest updated_at across the settings table,
// or the zero time when no fleet default is set. ProjectsFingerprint folds this
// into every project's timestamp so a fleet default change advances all
// store-backed flows' watchers, hot-reloading them without a restart (#839).
func (s *Store) settingsFingerprint(ctx context.Context) (time.Time, error) {
	var updatedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(updated_at) FROM settings`).Scan(&updatedAt)
	if err != nil {
		return time.Time{}, err
	}
	if !updatedAt.Valid || updatedAt.String == "" {
		return time.Time{}, nil
	}
	ts, err := time.Parse(time.RFC3339Nano, updatedAt.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse settings updated_at: %w", err)
	}
	return ts, nil
}

// AuditRecord is one journaled settings change (#839 AC4).
type AuditRecord struct {
	Key      string
	Scope    string
	OldValue string
	NewValue string
	Actor    string
	At       string // RFC3339
}

// SettingsAudit returns the change journal, most recent first. limit <= 0
// returns the whole journal.
func (s *Store) SettingsAudit(ctx context.Context, limit int) ([]AuditRecord, error) {
	query := `SELECT key, scope, old_value, new_value, actor, at FROM settings_audit ORDER BY id DESC`
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
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		if err := rows.Scan(&r.Key, &r.Scope, &r.OldValue, &r.NewValue, &r.Actor, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolvedSetting is a key's effective value in a project plus where it came
// from (project override / fleet default / built-in).
type ResolvedSetting struct {
	Key    string
	Value  string
	Source string
}

// ResolveProjectSettings returns every cost-control knob's effective value and
// source for one project, applying the project > fleet > builtin precedence.
func (s *Store) ResolveProjectSettings(ctx context.Context, project string) ([]ResolvedSetting, error) {
	cfg, err := s.Load(ctx, project)
	if err != nil {
		return nil, err
	}
	var projectYAML string
	err = s.db.QueryRowContext(ctx, `SELECT config_yaml FROM project WHERE name = ?`, project).Scan(&projectYAML)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project %q not found in config store", project)
	}
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(projectYAML), &root); err != nil {
		return nil, err
	}
	fleet, err := s.FleetSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedSetting, 0, len(settingSpecs))
	for _, spec := range SettingSpecs() {
		source := SourceBuiltin
		if settingPresent(&root, spec.Path) {
			source = SourceProject
		} else if _, ok := fleet[spec.Name]; ok {
			source = SourceFleet
		}
		out = append(out, ResolvedSetting{
			Key:    spec.Name,
			Value:  settingValueFromConfig(cfg, spec.Name),
			Source: source,
		})
	}
	return out, nil
}

// CostControlValues returns each fleet-settable knob's effective (post-overlay)
// value read off a resolved config, keyed by setting name. Paired with
// config.Config.SettingSources, it lets the fleet API render effective_config's
// cost-control block with both value and provenance (#839 AC2).
func CostControlValues(cfg *config.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	out := make(map[string]string, len(settingSpecs))
	for _, spec := range settingSpecs {
		out[spec.Name] = settingValueFromConfig(cfg, spec.Name)
	}
	return out
}

// settingValueFromConfig reads a knob's effective (post-overlay) value off a
// resolved config. Switch is exhaustive over settingSpecs — a new spec needs a
// case here so the CLI/API report its value.
func settingValueFromConfig(cfg *config.Config, key string) string {
	switch key {
	case "supervisor.enabled":
		return strconv.FormatBool(cfg.Supervisor.Enabled)
	case "supervisor.backend":
		return strings.TrimSpace(cfg.Supervisor.Backend)
	case "supervisor.model":
		return strings.TrimSpace(cfg.Supervisor.Model)
	case "supervisor.effort":
		return strings.TrimSpace(cfg.Supervisor.Effort)
	case "supervisor.allow_metered_backend":
		return strconv.FormatBool(cfg.Supervisor.AllowMeteredBackend)
	case "supervisor.always_consult_llm":
		return strconv.FormatBool(cfg.Supervisor.AlwaysConsultLLM)
	case "poll_interval_seconds":
		return strconv.Itoa(cfg.PollIntervalSeconds)
	case "worker_max_tokens":
		return strconv.Itoa(cfg.WorkerMaxTokens)
	default:
		return ""
	}
}

// applyFleetSettings overlays each fleet default onto root at its YAML path,
// but ONLY where the project has not already set that path — a per-project
// value always wins (#839 precedence). Mutates root in place.
func applyFleetSettings(root *yaml.Node, fleet map[string]string) {
	if len(fleet) == 0 {
		return
	}
	for _, spec := range settingSpecs {
		val, ok := fleet[spec.Name]
		if !ok {
			continue
		}
		if settingPresent(root, spec.Path) {
			continue // project override wins
		}
		setSettingPath(root, spec.Path, spec.scalarNode(val))
	}
}

// settingSources reports each knob's source (project/fleet/builtin) for a
// project whose raw YAML is root and whose fleet defaults are fleet. Used to
// annotate a resolved config so effective_config can show provenance (#839 AC2).
func settingSources(root *yaml.Node, fleet map[string]string) map[string]string {
	out := make(map[string]string, len(settingSpecs))
	for _, spec := range settingSpecs {
		switch {
		case settingPresent(root, spec.Path):
			out[spec.Name] = SourceProject
		case fleet != nil && fleetHas(fleet, spec.Name):
			out[spec.Name] = SourceFleet
		default:
			out[spec.Name] = SourceBuiltin
		}
	}
	return out
}

func fleetHas(fleet map[string]string, key string) bool {
	_, ok := fleet[key]
	return ok
}

// settingPresent reports whether root explicitly contains a value at path.
func settingPresent(root *yaml.Node, path []string) bool {
	node := documentValue(root)
	for i, key := range path {
		if node == nil || node.Kind != yaml.MappingNode {
			return false
		}
		idx := mappingKeyIndex(node, key)
		if idx < 0 {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		node = node.Content[idx+1]
	}
	return true
}

// scalarAtPath returns the scalar value at path, or "" when absent / not a
// scalar. Used to capture the old value for the audit trail.
func scalarAtPath(root *yaml.Node, path []string) string {
	node := documentValue(root)
	for i, key := range path {
		if node == nil || node.Kind != yaml.MappingNode {
			return ""
		}
		idx := mappingKeyIndex(node, key)
		if idx < 0 {
			return ""
		}
		child := node.Content[idx+1]
		if i == len(path)-1 {
			if child.Kind == yaml.ScalarNode {
				return strings.TrimSpace(child.Value)
			}
			return ""
		}
		node = child
	}
	return ""
}

// setSettingPath sets root's value at path to value, creating intermediate
// mappings (e.g. the `supervisor` block for supervisor.enabled) as needed.
func setSettingPath(root *yaml.Node, path []string, value *yaml.Node) {
	node := documentValue(root)
	if node == nil {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		node = root.Content[0]
	}
	for i, key := range path {
		if i == len(path)-1 {
			setKeyOnMapping(node, key, value)
			return
		}
		node = childMapping(node, key)
	}
}

// setKeyOnMapping upserts key→value on a mapping node.
func setKeyOnMapping(node *yaml.Node, key string, value *yaml.Node) {
	idx := mappingKeyIndex(node, key)
	if idx >= 0 {
		node.Content[idx+1] = value
		return
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// childMapping returns node's child mapping for key, creating an empty one when
// absent (or when the existing child is not a mapping).
func childMapping(node *yaml.Node, key string) *yaml.Node {
	idx := mappingKeyIndex(node, key)
	if idx >= 0 {
		if child := node.Content[idx+1]; child.Kind == yaml.MappingNode {
			return child
		}
		child := &yaml.Node{Kind: yaml.MappingNode}
		node.Content[idx+1] = child
		return child
	}
	child := &yaml.Node{Kind: yaml.MappingNode}
	setKeyOnMapping(node, key, child)
	return child
}
