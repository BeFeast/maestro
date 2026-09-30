package selfdeploy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPromotionFencePrecedesScriptStagingAndLauncher(t *testing.T) {
	for _, policy := range []string{"explicit", "typo"} {
		t.Run(policy, func(t *testing.T) {
			cfg := triggerTestConfig(t)
			cfg.SelfDeploy.PromotionPolicy = policy
			cfg.StateDir = filepath.Join(t.TempDir(), "untouched")
			// A fake checkout makes unguarded preparation reach git fetch/show.
			if err := os.Mkdir(filepath.Join(cfg.LocalPath, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			called := filepath.Join(t.TempDir(), "called")
			t.Setenv("PROMOTION_TEST_CALLED", called)
			for _, name := range []string{"git", "systemd-run", "sudo"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s\\n' called >> \"$PROMOTION_TEST_CALLED\"\nexit 1\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			name, args, err := TriggerCommand(cfg, 1190, time.Now())
			if err == nil || name != "" || args != nil {
				t.Fatalf("blocked command: name=%q args=%v err=%v", name, args, err)
			}
			err = Trigger(cfg, 1190)
			if err == nil || (policy == "explicit" && !errors.Is(err, ErrExplicitPromotionRequired)) {
				t.Fatalf("blocked trigger error = %v", err)
			}
			if _, err := os.Stat(called); !os.IsNotExist(err) {
				t.Fatalf("blocked trigger invoked a subprocess: %v", err)
			}
			if _, err := os.Stat(cfg.StateDir); !os.IsNotExist(err) {
				t.Fatalf("blocked trigger created state directory: %v", err)
			}
		})
	}
}

func TestAutomaticAndLegacyPoliciesReachLauncher(t *testing.T) {
	for _, policy := range []string{"", "automatic"} {
		t.Run(policy, func(t *testing.T) {
			cfg := triggerTestConfig(t)
			cfg.SelfDeploy.PromotionPolicy = policy
			cfg.SelfDeploy.Script = filepath.Join(cfg.LocalPath, "scripts", "self-deploy.sh")
			bin := t.TempDir()
			called := filepath.Join(t.TempDir(), "called")
			t.Setenv("PROMOTION_TEST_CALLED", called)
			if err := os.WriteFile(filepath.Join(bin, "systemd-run"), []byte("#!/bin/sh\nprintf '%s\\n' called >> \"$PROMOTION_TEST_CALLED\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := Trigger(cfg, 1190); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(called); err != nil || string(got) != "called\n" {
				t.Fatalf("automatic launcher calls = %q, err=%v", got, err)
			}
		})
	}
}
