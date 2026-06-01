package approver

import (
	"errors"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// --- #489: cross-project repo guard ---------------------------------------

func TestExecute_CrossProjectMutation_Refused(t *testing.T) {
	gh := &fakeGH{}
	cfg := newCfg() // cfg.Repo = "owner/repo"
	ex := &Executor{GH: gh, Cfg: cfg}

	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")
	a.Repo = "BeFeast/scribe-service" // mismatch

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("res = %+v, want execution_failed", res)
	}
	if len(gh.mergeCalls) != 0 {
		t.Fatalf("MergePR was called %v — repo guard failed", gh.mergeCalls)
	}
	if res.Err == nil || !errors.Is(res.Err, res.Err) {
		t.Fatalf("expected non-nil Err carrying the mismatch reason; got %v", res.Err)
	}
}

func TestExecute_RepoMatch_Proceeds(t *testing.T) {
	gh := &fakeGH{}
	cfg := newCfg()
	ex := &Executor{GH: gh, Cfg: cfg}

	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")
	a.Repo = cfg.Repo // explicit match

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed", res)
	}
	if len(gh.mergeCalls) != 1 {
		t.Fatalf("expected 1 MergePR call, got %v", gh.mergeCalls)
	}
}

func TestExecute_LegacyApprovalNoRepo_FallsThrough(t *testing.T) {
	// Approvals created before #489 don't carry Repo. Don't break them
	// on upgrade — fall through to the existing behaviour.
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}

	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")
	// a.Repo intentionally left empty.

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed (back-compat for unstamped approvals)", res)
	}
	if len(gh.mergeCalls) != 1 {
		t.Fatalf("expected 1 MergePR call, got %v", gh.mergeCalls)
	}
}

func TestExecute_NoCfgRepo_SkipsGuard(t *testing.T) {
	// Defensive: if cfg.Repo is somehow empty, don't trip the guard
	// (we can't tell the operator anything useful).
	gh := &fakeGH{}
	cfg := newCfg()
	cfg.Repo = ""
	ex := &Executor{GH: gh, Cfg: cfg}

	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")
	a.Repo = "BeFeast/something" // irrelevant — cfg has nothing to compare against

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed (cfg.Repo empty -> guard skipped)", res)
	}
}

// #489: when a pre-#489 approval (no Repo stamp) falls through the guard,
// Execute must succeed AND surface a deprecation warning on Result so the
// caller can log it for operator visibility.
func TestExecute_LegacyApprovalNoRepo_EmitsDeprecationWarning(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}

	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")
	// a.Repo intentionally left empty — simulates a pre-#489 approval.

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed (legacy approval must not abort)", res)
	}
	if res.Warning == "" {
		t.Fatalf("res.Warning is empty, want deprecation advisory for unstamped legacy approval")
	}
	if !strings.Contains(res.Warning, "MigrateApprovalsBindRepo") {
		t.Fatalf("res.Warning = %q, want hint to call MigrateApprovalsBindRepo", res.Warning)
	}
}

// #489: a freshly-stamped, matching approval must execute clean — no
// stray legacy warning.
func TestExecute_RepoMatch_NoLegacyWarning(t *testing.T) {
	gh := &fakeGH{}
	cfg := newCfg()
	ex := &Executor{GH: gh, Cfg: cfg}

	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")
	a.Repo = cfg.Repo

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed", res)
	}
	if res.Warning != "" {
		t.Fatalf("res.Warning = %q, want empty (no legacy fall-through)", res.Warning)
	}
}
