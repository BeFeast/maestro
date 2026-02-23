package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse_SessionPrefixDefault(t *testing.T) {
	yaml := `repo: BeFeast/panoptikon`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionPrefix != "pan" {
		t.Errorf("expected session_prefix=pan, got %q", cfg.SessionPrefix)
	}
}

func TestParse_SessionPrefixExplicit(t *testing.T) {
	yaml := `
repo: BeFeast/panoptikon
session_prefix: myapp
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionPrefix != "myapp" {
		t.Errorf("expected session_prefix=myapp, got %q", cfg.SessionPrefix)
	}
}

func TestParse_SessionPrefixShortRepoName(t *testing.T) {
	yaml := `repo: user/ab`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionPrefix != "ab" {
		t.Errorf("expected session_prefix=ab, got %q", cfg.SessionPrefix)
	}
}

func TestParse_StateDirDefault(t *testing.T) {
	yaml := `repo: BeFeast/panoptikon`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	home := os.Getenv("HOME")
	// Default should be ~/.maestro/<md5-hash>
	if !filepath.HasPrefix(cfg.StateDir, filepath.Join(home, ".maestro")) {
		t.Errorf("expected state_dir under ~/.maestro, got %q", cfg.StateDir)
	}
	if cfg.StateDir == "" {
		t.Error("state_dir should not be empty")
	}
}

func TestParse_StateDirExplicit(t *testing.T) {
	yaml := `
repo: BeFeast/panoptikon
state_dir: /tmp/maestro-test
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StateDir != "/tmp/maestro-test" {
		t.Errorf("expected state_dir=/tmp/maestro-test, got %q", cfg.StateDir)
	}
}

func TestParse_StateDirExpandsHome(t *testing.T) {
	yaml := `
repo: BeFeast/panoptikon
state_dir: ~/.maestro/panoptikon
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	home := os.Getenv("HOME")
	expected := filepath.Join(home, ".maestro/panoptikon")
	if cfg.StateDir != expected {
		t.Errorf("expected state_dir=%s, got %q", expected, cfg.StateDir)
	}
}

func TestParse_DifferentReposDifferentStateDirs(t *testing.T) {
	yaml1 := `repo: BeFeast/panoptikon`
	yaml2 := `repo: BeFeast/myapp`

	cfg1, err := parse([]byte(yaml1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg2, err := parse([]byte(yaml2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg1.StateDir == cfg2.StateDir {
		t.Errorf("different repos should have different default state_dirs, both got %q", cfg1.StateDir)
	}
}
