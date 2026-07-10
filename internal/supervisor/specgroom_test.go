package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/specgroom"
	"github.com/befeast/maestro/internal/state"
)

// editBodyGH is a minimal approver.GitHubClient that records EditIssueBody
// calls, used by the end-to-end mint→approve→execute test.
type editBodyGH struct {
	editIssue int
	editBody  string
	edits     int
	// currentBody is what IssueBody reports as the live issue body — the
	// stale-edit guard hashes it against the approval's BaseBodyHash before
	// applying the rewrite.
	currentBody string
}

func (g *editBodyGH) MergePR(int) error               { return nil }
func (g *editBodyGH) CloseIssue(int, string) error    { return nil }
func (g *editBodyGH) AddIssueLabel(int, string) error { return nil }
func (g *editBodyGH) PRMergeStatus(int) (string, string, error) {
	return "", "", nil
}
func (g *editBodyGH) UpdateBranch(int) error { return nil }
func (g *editBodyGH) EditIssueBody(issue int, body string) error {
	g.editIssue = issue
	g.editBody = body
	g.edits++
	return nil
}
func (g *editBodyGH) IssueBody(int) (string, error) { return g.currentBody, nil }

func issueWithBody(number int, title, body string, labels ...string) github.Issue {
	issue := testIssue(number, title, labels...)
	issue.Body = body
	return issue
}

func specGroomEngine(t *testing.T, reader *fakeReader, llmOut string) *Engine {
	t.Helper()
	cfg := testConfig(t)
	cfg.Supervisor.SpecGroom.Enabled = true
	eng := testEngine(cfg, reader)
	eng.llm = &fakeLLM{output: llmOut}
	return eng
}

const failVerdict = `{"pass": false, "summary": "acceptance criteria are not testable",
  "checklist": [
    {"rule":"testable_acceptance","ok":false,"note":"no observable outcome"},
    {"rule":"explicit_scope","ok":true},
    {"rule":"no_broad_refactor","ok":true},
    {"rule":"single_repo","ok":true},
    {"rule":"observable_verification","ok":false,"note":"unit tests only"}
  ],
  "rewritten_body": "## Summary\nGroomed rewrite\n## Acceptance criteria\n- observable thing"}`

const passVerdict = `{"pass": true, "summary": "well-formed spec",
  "checklist": [
    {"rule":"testable_acceptance","ok":true},
    {"rule":"explicit_scope","ok":true},
    {"rule":"no_broad_refactor","ok":true},
    {"rule":"single_repo","ok":true},
    {"rule":"observable_verification","ok":true}
  ]}`

func TestRunSpecGroom_DisabledIsNoOp(t *testing.T) {
	reader := &fakeReader{issues: []github.Issue{issueWithBody(1, "vague", "do stuff")}}
	cfg := testConfig(t)
	// SpecGroom.Enabled defaults false.
	eng := testEngine(cfg, reader)
	llm := &fakeLLM{output: failVerdict}
	eng.llm = llm
	st := state.NewState()

	eng.runSpecGroom(st, reader)

	if llm.calls != 0 {
		t.Fatalf("disabled feature must not call the LLM (calls=%d)", llm.calls)
	}
	if len(reader.comments) != 0 {
		t.Fatalf("disabled feature must post no comments: %v", reader.comments)
	}
}

func TestRunSpecGroom_EmergencyHaltSkipsLLM(t *testing.T) {
	reader := &fakeReader{issues: []github.Issue{issueWithBody(1, "vague", "do stuff")}}
	llm := &fakeLLM{output: failVerdict}
	cfg := testConfig(t)
	cfg.Supervisor.SpecGroom.Enabled = true
	eng := testEngine(cfg, reader)
	eng.llm = llm
	eng.SetEmergencyLLMHalt(true) // #840 fleet-wide EMERGENCY STOP LLM gate
	st := state.NewState()

	eng.runSpecGroom(st, reader)

	if llm.calls != 0 {
		t.Fatalf("emergency LLM halt must skip the grooming LLM pass (calls=%d)", llm.calls)
	}
	if len(reader.comments) != 0 {
		t.Fatalf("emergency LLM halt must post no comments: %v", reader.comments)
	}
}

