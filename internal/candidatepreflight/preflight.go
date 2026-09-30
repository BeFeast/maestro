// Package candidatepreflight compares supplied resource declarations. It never
// opens databases, constructs runtimes, resolves DNS, or authorizes activation.
package candidatepreflight

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/daemonresources"
	"gopkg.in/yaml.v3"
)

type Input struct {
	Version   int       `json:"version"`
	Stable    *Snapshot `json:"stable"`
	Candidate *Snapshot `json:"candidate"`
}

type Snapshot struct {
	Options  json.RawMessage `json:"options"`
	Projects []Project       `json:"projects"`
}

type Project struct {
	Name       string `json:"name"`
	ConfigYAML string `json:"config_yaml"`
}

type Resource struct {
	Role       string `json:"role"`
	Kind       string `json:"kind"`
	Identity   string `json:"identity,omitempty"`
	Active     bool   `json:"active"`
	Defaulted  bool   `json:"defaulted"`
	Unresolved string `json:"unresolved,omitempty"`
	info       os.FileInfo
}

type Manifest struct {
	Resources []Resource `json:"resources"`
}

type Finding struct {
	Code      string `json:"code"`
	Stable    string `json:"stable,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Detail    string `json:"detail"`
}

type Report struct {
	Version              int       `json:"version"`
	Status               string    `json:"status"`
	ActivationAuthorized bool      `json:"activation_authorized"`
	Stable               Manifest  `json:"stable"`
	Candidate            Manifest  `json:"candidate"`
	Findings             []Finding `json:"findings"`
	Limitations          []string  `json:"limitations"`
}

func decode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON document")
	}
	return nil
}

// Evaluate consumes only the supplied snapshots. Path inspection uses stat and
// readlink, including existing ancestors of resources not yet created.
func Evaluate(data []byte) (Report, error) {
	var in Input
	if err := decode(data, &in); err != nil {
		return Report{}, fmt.Errorf("input: %w", err)
	}
	if in.Version != 1 || in.Stable == nil || in.Candidate == nil {
		return Report{}, fmt.Errorf("version 1, stable and candidate snapshots are required")
	}
	r := Report{Version: 1, Status: "no_declared_overlap", Findings: []Finding{}, Limitations: []string{
		"Declaration-level offline comparison only; no result authorizes candidate activation.",
		"No active-owner or cross-host fencing, queue partition, process/cgroup/tmux isolation, credential confinement, promotion or rollback is verified.",
		"Host-wide cleanup and emergency effects are not isolated by separate paths or ports. Candidate execution remains gated.",
		"Defaults and filesystem aliases are evaluated on this host at this instant. No DNS aliases, remote resources or live queue state are resolved.",
	}}
	r.Stable = build(*in.Stable)
	r.Candidate = build(*in.Candidate)
	for _, side := range []struct {
		name string
		m    Manifest
	}{{"stable", r.Stable}, {"candidate", r.Candidate}} {
		for _, res := range side.m.Resources {
			if res.Unresolved != "" {
				f := Finding{Code: "unresolved", Detail: res.Unresolved}
				if side.name == "stable" {
					f.Stable = res.Role
				} else {
					f.Candidate = res.Role
				}
				r.Findings = append(r.Findings, f)
				r.Status = "unresolved"
			}
		}
	}
	for _, a := range r.Stable.Resources {
		for _, b := range r.Candidate.Resources {
			if a.Unresolved != "" || b.Unresolved != "" || a.Identity == "" || b.Identity == "" {
				continue
			}
			if detail := overlap(a, b); detail != "" {
				r.Findings = append(r.Findings, Finding{Code: "overlap", Stable: a.Role, Candidate: b.Role, Detail: detail})
				if r.Status != "unresolved" {
					r.Status = "overlap"
				}
			}
		}
	}
	return r, nil
}

func build(s Snapshot) Manifest {
	m := Manifest{Resources: []Resource{}}
	addUnknown := func(role, why string) {
		m.Resources = append(m.Resources, Resource{Role: role, Kind: "unknown", Unresolved: why})
	}
	o := daemonresources.Defaults("")
	if len(s.Options) == 0 || string(s.Options) == "null" {
		addUnknown("options", "explicit options object with selected store is required; no runtime discovery is performed")
		return m
	}
	if err := decode(s.Options, &o); err != nil {
		addUnknown("options", "invalid daemon resource options: "+err.Error())
		return m
	}
	var supplied map[string]json.RawMessage
	_ = json.Unmarshal(s.Options, &supplied)
	for k, v := range supplied {
		if string(v) == "null" {
			addUnknown("options."+k, "null is not an effective option value")
		}
	}
	addPath := func(role, kind, path string, active, defaulted bool) {
		res := Resource{Role: role, Kind: kind, Active: active, Defaulted: defaulted}
		res.Identity, res.info, res.Unresolved = pathIdentity(path, kind)
		m.Resources = append(m.Resources, res)
	}
	for _, db := range []struct {
		role, path string
		active     bool
	}{{"store", o.Store, true}, {"approvals_db", o.ApprovalsDB, true}, {"state_db", o.StateDB, o.StateStore == "sqlite"}, {"webhook_db", o.WebhookDB, o.WebhookSecretFile != ""}, {"emergency_db", o.EmergencyDB, true}} {
		_, present := supplied[db.role]
		addPath(db.role, "database", db.path, db.active, !present)
	}
	for _, mode := range []struct{ name, value string }{{"approvals_store", o.ApprovalsStore}, {"state_store", o.StateStore}} {
		if mode.value != "json" && mode.value != "sqlite" {
			addUnknown(mode.name, "unsupported store mode; expected json or sqlite")
		}
	}
	if o.Store != "" {
		addPath("self_deploy_state_dir", "directory", filepath.Join(filepath.Dir(o.Store), "self-deploy"), true, true)
	}
	ep := Resource{Role: "http_endpoint", Kind: "endpoint", Active: o.Port != 0}
	_, hostGiven := supplied["host"]
	_, portGiven := supplied["port"]
	ep.Defaulted = !hostGiven || !portGiven
	if o.Port < 0 || o.Port > 65535 {
		ep.Unresolved = "invalid HTTP port"
	} else if o.Port != 0 {
		host := strings.TrimSpace(o.Host)
		if host == "" {
			host = daemonresources.Defaults("").Host
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}
		ip := net.ParseIP(host)
		if ip == nil {
			ep.Unresolved = "HTTP host is not a literal IP or localhost; DNS is not consulted"
		} else {
			ep.Identity = net.JoinHostPort(ip.String(), fmt.Sprint(o.Port))
		}
	}
	m.Resources = append(m.Resources, ep)
	if s.Projects == nil {
		addUnknown("projects", "projects must be an explicit array; use [] for a declared empty fleet")
	}
	names := map[string]bool{}
	for _, p := range s.Projects {
		prefix := "project:" + p.Name + ":"
		if strings.TrimSpace(p.Name) == "" || names[p.Name] {
			addUnknown(prefix, "project names must be nonempty and unique")
			continue
		}
		names[p.Name] = true
		cfg, err := config.ParseStrict([]byte(p.ConfigYAML))
		if err != nil {
			addUnknown(prefix, "invalid supplied config: "+err.Error())
			continue
		}
		var declared struct {
			StateDir string `yaml:"state_dir"`
		}
		_ = yaml.Unmarshal([]byte(p.ConfigYAML), &declared) // already validated above
		addPath(prefix+"state_dir", "directory", cfg.StateDir, true, declared.StateDir == "")
		addPath(prefix+"local_path", "directory", cfg.LocalPath, true, false)
		addPath(prefix+"worktree_base", "directory", cfg.WorktreeBase, true, false)
		addPath(prefix+"supervisor_temp_dir", "directory", cfg.Supervisor.EffectiveTempDir(), cfg.Supervisor.Enabled, cfg.Supervisor.TempDir == "")
		if cfg.WorkerRuntime.IsolatedEnabled() {
			addPath(prefix+"scratch_root", "directory", cfg.WorkerRuntime.EffectiveScratchRoot(), true, cfg.WorkerRuntime.ScratchRoot == "")
		} else {
			addUnknown(prefix+"scratch_root", "legacy worker runtime has no declared isolated scratch root")
		}
		if cfg.RemoteRunner.Enabled {
			addUnknown(prefix+"remote_runner", "remote runner resources cannot be inspected by this host-local preflight")
		}
		q := Resource{Role: prefix + "queue", Kind: "queue", Active: true}
		q.Identity, q.Unresolved = queueIdentity(cfg)
		m.Resources = append(m.Resources, q)
		policy := Resource{Role: prefix + "promotion_policy", Kind: "policy", Identity: cfg.SelfDeploy.EffectivePromotionPolicy(), Active: cfg.SelfDeploy.Enabled}
		m.Resources = append(m.Resources, policy)
	}
	return m
}

func pathIdentity(raw, kind string) (string, os.FileInfo, string) {
	if strings.TrimSpace(raw) == "" || strings.ContainsAny(raw, "\x00\r\n?#") || strings.HasPrefix(raw, "file:") || raw == ":memory:" || strings.Contains(raw, "://") {
		return "", nil, "resource must be a nonempty plain filesystem path; SQLite URIs and query parameters are unsupported"
	}
	if strings.HasPrefix(raw, "~") {
		return "", nil, "effective resource paths must not contain unexpanded home shorthand"
	}
	// Preserve symlink/.. traversal until EvalSymlinks resolves it. filepath.Abs
	// and Join clean lexically and can select a different file before symlinks.
	abs := raw
	if !filepath.IsAbs(raw) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", nil, err.Error()
		}
		abs = cwd + string(filepath.Separator) + raw
	}
	probe := abs
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				if tail[i] == ".." {
					return "", nil, "parent traversal after a missing component is unresolved"
				}
				resolved = filepath.Join(resolved, tail[i])
			}
			info, statErr := os.Stat(resolved)
			if statErr != nil && !os.IsNotExist(statErr) {
				return "", nil, statErr.Error()
			}
			if info != nil && ((kind == "directory" && !info.IsDir()) || (kind == "database" && !info.Mode().IsRegular())) {
				return "", nil, "resource has incompatible filesystem type"
			}
			return resolved, info, ""
		}
		if !os.IsNotExist(err) {
			return "", nil, err.Error()
		}
		// A dangling symlink is unknown, not a missing directory declaration.
		if info, e := os.Lstat(probe); e == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", nil, "resource contains a dangling symlink"
		}
		end := strings.LastIndexByte(probe, byte(filepath.Separator))
		if end < 0 || probe == string(filepath.Separator) {
			return "", nil, "no resolvable filesystem ancestor"
		}
		tail = append(tail, probe[end+1:])
		probe = probe[:end]
		if probe == "" {
			probe = string(filepath.Separator)
		}
	}
}

func queueIdentity(cfg *config.Config) (string, string) {
	parts := strings.Split(cfg.Repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(cfg.Repo, " \t\r\n?#%:") || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return "", "queue repository must be an unambiguous owner/repo identity"
	}
	if !cfg.Forge.IsForgejo() {
		return "github:https://github.com/" + strings.ToLower(cfg.Repo), ""
	}
	u, err := url.Parse(cfg.Forge.BaseURL)
	if err != nil || u.Hostname() == "" || u.RawPath != "" {
		return "", "forge identity cannot be normalized without ambiguity"
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", "forge identity has an invalid port"
	}
	port = strconv.Itoa(number)
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	return "forgejo:" + u.Scheme + "://" + net.JoinHostPort(host, port) + strings.TrimRight(u.Path, "/") + "/" + strings.ToLower(cfg.Repo), ""
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func overlap(a, b Resource) string {
	pathKind := func(k string) bool { return k == "directory" || k == "database" }
	if pathKind(a.Kind) && pathKind(b.Kind) {
		if a.Identity == b.Identity || (a.info != nil && b.info != nil && os.SameFile(a.info, b.info)) {
			return "same declared filesystem resource (including aliases); inactive configured roles are also reported"
		}
		if (a.Kind == "directory" && within(a.Identity, b.Identity)) || (b.Kind == "directory" && within(b.Identity, a.Identity)) {
			return "mutable directory contains another declared resource"
		}
	}
	if a.Kind == "queue" && b.Kind == "queue" && a.Identity == b.Identity {
		return "same forge repository queue; labels, project UUIDs and DB copies do not partition ownership"
	}
	if a.Kind == "endpoint" && b.Kind == "endpoint" {
		ha, pa, _ := net.SplitHostPort(a.Identity)
		hb, pb, _ := net.SplitHostPort(b.Identity)
		if pa == pb && (ha == hb || net.ParseIP(ha).IsUnspecified() || net.ParseIP(hb).IsUnspecified() || (net.ParseIP(ha).IsLoopback() && net.ParseIP(hb).IsLoopback())) {
			return "HTTP bind declarations overlap (wildcard and loopback treated conservatively)"
		}
	}
	return ""
}
