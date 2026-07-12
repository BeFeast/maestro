package supervisor

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestApplyQueueActionRedactsManagementHomePathFromIssueComment(t *testing.T) {
	const privatePath = "/srv/example-vault/Dev/Areas/maestro"
	cfg := testConfig(t)
	cfg.ManagementHome = config.ManagementHomeConfig{
		Kind:      config.ManagementHomeKindObsidian,
		Path:      privatePath,
		Vault:     "Example Vault",
		VaultPath: "Dev/Areas/maestro",
	}
	reader := &fakeReader{}
	decision := &state.SupervisorDecision{
		Target: &state.SupervisorTarget{Issue: 870},
		Mutations: []state.SupervisorMutation{{
			Type:   MutationIssueComment,
			Issue:  870,
			Body:   "Management context was found at " + privatePath + "; continue from the issue contract.",
			Status: MutationStatusPlanned,
		}},
	}

	applyQueueAction(cfg, decision, reader)

	if len(reader.comments) != 1 {
		t.Fatalf("comments = %#v, want one", reader.comments)
	}
	if strings.Contains(reader.comments[0], privatePath) {
		t.Fatalf("GitHub-facing comment leaked private Management Home path: %q", reader.comments[0])
	}
	if !strings.Contains(reader.comments[0], "[management-home-path]") {
		t.Fatalf("GitHub-facing comment = %q, want explicit redaction marker", reader.comments[0])
	}
	if strings.Contains(decision.Mutations[0].Body, privatePath) {
		t.Fatalf("persisted mutation leaked private Management Home path: %q", decision.Mutations[0].Body)
	}
}
