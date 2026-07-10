// Package specgroom holds the pure, dependency-free core of the issue-grooming
// agent + spec-lint quality gate (#851):
//
//   - the "good spec" rubric derived from docs/po-spec-workflow.md,
//   - assembly of the single per-issue LLM prompt,
//   - strict-ish parsing of the structured verdict it returns,
//   - `@maestro groom` mention detection,
//   - body hashing for lint idempotency, and
//   - rendering the lint-checklist and groom-proposal comments.
//
// It imports only the standard library so it is trivially unit-testable and
// carries no coupling to the supervisor, GitHub, or config packages. The
// supervisor wires it to a real LLM backend and GitHub surfaces.
package specgroom

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// GroomTrigger is the comment mention that requests a grooming proposal.
const GroomTrigger = "@maestro groom"

// Comment markers let the supervisor (and humans) recognize maestro's own
// spec-groom comments. They are stable, machine-greppable HTML comments.
const (
	LintCommentMarker  = "<!-- maestro:spec-lint -->"
	GroomCommentMarker = "<!-- maestro:groom-proposal -->"
)

// Completer is the one-method LLM surface specgroom needs. supervisor's
// LLMClient (Complete(prompt) (string, error)) satisfies it directly.
type Completer interface {
	Complete(prompt string) (string, error)
}

// Issue is the minimal issue view the lint pass reasons over.
type Issue struct {
	Number int
	Title  string
	Body   string
	Labels []string
}

// Comment is the minimal issue-comment view used for mention detection.
type Comment struct {
	ID     int64
	Body   string
	Author string
}

// Rule is one canonical "good spec" criterion. Key is stable (used as the
// checklist item id the model echoes back); Label is human-facing.
type Rule struct {
	Key   string
	Label string
}

// Rules is the spec-lint rubric, lifted from docs/po-spec-workflow.md
// ("What makes a good spec"). The order is the order rendered in the checklist.
var Rules = []Rule{
	{Key: "testable_acceptance", Label: "Acceptance criteria are testable without re-asking the PO"},
	{Key: "explicit_scope", Label: "Scope and non-goals are explicit"},
	{Key: "no_broad_refactor", Label: "No implied broad refactor (worker prompt forbids them)"},
	{Key: "single_repo", Label: "No multi-repo coordination in one spec"},
	{Key: "observable_verification", Label: "Verification is observable against live services, not just unit tests"},
}

// ChecklistItem is the model's verdict for one rubric rule.
type ChecklistItem struct {
	Rule string `json:"rule"`
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

// Verdict is the structured output of the single LLM pass: the lint verdict
// plus (when failing or when grooming was requested) a full Spec-template
// rewrite of the issue body.
type Verdict struct {
	Pass          bool            `json:"pass"`
	Summary       string          `json:"summary"`
	Checklist     []ChecklistItem `json:"checklist,omitempty"`
	RewrittenBody string          `json:"rewritten_body,omitempty"`
}

// FailingRules returns the rubric items the model marked not-OK, in rubric
// order, resolving each to its human label.
func (v Verdict) FailingRules() []ChecklistItem {
	byKey := make(map[string]ChecklistItem, len(v.Checklist))
	for _, item := range v.Checklist {
		byKey[strings.TrimSpace(item.Rule)] = item
	}
	var failing []ChecklistItem
	for _, rule := range Rules {
		if item, ok := byKey[rule.Key]; ok && !item.OK {
			failing = append(failing, item)
		}
	}
	return failing
}

// BodyHash returns a stable digest of an issue body, used to lint at most once
// per body change. Leading/trailing whitespace is normalized so a trivial edit
// (a trailing newline) does not force a re-lint.
func BodyHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:])
}

// DetectGroomMention returns the most recent (highest-id) comment that contains
// the `@maestro groom` trigger. Maestro's own spec-groom comments are skipped
// via their markers — the lint comment itself names the trigger, so a naive
// scan would otherwise self-fire grooming on every lint. Comments authored by
// any login in selfLogins are also skipped. Match is case-insensitive.
func DetectGroomMention(comments []Comment, selfLogins ...string) (Comment, bool) {
	self := make(map[string]struct{}, len(selfLogins))
	for _, login := range selfLogins {
		if login = strings.TrimSpace(strings.ToLower(login)); login != "" {
			self[login] = struct{}{}
		}
	}
	var latest Comment
	found := false
	for _, c := range comments {
		if strings.Contains(c.Body, LintCommentMarker) || strings.Contains(c.Body, GroomCommentMarker) {
			continue
		}
		if _, isSelf := self[strings.ToLower(strings.TrimSpace(c.Author))]; isSelf {
			continue
		}
		if !strings.Contains(strings.ToLower(c.Body), GroomTrigger) {
			continue
		}
		if !found || c.ID > latest.ID {
			latest = c
			found = true
		}
	}
	return latest, found
}

const promptTemplate = `You are the Maestro spec-lint and issue-grooming agent.

Score ONE GitHub issue against Maestro's "good spec" rules and, when it falls
short (or when grooming was explicitly requested), rewrite it in the Spec
template structure. Do not invent product requirements: when the issue omits
information, mark it TBD rather than guessing.

Good-spec rules (each maps to a checklist item "rule" key):
%s

Rewrite structure (use these exact section headings, keep every existing
detail, mark genuine gaps as "TBD"):
## Summary
## Why
## Scope
## Acceptance criteria
## Test plan
## Non-goals

Return ONE JSON object and nothing else (no Markdown, no prose outside JSON):
{
  "pass": true,
  "summary": "one sentence on the overall verdict",
  "checklist": [{"rule": "testable_acceptance", "ok": true, "note": "short reason"}],
  "rewritten_body": "%s"
}

Rules for the JSON:
- Include exactly one checklist entry per rule key above.
- "pass" is true only when every checklist item is ok.
- %s
- Never include secrets, tokens, host paths, or internal identifiers.

Issue #%d: %s

Labels: %s

Issue body:
%s
`

