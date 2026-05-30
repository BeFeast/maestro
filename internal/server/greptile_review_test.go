package server

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

// Greptile P1 #478 — safeActionReadyLabel must NOT fall back to
// cfg.IssueLabels[0] (which is the FILTER set, not the readiness
// signal). A bare "bug"/"feature" entry there must not become the
// label `remove_ready_label` strips off an issue.
func TestSafeActionReadyLabel_NoIssueLabelsFallback(t *testing.T) {
	cfg := &config.Config{
		Supervisor:  config.SupervisorConfig{ReadyLabel: ""},
		IssueLabels: []string{"bug", "feature"},
	}
	if got := safeActionReadyLabel(cfg); got != "" {
		t.Fatalf("safeActionReadyLabel should return empty when ReadyLabel is empty, got %q", got)
	}
}

func TestSafeActionReadyLabel_HonoursExplicitReadyLabel(t *testing.T) {
	cfg := &config.Config{
		Supervisor:  config.SupervisorConfig{ReadyLabel: "maestro-ready"},
		IssueLabels: []string{"bug"},
	}
	if got := safeActionReadyLabel(cfg); got != "maestro-ready" {
		t.Fatalf("got %q, want maestro-ready", got)
	}
}

// Greptile P1 #479 — buildApprovalDecision must produce DISTINCT IDs
// even when two requests land in the same nanosecond (clock-tick
// collision). The crypto/rand suffix makes the chance vanishingly low.
func TestBuildApprovalDecision_IDsDistinctOnSameTimestamp(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo"}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	req := controlActionRequest{ActionID: "merge_pr", PRNumber: 1, Project: "p"}

	a := buildApprovalDecision("merge_pr", req, cfg, now)
	b := buildApprovalDecision("merge_pr", req, cfg, now)
	if a.ID == b.ID {
		t.Fatalf("two decisions at the same `now` produced identical ID %q (greptile P1 #479)", a.ID)
	}
	// The ID must still carry the timestamp prefix so existing log
	// formatting / sort behaviour is preserved.
	if !strings.Contains(a.ID, "20260530T120000") || !strings.Contains(b.ID, "20260530T120000") {
		t.Fatalf("IDs lost the timestamp prefix: %q / %q", a.ID, b.ID)
	}
}
