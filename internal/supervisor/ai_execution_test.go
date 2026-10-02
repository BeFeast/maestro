package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestStrictCustomSupervisorAndSpecGroomNeverCall(t *testing.T) {
	cfg := testConfig(t)
	cfg.AIExecution.RequireVerifiedRoute = true
	cfg.Supervisor.AlwaysConsultLLM = true
	cfg.Supervisor.SpecGroom.Enabled = true
	llm := idleLLM()
	reader := &fakeReader{issues: []github.Issue{issueWithBody(1, "vague", "do stuff")}}
	e := testLLMEngine(cfg, reader, llm)
	_, _ = e.Decide(state.NewState())
	e.runSpecGroom(state.NewState(), reader)
	if llm.calls != 0 {
		t.Fatalf("strict custom completer called %d times", llm.calls)
	}
}
