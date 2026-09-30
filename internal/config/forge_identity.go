package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// RepositoryIdentity binds a project to one configured forge instance and repo.
// SSH origin identity never supplies the HTTP fetch endpoint or a DNS alias.
type RepositoryIdentity struct {
	Kind    string
	BaseURL string
	Repo    string
}

func canonicalRepo(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", errors.New("repo must use the canonical owner/repository form")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("repo must use the canonical owner/repository form")
		}
		for _, c := range part {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
				return "", errors.New("repo must use the canonical owner/repository form")
			}
		}
	}
	return repo, nil
}

// CanonicalBaseURL contains no credentials, query, fragment, aliases or API path.
// Equivalent explicit/default web ports produce the same execution identity.
func (f ForgeConfig) CanonicalBaseURL() (string, error) {
	if f.EffectiveKind() == ForgeKindGitHub {
		if f.BaseURL != "" {
			return "", errors.New("GitHub identity cannot override the canonical base URL")
		}
		return "https://github.com", nil
	}
	if !f.IsForgejo() {
		return "", errors.New("unsupported forge identity")
	}
	base, err := url.Parse(f.BaseURL)
	if err != nil || base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.Opaque != "" || strings.ContainsAny(f.BaseURL, "?#\r\n\t ") || base.EscapedPath() != base.Path {
		return "", errors.New("forge identity requires a canonical credential-free HTTP(S) base URL")
	}
	prefix := strings.TrimSuffix(base.Path, "/")
	if strings.Contains(base.Path, "//") {
		return "", errors.New("forge identity has an ambiguous base path")
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "." || part == ".." {
			return "", errors.New("forge identity has an ambiguous base path")
		}
	}
	if strings.HasSuffix(prefix, "/api/v1") {
		return "", errors.New("forge identity requires an instance root, not an API URL")
	}
	port, ok := webPort(base)
	if !ok {
		return "", errors.New("forge identity has an invalid web port")
	}
	host := strings.ToLower(base.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (base.Scheme == "https" && port != "443") || (base.Scheme == "http" && port != "80") {
		host = net.JoinHostPort(strings.ToLower(base.Hostname()), port)
	}
	return base.Scheme + "://" + host + prefix, nil
}

func (f ForgeConfig) RepositoryIdentity(repo string) (RepositoryIdentity, error) {
	base, err := f.CanonicalBaseURL()
	if err != nil {
		return RepositoryIdentity{}, err
	}
	repo, err = canonicalRepo(repo)
	if err != nil {
		return RepositoryIdentity{}, err
	}
	return RepositoryIdentity{Kind: f.EffectiveKind(), BaseURL: base, Repo: repo}, nil
}

func (i RepositoryIdentity) FetchURL() string { return i.BaseURL + "/" + i.Repo + ".git" }

// MatchesOrigin is shared by onboarding and isolated delivery. Strict delivery
// excludes legacy GitHub HTTP/git transports; onboarding retains those accepted
// spellings. Forgejo HTTP(S) always binds scheme, effective port and base path.
func (i RepositoryIdentity) MatchesOrigin(remote string, strict bool) bool {
	r := strings.TrimSpace(remote)
	if strings.ContainsAny(r, "?#\r\n\t ") {
		return false
	}
	base, err := url.Parse(i.BaseURL)
	if err != nil || base.Hostname() == "" {
		return false
	}
	want := strings.ToLower(i.Repo)
	matchRepo := func(path string) bool { path = strings.TrimSuffix(strings.ToLower(path), ".git"); return path == want }
	if !strings.Contains(r, "://") && strings.HasPrefix(strings.ToLower(r), "git@") {
		host, path, ok := strings.Cut(r[len("git@"):], ":")
		return ok && strings.EqualFold(host, base.Hostname()) && matchRepo(path)
	}
	u, err := url.Parse(r)
	if err != nil || !strings.EqualFold(u.Hostname(), base.Hostname()) || u.Opaque != "" || u.EscapedPath() != u.Path || strings.HasSuffix(u.Host, ":") {
		return false
	}
	path := strings.TrimPrefix(u.Path, "/")
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.User != nil {
			return false
		}
		port, ok := webPort(u)
		if !ok {
			return false
		}
		if i.Kind == ForgeKindForgejo {
			basePort, _ := webPort(base)
			if !strings.EqualFold(u.Scheme, base.Scheme) || port != basePort {
				return false
			}
			prefix := strings.Trim(base.Path, "/")
			if prefix != "" {
				var ok bool
				path, ok = strings.CutPrefix(path, prefix+"/")
				if !ok {
					return false
				}
			}
		} else {
			if strict && !strings.EqualFold(u.Scheme, "https") {
				return false
			}
			if (strings.EqualFold(u.Scheme, "https") && port != "443") || (strings.EqualFold(u.Scheme, "http") && port != "80") {
				return false
			}
		}
	case "ssh":
		if u.User == nil || u.User.String() != "git" {
			return false
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 || (strict && i.Kind == ForgeKindGitHub && n != 22) {
				return false
			}
		}
	case "git":
		if strict || u.User != nil || u.Port() != "" {
			return false
		}
	default:
		return false
	}
	return matchRepo(path)
}

func webPort(u *url.URL) (string, bool) {
	if strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		return strconv.Itoa(n), true
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443", true
	}
	return "80", true
}