func TestRunSpecGroom_RedactsIssueTextBeforePrompt(t *testing.T) {
	// A credential-shaped token in the title AND body must be scrubbed before
	// it reaches the LLM backend (#851 review), matching how every other
	// supervisor prompt redacts issue text.
	secret := "sk-abcdefghijklmnopqrstuvwxyz012345"
	reader := &fakeReader{issues: []github.Issue{
		issueWithBody(1, "leaky "+secret, "Deploy with Authorization: Bearer "+secret),
	}}
	llm := &fakeLLM{output: failVerdict}
	cfg := testConfig(t)
	cfg.Supervisor.SpecGroom.Enabled = true
	eng := testEngine(cfg, reader)
	eng.llm = llm
	st := state.NewState()

	eng.runSpecGroom(st, reader)

	if llm.calls == 0 {
		t.Fatalf("expected the grooming LLM pass to run")
	}
	if strings.Contains(llm.prompt, secret) {
		t.Fatalf("issue text with a credential shape leaked into the prompt: %q", llm.prompt)
	}
	if !strings.Contains(llm.prompt, "[REDACTED") {
		t.Fatalf("expected a redaction marker in the prompt, got: %q", llm.prompt)
	}
}

func TestRunSpecGroom_RotatesWindowToDrainBacklog(t *testing.T) {
	// More open issues than the per-cycle cap: the old fixed [0,cap) window
	// examined the same first N every cycle and starved the tail. The rotating
	// window (#851 review) must reach issue cap+1.. over successive cycles.
	total := maxSpecGroomIssuesPerCycle + 5
	var issues []github.Issue
	for i := 1; i <= total; i++ {
		issues = append(issues, issueWithBody(i, "vague", "needs work"))
	}
	reader := &fakeReader{issues: issues}
	eng := specGroomEngine(t, reader, failVerdict)
	st := state.NewState()

	// Cycle 1 examines exactly the cap; the tail is untouched.
	eng.runSpecGroom(st, reader)
	if len(reader.comments) != maxSpecGroomIssuesPerCycle {
		t.Fatalf("cycle 1 should lint exactly the cap (%d), got %d", maxSpecGroomIssuesPerCycle, len(reader.comments))
	}
	if _, ok := st.SpecLintTrackFor(total); ok {
		t.Fatalf("issue #%d must not be examined in cycle 1 (past the cap)", total)
	}

	// Cycle 2 resumes past the window and reaches the tail.
	eng.runSpecGroom(st, reader)
	if _, ok := st.SpecLintTrackFor(total); !ok {
		t.Fatalf("issue #%d must be examined by cycle 2 (rotating window)", total)
	}
	for i := 1; i <= total; i++ {
		if _, ok := st.SpecLintTrackFor(i); !ok {
			t.Fatalf("issue #%d never linted across two cycles — window did not drain", i)
		}
	}
}

func TestRunSpecGroom_LintsFailingCandidateExactlyOnce(t *testing.T) {
	reader := &fakeReader{issues: []github.Issue{issueWithBody(1, "vague", "do stuff")}}
	eng := specGroomEngine(t, reader, failVerdict)
	st := state.NewState()

	eng.runSpecGroom(st, reader)
	if len(reader.comments) != 1 {
		t.Fatalf("expected exactly one lint comment, got %d: %v", len(reader.comments), reader.comments)
	}
	if !strings.Contains(reader.comments[0], specgroom.LintCommentMarker) {
		t.Fatalf("comment missing lint marker: %q", reader.comments[0])
	}
	if track, ok := st.SpecLintTrackFor(1); !ok || track.Pass {
		t.Fatalf("expected a recorded failing lint track, got %+v ok=%v", track, ok)
	}

	// Second cycle over the SAME body → zero new comments (idempotency).
	eng.runSpecGroom(st, reader)
	if len(reader.comments) != 1 {
		t.Fatalf("unchanged body must not re-lint: %d comments", len(reader.comments))
	}
}

func TestRunSpecGroom_WellFormedIssueGetsNoComment(t *testing.T) {
	reader := &fakeReader{issues: []github.Issue{issueWithBody(1, "good spec", "## Summary...")}}
	eng := specGroomEngine(t, reader, passVerdict)
	st := state.NewState()

	eng.runSpecGroom(st, reader)
	if len(reader.comments) != 0 {
		t.Fatalf("a passing issue must get no comment: %v", reader.comments)
	}
	if !st.SpecLintPassedForBody(1, specgroom.BodyHash("## Summary...")) {
		t.Fatalf("passing lint should be recorded so require_lint_pass can allow the label")
	}
}

func TestRunSpecGroom_ChangedBodyRelints(t *testing.T) {
	reader := &fakeReader{issues: []github.Issue{issueWithBody(1, "vague", "v1")}}
	eng := specGroomEngine(t, reader, failVerdict)
	st := state.NewState()

	eng.runSpecGroom(st, reader)
	if len(reader.comments) != 1 {
		t.Fatalf("expected one lint comment, got %d", len(reader.comments))
	}
	// Body changes → the lint must run again.
	reader.issues[0].Body = "v2 different content"
	eng.runSpecGroom(st, reader)
	if len(reader.comments) != 2 {
		t.Fatalf("changed body should re-lint: got %d comments", len(reader.comments))
	}
}

