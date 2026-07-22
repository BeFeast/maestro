// Package workerlease owns the durable private scratch receipt for an isolated
// worker attempt. Its manifest is bound to the exact process lease created by
// tmuxsession; scratch never introduces a second process ownership model.
package workerlease

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/tmuxsession"
	"golang.org/x/sys/unix"
)

const (
	ManifestVersion = 1
	ManifestName    = "lease.json"
	WorkerSlice     = "maestro-workers-isolated.slice"
	// The shared worker slice keeps compiler/test pressure outside the
	// orchestrator's cgroup and preserves deterministic host headroom. The
	// isolated runtime config-gate is what opts workers into this slice; legacy
	// workers remain untouched during rollout and rollback.
	WorkerSliceMemoryHigh = "70%"
	WorkerSliceMemoryMax  = "80%"

	ScopeSystem = "system"
	ScopeUser   = "user"

	workerLeaseInspectTimeout = 15 * time.Second
)

var leaseIDPattern = regexp.MustCompile(`^mw-[a-f0-9]{10}-[a-z0-9][a-z0-9-]{0,31}-[a-f0-9]{24}$`)

type Spec struct {
	Root       string
	ProjectKey string
	Repo       string
	Slot       string
	Attempt    string
	Unit       string
	Scope      string
	Now        time.Time
}

type Lease struct {
	ID           string
	Unit         string
	Scope        string
	ScratchDir   string
	TempDir      string
	GoTempDir    string
	CargoTarget  string
	ManifestPath string
	ProjectKey   string
	Repo         string
	Slot         string
	Attempt      string
	CreatedAt    time.Time
}

type Manifest struct {
	Version    int       `json:"version"`
	LeaseID    string    `json:"lease_id"`
	Unit       string    `json:"unit"`
	Scope      string    `json:"scope"`
	ScratchDir string    `json:"scratch_dir"`
	ProjectKey string    `json:"project_key"`
	Repo       string    `json:"repo"`
	Slot       string    `json:"slot"`
	Attempt    string    `json:"attempt"`
	CreatedAt  time.Time `json:"created_at"`
}

type Attention struct {
	Entry  string
	Reason string
}

