package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonProjectFlags(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_DAEMON_PROJECT_FLAGS") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				daemonCmd(os.Args[i+1:])
				return
			}
		}
		t.Fatal("missing child command arguments")
	}
	for _, tc := range []struct {
		name, want string
		args       []string
		invalid    bool
	}{
		{name: "empty", args: []string{"--project", ""}, want: "exact non-empty store row name", invalid: true},
		{name: "duplicate", args: []string{"--project", "alpha", "--project", "alpha"}, want: `duplicate --project "alpha"`, invalid: true},
		{name: "repeatable", args: []string{"--project", "alpha", "--project", "beta"}, want: `--project "alpha" is not in the config store`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "config.db")
			args := []string{"-test.run=^TestDaemonProjectFlags$", "--", "--store", db}
			cmd := exec.Command(os.Args[0], append(args, tc.args...)...)
			cmd.Env = append(os.Environ(), "MAESTRO_TEST_DAEMON_PROJECT_FLAGS=1")
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("command error=%v output=%s, want %q", err, output, tc.want)
			}
			if tc.invalid {
				if _, err := os.Stat(db); !os.IsNotExist(err) {
					t.Fatalf("invalid flags opened config store: %v", err)
				}
			}
		})
	}
}
