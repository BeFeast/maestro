package supervisor

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func mintedStreakIssue(number int) github.Issue {
	return github.Issue{
		Number: number,
		Title:  "gate failure streak: healthcheck_command failing 2 consecutive scheduled runs",
		Body:   gateFailStreakBodyMarker + " 658f0089bedb2248 -->\n\nScheduled gate failed.",
	}
}

// Codex review catch (P1): the hold lived only inside decideDynamicWave, but
// gate-streak intake is enabled independently of dynamic-wave policy. With
// dynamic wave off, the default eligibility path decided the same report was
// spawnable, so the hold was one branch wide instead of fleet wide.
func TestIssueSkipReason_HoldsUntriagedGateStreakReport(t *testing.T) {
	e := &Engine{cfg: &config.Config{}} // no issue_labels, no ready_label
	st := state.NewState()

	reason, err := e.issueSkipReason(st, mintedStreakIssue(277))
	if err != nil {
		t.Fatalf("issueSkipReason: %v", err)
	}
	if !strings.Contains(reason, "gate-fail-streak") {
		t.Fatalf("skip reason = %q, want the untriaged gate-streak hold — the default "+
			"dispatch path must not depend on dynamic wave being enabled", reason)
	}

	// A human-written issue is untouched by the hold.
	human := github.Issue{Number: 628, Body: "## Summary\nreal work"}
	reason, err = e.issueSkipReason(st, human)
	if err != nil {
		t.Fatalf("issueSkipReason: %v", err)
	}
	if strings.Contains(reason, "gate-fail-streak") {
		t.Fatalf("human issue skipped as a streak report: %q", reason)
	}
}

// The repair path reaches dispatch through its own skip predicate, so the hold
// has to be there too or a minted report can still be picked up as repair work.
func TestIssueRepairSkipReason_HoldsUntriagedGateStreakReport(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	st := state.NewState()

	reason, err := e.issueRepairSkipReason(st, mintedStreakIssue(277))
	if err != nil {
		t.Fatalf("issueRepairSkipReason: %v", err)
	}
	if !strings.Contains(reason, "gate-fail-streak") {
		t.Fatalf("repair skip reason = %q, want the untriaged gate-streak hold", reason)
	}
}

// An operator-labelled report runs normally through both predicates.
func TestSkipReasons_LabelledGateStreakReportRunsNormally(t *testing.T) {
	cfg := &config.Config{}
	cfg.Supervisor.ReadyLabel = "maestro-ready"
	e := &Engine{cfg: cfg}
	st := state.NewState()

	issue := mintedStreakIssue(277)
	issue.Labels = append(issue.Labels, struct {
		Name string `json:"name"`
	}{Name: "maestro-ready"})

	for name, fn := range map[string]func(*state.State, github.Issue) (string, error){
		"issueSkipReason":       e.issueSkipReason,
		"issueRepairSkipReason": e.issueRepairSkipReason,
	} {
		reason, err := fn(st, issue)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(reason, "gate-fail-streak") {
			t.Fatalf("%s held an operator-labelled report: %q", name, reason)
		}
	}
}
