package pipeline

import (
	"errors"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeWorktreeHostToolsHoldBeforeMutation(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for registered native workspace fixture")
	}
	dir := t.TempDir()
	if err := aiexecution.RegisterNativeGit(dir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "host-command-ran")
	_, err := RunVisualCapture(config.VerifyVisualConfig{Command: "touch " + marker}, dir)
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != "native_visual_capture_requires_native_tool" {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("host command ran")
	}
	_, err = findSymbolContexts(dir, "test symbol", "test")
	if !errors.As(err, &hold) || hold.Code != "native_symbol_context_requires_native_tool" {
		t.Fatal(err)
	}
}
