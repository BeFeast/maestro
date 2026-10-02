package aiexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var fixtureDestination = NativeForgejoDestination{Host: "forge.example.test", Repository: "acme/widget"}

const fixtureOrigin = "https://forge.example.test/acme/widget.git"

func fixtureContainmentProfile(repo string) NativeContainmentProfile {
	return NativeContainmentProfile{
		Version: 1, ProjectID: "fixture-project", UID: 1001, GID: 1001,
		Namespace: "/run/netns/fixture", NamespaceIno: 7, RulesSHA256: digest([]byte("rules")),
		GatewayURL: "http://192.0.2.1:8317", ForgejoIP: "192.0.2.2", ForgejoRepository: repo,
		ClaimDir: "/var/lib/maestro/native-claims/1001", WorktreeRoot: "/srv/fixture/worktrees", ScratchRoot: "/srv/fixture/scratch",
		MemoryMaxMB: 512,
		ReadOnly:    []NativeReadOnlyMount{{"/usr", "/usr"}, {"/etc/hosts", "/etc/hosts"}, {"/etc/passwd", "/etc/passwd"}},
	}
}

func requireHold(t *testing.T, err error, code string) {
	t.Helper()
	var hold *Hold
	if !errors.As(err, &hold) || hold.Code != code {
		t.Fatalf("want hold %s, got %v", code, err)
	}
}

func TestContainmentProfileRepositoryIsPinnedByProfileNotOrganization(t *testing.T) {
	// "BeFeast/maestro" is the shape every profile provisioned under the former
	// organization-prefix rule carries; those stay valid without regeneration.
	for _, repo := range []string{"acme/widget", "Acme-Org/widget_2.svc", "BeFeast/maestro"} {
		if err := validateContainmentProfile(fixtureContainmentProfile(repo)); err != nil {
			t.Fatalf("profile repository %q rejected: %v", repo, err)
		}
	}
	for _, repo := range []string{"", "acme", "acme/", "/widget", "acme/widget/extra", "acme/../widget", "acme/..", "../widget", "acme/wid get", "acme/widget?x", "acme/widget#x", "acme/wid%67et", "acme/widget\n", `acme\widget`, "acme/widget\x00"} {
		requireHold(t, validateContainmentProfile(fixtureContainmentProfile(repo)), "containment_forgejo_repository_invalid")
	}
}

func TestNativeForgejoOriginComesOnlyFromCanonicalBaseURL(t *testing.T) {
	for _, base := range []string{"https://forge.example.test", "https://forge.example.test/"} {
		if origin, err := NativeForgejoOrigin(base, "acme/widget"); err != nil || origin != fixtureOrigin {
			t.Fatalf("base %q: origin=%q err=%v", base, origin, err)
		}
	}
	for _, base := range []string{
		"", "forge.example.test", "//forge.example.test", "http://forge.example.test", "HTTPS://forge.example.test", "ssh://forge.example.test",
		"https://FORGE.example.test", "https://Forge.Example.Test", "https://forge.example.test.", "https://forge..example.test", "https://-forge.example.test",
		"https://forge.example.test:443", "https://forge.example.test:8443", "https://forge.example.test:",
		"https://user@forge.example.test", "https://user:pw@forge.example.test",
		"https://forge.example.test/forgejo", "https://forge.example.test//", "https://forge.example.test?x=1", "https://forge.example.test?", "https://forge.example.test#x",
		"https://forge%2eexample.test", "https://[2001:db8::1]", " https://forge.example.test", "https://forge.example.test ",
	} {
		if origin, err := NativeForgejoOrigin(base, "acme/widget"); err == nil {
			t.Fatalf("base %q accepted as %q", base, origin)
		}
	}
	for _, repo := range []string{"", "acme", "acme/widget/x", "../widget", "acme/..", "acme/widget?x"} {
		if origin, err := NativeForgejoOrigin("https://forge.example.test", repo); err == nil {
			t.Fatalf("repository %q accepted as %q", repo, origin)
		}
	}
}

