package aiexecution

import (
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func openNativeWorkspaceFile(path string, flags uint64, mode os.FileMode) (*os.File, error) {
	root := nativeGitRoot(path)
	if root == "" {
		return os.OpenFile(path, int(flags), mode)
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, Held("native_workspace_path_unsafe")
	}
	defer unix.Close(fd)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, Held("native_workspace_path_unsafe")
	}
	child, err := unix.Openat2(fd, rel, &unix.OpenHow{Flags: (flags &^ unix.O_TRUNC) | unix.O_CLOEXEC | unix.O_NONBLOCK, Mode: uint64(mode.Perm()), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(child), path)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, Held("native_workspace_file_unsafe")
	}
	if flags&unix.O_TRUNC != 0 {
		identity, ok := st.Sys().(*syscall.Stat_t)
		if !ok || identity.Nlink != 1 || identity.Uid != uint32(os.Getuid()) {
			f.Close()
			return nil, Held("native_workspace_file_unsafe")
		}
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// ReadWorkspaceFile treats native artifacts as attacker-controlled paths.
// No symlink in any component can disclose host data during prompt/recovery.
func ReadWorkspaceFile(path string) ([]byte, error) {
	if nativeGitRoot(path) == "" {
		return os.ReadFile(path)
	}
	f, err := openNativeWorkspaceFile(path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || len(b) > 16<<20 {
		return nil, Held("native_workspace_file_oversized")
	}
	return b, nil
}

func WriteWorkspaceFile(path string, data []byte, mode os.FileMode) error {
	if nativeGitRoot(path) == "" {
		return os.WriteFile(path, data, mode)
	}
	f, err := openNativeWorkspaceFile(path, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
