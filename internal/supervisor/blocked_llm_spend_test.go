package supervisor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestOperatorGateSkipsLLMAcrossCyclesAndReload(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	cfg.Supervisor.AllowedActions = append(defaultAllowedActions(), ActionMergePR)
	cfg.Supervisor.AlwaysConsultLLM = true // A poll preference cannot unlock a human gate.
	reader := &fakeReader{prs: []github.PR{{Number: 17, HeadRefName: "feat/release", Mergeable: "MERGEABLE"}}, ciStatuses: map[int]string{17: "success"}}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 5, Status: state.StatusPROpen, Branch: "feat/release", PRNumber: 17, StartedAt: time.Now().UTC().Add(-time.Hour), OperatorGateName: "review-thread:pending"}
	llm := &fakeLLM{output: `{"summary":"Merge the approved release.","recommended_action":"merge_pr","target":{"issue":5,"pr":17,"session":"slot-1"},"risk":"mutating","confidence":0.9,"reasons":["review gate cleared"],"requires_approval":false}`}
	for i := 0; i < 3; i++ {
		// Simulate a new RunOnce engine and durable-state reload each cycle.
		data, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		reloaded := state.NewState()
		if err := json.Unmarshal(data, reloaded); err != nil {
			t.Fatal(err)
		}
		st = reloaded
		decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
		if err != nil {
			t.Fatal(err)
		}
		if decision.RecommendedAction != ActionNone || len(decision.Mutations) != 0 || llm.calls != 0 {
			t.Fatalf("blocked cycle %d spent or acted: calls=%d decision=%+v", i, llm.calls, decision)
		}
	}
	st.Sessions["slot-1"].OperatorGateName = ""
	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 || decision.RecommendedAction != ActionMergePR {
		t.Fatalf("cleared gate did not permit consultation: calls=%d decision=%+v", llm.calls, decision)
	}
}

func TestPolicyExclusionSkipsImpossibleLLMConsultation(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{prs: []github.PR{{Number: 17, HeadRefName: "feat/release", Mergeable: "MERGEABLE"}}, ciStatuses: map[int]string{17: "success"}}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 5, Status: state.StatusPROpen, Branch: "feat/release", PRNumber: 17, StartedAt: time.Now().UTC().Add(-time.Hour)}
	llm := &fakeLLM{}
	for i := 0; i < 3; i++ {
		decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
		if err != nil {
			t.Fatal(err)
		}
		if llm.calls != 0 || decision.RecommendedAction != ActionNone {
			t.Fatalf("excluded merge consulted model: calls=%d decision=%+v", llm.calls, decision)
		}
	}
	cfg.Supervisor.AllowedActions = append(defaultAllowedActions(), ActionMergePR)
	llm.output = `{"summary":"Merge the approved release.","recommended_action":"merge_pr","target":{"issue":5,"pr":17,"session":"slot-1"},"risk":"mutating","confidence":0.9,"reasons":["policy now allows merge"],"requires_approval":false}`
	decision, err := testLLMEngine(cfg, reader, llm).Decide(st)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 || decision.RecommendedAction != ActionMergePR {
		t.Fatalf("policy change did not permit consultation: calls=%d decision=%+v", llm.calls, decision)
	}
}

func TestOperatorGateOnOtherSessionDoesNotBlockSelectedAction(t *testing.T) {
	st := state.NewState()
	st.Sessions["blocked"] = &state.Session{OperatorGateName: "review-thread:pending"}
	st.Sessions["ready"] = &state.Session{}
	cfg := testConfig(t)
	cfg.Supervisor.AllowedActions = []string{ActionMergePR}
	decision := state.SupervisorDecision{RecommendedAction: ActionMergePR, Risk: RiskMutating, Target: &state.SupervisorTarget{Session: "ready", PR: 18}}
	if _, held := holdBlockedSupervisorDecision(st, decision, newSupervisorPolicy(cfg)); held {
		t.Fatal("unrelated operator gate blocked selected session")
	}
}