func TestRunSpecGroom_ReadyIssueNotPassivelyLinted(t *testing.T) {
	reader := &fakeReader{issues: []github.Issue{issueWithBody(1, "ready one", "body", "maestro-ready")}}
	eng := specGroomEngine(t, reader, failVerdict)
	eng.cfg.Supervisor.ReadyLabel = "maestro-ready"
	st := state.NewState()

	eng.runSpecGroom(st, reader)
	if len(reader.comments) != 0 {
		t.Fatalf("already-ready issues must not be passively re-linted: %v", reader.comments)
	}
}

func TestRunSpecGroom_GroomMentionMintsProposalAndApproval(t *testing.T) {
	reader := &fakeReader{
		issues: []github.Issue{issueWithBody(1, "needs work", "vague body", "maestro-ready")},
		issueComments: map[int][]github.IssueComment{
			1: {{ID: 500, Body: "@maestro groom please", Author: "po"}},
		},
	}
	eng := specGroomEngine(t, reader, failVerdict)
	eng.cfg.Supervisor.ReadyLabel = "maestro-ready" // ready issue: no passive lint, but mention still grooms
	st := state.NewState()

	eng.runSpecGroom(st, reader)

	if len(reader.comments) != 1 || !strings.Contains(reader.comments[0], specgroom.GroomCommentMarker) {
		t.Fatalf("expected one groom proposal comment, got %v", reader.comments)
	}
	approval, ok := st.PendingEditIssueBodyApproval(1)
	if !ok {
		t.Fatalf("expected a pending edit_issue_body approval")
	}
	if approval.Target == nil || !strings.Contains(approval.Target.Body, "Groomed rewrite") {
		t.Fatalf("approval must carry the rewrite body: %+v", approval.Target)
	}

	// Second cycle: the mention is already handled → no duplicate comment/approval.
	eng.runSpecGroom(st, reader)
	if len(reader.comments) != 1 {
		t.Fatalf("handled mention must not re-post: %d comments", len(reader.comments))
	}
	if len(st.Approvals) != 1 {
		t.Fatalf("handled mention must not mint a duplicate approval: %d", len(st.Approvals))
	}
}

func TestRunSpecGroom_NewMentionAfterHandledOneReGrooms(t *testing.T) {
	reader := &fakeReader{
		issues: []github.Issue{issueWithBody(1, "needs work", "vague body")},
		issueComments: map[int][]github.IssueComment{
			1: {{ID: 500, Body: "@maestro groom", Author: "po"}},
		},
	}
	eng := specGroomEngine(t, reader, failVerdict)
	st := state.NewState()

	eng.runSpecGroom(st, reader)
	firstComments := len(reader.comments)

	// A newer @maestro groom comment is a fresh request.
	reader.issueComments[1] = append(reader.issueComments[1], github.IssueComment{ID: 600, Body: "@maestro groom again", Author: "po"})
	eng.runSpecGroom(st, reader)
	if len(reader.comments) <= firstComments {
		t.Fatalf("a newer mention should re-groom (comments %d -> %d)", firstComments, len(reader.comments))
	}
}

func TestSpecLintAllowsReady_RequireLintPassGate(t *testing.T) {
	reader := &fakeReader{}
	eng := specGroomEngine(t, reader, passVerdict)
	eng.cfg.Supervisor.SpecGroom.RequireLintPass = true
	st := state.NewState()
	issue := issueWithBody(1, "candidate", "the body")

	// Not yet linted → default-closed: label withheld.
	if eng.specLintAllowsReady(st, issue) {
		t.Fatalf("require_lint_pass must withhold the label for a not-yet-linted issue")
	}
	// Failing lint → withheld.
	st.RecordSpecLint(1, specgroom.BodyHash("the body"), false, eng.now())
	if eng.specLintAllowsReady(st, issue) {
		t.Fatalf("require_lint_pass must withhold the label for a failing issue")
	}
	// Passing lint for the current body → allowed.
	st.RecordSpecLint(1, specgroom.BodyHash("the body"), true, eng.now())
	if !eng.specLintAllowsReady(st, issue) {
		t.Fatalf("require_lint_pass must allow the label once lint passes")
	}
	// A body change invalidates the pass.
	issue.Body = "changed"
	if eng.specLintAllowsReady(st, issue) {
		t.Fatalf("a passing verdict must not carry over to a changed body")
	}
}