func TestParseNativeForgejoOriginRejectsLookalikes(t *testing.T) {
	if d, err := ParseNativeForgejoOrigin(fixtureOrigin); err != nil || d != fixtureDestination {
		t.Fatalf("canonical origin: %+v %v", d, err)
	}
	for _, origin := range []string{
		"", "https://forge.example.test/acme/widget", "https://forge.example.test/acme/widget.git/", "https://forge.example.test//acme/widget.git",
		"http://forge.example.test/acme/widget.git", "ssh://forge.example.test/acme/widget.git", "HTTPS://forge.example.test/acme/widget.git",
		"https://forge.example.test:443/acme/widget.git", "https://forge.example.test:8443/acme/widget.git",
		"https://user@forge.example.test/acme/widget.git", "https://FORGE.example.test/acme/widget.git", "https://forge.example.test./acme/widget.git",
		"https://forge.example.test/acme/widget.git?x", "https://forge.example.test/acme/widget.git#x", "https://forge.example.test/acme/sub/widget.git",
		"https://forge.example.test/acme/wid%67et.git", "https://forge.example.test/acme/../widget.git",
	} {
		if d, err := ParseNativeForgejoOrigin(origin); err == nil {
			t.Fatalf("origin %q accepted as %+v", origin, d)
		}
	}
}

func TestNativeForgejoCredentialAnswersOnlyConfiguredHostAndRepository(t *testing.T) {
	request := func(protocol, host, path string) string {
		return "protocol=" + protocol + "\nhost=" + host + "\npath=" + path + "\n\n"
	}
	var out bytes.Buffer
	if err := NativeForgejoCredential("get", fixtureDestination, "secret", strings.NewReader(request("https", "forge.example.test", "acme/widget.git")), &out); err != nil || !strings.Contains(out.String(), "password=secret\n") {
		t.Fatalf("configured destination refused: %q %v", out.String(), err)
	}
	for name, r := range map[string]string{
		"subdomain":     request("https", "evil.forge.example.test", "acme/widget.git"),
		"suffix":        request("https", "forge.example.test.evil.test", "acme/widget.git"),
		"default port":  request("https", "forge.example.test:443", "acme/widget.git"),
		"other port":    request("https", "forge.example.test:8443", "acme/widget.git"),
		"http":          request("http", "forge.example.test", "acme/widget.git"),
		"other org":     request("https", "forge.example.test", "other/widget.git"),
		"other repo":    request("https", "forge.example.test", "acme/gadget.git"),
		"host case":     request("https", "FORGE.example.test", "acme/widget.git"),
		"org case":      request("https", "forge.example.test", "Acme/widget.git"),
		"trailing dot":  request("https", "forge.example.test.", "acme/widget.git"),
		"userinfo host": request("https", "user@forge.example.test", "acme/widget.git"),
		"bare path":     request("https", "forge.example.test", "acme/widget"),
	} {
		out.Reset()
		if err := NativeForgejoCredential("get", fixtureDestination, "secret", strings.NewReader(r), &out); err == nil || out.Len() != 0 {
			t.Fatalf("%s: credential disclosed: %q %v", name, out.String(), err)
		}
	}
	// A non-canonical destination answers nothing, not even an exact echo.
	for _, d := range []NativeForgejoDestination{{"FORGE.example.test", "acme/widget"}, {"forge.example.test.", "acme/widget"}, {"forge.example.test:8443", "acme/widget"}, {"", "acme/widget"}, {"forge.example.test", ""}, {"forge.example.test", "acme"}} {
		out.Reset()
		if err := NativeForgejoCredential("get", d, "secret", strings.NewReader(request("https", d.Host, d.Repository+".git")), &out); err == nil || out.Len() != 0 {
			t.Fatalf("non-canonical destination %+v answered: %q", d, out.String())
		}
	}
}

