package controllerowner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestControllerOwnerHelper(t *testing.T) {
	mode := os.Getenv("MAESTRO_OWNER_TEST_HELPER")
	if mode == "" {
		return
	}
	if mode == "wait-after-exec" {
		fmt.Println("exec-ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	lease, err := acquireAtHome(os.Getenv("MAESTRO_OWNER_TEST_HOME"), os.Geteuid())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if mode == "exec" {
		_ = os.Setenv("MAESTRO_OWNER_TEST_HELPER", "wait-after-exec")
		exe, _ := os.Executable()
		if err := unix.Exec(exe, []string{exe, "-test.run=^TestControllerOwnerHelper$"}, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	fmt.Println("owned")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = lease.Close()
	os.Exit(0)
}

func ownerHelper(t *testing.T, home, mode string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestControllerOwnerHelper$")
	cmd.Env = append(os.Environ(), "MAESTRO_OWNER_TEST_HELPER="+mode, "MAESTRO_OWNER_TEST_HOME="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- strings.TrimSpace(line) }()
	want := "owned"
	if mode == "exec" {
		want = "exec-ready"
	}
	select {
	case got := <-ready:
		if got != want {
			t.Fatalf("helper readiness = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ownership helper did not become ready")
	}
	return cmd, stdin
}

func TestControllerOwnerDuplicateReleaseAndCrash(t *testing.T) {
	for _, ending := range []string{"close", "crash"} {
		t.Run(ending, func(t *testing.T) {
			home := t.TempDir()
			cmd, stdin := ownerHelper(t, home, "hold")
			lockPath := filepath.Join(home, registryName, lockName)
			before, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if lease, err := acquireAtHome(home, os.Geteuid()); !errors.Is(err, ErrAlreadyOwned) || lease != nil {
				if lease != nil {
					_ = lease.Close()
				}
				t.Fatalf("duplicate lease=%v err=%v", lease, err)
			}
			if ending == "crash" {
				_ = cmd.Process.Kill()
			} else {
				_ = stdin.Close()
			}
			_ = cmd.Wait()
			lease, err := acquireAtHome(home, os.Geteuid())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			after, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("release/recovery replaced the persistent lock inode")
			}
		})
	}
}

func TestControllerOwnerCloseOnExec(t *testing.T) {
	home := t.TempDir()
	cmd, _ := ownerHelper(t, home, "exec")
	if err := cmd.Process.Signal(unix.Signal(0)); err != nil {
		t.Fatalf("exec child no longer alive: %v", err)
	}
	lease, err := acquireAtHome(home, os.Geteuid())
	if err != nil {
		t.Fatalf("exec inherited controller ownership: %v", err)
	}
	defer lease.Close()
}

func TestControllerOwnerUsesOSAccountNotEnvironment(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	uid := os.Geteuid()
	lookup := func(id string) (*user.User, error) {
		if id != strconv.Itoa(uid) {
			t.Fatalf("lookup UID=%q", id)
		}
		return &user.User{Uid: id, HomeDir: home}, nil
	}
	lease, err := acquireForUID(uid, lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	for _, name := range []string{"HOME", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "MAESTRO_HOME"} {
		t.Setenv(name, other)
	}
	if next, err := acquireForUID(uid, lookup); !errors.Is(err, ErrAlreadyOwned) || next != nil {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("environment changed ownership namespace: lease=%v err=%v", next, err)
	}
	entries, err := os.ReadDir(other)
	if err != nil || len(entries) != 0 {
		t.Fatal("environment directory was written")
	}
}

func TestControllerOwnerUnsafeResourcesFailWithoutOverwriting(t *testing.T) {
	for _, scenario := range []string{"home-symlink", "home-parent-symlink", "home-parent-writable", "registry-symlink", "registry-public", "lock-symlink", "lock-public", "lock-fifo", "lock-hardlink", "wrong-owner"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, registryName)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(t.TempDir(), "preserved")
			if err := os.WriteFile(payload, []byte("existing user data"), 0600); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(dir, lockName)
			uid := os.Geteuid()
			switch scenario {
			case "home-symlink":
				alias := filepath.Join(t.TempDir(), "home")
				if err := os.Symlink(home, alias); err != nil {
					t.Fatal(err)
				}
				home = alias
			case "home-parent-symlink":
				alias := filepath.Join(t.TempDir(), "parent")
				if err := os.Symlink(filepath.Dir(home), alias); err != nil {
					t.Fatal(err)
				}
				home = filepath.Join(alias, filepath.Base(home))
			case "home-parent-writable":
				parent := t.TempDir()
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
				home = filepath.Join(parent, "account")
				if err := os.Mkdir(home, 0700); err != nil {
					t.Fatal(err)
				}
			case "registry-symlink":
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(payload), dir); err != nil {
					t.Fatal(err)
				}
			case "registry-public":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "lock-symlink":
				if err := os.Symlink(payload, lock); err != nil {
					t.Fatal(err)
				}
			case "lock-public":
				if err := os.WriteFile(lock, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(lock, 0644); err != nil {
					t.Fatal(err)
				}
			case "lock-fifo":
				if err := unix.Mkfifo(lock, 0600); err != nil {
					t.Fatal(err)
				}
			case "lock-hardlink":
				if err := os.Link(payload, lock); err != nil {
					t.Fatal(err)
				}
			case "wrong-owner":
				uid++ // fixture expects another owner; no chown required
			}
			lease, err := acquireAtHome(home, uid)
			if err == nil || lease != nil {
				if lease != nil {
					_ = lease.Close()
				}
				t.Fatalf("unsafe resource admitted: %v", scenario)
			}
			if got, err := os.ReadFile(payload); err != nil || string(got) != "existing user data" {
				t.Fatal("existing user data overwritten")
			}
		})
	}
}

func TestControllerOwnerPreservesExistingLockContentsAndClosesIdempotently(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, registryName)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, lockName)
	if err := os.WriteFile(path, []byte("old diagnostic data"), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireAtHome(home, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "old diagnostic data" {
		t.Fatal("lock contents changed")
	}
	next, err := acquireAtHome(home, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
}