func TestSpecLintAllowsReady_DefaultOffAlwaysAllows(t *testing.T) {
	reader := &fakeReader{}
	cfg := testConfig(t) // SpecGroom disabled, RequireLintPass false
	eng := testEngine(cfg, reader)
	st := state.NewState()
	if !eng.specLintAllowsReady(st, issueWithBody(1, "x", "y")) {
		t.Fatalf("default config must never withhold the ready label")
	}

	// Enabled but require_lint_pass off → still always allows.
	eng.cfg.Supervisor.SpecGroom.Enabled = true
	if !eng.specLintAllowsReady(st, issueWithBody(1, "x", "y")) {
		t.Fatalf("warn-only mode must not withhold the ready label")
	}
}

// TestSpecGroom_EndToEnd_MintApproveExecute is the integration flow from the
// spec (#851): a vague issue with an `@maestro groom` mention yields a proposal
// comment + a pending edit_issue_body approval; approving it drives the approver
// executor to apply the rewrite; rejecting a fresh one leaves the issue
// untouched.
func TestSpecGroom_EndToEnd_MintApproveExecute(t *testing.T) {
	reader := &fakeReader{
		issues: []github.Issue{issueWithBody(42, "vague", "do the thing")},
		issueComments: map[int][]github.IssueComment{
			42: {{ID: 900, Body: "@maestro groom", Author: "po"}},
		},
	}
	eng := specGroomEngine(t, reader, failVerdict)
	eng.cfg.Repo = "owner/repo"
	st := state.NewState()

	// 1. Mention → proposal comment + pending approval (no body change yet).
	eng.runSpecGroom(st, reader)
	approval, ok := st.PendingEditIssueBodyApproval(42)
	if !ok {
		t.Fatalf("expected a pending edit_issue_body approval")
	}

	// 2. Approve → 3. execute via the approver executor → issue body updated.
	if _, err := st.ApproveApproval(approval.ID, time.Now().UTC(), "operator", "looks good"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// The live body still matches what the rewrite was groomed against, so the
	// stale-edit guard passes and the rewrite is applied.
	gh := &editBodyGH{currentBody: "do the thing"}
	ex := &approver.Executor{GH: gh, Cfg: &config.Config{Repo: "owner/repo"}}
	res := ex.Execute(approval)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("execute edit_issue_body: %+v", res)
	}
	if gh.edits != 1 || gh.editIssue != 42 || !strings.Contains(gh.editBody, "Groomed rewrite") {
		t.Fatalf("executor did not apply the rewrite: issue=%d edits=%d body=%q", gh.editIssue, gh.edits, gh.editBody)
	}

	// Reject path: a fresh proposal that is rejected fires no side effect.
	rej := st.RecordEditIssueBodyApproval(99, "## Summary\nx", "basehash", "apply", "owner/repo", "owner/repo", nil, time.Now().UTC())
	if _, err := st.RejectApproval(rej.ID, time.Now().UTC(), "operator", "no thanks"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	gh2 := &editBodyGH{}
	// A rejected approval never reaches the executor loop; assert it is not in
	// the approved set.
	for _, a := range st.ListApprovedApprovals() {
		if a.ID == rej.ID {
			t.Fatalf("rejected approval must not appear in the approved set")
		}
	}
	if gh2.edits != 0 {
		t.Fatalf("reject must not edit any issue body")
	}
}

func TestFirstQueueActionCandidate_RequireLintPassWithholdsReadyLabel(t *testing.T) {
	issue := issueWithBody(7, "promote me", "the body")
	reader := &fakeReader{issues: []github.Issue{issue}}
	eng := specGroomEngine(t, reader, passVerdict)
	eng.cfg.Supervisor.ReadyLabel = "maestro-ready"
	eng.cfg.Supervisor.SpecGroom.RequireLintPass = true
	st := state.NewState()

	// No passing lint recorded → the ready-label queue action is withheld.
	cand, err := eng.firstQueueActionCandidate(st, []github.Issue{issue})
	if err != nil {
		t.Fatalf("firstQueueActionCandidate: %v", err)
	}
	if cand != nil && cand.addReady {
		t.Fatalf("expected add_ready_label withheld under require_lint_pass, got candidate %+v", cand)
	}

	// Record a passing lint → the label is now permitted.
	st.RecordSpecLint(7, specgroom.BodyHash("the body"), true, eng.now())
	cand, err = eng.firstQueueActionCandidate(st, []github.Issue{issue})
	if err != nil {
		t.Fatalf("firstQueueActionCandidate: %v", err)
	}
	if cand == nil || !cand.addReady {
		t.Fatalf("expected add_ready_label allowed after a passing lint, got %+v", cand)
	}
}
