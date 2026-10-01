package aiexecution

import (
	"encoding/binary"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	sockDiagByFamily    = 20
	inetDiagIPv6Only    = 11
	tcpListenState      = 10
	inetDiagMessageSize = 72
)

// dualStackListener observes the exact candidate already owned by the gateway
// in /proc. Every unavailable, incomplete or ambiguous kernel dump fails closed.
// No provider request or gateway socket mutation is performed.
func dualStackListener(inode string, port int) bool {
	wantInode, err := strconv.ParseUint(inode, 10, 32)
	if err != nil || wantInode == 0 || port <= 0 || port > 65535 {
		return false
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return false
	}
	local, err := unix.Getsockname(fd)
	address, ok := local.(*unix.SockaddrNetlink)
	if err != nil || !ok {
		return false
	}
	// inet_diag_req_v2, including INET_DIAG_NOCOOKIE. The complete dump is
	// checked even after finding a match, so duplicates/interruption cannot win.
	request := make([]byte, unix.NLMSG_HDRLEN+56)
	binary.NativeEndian.PutUint32(request[0:4], uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:6], sockDiagByFamily)
	binary.NativeEndian.PutUint16(request[6:8], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(request[8:12], 1)
	binary.NativeEndian.PutUint32(request[12:16], address.Pid)
	body := request[unix.NLMSG_HDRLEN:]
	body[0], body[1] = unix.AF_INET6, unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(body[4:8], 1<<tcpListenState)
	binary.BigEndian.PutUint16(body[8:10], uint16(port))
	binary.NativeEndian.PutUint32(body[48:52], ^uint32(0))
	binary.NativeEndian.PutUint32(body[52:56], ^uint32(0))
	if err := unix.Sendto(fd, request, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return false
	}
	buf := make([]byte, 64<<10)
	matches, dual := 0, false
	deadline := time.Now().Add(2 * time.Second)
	for datagram := 0; datagram < 64; datagram++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		if remaining > time.Second {
			remaining = time.Second
		}
		timeout := unix.NsecToTimeval(remaining.Nanoseconds())
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
			return false
		}
		n, _, flags, from, err := unix.Recvmsg(fd, buf, nil, 0)
		sender, ok := from.(*unix.SockaddrNetlink)
		if err != nil || !ok || sender.Pid != 0 || flags&unix.MSG_TRUNC != 0 {
			return false
		}
		messages, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil || len(messages) == 0 {
			return false
		}
		consumed := 0
		for _, message := range messages {
			consumed += (int(message.Header.Len) + 3) &^ 3
		}
		if consumed != n {
			return false
		}
		for index, message := range messages {
			if message.Header.Seq != 1 || message.Header.Pid != address.Pid || message.Header.Flags&unix.NLM_F_DUMP_INTR != 0 {
				return false
			}
			switch message.Header.Type {
			case unix.NLMSG_DONE:
				if index != len(messages)-1 || len(message.Data) != 0 && (len(message.Data) != 4 || binary.NativeEndian.Uint32(message.Data) != 0) {
					return false
				}
				return matches == 1 && dual
			case sockDiagByFamily:
				matched, allowed, valid := parseDualStackDiagnostic(message.Data, uint32(wantInode), uint16(port))
				if !valid {
					return false
				}
				if matched {
					matches++
					dual = allowed
				}
				if matches > 1 {
					return false
				}
			default:
				return false
			}
		}
	}
	return false
}

func parseDualStackDiagnostic(data []byte, inode uint32, port uint16) (matched, dual, valid bool) {
	if len(data) < inetDiagMessageSize {
		return false, false, false
	}
	if binary.NativeEndian.Uint32(data[68:72]) != inode {
		return false, false, true
	}
	if data[0] != unix.AF_INET6 || data[1] != tcpListenState || binary.BigEndian.Uint16(data[4:6]) != port {
		return false, false, false
	}
	for _, b := range data[8:24] {
		if b != 0 {
			return false, false, false
		}
	}
	seen := false
	for attrs := data[inetDiagMessageSize:]; len(attrs) > 0; {
		if len(attrs) < 4 {
			return false, false, false
		}
		size := int(binary.NativeEndian.Uint16(attrs[0:2]))
		kind := binary.NativeEndian.Uint16(attrs[2:4])
		aligned := (size + 3) &^ 3
		if size < 4 || size > len(attrs) || aligned > len(attrs) {
			return false, false, false
		}
		if kind == inetDiagIPv6Only {
			if seen || size != 5 || attrs[4] > 1 {
				return false, false, false
			}
			seen, dual = true, attrs[4] == 0
		}
		attrs = attrs[aligned:]
	}
	return true, dual && seen, true
}
