//go:build linux

package aiexecution

import (
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// listenerOwner hands the test server's listening socket to a child process
// that becomes the pinned "gateway": inspectListener proves the child, while
// this process keeps serving on the shared socket. Killing the child leaves
// the endpoint served by a process that is not the pinned gateway.
func listenerOwner(t *testing.T, s *bindingTestServer) (*httptest.Server, *exec.Cmd) {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep unavailable")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "60")
	child.ExtraFiles = []*os.File{f}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	// Passing the descriptor made the shared open file description blocking;
	// this process's poller needs it non-blocking to keep serving and closing.
	if err := syscall.SetNonblock(int(f.Fd()), true); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	srv := httptest.NewUnstartedServer(s)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, child
}

func TestBindingObservationProvesListenerBeforeEveryKeyedRequest(t *testing.T) {
	run := func(t *testing.T, lose func(*bindingTestServer, func())) (*bindingTestServer, error) {
		resetIdentityVerificationLimiter(t)
		m, cold, verified := rotatedTokenLane()
		t.Setenv(m.Runtime.ManagementKeyEnv, "synthetic-management-key")
		s := &bindingTestServer{t: t, receipts: []claudeBindingReceipt{cold, verified}}
		srv, child := listenerOwner(t, s)
		if lose != nil {
			lose(s, func() {
				_ = child.Process.Kill()
				_ = child.Wait()
			})
		}
		m.GatewayURL, m.Gateway.PID = srv.URL, child.Process.Pid
		return s, observeClaudeBindings(m)
	}
	t.Run("owned throughout", func(t *testing.T) {
		s, err := run(t, nil)
		if err != nil {
			t.Fatalf("pinned listener owner was not accepted: %v", err)
		}
		if s.gets.Load() != 2 || s.posts.Load() != 1 {
			t.Fatalf("gets=%d posts=%d, want 2 and 1", s.gets.Load(), s.posts.Load())
		}
	})
	t.Run("owner lost during verification", func(t *testing.T) {
		s, err := run(t, func(s *bindingTestServer, kill func()) { s.onVerify = kill })
		wantHold(t, err, "gateway_network_namespace_mismatch")
		if s.gets.Load() != 1 || s.posts.Load() != 1 {
			t.Fatalf("the management key reached an unproven endpoint: gets=%d posts=%d, want 1 and 1", s.gets.Load(), s.posts.Load())
		}
	})
	t.Run("owner lost after the first observation", func(t *testing.T) {
		s, err := run(t, func(s *bindingTestServer, kill func()) {
			s.onGet = func(n int) {
				if n == 1 {
					kill()
				}
			}
		})
		wantHold(t, err, "gateway_network_namespace_mismatch")
		if s.gets.Load() != 1 || s.posts.Load() != 0 {
			t.Fatalf("the management key reached an unproven endpoint: gets=%d posts=%d, want 1 and 0", s.gets.Load(), s.posts.Load())
		}
	})
}
