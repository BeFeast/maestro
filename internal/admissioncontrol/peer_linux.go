//go:build linux

package admissioncontrol

import (
	"golang.org/x/sys/unix"
	"net"
)

func verifyPeer(conn net.Conn, expectedUID uint32) error {
	stream, ok := conn.(*net.UnixConn)
	if !ok {
		return &Hold{Code: "authority_peer_mismatch"}
	}
	raw, err := stream.SyscallConn()
	if err != nil {
		return &Hold{Code: "authority_peer_mismatch"}
	}
	var credentials *unix.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || peerErr != nil || credentials == nil || credentials.Uid != expectedUID {
		return &Hold{Code: "authority_peer_mismatch"}
	}
	return nil
}
