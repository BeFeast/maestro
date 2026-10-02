package aiexecution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// An explicit installed-profile read exercises the unprivileged observer's
// sudo/nsenter access to its retained descriptor without changing the namespace.
func TestObservePinnedNetworkNamespaceInstalled(t *testing.T) {
	path := os.Getenv("MAESTRO_NATIVE_OBSERVE_PROFILE")
	if path == "" {
		t.Skip("set MAESTRO_NATIVE_OBSERVE_PROFILE for installed read-only observation")
	}
	if os.Geteuid() == 0 {
		t.Fatal("run as the unprivileged profile owner to exercise sudo access")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p NativeContainmentProfile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.UID != uint32(os.Geteuid()) {
		t.Fatal("profile owner differs from observer")
	}
	if err := observeContainmentNetwork(p); err != nil {
		t.Fatal(err)
	}
}

// Actual nsfs bind mounts exercise the overflow-owner case on unprivileged LXC.
// Only a temporary /run directory and an anonymous child netns are used.
func TestPinnedNetworkNamespaceKernel(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("requires root and MAESTRO_NATIVE_KERNEL_TESTS=1 for private nsfs fixture")
	}
	dir, err := os.MkdirTemp("/run", "maestro-ns-validation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "unshare", "--net", "sleep", "15")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	self, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	selfStat := self.Sys().(*syscall.Stat_t)
	source := fmt.Sprintf("/proc/%d/ns/net", child.Process.Pid)
	for deadline := time.Now().Add(3 * time.Second); ; {
		st, err := os.Stat(source)
		if err == nil && st.Sys().(*syscall.Stat_t).Ino != selfStat.Ino {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child network namespace unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	bind := func(name, source string) (string, uint64, uint64) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount(source, path, "", unix.MS_BIND, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Unmount(path, unix.MNT_DETACH) })
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		s := st.Sys().(*syscall.Stat_t)
		t.Logf("%s actual nsfs inode owner=%d", name, s.Uid)
		return path, uint64(s.Dev), s.Ino
	}
	path, dev, ino := bind("net", source)
	ns, err := openPinnedNetworkNamespace(path, dev, ino)
	if err != nil {
		t.Fatalf("actual netns rejected: %v", err)
	}
	defer ns.Close()
	reject := func(name, path string, dev, ino uint64) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			f, err := openPinnedNetworkNamespace(path, dev, ino)
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Fatal("unsafe namespace accepted")
			}
		})
	}
	reject("wrong inode", path, dev, ino+1)
	reject("wrong device", path, dev+1, ino)
	reject("unclean path", dir+"/./net", dev, ino)
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0600); err != nil {
		t.Fatal(err)
	}
	rst, err := os.Stat(regular)
	if err != nil {
		t.Fatal(err)
	}
	rstat := rst.Sys().(*syscall.Stat_t)
	reject("regular file with matching pin", regular, uint64(rstat.Dev), rstat.Ino)
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	reject("symlink", link, dev, ino)
	wrong, wrongDev, wrongIno := bind("uts", "/proc/self/ns/uts")
	reject("wrong namespace type", wrong, wrongDev, wrongIno)
	current, currentDev, currentIno := bind("current", "/proc/self/ns/net")
	reject("current namespace", current, currentDev, currentIno)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	reject("writable parent", path, dev, ino)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// The held namespace survives a name replacement; observation must use it.
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil {
		t.Fatal(err)
	}
	pinned := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), ns.Fd())
	if out, err := exec.Command("nsenter", "--net="+pinned, "--", "true").CombinedOutput(); err != nil {
		t.Fatalf("retained namespace descriptor unusable: %s %v", out, err)
	}
	reject("replaced namespace name", path, dev, ino)
}