// EnsureScratchBase creates the configured fleet/project scratch base without
// weakening an existing host directory. A pre-existing shared or symlinked
// path is rejected rather than chmodded: operators must point Maestro at a
// dedicated private disk-backed directory, never at /var/tmp itself.
func EnsureScratchBase(path string) error {
	root, err := validateRoot(path)
	if err != nil {
		return err
	}
	if err := ensureDiskBacked(root); err != nil {
		return err
	}
	if err := ensureNoSymlinkComponents(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("worker lease: create scratch base: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("worker lease: inspect scratch base: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("worker lease: scratch base is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("worker lease: scratch base must be a dedicated private directory")
	}
	return nil
}

// Prepare creates one uniquely named private scratch tree and writes its
// ownership manifest before a process is launched. The manifest is the durable
// bridge between failed-spawn cleanup and daemon restart reconciliation.
func Prepare(spec Spec) (Lease, error) {
	root, err := validateRoot(spec.Root)
	if err != nil {
		return Lease{}, err
	}
	scope := strings.ToLower(strings.TrimSpace(spec.Scope))
	if scope == "" {
		scope = ScopeSystem
	}
	if scope != ScopeSystem && scope != ScopeUser {
		return Lease{}, fmt.Errorf("worker lease: invalid scope %q", spec.Scope)
	}
	unit := strings.TrimSpace(spec.Unit)
	if !ValidProcessLeaseUnit(unit) {
		return Lease{}, fmt.Errorf("worker lease: invalid process lease unit %q", spec.Unit)
	}
	if err := ensureDiskBacked(root); err != nil {
		return Lease{}, err
	}
	if err := ensureNoSymlinkComponents(root); err != nil {
		return Lease{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Lease{}, fmt.Errorf("worker lease: create scratch root: %w", err)
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return Lease{}, err
	}

	id, err := newLeaseID(spec.ProjectKey, spec.Slot)
	if err != nil {
		return Lease{}, err
	}
	createdAt := spec.Now.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	dir := filepath.Join(root, id)
	lease := Lease{
		ID:           id,
		Unit:         unit,
		Scope:        scope,
		ScratchDir:   dir,
		TempDir:      filepath.Join(dir, "tmp"),
		GoTempDir:    filepath.Join(dir, "tmp", "go"),
		CargoTarget:  filepath.Join(dir, "tmp", "cargo-target"),
		ManifestPath: filepath.Join(dir, ManifestName),
		ProjectKey:   strings.TrimSpace(spec.ProjectKey),
		Repo:         strings.TrimSpace(spec.Repo),
		Slot:         strings.TrimSpace(spec.Slot),
		Attempt:      strings.TrimSpace(spec.Attempt),
		CreatedAt:    createdAt,
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Lease{}, fmt.Errorf("worker lease: create private scratch: %w", err)
	}
	prepared := false
	defer func() {
		if !prepared {
			_ = os.RemoveAll(dir)
		}
	}()
	for _, path := range []string{lease.TempDir, lease.GoTempDir, lease.CargoTarget} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return Lease{}, fmt.Errorf("worker lease: create private temp path: %w", err)
		}
	}
	manifest := Manifest{
		Version: ManifestVersion, LeaseID: lease.ID, Unit: lease.Unit, Scope: lease.Scope,
		ScratchDir: lease.ScratchDir, ProjectKey: lease.ProjectKey, Repo: lease.Repo,
		Slot: lease.Slot, Attempt: lease.Attempt, CreatedAt: lease.CreatedAt,
	}
	if err := writeManifest(lease.ManifestPath, manifest); err != nil {
		return Lease{}, err
	}
	prepared = true
	return lease, nil
}

func ValidLeaseID(leaseID string) bool {
	return leaseIDPattern.MatchString(strings.TrimSpace(leaseID))
}

func ValidProcessLeaseUnit(unit string) bool {
	unit = strings.TrimSpace(unit)
	return strings.HasSuffix(unit, ".service") && tmuxsession.ValidProcessLeaseUnit(unit)
}

func LeaseFromManifest(m Manifest, manifestPath string) Lease {
	dir := filepath.Dir(manifestPath)
	return Lease{
		ID: m.LeaseID, Unit: m.Unit, Scope: m.Scope, ScratchDir: dir,
		TempDir: filepath.Join(dir, "tmp"), GoTempDir: filepath.Join(dir, "tmp", "go"),
		CargoTarget: filepath.Join(dir, "tmp", "cargo-target"), ManifestPath: manifestPath,
		ProjectKey: m.ProjectKey, Repo: m.Repo, Slot: m.Slot, Attempt: m.Attempt, CreatedAt: m.CreatedAt,
	}
}

// EnsureWorkerSlice applies the aggregate memory boundary used by every
// isolated worker service. The setting is runtime-only: disabling isolated
// mode immediately returns new workers to the legacy path, and a reboot drops
// the transient property override. Reapplying identical properties before a
// spawn is idempotent and closes the race where the implicit slice exists but
// has not yet received its limits.
func EnsureWorkerSlice(scope string) error {
	binary, args, err := workerSliceControlCommand(scope)
	if err != nil {
		return err
	}
	out, err := runControlCommand(workerLeaseInspectTimeout, binary, args...)
	if err != nil {
		return fmt.Errorf("configure aggregate worker slice: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func workerSliceControlCommand(scope string) (string, []string, error) {
	return systemctlCommand(scope,
		"set-property", "--runtime", WorkerSlice,
		"MemoryAccounting=yes",
		"MemoryHigh="+WorkerSliceMemoryHigh,
		"MemoryMax="+WorkerSliceMemoryMax,
	)
}

func runControlCommand(timeout time.Duration, binary string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("worker lease control command timed out: %w", ctx.Err())
	}
	return out, err
}

func systemctlCommand(scope string, args ...string) (string, []string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case ScopeSystem:
		return "sudo", append([]string{"-n", "systemctl"}, args...), nil
	case ScopeUser:
		return "systemctl", append([]string{"--user"}, args...), nil
	default:
		return "", nil, fmt.Errorf("worker lease: invalid scope %q", scope)
	}
}

// CleanupManifest removes only the manifest's own parent directory after
// validating that the path, lease ID, unit, and recorded scratch identity all
// agree. Missing paths are success, making ExecStopPost and reconciliation
// exactly-once in effect even when both observe the same terminal lease.
func CleanupManifest(manifestPath, expectedLeaseID string) error {
	manifestPath = filepath.Clean(strings.TrimSpace(manifestPath))
	if manifestPath == "." || manifestPath == "" {
		return fmt.Errorf("worker lease cleanup: manifest path is required")
	}
	m, err := ValidateManifest(manifestPath, expectedLeaseID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dir := filepath.Dir(manifestPath)
	if filepath.Clean(m.ScratchDir) != dir {
		return fmt.Errorf("worker lease cleanup: manifest scratch identity mismatch")
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker lease cleanup: inspect scratch: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("worker lease cleanup: scratch path is not a private directory")
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("worker lease cleanup: remove exact scratch: %w", err)
	}
	parent, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return fmt.Errorf("worker lease cleanup: open scratch parent: %w", err)
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("worker lease cleanup: sync scratch parent: %w", err)
	}
	return nil
}

func ValidateManifest(manifestPath, expectedLeaseID string) (Manifest, error) {
	manifestPath = filepath.Clean(strings.TrimSpace(manifestPath))
	if filepath.Base(manifestPath) != ManifestName {
		return Manifest{}, fmt.Errorf("worker lease: manifest must be named %s", ManifestName)
	}
	if err := ensureNoSymlinkComponents(manifestPath); err != nil {
		return Manifest{}, err
	}
	dir := filepath.Dir(manifestPath)
	id := filepath.Base(dir)
	if !leaseIDPattern.MatchString(id) {
		return Manifest{}, fmt.Errorf("worker lease: invalid lease directory identity")
	}
	if expected := strings.TrimSpace(expectedLeaseID); expected != "" && id != expected {
		return Manifest{}, fmt.Errorf("worker lease: expected lease %s, found %s", expected, id)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("worker lease: parse manifest: %w", err)
	}
	if m.Version != ManifestVersion || m.LeaseID != id || !ValidProcessLeaseUnit(m.Unit) ||
		(m.Scope != ScopeSystem && m.Scope != ScopeUser) || filepath.Clean(m.ScratchDir) != dir ||
		strings.TrimSpace(m.ProjectKey) == "" || strings.TrimSpace(m.Slot) == "" {
		return Manifest{}, fmt.Errorf("worker lease: manifest ownership is invalid or ambiguous")
	}
	return m, nil
}

// List returns valid owned manifests and attention records for everything else
// under the dedicated root. Unknown/ambiguous entries are never deleted.
func List(root string) ([]Lease, []Attention, error) {
	root, err := validateRoot(root)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureNoSymlinkComponents(root); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("worker lease: inspect scratch root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("worker lease: scratch root ownership is invalid or ambiguous")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("worker lease: list scratch root: %w", err)
	}
	leases := make([]Lease, 0, len(entries))
	attention := make([]Attention, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !leaseIDPattern.MatchString(name) {
			attention = append(attention, Attention{Entry: name, Reason: "ambiguous scratch entry"})
			continue
		}
		manifestPath := filepath.Join(root, name, ManifestName)
		m, err := ValidateManifest(manifestPath, name)
		if err != nil {
			attention = append(attention, Attention{Entry: name, Reason: "invalid ownership manifest"})
			continue
		}
		leases = append(leases, LeaseFromManifest(m, manifestPath))
	}
	return leases, attention, nil
}

func validateRoot(root string) (string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == "" || !filepath.IsAbs(root) || root == string(filepath.Separator) {
		return "", fmt.Errorf("worker lease: scratch root must be a non-root absolute path")
	}
	if strings.ContainsAny(root, "\n\r\x00:%") {
		return "", fmt.Errorf("worker lease: scratch root contains unsupported characters")
	}
	return root, nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("worker lease: inspect scratch root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("worker lease: scratch root is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("worker lease: make scratch root private: %w", err)
		}
	}
	return nil
}

