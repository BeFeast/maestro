// Package controllerowner admits one participating scheduler controller per
// host and effective OS user. It is not a distributed queue ownership service.
package controllerowner

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const registryName = ".maestro-controller"
const lockName = "owner.lock"

var ErrAlreadyOwned = errors.New("another Maestro scheduler controller owns this host and OS user")

type Lease struct {
	fd   int
	once sync.Once
}

// Close releases ownership without removing the persistent lock inode. The
// kernel also releases it on process exit, including an ungraceful exit.
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	var err error
	l.once.Do(func() { err = unix.Close(l.fd) })
	return err
}

// Acquire uses the OS account database, never HOME/XDG or caller store options.
// No alternate path is chosen if this fixed coordination location is unsafe.
func Acquire() (*Lease, error) {
	return acquireForUID(os.Geteuid(), user.LookupId)
}

func acquireForUID(uid int, lookup func(string) (*user.User, error)) (*Lease, error) {
	account, err := lookup(strconv.Itoa(uid))
	if err != nil {
		return nil, fmt.Errorf("controller ownership: resolve OS account: %w", err)
	}
	if account == nil || account.Uid != strconv.Itoa(uid) || !filepath.IsAbs(account.HomeDir) || strings.TrimSpace(account.HomeDir) == "" || filepath.Clean(account.HomeDir) != account.HomeDir || account.HomeDir == "/" {
		return nil, fmt.Errorf("controller ownership: OS account has no valid absolute home for UID %d", uid)
	}
	return acquireAtHome(account.HomeDir, uid)
}

// acquireAtHome is an internal fixture seam, not a runtime configuration knob.
func acquireAtHome(home string, uid int) (*Lease, error) {
	homeFD, err := openHomeNoFollow(home, uid)
	if err != nil {
		return nil, fmt.Errorf("controller ownership: open OS-account home: %w", err)
	}
	defer unix.Close(homeFD)
	if err := validateFD(homeFD, uid, true, false); err != nil {
		return nil, fmt.Errorf("controller ownership: unsafe OS-account home: %w", err)
	}
	if err := unix.Mkdirat(homeFD, registryName, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("controller ownership: create registry: %w", err)
	}
	dirFD, err := unix.Openat(homeFD, registryName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("controller ownership: open registry: %w", err)
	}
	defer unix.Close(dirFD)
	if err := validateFD(dirFD, uid, true, true); err != nil {
		return nil, fmt.Errorf("controller ownership: unsafe registry: %w", err)
	}
	// O_NONBLOCK ensures an unexpected FIFO/device cannot hang before Fstat;
	// O_NOFOLLOW and the descriptor checks reject aliases and unsafe files.
	fd, err := unix.Openat(dirFD, lockName, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("controller ownership: open lock: %w", err)
	}
	if err := validateFD(fd, uid, false, true); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("controller ownership: unsafe lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("controller ownership: %w (UID %d); stop the existing scheduler before starting another", ErrAlreadyOwned, uid)
		}
		return nil, fmt.Errorf("controller ownership: acquire lock: %w", err)
	}
	return &Lease{fd: fd}, nil
}

func openHomeNoFollow(home string, uid int) (int, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || home == "/" {
		return -1, fmt.Errorf("home must be a clean absolute non-root path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(home, "/"), "/") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return -1, err
		}
		// Root-owned sticky scratch parents permit private test/home directories
		// without allowing another user to replace that user's existing entry.
		stickyRoot := st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
		if (int(st.Uid) != uid && st.Uid != 0) || (st.Mode&0022 != 0 && !stickyRoot) {
			unix.Close(fd)
			return -1, fmt.Errorf("unsafe home ancestor ownership or permissions")
		}
	}
	return fd, nil
}

func validateFD(fd, uid int, directory, private bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	wantType := uint32(unix.S_IFREG)
	if directory {
		wantType = unix.S_IFDIR
	}
	if uint32(st.Mode)&uint32(unix.S_IFMT) != wantType || int(st.Uid) != uid {
		return fmt.Errorf("wrong filesystem type or OS owner")
	}
	if st.Mode&0022 != 0 || (private && st.Mode&0077 != 0) {
		return fmt.Errorf("unsafe group/other permissions")
	}
	if !directory && st.Nlink != 1 {
		return fmt.Errorf("lock must not have hardlink aliases")
	}
	return nil
}
