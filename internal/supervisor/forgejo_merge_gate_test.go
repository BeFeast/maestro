package supervisor

// #1172 M4 D2: the #425 legacy-status escape is DISABLED on forgejo rows. On
// GitHub, commit statuses can be legacy noise next to check-runs and a
// "status-only pending" head may merge once mergeable_state confirms the
// required checks; on forgejo, commit statuses ARE the CI — including the
// pending llm-review producer statuses — so the same escape would merge a PR
// whose review is still pending. Both arms are pinned here against the SAME
// reader state: only the config's forge kind flips the outcome.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// legacyStatusOnlyPendingRollup is exactly the #425 shape: aggregate pending,
// every check-run complete and green, one legacy commit status stuck pending.
func legacyStatusOnlyPendingRollup() github.PRCheckRollup {
	return github.PRCheckRollup{
		HeadSHA:  strings.Repeat("a", 40),
		Verdict:  "pending",
		Complete: true,
		Signals: []github.PRCheckSignal{
			{Source: "check_run", Name: "build", Status: "completed", Conclusion: "success"},
			{Source: "commit_status", Name: "legacy-bot", Status: "pending", Conclusion: "pending"},
		},
	}
}

func forgejoEscapeReader(pr github.PR) *fakeReader {
	return &fakeReader{
		prs:          []github.PR{pr},
		checkRollups: map[int]github.PRCheckRollup{pr.Number: legacyStatusOnlyPendingRollup()},
		// mergeable_state "clean" makes mergeStateAllowsMerge answer true —
		// the strongest possible pro-escape signal, so the forgejo arm below
		// proves the config gate (not the synthesis) is what blocks.
		mergeStates: map[int]string{pr.Number: "clean"},
	}
}

func TestOpenPRReadyToMerge_LegacyStatusEscape_GitHubArm(t *testing.T) {
	cfg := testConfig(t)
	pr := github.PR{Number: 7, Mergeable: "MERGEABLE"}
	eng := testEngine(cfg, forgejoEscapeReader(pr))
	sess := &state.Session{IssueNumber: 3, PRNumber: 7}

	ready, headSHA, reasons := eng.openPRReadyToMerge("slot-1", sess, pr)
	if !ready {
		t.Fatalf("github row: ready = false, want the #425 escape to authorize the merge (rollup=legacy-status-only pending, mergeable_state=clean)")
	}
	if headSHA != strings.Repeat("a", 40) {
		t.Fatalf("headSHA = %q, want the rollup head", headSHA)
	}
	found := false
	for _, reason := range reasons {
		if strings.Contains(reason, "mergeable_state confirms required checks passed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want the #425 override recorded", reasons)
	}
}

func TestOpenPRReadyToMerge_LegacyStatusEscape_DisabledOnForgejo(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forge = config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.com"}
	pr := github.PR{Number: 7, Mergeable: "MERGEABLE"}
	eng := testEngine(cfg, forgejoEscapeReader(pr))
	sess := &state.Session{IssueNumber: 3, PRNumber: 7}

	ready, _, _ := eng.openPRReadyToMerge("slot-1", sess, pr)
	if ready {
		t.Fatal("forgejo row: the #425 escape merged a PR with a pending commit status — on forgejo statuses ARE the CI (pending llm-review included), the escape must be disabled")
	}
}

// TestOpenPRReadyToMerge_ForgejoSuccessStillMerges proves the forgejo gate
// above only disables the ESCAPE, not the normal green path: a success
// verdict on a forgejo row still authorizes the merge.
func TestOpenPRReadyToMerge_ForgejoSuccessStillMerges(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forge = config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.com"}
	pr := github.PR{Number: 7, Mergeable: "MERGEABLE"}
	reader := forgejoEscapeReader(pr)
	rollup := reader.checkRollups[7]
	rollup.Verdict = "success"
	rollup.Signals = []github.PRCheckSignal{
		{Source: "commit_status", Name: "llm-review-opus", Status: "success", Conclusion: "success"},
	}
	reader.checkRollups[7] = rollup
	eng := testEngine(cfg, reader)
	sess := &state.Session{IssueNumber: 3, PRNumber: 7}

	ready, _, _ := eng.openPRReadyToMerge("slot-1", sess, pr)
	if !ready {
		t.Fatal("forgejo row with CI success must still be merge-ready — the M4 gate only disables the pending-escape")
	}
}

// --- #1172 M4 D4: the Greptile review-gate arm on forgejo rows ---------------

// greptileUnsupportedReader is the production forgejo shape: PRGreptileApproved
// answers with the ErrForgejoNotSupported sentinel (github.Client does exactly
// this on a forgejo-mode client) instead of the fakeReader's default approval.
type greptileUnsupportedReader struct {
	*fakeReader
	calls int
}

