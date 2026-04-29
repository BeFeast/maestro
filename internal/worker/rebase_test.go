package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeepBothSides_SimpleConflict(t *testing.T) {
	input := "line1\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> branch\nline2\n"
	got, changed, err := keepBothSides(input)
	if err != nil {
		t.Fatalf("keepBothSides: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	want := "line1\nours\ntheirs\nline2\n"
	if got != want {
		t.Fatalf("resolved content mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestKeepBothSides_Diff3Markers(t *testing.T) {
	input := "<<<<<<< HEAD\nours\n||||||| parent\nbase\n=======\ntheirs\n>>>>>>> branch\n"
	got, changed, err := keepBothSides(input)
	if err != nil {
		t.Fatalf("keepBothSides: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	want := "ours\ntheirs\n"
	if got != want {
		t.Fatalf("resolved content mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestKeepBothSides_NoConflict(t *testing.T) {
	input := "line1\nline2\n"
	got, changed, err := keepBothSides(input)
	if err != nil {
		t.Fatalf("keepBothSides: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false")
	}
	if got != input {
		t.Fatalf("content should remain unchanged\nwant:\n%s\n\ngot:\n%s", input, got)
	}
}

func TestKeepBothSides_UnterminatedConflict(t *testing.T) {
	input := "<<<<<<< HEAD\nours\n=======\ntheirs\n"
	_, _, err := keepBothSides(input)
	if err == nil {
		t.Fatal("expected error for unterminated conflict")
	}
}

func TestRestoreAllowedDirtyFiles_RestoresOnlyConfiguredPaths(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	gitTest(t, dir, "config", "user.name", "Test User")

	writeTestFile(t, dir, "ok-gobot", "original binary")
	writeTestFile(t, dir, "main.go", "package main\n")
	gitTest(t, dir, "add", "-f", "ok-gobot", "main.go")
	gitTest(t, dir, "commit", "-m", "initial")

	writeTestFile(t, dir, "ok-gobot", "rebuilt binary")
	writeTestFile(t, dir, "main.go", "package main\n\nfunc main() {}\n")

	if err := restoreAllowedDirtyFiles(dir, []string{"ok-gobot"}); err != nil {
		t.Fatalf("restoreAllowedDirtyFiles: %v", err)
	}

	if got := readTestFile(t, dir, "ok-gobot"); got != "original binary" {
		t.Fatalf("ok-gobot = %q, want restored original binary", got)
	}
	if got := readTestFile(t, dir, "main.go"); got != "package main\n\nfunc main() {}\n" {
		t.Fatalf("main.go = %q, want unrelated dirty file unchanged", got)
	}

	dirty, err := worktreeDirty(dir)
	if err != nil {
		t.Fatalf("worktreeDirty: %v", err)
	}
	if strings.Contains(dirty, "ok-gobot") {
		t.Fatalf("ok-gobot should be clean after restore, dirty status:\n%s", dirty)
	}
	if !strings.Contains(dirty, "main.go") {
		t.Fatalf("main.go should remain dirty, dirty status:\n%s", dirty)
	}
}

func TestRestoreAllowedDirtyFiles_CleansConfiguredUntrackedPaths(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	gitTest(t, dir, "config", "user.name", "Test User")

	writeTestFile(t, dir, "main.go", "package main\n")
	gitTest(t, dir, "add", "main.go")
	gitTest(t, dir, "commit", "-m", "initial")

	writeTestFile(t, dir, "build/artifact.bin", "artifact")

	if err := restoreAllowedDirtyFiles(dir, []string{"build"}); err != nil {
		t.Fatalf("restoreAllowedDirtyFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "artifact.bin")); !os.IsNotExist(err) {
		t.Fatalf("configured untracked artifact should be removed, stat err=%v", err)
	}
}

func TestNormalizedGitPaths_TrimsDeduplicatesAndUsesSlash(t *testing.T) {
	got := normalizedGitPaths([]string{" ok-gobot ", "build\\artifact.bin", "ok-gobot", ""})
	want := []string{"ok-gobot", "build/artifact.bin"}
	if len(got) != len(want) {
		t.Fatalf("normalizedGitPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizedGitPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}
