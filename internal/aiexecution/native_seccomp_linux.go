package aiexecution

import (
	"bytes"
	"encoding/binary"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// This post-setup filter forbids namespace/mount mutation while preserving
// ordinary processes and threads. clone3 returns ENOSYS so libc uses clone,
// whose namespace bits are checked. It does not alter any host sysctl.
func nativeSeccompBytes() ([]byte, error) {
	if runtime.GOARCH != "amd64" {
		return nil, Held("containment_seccomp_arch_unsupported")
	}
	type insn struct {
		Code   uint16
		JT, JF uint8
		K      uint32
	}
	const load = 0x20
	const equal = 0x15
	const bitset = 0x45
	const ret = 0x06
	const errno = 0x00050000
	f := []insn{{load, 0, 0, 4}, {equal, 1, 0, 0xc000003e}, {ret, 0, 0, 0x80000000}, {load, 0, 0, 0}}
	// x32 syscalls use the same architecture tag but a separate syscall table.
	f = append(f, insn{bitset, 0, 1, 0x40000000}, insn{ret, 0, 0, 0x80000000})
	for _, nr := range []uint32{272, 308, 165, 166, 155, 428, 429, 430, 431, 432, 442} {
		f = append(f, insn{equal, 0, 1, nr}, insn{ret, 0, 0, errno | uint32(unix.EPERM)})
	}
	f = append(f, insn{equal, 0, 1, 435}, insn{ret, 0, 0, errno | uint32(unix.ENOSYS)})
	f = append(f, insn{equal, 0, 3, 56}, insn{load, 0, 0, 16}, insn{bitset, 0, 1, 0x7e020000}, insn{ret, 0, 0, errno | uint32(unix.EPERM)}, insn{ret, 0, 0, 0x7fff0000})
	var out bytes.Buffer
	if err := binary.Write(&out, binary.LittleEndian, f); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func nativeSeccompFile() (*os.File, error) {
	b, err := nativeSeccompBytes()
	if err != nil {
		return nil, err
	}
	fd, err := unix.MemfdCreate("maestro-native-seccomp", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, Held("containment_seccomp_unavailable")
	}
	f := os.NewFile(uintptr(fd), "maestro-native-seccomp")
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
