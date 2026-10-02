package aiexecution

import (
	"io"
	"net/url"
	"os"
	"strings"
)

// NativeCloneMarker names the daemon-written clone identity inside a native
// clone's .git directory. The native monitor binds it read-only into the
// sandbox, where the forge helpers read the pinned origin from it.
const NativeCloneMarker = "maestro-native-clone.json"

// NativeCloneIdentity is the clone identity the daemon records after it has
// checked the parent origin against the project's pinned forge base URL and
// repository.
type NativeCloneIdentity struct {
	Version    int    `json:"version"`
	Parent     string `json:"parent"`
	Worktree   string `json:"worktree"`
	Origin     string `json:"origin"`
	BaseCommit string `json:"base_commit"`
}

// In-sandbox locations of the two read-only inputs that select the forge
// destination. Both are bound by the native monitor; the worker can neither
// write nor remount them, and its environment selects nothing.
const (
	nativeSandboxProfilePath       = "/runtime/profile.json"
	nativeSandboxCloneIdentityPath = "/work/.git/" + NativeCloneMarker
)

// NativeForgejoDestination is the only forge repository a native credential or
// delivery helper may address. Host is a canonical lowercase DNS name without
// port; Repository is the exact owner/name pinned in the containment profile.
type NativeForgejoDestination struct {
	Host       string
	Repository string
}

func (d NativeForgejoDestination) valid() bool {
	return validNativeForgejoHost(d.Host) && validNativeForgejoRepository(d.Repository)
}

// Origin is the exact clone URL for the destination.
func (d NativeForgejoDestination) Origin() string {
	return "https://" + d.Host + "/" + d.Repository + ".git"
}

// validNativeForgejoHost accepts only a canonical lowercase DNS name. A port,
// userinfo, IP-literal brackets, a trailing dot, an empty label or any
// upper-case or non-ASCII byte is rejected rather than normalized.
func validNativeForgejoHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// validNativeForgejoRepository accepts exactly owner/name with Forgejo name
// characters; no further separators, dot segments or escapes.
func validNativeForgejoRepository(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && validNativeForgejoName(owner) && validNativeForgejoName(name)
}

func validNativeForgejoName(s string) bool {
	if s == "" || len(s) > 100 || strings.Contains(s, "..") || s == "." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// NativeForgejoOrigin derives the only origin a native clone may carry from the
// project's forge base URL and repository (both covered by the manifest's
// project config digest). It fails closed on anything but a bare https
// instance root: the base URL must be spelled exactly https://<host>[/], so
// userinfo, a port, a path, a query, a fragment or any spelling a URL parser
// would normalize is rejected.
func NativeForgejoOrigin(baseURL, repo string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (baseURL != "https://"+u.Host && baseURL != "https://"+u.Host+"/") {
		return "", Held("containment_forgejo_destination_invalid")
	}
	d := NativeForgejoDestination{Host: u.Host, Repository: repo}
	if !d.valid() {
		return "", Held("containment_forgejo_destination_invalid")
	}
	return d.Origin(), nil
}

// ParseNativeForgejoOrigin accepts exactly https://<host>/<owner>/<name>.git
// in the canonical form NativeForgejoOrigin produces.
func ParseNativeForgejoOrigin(origin string) (NativeForgejoDestination, error) {
	rest, ok := strings.CutPrefix(origin, "https://")
	host, path, found := strings.Cut(rest, "/")
	repo, git := strings.CutSuffix(path, ".git")
	d := NativeForgejoDestination{Host: host, Repository: repo}
	if !ok || !found || !git || !d.valid() || d.Origin() != origin {
		return NativeForgejoDestination{}, Held("containment_forgejo_destination_invalid")
	}
	return d, nil
}

// pinnedNativeForgejoDestination joins the root-owned profile's repository
// with the clone identity's origin. They must agree exactly; the helper's
// environment may only repeat the pinned repository, never select another.
func pinnedNativeForgejoDestination(profilePath, clonePath, envRepository string) (NativeForgejoDestination, error) {
	var p NativeContainmentProfile
	b, err := readNativeSandboxFile(profilePath, 128<<10)
	if err != nil || DecodeStrict(b, &p) != nil || validateContainmentProfile(p) != nil {
		return NativeForgejoDestination{}, Held("containment_forgejo_destination_unavailable")
	}
	var identity NativeCloneIdentity
	b, err = readNativeSandboxFile(clonePath, 16<<10)
	if err != nil || DecodeStrict(b, &identity) != nil || identity.Version != 1 {
		return NativeForgejoDestination{}, Held("containment_forgejo_destination_unavailable")
	}
	d, err := ParseNativeForgejoOrigin(identity.Origin)
	if err != nil || d.Repository != p.ForgejoRepository {
		return NativeForgejoDestination{}, Held("containment_forgejo_destination_invalid")
	}
	if envRepository != d.Repository {
		return NativeForgejoDestination{}, Held("containment_forgejo_repository_mismatch")
	}
	return d, nil
}

func readNativeSandboxFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, Held("containment_forgejo_destination_unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, Held("containment_forgejo_destination_unavailable")
	}
	return b, nil
}

// RunNativeForgejoCredential is the in-sandbox Git credential helper entry.
func RunNativeForgejoCredential(operation, envRepository, token string, input io.Reader, output io.Writer) error {
	if operation != "get" {
		return nil
	}
	d, err := pinnedNativeForgejoDestination(nativeSandboxProfilePath, nativeSandboxCloneIdentityPath, envRepository)
	if err != nil {
		return err
	}
	return NativeForgejoCredential(operation, d, token, input, output)
}

// RunNativeForgejoPullRequest is the in-sandbox pull-request delivery entry.
func RunNativeForgejoPullRequest(envRepository, token string, input io.Reader, output io.Writer) error {
	d, err := pinnedNativeForgejoDestination(nativeSandboxProfilePath, nativeSandboxCloneIdentityPath, envRepository)
	if err != nil {
		return err
	}
	return NativeForgejoCreatePullRequest(d, token, input, output)
}
