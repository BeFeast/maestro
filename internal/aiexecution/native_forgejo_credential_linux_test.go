package aiexecution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeForgejoCredentialProjectIsolationAndSnapshot(t *testing.T) {
	// /tmp is intentionally rejected by the production parent-path check.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// testing.T creates its per-test parent with MkdirAll; normalize that
	// test-owned ancestor too when the caller uses a permissive umask.
	if err := os.Chmod(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	profiles := make([]NativeContainmentProfile, 0, 2)
	for _, repo := range []string{"hedroom", "halenote"} {
		b, _ := json.Marshal(map[string]any{"version": 1, "repository": "BeFeast/" + repo, "token": repo + "-private"})
		path := filepath.Join(dir, repo+".json")
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		p := NativeContainmentProfile{UID: uint32(os.Getuid()), ForgejoRepository: "BeFeast/" + repo, ForgejoCredential: FileProof{Path: path, SHA256: digest(b)}, ForgejoTokenSHA256: digest([]byte("maestro-native-forgejo:v1\x00" + repo + "-private"))}
		profiles = append(profiles, p)
	}
	if strings.HasPrefix(dir, "/tmp/") || strings.HasPrefix(dir, "/var/tmp/") {
		if _, err := readNativeForgejoCredential(profiles[0]); err == nil {
			t.Fatal("world-writable ancestor accepted")
		}
		t.Skip("private TMPDIR required for positive credential path fixture")
	}
	for _, p := range profiles {
		token, err := readNativeForgejoCredential(p)
		if err != nil || token != strings.TrimPrefix(p.ForgejoRepository, "BeFeast/")+"-private" {
			t.Fatal("wrong profile credential", err)
		}
	}
	wrong := profiles[0]
	wrong.ForgejoCredential = profiles[1].ForgejoCredential
	if _, err := readNativeForgejoCredential(wrong); err == nil {
		t.Fatal("cross-project credential accepted")
	}
	token, _ := readNativeForgejoCredential(profiles[0])
	contained := ContainedNativeCommand{redactionSecrets: []string{token}}
	if err := os.WriteFile(profiles[0].ForgejoCredential.Path, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeForgejoCredential(profiles[0]); err == nil {
		t.Fatal("rotated credential accepted against stale pin")
	}
	secrets := contained.RedactionSecrets()
	if len(secrets) != 1 || secrets[0] != token {
		t.Fatal("lost original redaction snapshot")
	}
	secrets[0] = "changed"
	if contained.RedactionSecrets()[0] != token {
		t.Fatal("mutable redaction snapshot")
	}
	if err := os.Chmod(profiles[1].ForgejoCredential.Path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeForgejoCredential(profiles[1]); err == nil {
		t.Fatal("public credential accepted")
	}
	if err := os.Chmod(profiles[1].ForgejoCredential.Path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(profiles[1].ForgejoCredential.Path, link); err != nil {
		t.Fatal(err)
	}
	unsafe := profiles[1]
	unsafe.ForgejoCredential.Path = link
	if _, err := readNativeForgejoCredential(unsafe); err == nil {
		t.Fatal("symlink credential accepted")
	}
	if err := os.Link(profiles[1].ForgejoCredential.Path, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeForgejoCredential(profiles[1]); err == nil {
		t.Fatal("hardlinked credential accepted")
	}
}
