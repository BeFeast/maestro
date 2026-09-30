package aiexecution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func nativeGitFixture(t *testing.T) string {
	t.Helper()
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for native Git kernel fixture")
	}
	if _, err := os.Stat("/usr/bin/bwrap"); err != nil {
		t.Skip("bubblewrap unavailable")
	}
	repo := filepath.Join(t.TempDir(), "clone")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@localhost"}, {"commit", "--allow-empty", "-qm", "initial"}} {
		if b, err := exec.Command("/usr/bin/git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("init: %v %s", err, b)
		}
	}
	if err := RegisterNativeGit(repo); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestNativeGitActualSandboxAllowsCommitsAndBlocksMetadataExfiltration(t *testing.T) {
	repo := nativeGitFixture(t)
	secretPath := filepath.Join(t.TempDir(), "host-secret")
	secret := "synthetic-host-only-secret-85bf"
	if err := os.WriteFile(secretPath, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source"), []byte("source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "source"}, {"commit", "-m", "contained commit"}, {"status", "--porcelain"}} {
		if b, err := NativeGitCommand(append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("positive Git: %v %s", err, b)
		}
	}
	for _, rel := range []string{"shallow", "packed-refs", "index", "config", "commondir"} {
		t.Run(rel, func(t *testing.T) {
			path := filepath.Join(repo, ".git", rel)
			prior, err := os.ReadFile(path)
			existed := err == nil
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink(secretPath, path); err != nil {
				t.Fatal(err)
			}
			defer func() {
				os.Remove(path)
				if existed {
					os.WriteFile(path, prior, 0600)
				}
			}()
			out, _ := NativeGitCommand("-C", repo, "status", "--porcelain").CombinedOutput()
			if strings.Contains(string(out), secret) {
				t.Fatal("host secret escaped through Git metadata")
			}
		})
	}
	// A malicious Git alias may execute a shell, but it is still inside the
	// same sandbox and cannot re-enter host namespaces or read host secrets.
	alias := "!if cat '" + secretPath + "' 2>/dev/null; then exit 81; fi; if unshare -Ur true 2>/dev/null; then exit 82; fi; echo contained"
	out, err := NativeGitCommand("-C", repo, "-c", "alias.probe="+alias, "probe").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "contained" {
		t.Fatalf("sandbox denial: %s %v", out, err)
	}
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	cmd := NativeGitCommand("-C", repo, "status")
	if cmd.Path == "git" || cmd.Path == "/usr/bin/git" {
		t.Fatal("deleted .git disabled containment")
	}
}

func TestNativeWorkspaceSymlinksNeverReadOrOverwriteHostFiles(t *testing.T) {
	repo := nativeGitFixture(t)
	host := filepath.Join(t.TempDir(), "host")
	os.WriteFile(host, []byte("preserve"), 0600)
	path := filepath.Join(repo, "CHECKPOINT.md")
	os.Symlink(host, path)
	if _, err := ReadWorkspaceFile(path); err == nil {
		t.Fatal("host read")
	}
	if err := WriteWorkspaceFile(path, []byte("overwrite"), 0600); err == nil {
		t.Fatal("host write")
	}
	got, _ := os.ReadFile(host)
	if string(got) != "preserve" {
		t.Fatal("host damaged")
	}
	os.Remove(path)
	if err := WriteWorkspaceFile(path, []byte("checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWorkspaceFile(path)
	if err != nil || string(got) != "checkpoint" {
		t.Fatal(string(got), err)
	}
	outside := t.TempDir()
	directoryLink := filepath.Join(repo, ".maestro")
	if err := os.Symlink(outside, directoryLink); err != nil {
		t.Fatal(err)
	}
	if err := MkdirWorkspaceAll(filepath.Join(directoryLink, "research"), 0700); err == nil {
		t.Fatal("directory symlink followed")
	}
	if _, err := os.Stat(filepath.Join(outside, "research")); !os.IsNotExist(err) {
		t.Fatal("created directory outside clone")
	}
	os.Remove(directoryLink)
	if err := MkdirWorkspaceAll(filepath.Join(directoryLink, "research"), 0700); err != nil {
		t.Fatal(err)
	}
}
