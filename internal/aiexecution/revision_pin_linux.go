package aiexecution

import (
	"os"

	"golang.org/x/sys/unix"
)

func newLiveRevisionPin(data []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("maestro-ai-revision", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "maestro-ai-revision")
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
