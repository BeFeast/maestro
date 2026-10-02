package aiexecution

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeSharedBubblewrapActualFilesystemAndCompiler(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for native mount/compiler kernel fixture")
	}
	if _, err := os.Stat("/usr/bin/bwrap"); err != nil {
		t.Skip("bubblewrap unavailable")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	scratch := filepath.Join(dir, "scratch")
	for _, path := range []string{work, filepath.Join(work, ".git/objects/info"), filepath.Join(work, ".git/hooks"), filepath.Join(scratch, "tmp/go")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"config": "[core]\nrepositoryformatversion=0\n", "maestro-native-clone.json": "{}", "commondir": ".\n", "config.worktree": "", "objects/info/alternates": "", "objects/info/http-alternates": ""} {
		if err := os.WriteFile(filepath.Join(work, ".git", name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	profile := filepath.Join(dir, "profile.json")
	os.WriteFile(profile, []byte("{}"), 0600)
	// Probe the invoking user's real home, resolved at runtime, for host
	// credentials that must not be visible inside the sandbox.
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) || filepath.Clean(account.HomeDir) == "/" {
		t.Fatalf("invoking user's home unavailable: %v", err)
	}
	home := filepath.Clean(account.HomeDir)
	secret := filepath.Join(dir, "host-secret")
	os.WriteFile(secret, []byte("must-not-be-visible"), 0600)
	proof := func(path string) FileProof {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return FileProof{path, digest(b)}
	}
	p := NativeContainmentProfile{UID: uint32(os.Getuid()), Bubblewrap: proof("/usr/bin/bwrap"), Maestro: proof("/usr/bin/true"), Harness: proof("/usr/bin/true"), ReadOnly: []NativeReadOnlyMount{{"/usr", "/usr"}, {"/usr/lib", "/lib"}, {"/usr/lib64", "/lib64"}}}
	e := nativeLaunchEnvelope{Role: "implementer", Worktree: work, Scratch: scratch, Profile: proof(profile)}
	args, err := nativeBubblewrapArguments(p, e)
	if err != nil {
		t.Fatal(err)
	}
	// Invoke the production mount/seccomp builder, replacing only native entry
	// with deterministic offline probes. No systemd proof is claimed here.
	script := `set -eu
 test ! -e "$1"
 test ! -e /run/docker.sock
 test ! -e "$2/.ssh"
 test ! -e "/proc/1/root$2/.ssh"
 if unshare -Ur true 2>/dev/null; then exit 81; fi
 if (echo poison > /work/.git/config) 2>/dev/null; then exit 82; fi
 if (echo poison > /work/.git/commondir) 2>/dev/null; then exit 83; fi
 printf 'package probe\nimport "testing"\nfunc TestLocal(t *testing.T) {}\n' > /work/probe_test.go
 printf 'module probe\ngo 1.23\n' > /work/go.mod
 HOME=/home/native PATH=/usr/local/go/bin:/usr/bin:/bin TMPDIR=/tmp GOTMPDIR=/tmp/go GOCACHE=/scratch/cache GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local /usr/local/go/bin/go test ./...
 echo native-contained
 `
	args = append([]string{"--unshare-net"}, args[:len(args)-2]...)
	args = append(args, "/bin/bash", "-c", script, "probe", secret, home)
	filter, err := nativeSeccompFile()
	if err != nil {
		t.Fatal(err)
	}
	defer filter.Close()
	cmd := exec.Command(p.Bubblewrap.Path, args...)
	cmd.ExtraFiles = []*os.File{filter}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "native-contained") {
		t.Fatalf("actual native boundary: %s %v", out, err)
	}
}