func ensureDiskBacked(path string) error {
	probe := filepath.Clean(path)
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("worker lease: inspect scratch filesystem: %w", err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("worker lease: no existing scratch filesystem ancestor")
		}
		probe = parent
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(probe, &fs); err != nil {
		return fmt.Errorf("worker lease: inspect scratch filesystem: %w", err)
	}
	if fs.Type == unix.TMPFS_MAGIC || fs.Type == unix.RAMFS_MAGIC {
		return fmt.Errorf("worker lease: scratch root must be disk-backed, found memory-backed filesystem")
	}
	return nil
}

func ensureNoSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("worker lease: path must be absolute")
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("worker lease: inspect path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("worker lease: scratch path contains a symlink")
		}
	}
	return nil
}

func newLeaseID(projectKey, slot string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("worker lease: create identity: %w", err)
	}
	projectHash := sha256.Sum256([]byte(strings.TrimSpace(projectKey)))
	return "mw-" + hex.EncodeToString(projectHash[:5]) + "-" + slug(slot) + "-" + hex.EncodeToString(raw[:]), nil
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && b.String()[b.Len()-1] != '-':
			b.WriteByte('-')
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "worker"
	}
	return out
}

func writeManifest(path string, manifest Manifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("worker lease: encode manifest: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("worker lease: write manifest: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		cleanup()
		return fmt.Errorf("worker lease: write manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("worker lease: sync manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("worker lease: close manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("worker lease: commit manifest: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("worker lease: open manifest directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("worker lease: sync manifest directory: %w", err)
	}
	return nil
}

func systemdExecCommand(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		arg = strings.ReplaceAll(arg, "%", "%%")
		arg = strings.ReplaceAll(arg, "\\", "\\\\")
		arg = strings.ReplaceAll(arg, "\"", "\\\"")
		quoted = append(quoted, "\""+arg+"\"")
	}
	return strings.Join(quoted, " ")
}

// CleanupExec renders one exact ExecStopPost command. Percent and quoting
// rules are systemd-specific, so callers should not treat this as shell text.
func CleanupExec(maestroBin, manifestPath, leaseID string) string {
	return systemdExecCommand([]string{
		maestroBin, "_worker-lease-cleanup", "--manifest", manifestPath, "--lease", leaseID,
	})
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
