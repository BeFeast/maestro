package aiexecution

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func kernelIPv6Listener(t *testing.T, only int) (net.Listener, string, int) {
	t.Helper()
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(fd uintptr) { optionErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, only) }); err != nil {
			return err
		}
		return optionErr
	}}
	listener, err := config.Listen(context.Background(), "tcp6", "[::]:0")
	if err != nil {
		t.Skipf("kernel IPv6 listener unavailable: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var inode uint64
	if err := raw.Control(func(fd uintptr) {
		var stat unix.Stat_t
		if err := unix.Fstat(int(fd), &stat); err != nil {
			t.Error(err)
			return
		}
		observed, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
		if err != nil || observed != only {
			t.Errorf("IPV6_V6ONLY=%d want=%d error=%v", observed, only, err)
		}
		inode = stat.Ino
	}); err != nil {
		t.Fatal(err)
	}
	return listener, strconv.FormatUint(inode, 10), listener.Addr().(*net.TCPAddr).Port
}

func TestRuntimeListenerDualStackUsesExactKernelSocketOption(t *testing.T) {
	_, dualInode, dualPort := kernelIPv6Listener(t, 0)
	_, onlyInode, onlyPort := kernelIPv6Listener(t, 1)
	if !dualStackListener(dualInode, dualPort) {
		t.Fatal("owned dual-stack wildcard rejected")
	}
	if dualStackListener(onlyInode, onlyPort) {
		t.Fatal("IPv6-only socket accepted as IPv4")
	}
	if dualStackListener(dualInode, onlyPort) || dualStackListener(onlyInode, dualPort) || dualStackListener("0", dualPort) {
		t.Fatal("foreign inode or port accepted")
	}
	if err := inspectListener(os.Getpid(), net.ParseIP("127.0.0.1"), dualPort); err != nil {
		t.Fatal(err)
	}
	assertExecutionHold(t, inspectListener(os.Getpid(), net.ParseIP("127.0.0.1"), onlyPort), "gateway_listener_mismatch")
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(dualPort)), time.Second)
	if err != nil {
		t.Fatalf("kernel dual-stack IPv4 connection failed: %v", err)
	}
	conn.Close()
}

func TestRuntimeListenerDiagnosticParserRejectsUnprovenEvidence(t *testing.T) {
	fixture := func() []byte {
		b := make([]byte, 80)
		b[0], b[1] = unix.AF_INET6, tcpListenState
		binary.BigEndian.PutUint16(b[4:6], 23020)
		binary.NativeEndian.PutUint32(b[68:72], 42)
		binary.NativeEndian.PutUint16(b[72:74], 5)
		binary.NativeEndian.PutUint16(b[74:76], inetDiagIPv6Only)
		return b
	}
	for _, kind := range []string{"valid", "v6only", "no_option", "duplicate", "short_message", "short_attribute", "bad_attribute_size", "bad_option_size", "bad_option_value", "foreign_inode", "foreign_port", "foreign_family", "not_listening", "bound_address"} {
		t.Run(kind, func(t *testing.T) {
			b := fixture()
			switch kind {
			case "v6only":
				b[76] = 1
			case "no_option":
				b = b[:72]
			case "duplicate":
				b = append(b, b[72:]...)
			case "short_message":
				b = b[:50]
			case "short_attribute":
				b = b[:74]
			case "bad_attribute_size":
				binary.NativeEndian.PutUint16(b[72:74], 12)
			case "bad_option_size":
				binary.NativeEndian.PutUint16(b[72:74], 4)
			case "bad_option_value":
				b[76] = 2
			case "foreign_inode":
				binary.NativeEndian.PutUint32(b[68:72], 43)
			case "foreign_port":
				binary.BigEndian.PutUint16(b[4:6], 23021)
			case "foreign_family":
				b[0] = unix.AF_INET
			case "not_listening":
				b[1] = 1
			case "bound_address":
				b[23] = 1
			}
			match, dual, valid := parseDualStackDiagnostic(b, 42, 23020)
			if (match && dual && valid) != (kind == "valid") {
				t.Fatalf("diagnostic %s: %v/%v/%v", kind, match, dual, valid)
			}
		})
	}
}
