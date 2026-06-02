package configstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
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

func (s *Store) Load(ctx context.Context, name string) (*config.Config, error) {
	data, sourcePath, err := s.exportProjectYAML(ctx, name)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, err
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
	now := time.Now().UTC().Format(time.RFC3339)
	for name, def := range backends {
		defYAML, err := marshalNode(def)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT definition_yaml FROM backends WHERE name = ?`, name).Scan(&existing)
		if err == nil && strings.TrimSpace(existing) != strings.TrimSpace(string(defYAML)) {
			return fmt.Errorf("backend %q differs from an already imported definition", name)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO backends(name, definition_yaml, updated_at)
VALUES(?, ?, ?)
ON CONFLICT(name) DO UPDATE SET definition_yaml = excluded.definition_yaml, updated_at = excluded.updated_at
`, name, string(defYAML), now); err != nil {
			return err
		}
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