func TestPinnedNativeForgejoDestinationRequiresProfileAndCloneAgreement(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	clone := func(name, origin string) string {
		return write(name, NativeCloneIdentity{Version: 1, Parent: "/fixture/parent", Worktree: "/fixture/worktree", Origin: origin, BaseCommit: strings.Repeat("a", 40)})
	}
	profile := write("profile.json", fixtureContainmentProfile("acme/widget"))
	pinned := clone("clone.json", fixtureOrigin)
	if d, err := pinnedNativeForgejoDestination(profile, pinned, "acme/widget"); err != nil || d != fixtureDestination {
		t.Fatalf("pinned destination: %+v %v", d, err)
	}
	// The helper environment can only repeat the pinned repository.
	for _, env := range []string{"", "acme/other", "other/widget", "Acme/widget", "acme/widget.git"} {
		requireHold(t, func() error { _, err := pinnedNativeForgejoDestination(profile, pinned, env); return err }(), "containment_forgejo_repository_mismatch")
	}
	// The clone origin must name the profile's repository in canonical form.
	for i, origin := range []string{"https://forge.example.test/acme/other.git", "https://forge.example.test/other/widget.git", "https://forge.example.test/Acme/widget.git", "https://forge.example.test:8443/acme/widget.git", "http://forge.example.test/acme/widget.git", "https://user@forge.example.test/acme/widget.git", "https://forge.example.test./acme/widget.git", "https://FORGE.example.test/acme/widget.git", ""} {
		path := clone("clone-"+string(rune('a'+i))+".json", origin)
		requireHold(t, func() error { _, err := pinnedNativeForgejoDestination(profile, path, "acme/widget"); return err }(), "containment_forgejo_destination_invalid")
	}
	otherProfile := write("other-profile.json", fixtureContainmentProfile("acme/other"))
	requireHold(t, func() error { _, err := pinnedNativeForgejoDestination(otherProfile, pinned, "acme/other"); return err }(), "containment_forgejo_destination_invalid")
	// Absent or malformed pinned inputs fail closed.
	invalidProfile := fixtureContainmentProfile("acme/widget")
	invalidProfile.Version = 2
	extra := filepath.Join(dir, "extra.json")
	if err := os.WriteFile(extra, []byte(`{"version":1,"parent":"/p","worktree":"/w","origin":"`+fixtureOrigin+`","base_commit":"x","host":"forge.example.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string][2]string{
		"missing clone":     {profile, filepath.Join(dir, "absent.json")},
		"clone directory":   {profile, dir},
		"clone version":     {profile, write("v2.json", NativeCloneIdentity{Version: 2, Origin: fixtureOrigin})},
		"clone extra field": {profile, extra},
		"missing profile":   {filepath.Join(dir, "absent-profile.json"), pinned},
		"invalid profile":   {write("invalid-profile.json", invalidProfile), pinned},
	} {
		if d, err := pinnedNativeForgejoDestination(paths[0], paths[1], "acme/widget"); err == nil {
			t.Fatalf("%s: accepted %+v", name, d)
		}
	}
}

func TestNativeForgejoPullRequestTargetsOnlyPinnedDestination(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	htmlURL := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Host+" "+r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		url := htmlURL
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": 7, "html_url": url})
	}))
	defer server.Close()
	// Dial the fixture for every name; the request still names the pinned host.
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	create := func(d NativeForgejoDestination, response string) (string, error) {
		mu.Lock()
		htmlURL = response
		mu.Unlock()
		var out bytes.Buffer
		err := createNativeForgejoPullRequest(d, "synthetic-token", strings.NewReader(`{"title":"t","body":"b","head":"feat/x","base":"main"}`), &out, transport)
		return out.String(), err
	}
	out, err := create(fixtureDestination, "https://forge.example.test/acme/widget/pulls/7")
	if err != nil || !strings.Contains(out, `"number":7`) {
		t.Fatalf("pinned delivery: %q %v", out, err)
	}
	if len(seen) != 1 || seen[0] != "forge.example.test POST /api/v1/repos/acme/widget/pulls token synthetic-token" {
		t.Fatalf("request left the pinned destination: %q", seen)
	}
	for _, lookalike := range []string{
		"https://evil.forge.example.test/acme/widget/pulls/7", "https://forge.example.test.evil.test/acme/widget/pulls/7",
		"https://forge.example.test:8443/acme/widget/pulls/7", "http://forge.example.test/acme/widget/pulls/7",
		"https://forge.example.test/other/widget/pulls/7", "https://forge.example.test/Acme/widget/pulls/7",
		"https://FORGE.example.test/acme/widget/pulls/7", "https://forge.example.test./acme/widget/pulls/7",
	} {
		if out, err := create(fixtureDestination, lookalike); err == nil || out != "" {
			t.Fatalf("response URL %q accepted: %q", lookalike, out)
		}
	}
	mu.Lock()
	seen = nil
	mu.Unlock()
	for _, d := range []NativeForgejoDestination{{"", "acme/widget"}, {"FORGE.example.test", "acme/widget"}, {"forge.example.test.", "acme/widget"}, {"forge.example.test:443", "acme/widget"}, {"forge.example.test", "acme"}, {"forge.example.test", "acme/widget/x"}} {
		if _, err := create(d, "https://"+d.Host+"/"+d.Repository+"/pulls/7"); err == nil {
			t.Fatalf("non-canonical destination %+v delivered", d)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("non-canonical destination reached the network: %q", seen)
	}
}
