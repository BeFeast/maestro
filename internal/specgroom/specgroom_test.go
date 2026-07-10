package specgroom

import (
	"strings"
	"testing"
)

func TestBodyHash_WhitespaceInsensitiveButContentSensitive(t *testing.T) {
	a := BodyHash("hello world")
	b := BodyHash("  hello world\n")
	if a != b {
		t.Fatalf("expected leading/trailing whitespace to not change the hash: %s != %s", a, b)
	}
	if a == BodyHash("hello  world") {
		t.Fatalf("expected an internal content change to change the hash")
	}
	if a == "" {
		t.Fatalf("hash must be non-empty")
	}
}

func TestBuildPrompt_IncludesRubricBodyAndGroomMode(t *testing.T) {
	issue := Issue{Number: 42, Title: "Improve the dashboard", Body: "make it better", Labels: []string{"enhancement"}}

	lintOnly := BuildPrompt(issue, false)
	for _, want := range []string{"#42", "Improve the dashboard", "make it better", "enhancement", "testable_acceptance", "observable_verification"} {
		if !strings.Contains(lintOnly, want) {
			t.Fatalf("lint prompt missing %q\n%s", want, lintOnly)
		}
	}
	if !strings.Contains(lintOnly, "leave it empty when pass is true") {
		t.Fatalf("lint-only prompt should make the rewrite conditional on failure")
	}

	groom := BuildPrompt(issue, true)
	if !strings.Contains(groom, "always set") {
		t.Fatalf("groom prompt should force a rewrite regardless of pass")
	}
}

func TestBuildPrompt_HandlesEmptyFields(t *testing.T) {
	prompt := BuildPrompt(Issue{Number: 7}, false)
	if !strings.Contains(prompt, "(empty)") || !strings.Contains(prompt, "(no title)") || !strings.Contains(prompt, "(none)") {
		t.Fatalf("empty fields should render placeholders:\n%s", prompt)
	}
}

