package outcome

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryRunnerTimeoutKillsDescendantProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	execution := (RecoveryRunner{}).Run(ctx, Brief{
		RecoveryCommand: fmt.Sprintf("(sleep 1; touch %q) & wait", marker),
	})
	if !execution.TimedOut {
		t.Fatalf("TimedOut = false, receipt=%+v", execution)
	}

	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("recovery descendant survived timeout and produced its marker")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}
