package supervisor

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestNativeOutputPreservesTimeoutAndLimitedPrefix(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "printf partial-output; sleep 5")
	out, launched, status, err := outputWithTimeoutReceiptContext(context.Background(), cmd, 100*time.Millisecond)
	if err == nil || !launched || status != "timed_out" || string(out) != "partial-output" {
		t.Fatalf("out=%q launched=%t status=%s error=%v", out, launched, status, err)
	}
	cmd = exec.Command("/bin/sh", "-c", "head -c 4194320 /dev/zero")
	out, launched, status, err = outputWithTimeoutReceiptContext(context.Background(), cmd, time.Second)
	if err == nil || !launched || status != "output_limit" || len(out) != 4<<20 {
		t.Fatalf("bytes=%d launched=%t status=%s error=%v", len(out), launched, status, err)
	}
}