func TestParseVerdict_Valid(t *testing.T) {
	raw := `{"pass": false, "summary": "acceptance criteria are not testable",
	  "checklist": [{"rule":"testable_acceptance","ok":false,"note":"no observable outcome"}],
	  "rewritten_body": "## Summary\nTBD"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.Pass {
		t.Fatalf("expected pass=false")
	}
	if len(v.Checklist) != 1 || v.Checklist[0].Rule != "testable_acceptance" {
		t.Fatalf("checklist not parsed: %+v", v.Checklist)
	}
	if !strings.Contains(v.RewrittenBody, "## Summary") {
		t.Fatalf("rewrite not parsed: %q", v.RewrittenBody)
	}
}

func TestParseVerdict_ExtractsFromSurroundingProse(t *testing.T) {
	raw := "Sure! Here is the verdict:\n{\"pass\": true, \"summary\": \"looks good\"}\nHope that helps."
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("ParseVerdict with prose: %v", err)
	}
	if !v.Pass || v.Summary != "looks good" {
		t.Fatalf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdict_RejectsEmptyOrSummaryless(t *testing.T) {
	for _, raw := range []string{"", "   ", "{}", `{"pass": true}`, "not json at all"} {
		if _, err := ParseVerdict(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestParseVerdict_RejectsTrailingSecondObject(t *testing.T) {
	// A valid object followed by a second object is ambiguous — reject rather
	// than silently taking the first. (Prose fallback needs a single {...}.)
	if _, err := ParseVerdict(`{"pass":true,"summary":"a"}{"pass":false,"summary":"b"}`); err == nil {
		t.Fatalf("expected error on two concatenated objects")
	}
}

func TestFailingRules_OrderedByRubric(t *testing.T) {
	v := Verdict{
		Checklist: []ChecklistItem{
			{Rule: "single_repo", OK: false, Note: "touches two repos"},
			{Rule: "testable_acceptance", OK: false},
			{Rule: "explicit_scope", OK: true},
		},
	}
	failing := v.FailingRules()
	if len(failing) != 2 {
		t.Fatalf("expected 2 failing rules, got %d: %+v", len(failing), failing)
	}
	// Rubric order puts testable_acceptance before single_repo.
	if failing[0].Rule != "testable_acceptance" || failing[1].Rule != "single_repo" {
		t.Fatalf("failing rules not in rubric order: %+v", failing)
	}
}

func TestDetectGroomMention_FindsLatestAndIgnoresOwnComments(t *testing.T) {
	comments := []Comment{
		{ID: 1, Body: "please @maestro groom this", Author: "po"},
		{ID: 2, Body: LintCommentMarker + "\nComment `@maestro groom` to get a rewrite", Author: "maestro-bot"},
		{ID: 3, Body: "@MAESTRO GROOM again", Author: "po"},
		{ID: 4, Body: "unrelated chatter", Author: "po"},
	}
	m, ok := DetectGroomMention(comments)
	if !ok {
		t.Fatalf("expected a mention")
	}
	if m.ID != 3 {
		t.Fatalf("expected the latest real mention (id 3), got id %d", m.ID)
	}
}

func TestDetectGroomMention_None(t *testing.T) {
	if _, ok := DetectGroomMention([]Comment{{ID: 1, Body: "no trigger here", Author: "po"}}); ok {
		t.Fatalf("did not expect a mention")
	}
}

func TestDetectGroomMention_SkipsSelfLogin(t *testing.T) {
	comments := []Comment{{ID: 5, Body: "@maestro groom", Author: "maestro-bot"}}
	if _, ok := DetectGroomMention(comments, "maestro-bot"); ok {
		t.Fatalf("comment from a self login must be ignored")
	}
}

func TestRenderLintComment_HasMarkerAndChecklist(t *testing.T) {
	v := Verdict{
		Pass:    false,
		Summary: "not testable yet",
		Checklist: []ChecklistItem{
			{Rule: "testable_acceptance", OK: false, Note: "no observable outcome"},
			{Rule: "explicit_scope", OK: true},
			{Rule: "no_broad_refactor", OK: true},
			{Rule: "single_repo", OK: true},
			{Rule: "observable_verification", OK: false, Note: "unit tests only"},
		},
	}
	out := RenderLintComment(v)
	if !strings.HasPrefix(out, LintCommentMarker) {
		t.Fatalf("comment must start with the lint marker:\n%s", out)
	}
	if !strings.Contains(out, "not testable yet") {
		t.Fatalf("summary missing")
	}
	if !strings.Contains(out, "no observable outcome") || !strings.Contains(out, "unit tests only") {
		t.Fatalf("failing-rule notes missing:\n%s", out)
	}
	if !strings.Contains(out, "- [ ] Acceptance criteria are testable") {
		t.Fatalf("failing rule should render an unchecked box:\n%s", out)
	}
	if !strings.Contains(out, "- [x] Scope and non-goals are explicit") {
		t.Fatalf("passing rule should render a checked box:\n%s", out)
	}
	if !strings.Contains(out, GroomTrigger) {
		t.Fatalf("lint comment should tell the PO how to request a rewrite")
	}
}

func TestRenderGroomComment_CarriesRewriteOrEmpty(t *testing.T) {
	if got := RenderGroomComment(Verdict{Summary: "x"}); got != "" {
		t.Fatalf("no rewrite → empty proposal, got %q", got)
	}
	out := RenderGroomComment(Verdict{Summary: "here is a rewrite", RewrittenBody: "## Summary\nDo the thing"})
	if !strings.HasPrefix(out, GroomCommentMarker) {
		t.Fatalf("proposal must start with the groom marker:\n%s", out)
	}
	if !strings.Contains(out, "## Summary") || !strings.Contains(out, "Do the thing") {
		t.Fatalf("proposal must embed the rewrite:\n%s", out)
	}
	if !strings.Contains(out, "edit_issue_body") {
		t.Fatalf("proposal must explain the approval gate:\n%s", out)
	}
}

type fakeCompleter struct {
	out    string
	err    error
	prompt string
	calls  int
}

func (f *fakeCompleter) Complete(prompt string) (string, error) {
	f.calls++
	f.prompt = prompt
	return f.out, f.err
}

func TestEvaluate_OnePassParsesVerdict(t *testing.T) {
	fc := &fakeCompleter{out: `{"pass": true, "summary": "well formed"}`}
	v, err := Evaluate(fc, Issue{Number: 1, Body: "body"}, false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if fc.calls != 1 {
		t.Fatalf("expected exactly one LLM pass, got %d", fc.calls)
	}
	if !v.Pass {
		t.Fatalf("expected pass")
	}
}

func TestEvaluate_NilCompleter(t *testing.T) {
	if _, err := Evaluate(nil, Issue{Number: 1}, false); err == nil {
		t.Fatalf("expected error for nil completer")
	}
}