func (r *greptileUnsupportedReader) PRGreptileApproved(prNumber int) (bool, bool, error) {
	r.calls++
	return false, false, fmt.Errorf("PRGreptileApproved (greptile is GitHub-only): %w", github.ErrForgejoNotSupported)
}

func greenRollupReader(pr github.PR) *greptileUnsupportedReader {
	reader := forgejoEscapeReader(pr)
	rollup := reader.checkRollups[pr.Number]
	rollup.Verdict = "success"
	rollup.Signals = []github.PRCheckSignal{
		{Source: "commit_status", Name: "llm-review-opus", Status: "success", Conclusion: "success"},
	}
	reader.checkRollups[pr.Number] = rollup
	return &greptileUnsupportedReader{fakeReader: reader}
}

// TestOpenPRReadyToMerge_GreptileArm_ForgejoSkips pins the fix: on a forgejo
// row whose review_gate is not "llm-review" (in practice "none" — config
// validation rejects greptile/simplicity as effective streams on forgejo), the
// Greptile arm is skipped rather than consulted. Before the fix the sentinel
// error pinned the row at permanently not-ready with no recorded reason, while
// the orchestrator treats the same row as gate-passes.
func TestOpenPRReadyToMerge_GreptileArm_ForgejoSkips(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forge = config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.com"}
	cfg.ReviewGate = "none"
	pr := github.PR{Number: 7, Mergeable: "MERGEABLE"}
	reader := greenRollupReader(pr)
	eng := testEngine(cfg, reader)
	sess := &state.Session{IssueNumber: 3, PRNumber: 7}

	ready, headSHA, reasons := eng.openPRReadyToMerge("slot-1", sess, pr)
	if !ready {
		t.Fatalf("forgejo row: ready = false — the Greptile sentinel must not block a review_gate:none row (reasons=%v)", reasons)
	}
	if headSHA != strings.Repeat("a", 40) {
		t.Fatalf("headSHA = %q, want the rollup head", headSHA)
	}
	if reader.calls != 0 {
		t.Fatalf("PRGreptileApproved called %d times on a forgejo row, want 0 (arm must be short-circuited)", reader.calls)
	}
	skipped := false
	for _, reason := range reasons {
		if strings.Contains(reason, "Greptile review gate skipped on forgejo") {
			skipped = true
		}
		if strings.Contains(reason, "Greptile review approved") {
			t.Fatalf("reasons = %v, must not claim a Greptile approval that cannot exist on forgejo", reasons)
		}
	}
	if !skipped {
		t.Fatalf("reasons = %v, want the skip recorded explicitly (never a silent pass)", reasons)
	}
}

// TestOpenPRReadyToMerge_GreptileArm_GitHubUnchanged is the other arm against
// the SAME reader state: on a github row the Greptile read still runs and its
// error still fails closed. Only forge.kind flips the outcome.
func TestOpenPRReadyToMerge_GreptileArm_GitHubUnchanged(t *testing.T) {
	cfg := testConfig(t)
	pr := github.PR{Number: 7, Mergeable: "MERGEABLE"}
	reader := greenRollupReader(pr)
	eng := testEngine(cfg, reader)
	sess := &state.Session{IssueNumber: 3, PRNumber: 7}

	ready, _, _ := eng.openPRReadyToMerge("slot-1", sess, pr)
	if ready {
		t.Fatal("github row: a Greptile read error must keep the merge path closed")
	}
	if reader.calls != 1 {
		t.Fatalf("PRGreptileApproved called %d times on a github row, want 1 (arm unchanged)", reader.calls)
	}
}

// TestOpenPRReadyToMerge_GreptileArm_GitHubApprovedReasonUnchanged proves the
// happy-path github reason string is byte-identical after the fix.
func TestOpenPRReadyToMerge_GreptileArm_GitHubApprovedReasonUnchanged(t *testing.T) {
	cfg := testConfig(t)
	pr := github.PR{Number: 7, Mergeable: "MERGEABLE"}
	reader := greenRollupReader(pr).fakeReader
	eng := testEngine(cfg, reader)
	sess := &state.Session{IssueNumber: 3, PRNumber: 7}

	ready, _, reasons := eng.openPRReadyToMerge("slot-1", sess, pr)
	if !ready {
		t.Fatalf("github row with an approving Greptile read must be merge-ready (reasons=%v)", reasons)
	}
	found := false
	for _, reason := range reasons {
		if reason == "PR #7 Greptile review approved" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want the unchanged github approval reason", reasons)
	}
}