// BuildPrompt assembles the single per-issue LLM prompt. When groomRequested is
// true the model is told to always return a rewrite; otherwise it returns a
// rewrite only when the issue fails lint.
func BuildPrompt(issue Issue, groomRequested bool) string {
	var rules strings.Builder
	for _, rule := range Rules {
		fmt.Fprintf(&rules, "- %s: %s\n", rule.Key, rule.Label)
	}
	rewriteHint := "full rewritten body when pass is false, else empty string"
	rewriteRule := "Set \"rewritten_body\" to a full Spec-template rewrite whenever pass is false; leave it empty when pass is true."
	if groomRequested {
		rewriteHint = "full rewritten body in the Spec template structure"
		rewriteRule = "Grooming was explicitly requested: always set \"rewritten_body\" to a full Spec-template rewrite, even if pass is true."
	}
	labels := strings.Join(issue.Labels, ", ")
	if strings.TrimSpace(labels) == "" {
		labels = "(none)"
	}
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = "(no title)"
	}
	body := strings.TrimSpace(issue.Body)
	if body == "" {
		body = "(empty)"
	}
	return fmt.Sprintf(promptTemplate,
		strings.TrimRight(rules.String(), "\n"),
		rewriteHint,
		rewriteRule,
		issue.Number, title,
		labels,
		body,
	)
}

// ParseVerdict decodes the model's JSON verdict, tolerating leading/trailing
// prose by extracting the first {...} block on a strict-decode failure. It is
// deliberately lenient about unknown fields (models add them) but requires a
// non-empty summary so a malformed empty object is rejected rather than posted.
func ParseVerdict(output string) (Verdict, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return Verdict{}, fmt.Errorf("specgroom: empty verdict output")
	}
	verdict, err := decodeVerdict(trimmed)
	if err != nil {
		if jsonText, ok := extractJSONObject(trimmed); ok && jsonText != trimmed {
			verdict, err = decodeVerdict(jsonText)
		}
	}
	if err != nil {
		return Verdict{}, fmt.Errorf("specgroom: parse verdict: %w", err)
	}
	if strings.TrimSpace(verdict.Summary) == "" {
		return Verdict{}, fmt.Errorf("specgroom: verdict is missing a summary")
	}
	return verdict, nil
}

func decodeVerdict(raw string) (Verdict, error) {
	var verdict Verdict
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	if err := decoder.Decode(&verdict); err != nil {
		return Verdict{}, err
	}
	// Reject trailing junk after the object so a concatenation of two objects
	// does not silently decode only the first.
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return Verdict{}, fmt.Errorf("extra content after JSON object")
	}
	return verdict, nil
}

func extractJSONObject(output string) (string, bool) {
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return output[start : end+1], true
}

// Evaluate runs the single LLM pass for one issue and returns the parsed
// verdict.
func Evaluate(c Completer, issue Issue, groomRequested bool) (Verdict, error) {
	if c == nil {
		return Verdict{}, fmt.Errorf("specgroom: nil completer")
	}
	output, err := c.Complete(BuildPrompt(issue, groomRequested))
	if err != nil {
		return Verdict{}, fmt.Errorf("specgroom: llm pass for issue #%d: %w", issue.Number, err)
	}
	return ParseVerdict(output)
}

// RenderLintComment renders the single spec-lint checklist comment for a
// failing issue. Every rubric rule is shown with a ✓/✗, and failing items carry
// the model's note. The stable LintCommentMarker leads the comment.
func RenderLintComment(v Verdict) string {
	byKey := make(map[string]ChecklistItem, len(v.Checklist))
	for _, item := range v.Checklist {
		byKey[strings.TrimSpace(item.Rule)] = item
	}
	var b strings.Builder
	b.WriteString(LintCommentMarker)
	b.WriteString("\n### Maestro spec-lint\n\n")
	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	b.WriteString("This issue is not yet a complete spec. Checklist against the good-spec rules:\n\n")
	for _, rule := range Rules {
		item, ok := byKey[rule.Key]
		passed := ok && item.OK
		fmt.Fprintf(&b, "- [%s] %s", boxFor(passed), rule.Label)
		if !passed && ok && strings.TrimSpace(item.Note) != "" {
			fmt.Fprintf(&b, " — %s", strings.TrimSpace(item.Note))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nComment `@maestro groom` to get a proposed rewrite in the Spec template structure.\n")
	return b.String()
}

func boxFor(ok bool) string {
	if ok {
		return "x"
	}
	return " "
}

// RenderGroomComment renders the groom-proposal comment carrying the rewritten
// body. It notes that applying the rewrite requires an approval — the comment
// itself changes nothing. Returns "" when there is no rewrite to propose.
func RenderGroomComment(v Verdict) string {
	rewrite := strings.TrimSpace(v.RewrittenBody)
	if rewrite == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(GroomCommentMarker)
	b.WriteString("\n### Maestro grooming proposal\n\n")
	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	b.WriteString("Proposed rewrite in the Spec template structure. This comment changes nothing on its own — applying it to the issue body is gated behind an `edit_issue_body` approval (approve to apply, reject to leave the issue untouched).\n\n")
	b.WriteString("<details><summary>Proposed issue body</summary>\n\n")
	b.WriteString("````markdown\n")
	b.WriteString(rewrite)
	b.WriteString("\n````\n\n</details>\n")
	return b.String()
}
