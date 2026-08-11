package supervisor

// #1172 M4 D2: the #425 legacy-status escape is DISABLED on forgejo rows. On
// GitHub, commit statuses can be legacy noise next to check-runs and a
// "status-only pending" head may merge once mergeable_state confirms the
// required checks; on forgejo, commit statuses ARE the CI — including the
// pending llm-review producer statuses — so the same escape would merge a PR
// whose review is still pending. Both arms are pinned here against the SAME
// reader state: only the config's forge kind flips the outcome.

import (
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
