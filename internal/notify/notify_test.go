package notify

import (
	"strings"
	"testing"
)

func TestDigestMode_BuffersMessages(t *testing.T) {
	n := &Notifier{Target: "test-chat", OpenclawURL: "http://fake:1234"}
	n.SetDigestMode(true)

	// Send should buffer, not send
	if err := n.Send("msg1"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := n.Send("msg2"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if n.Buffered() != 2 {
		t.Errorf("Buffered() = %d, want 2", n.Buffered())
	}
}

func TestDigestMode_Off_SendsImmediately(t *testing.T) {
	n := &Notifier{Target: "test-chat"}
	// No digest mode, no transport — should just log and return nil
	if err := n.Send("msg1"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n.Buffered() != 0 {
		t.Errorf("Buffered() = %d, want 0 (not buffered when digest off)", n.Buffered())
	}
}

func TestDigestMode_FlushClearsBuffer(t *testing.T) {
	n := &Notifier{Target: "test-chat"} // no transport — send will log-skip
	n.SetDigestMode(true)

	n.Send("msg1")
	n.Send("msg2")

	if n.Buffered() != 2 {
		t.Fatalf("Buffered() = %d, want 2", n.Buffered())
	}

	// Flush — will fail to send (no transport) but should clear buffer
	n.Flush()

	if n.Buffered() != 0 {
		t.Errorf("Buffered() = %d after Flush, want 0", n.Buffered())
	}
}

func TestDigestMode_FlushEmptyBuffer(t *testing.T) {
	n := &Notifier{Target: "test-chat"}
	n.SetDigestMode(true)

	// Flush with no messages should be a no-op
	if err := n.Flush(); err != nil {
		t.Errorf("Flush with empty buffer should not error, got: %v", err)
	}
}

func TestSendf_FormatsMessage(t *testing.T) {
	n := &Notifier{Target: "test-chat"} // no transport, will log-skip
	n.SetDigestMode(true)

	n.Sendf("hello %s #%d", "world", 42)

	if n.Buffered() != 1 {
		t.Fatalf("Buffered() = %d, want 1", n.Buffered())
	}

	// Verify the formatted message is in the buffer
	n.mu.Lock()
	msg := n.buffer[0]
	n.mu.Unlock()

	if !strings.Contains(msg, "hello world #42") {
		t.Errorf("buffer[0] = %q, want it to contain 'hello world #42'", msg)
	}
}

func TestSend_EmptyTarget_Skips(t *testing.T) {
	n := &Notifier{} // no target
	if err := n.Send("msg"); err != nil {
		t.Errorf("Send with empty target should return nil, got: %v", err)
	}
}
