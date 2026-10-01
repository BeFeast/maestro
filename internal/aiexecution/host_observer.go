package aiexecution

import (
	"os"
	"path/filepath"
)

// ObservationKeyEnvironment returns only the variable name from the pinned
// manifest. The value belongs to the host observer, never to a native command.
func ObservationKeyEnvironment(policy Policy) (string, error) {
	b, err := os.ReadFile(policy.ManifestPath)
	var m Manifest
	if err != nil || len(b) > 128<<10 || digest(b) != policy.ManifestSHA256 || DecodeStrict(b, &m) != nil || m.Runtime.ManagementKeyEnv == "" {
		return "", Held("runtime_management_key_unavailable")
	}
	return m.Runtime.ManagementKeyEnv, nil
}

// VerifyHostObservationPath rejects any credential path exposed by a native
// mount. Both the canonical host path and mount sources are resolved so a
// symlink alias cannot turn a host-only credential into a project file.
func VerifyHostObservationPath(policy Policy, runtimeKey, role, path string) error {
	pin, err := ContainmentProfilePin(policy, runtimeKey, role)
	if err != nil {
		return err
	}
	p, err := readContainmentProfile(pin)
	if err != nil {
		return err
	}
	return verifyHostObservationPath(p, path)
}

func verifyHostObservationPath(p NativeContainmentProfile, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Held("host_observer_path_unsafe")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || parent != filepath.Dir(path) {
		return Held("host_observer_path_unsafe")
	}
	sources := []string{p.WorktreeRoot, p.ScratchRoot}
	for _, mount := range p.ReadOnly {
		sources = append(sources, mount.Source)
	}
	for _, source := range sources {
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil || path == source || containedPath(source, path) || path == resolved || containedPath(resolved, path) {
			return Held("host_observer_path_unsafe")
		}
	}
	return nil
}
