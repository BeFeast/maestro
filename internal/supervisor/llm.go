package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

const defaultSupervisorPrompt = `You are the Maestro Supervisor LLM.

You receive a redacted state packet for one Maestro project. Synthesize the state, but do not invent permissions or actions.

Rules:
- Return one JSON object only. Do not include Markdown, comments, or prose outside JSON.
- recommended_action must be one of supervisor_policy.allowed_actions.
- If recommended_action is in supervisor_policy.approval_required_actions, requires_approval must be true.
- Do not request actions outside the packet policy.
- Treat deterministic detector output as a guardrail. Do not recommend an action that conflicts with it.
- Name the current project outcome in the summary or reasons. If you target an issue, explain why it advances that outcome.
- Runtime/deploy checks are read-only recommendations in v1; do not claim Maestro changed production state.
- The runtime will validate this JSON against policy before any action is recorded or executed.

Required JSON shape:
{
  "summary": "one sentence",
  "recommended_action": "one allowed action",
  "target": {"issue": 0, "pr": 0, "session": ""},
  "risk": "safe|mutating|approval_gated",
  "confidence": 0.0,
  "reasons": ["short reason"],
  "requires_approval": false
}

State packet:
{{STATE_PACKET}}
`

// LLMClient is the small backend surface Supervisor needs for one prompt.
type LLMClient interface {
	Complete(prompt string) (string, error)
}

type backendLLMClient struct {
	cfg *config.Config
}

func NewBackendLLMClient(cfg *config.Config) LLMClient {
	return &backendLLMClient{cfg: cfg}
}

func (c *backendLLMClient) Complete(prompt string) (string, error) {
	backendName, backendDef, err := supervisorBackend(c.cfg)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(c.cfg.StateDir, 0755); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	promptFile, err := os.CreateTemp(c.cfg.StateDir, "supervisor-prompt-*.md")
	if err != nil {
		return "", fmt.Errorf("create supervisor prompt file: %w", err)
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if _, err := promptFile.WriteString(prompt); err != nil {
		promptFile.Close()
		return "", fmt.Errorf("write supervisor prompt file: %w", err)
	}
	if err := promptFile.Close(); err != nil {
		return "", fmt.Errorf("close supervisor prompt file: %w", err)
	}

	backendCfg := worker.BackendConfig{
		Cmd:        backendDef.Cmd,
		ExtraArgs:  backendDef.ExtraArgs,
		PromptMode: backendDef.PromptMode,
		Provider:   backendDef.Provider,
		Model:      c.cfg.Supervisor.Model,
		Effort:     c.cfg.Supervisor.Effort,
	}
	worktree := c.cfg.LocalPath
	if strings.TrimSpace(worktree) == "" {
		worktree = "."
	}
	cmd, stdinFile, err := worker.BuildSupervisorCmd(backendName, backendCfg, promptPath, worktree)
	if err != nil {
		return "", fmt.Errorf("build supervisor backend cmd: %w", err)
	}
	if stdinFile != "" {
		in, err := os.Open(stdinFile)
		if err != nil {
			return "", fmt.Errorf("open supervisor prompt stdin: %w", err)
		}
		defer in.Close()
		cmd.Stdin = in
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run supervisor backend %q: %w", backendName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func supervisorBackend(cfg *config.Config) (string, config.BackendDef, error) {
	backendName := strings.TrimSpace(cfg.Supervisor.Backend)
	if backendName == "" {
		backendName = cfg.Model.Default
	}
	backendDef, ok := cfg.Model.Backends[backendName]
	if !ok {
		return "", config.BackendDef{}, fmt.Errorf("supervisor backend %q not found in model.backends", backendName)
	}
	return backendName, backendDef, nil
}

func (e *Engine) decideWithLLM(st *state.State) (state.SupervisorDecision, error) {
	if st == nil {
		st = state.NewState()
	}
	deterministic, err := e.decideDeterministic(st)
	if err != nil {
		return state.SupervisorDecision{}, err
	}

	policy := newSupervisorPolicy(e.cfg)
	packet, err := e.buildStatePacket(st, deterministic, policy)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	prompt, err := buildSupervisorPrompt(e.cfg, packet)
	if err != nil {
		return state.SupervisorDecision{}, err
	}

	client := e.llm
	if client == nil {
		client = NewBackendLLMClient(e.cfg)
	}
	output, err := client.Complete(prompt)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	llmDecision, err := ParseLLMDecision(output)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	decision, err := validateLLMDecision(llmDecision, deterministic, policy)
	var conflict *guardrailConflictError
	if errors.As(err, &conflict) {
		return resolveGuardrailConflict(llmDecision, deterministic, conflict), nil
	}
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	return decision, nil
}

func buildSupervisorPrompt(cfg *config.Config, packet supervisorStatePacket) (string, error) {
	packetJSON, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal supervisor state packet: %w", err)
	}
	statePacket := RedactSensitive(string(packetJSON))

	tmpl := defaultSupervisorPrompt
	if strings.TrimSpace(cfg.Supervisor.Prompt) != "" {
		data, err := os.ReadFile(cfg.Supervisor.Prompt)
		if err != nil {
			return "", fmt.Errorf("read supervisor prompt %s: %w", cfg.Supervisor.Prompt, err)
		}
		tmpl = string(data)
	}
	if strings.Contains(tmpl, "{{STATE_PACKET}}") {
		return strings.ReplaceAll(tmpl, "{{STATE_PACKET}}", statePacket), nil
	}
	return strings.TrimRight(tmpl, "\n") + "\n\nState packet:\n" + statePacket + "\n", nil
}

// LLMDecision is the strict JSON contract returned by the Supervisor LLM.
type LLMDecision struct {
	Summary           string                  `json:"summary"`
	RecommendedAction string                  `json:"recommended_action"`
	Target            *state.SupervisorTarget `json:"target"`
	Risk              string                  `json:"risk"`
	Confidence        float64                 `json:"confidence"`
	Reasons           []string                `json:"reasons"`
	RequiresApproval  bool                    `json:"requires_approval"`
}

func ParseLLMDecision(output string) (LLMDecision, error) {
	trimmed := strings.TrimSpace(output)
	decision, err := decodeLLMDecision(trimmed)
	if err == nil {
		return decision, validateLLMContractFields(decision)
	}
	if jsonText, ok := extractJSONObject(trimmed); ok && jsonText != trimmed {
		decision, err = decodeLLMDecision(jsonText)
		if err == nil {
			return decision, validateLLMContractFields(decision)
		}
	}
	return LLMDecision{}, fmt.Errorf("parse supervisor LLM decision: invalid JSON contract")
}

func decodeLLMDecision(raw string) (LLMDecision, error) {
	var decision LLMDecision
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return LLMDecision{}, err
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return LLMDecision{}, fmt.Errorf("extra content after JSON object")
	}
	return decision, nil
}

func extractJSONObject(output string) (string, bool) {
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return output[start : end+1], true
}

func validateLLMContractFields(decision LLMDecision) error {
	if strings.TrimSpace(decision.Summary) == "" {
		return fmt.Errorf("parse supervisor LLM decision: summary is required")
	}
	if strings.TrimSpace(decision.RecommendedAction) == "" {
		return fmt.Errorf("parse supervisor LLM decision: recommended_action is required")
	}
	if riskRank(decision.Risk) < 0 {
		return fmt.Errorf("parse supervisor LLM decision: unknown risk %q", decision.Risk)
	}
	if decision.Confidence < 0 || decision.Confidence > 1 {
		return fmt.Errorf("parse supervisor LLM decision: confidence must be between 0 and 1")
	}
	if len(compactReasons(decision.Reasons)) == 0 {
		return fmt.Errorf("parse supervisor LLM decision: at least one reason is required")
	}
	return nil
}

// guardrailConflictError marks a decision-layer disagreement between the
// supervisor LLM and the deterministic guardrail (action, target, or risk).
// It is deliberately a distinct type: a conflict is not a process fault, so
// decideWithLLM resolves it to a safe decision and keeps the loop alive
// instead of failing the cycle (#689).
type guardrailConflictError struct {
	reason string
}

func (e *guardrailConflictError) Error() string { return e.reason }

func validateLLMDecision(llm LLMDecision, deterministic state.SupervisorDecision, policy supervisorPolicy) (state.SupervisorDecision, error) {
	action := canonicalAction(llm.RecommendedAction)
	if action == "" || !policy.isAllowed(action) {
		return state.SupervisorDecision{}, fmt.Errorf("supervisor LLM action %q is not allowed by policy", llm.RecommendedAction)
	}
	if action != deterministic.RecommendedAction {
		return state.SupervisorDecision{}, &guardrailConflictError{reason: fmt.Sprintf("supervisor LLM action %q disagrees with deterministic guardrail %q", action, deterministic.RecommendedAction)}
	}
	if !targetsAgree(llm.Target, deterministic.Target) {
		return state.SupervisorDecision{}, &guardrailConflictError{reason: "supervisor LLM target disagrees with deterministic guardrail"}
	}
	if riskRank(llm.Risk) < riskRank(deterministic.Risk) {
		return state.SupervisorDecision{}, &guardrailConflictError{reason: fmt.Sprintf("supervisor LLM risk %q is lower than deterministic guardrail %q", llm.Risk, deterministic.Risk)}
	}
	requiresApproval := policy.requiresApproval(action)
	if requiresApproval && !llm.RequiresApproval {
		return state.SupervisorDecision{}, fmt.Errorf("supervisor LLM action %q requires approval by policy", action)
	}

	decision := deterministic
	decision.Summary = RedactSensitive(strings.TrimSpace(llm.Summary))
	decision.RecommendedAction = action
	decision.Target = copyTarget(llm.Target)
	decision.Risk = llm.Risk
	decision.Confidence = llm.Confidence
	decision.Reasons = compactReasons(append(outcomeGuardrailReasons(deterministic), redactReasons(llm.Reasons)...))
	decision.RequiresApproval = llm.RequiresApproval || requiresApproval
	return decision, nil
}

// resolveGuardrailConflict converts an LLM-vs-guardrail disagreement into a
// non-fatal decision (#689). Failing the cycle here turned a stable
// disagreement into a systemd crash-loop: the same project state reproduces
// the same answers on every restart (karaoke 2026-06-11, 13+ restarts in ~20
// minutes on PR #137), leaving the supervisor effectively down for the whole
// window. The conflict is a decision-layer condition, not a process fault,
// so it is logged, recorded as a guardrail_conflict stuck state, and
// resolved to the safe side:
//   - guardrail action is risk=safe (read-only, or operator-whitelisted
//     safe_actions in cautious mode): deterministic tie-break — the
//     guardrail decision wins and proceeds.
//   - guardrail action is mutating/approval-gated: neither side proceeds
//     unilaterally; hold an explicit no-op this cycle until LLM and
//     guardrail agree or an operator intervenes.
func resolveGuardrailConflict(llm LLMDecision, deterministic state.SupervisorDecision, conflict *guardrailConflictError) state.SupervisorDecision {
	// The policy check in validateLLMDecision runs before the guardrail
	// comparisons, so the LLM action is always canonical here.
	llmAction := canonicalAction(llm.RecommendedAction)
	log.Printf("[supervisor] guardrail conflict held as logged no-op, not a fatal exit (#689): %s", conflict.reason)

	stuck := state.SupervisorStuckState{
		Code:              state.StuckGuardrailConflict,
		Severity:          SeverityWarning,
		Summary:           fmt.Sprintf("Supervisor LLM and deterministic guardrail disagree: %s.", conflict.reason),
		Evidence:          compactReasons(append([]string{fmt.Sprintf("LLM recommended %q; deterministic guardrail computed %q", llmAction, deterministic.RecommendedAction)}, redactReasons(llm.Reasons)...)),
		RecommendedAction: "Inspect why the LLM and the deterministic guardrail disagree; the supervise loop stays alive and re-evaluates next cycle.",
		SupervisorCanAct:  false,
		Target:            copyTarget(deterministic.Target),
	}

	if deterministic.Risk == RiskSafe {
		decision := deterministic
		decision.Reasons = compactReasons(append([]string{
			conflict.reason,
			"Guardrail action is risk=safe, so the deterministic decision wins the tie-break",
		}, deterministic.Reasons...))
		decision.StuckStates = appendStuck(decision.StuckStates, stuck)
		return decision
	}

	decision := deterministic
	decision.RecommendedAction = ActionNone
	decision.Risk = RiskSafe
	decision.RequiresApproval = false
	decision.Mutations = nil
	decision.Target = nil
	decision.Confidence = 0.5
	decision.Summary = "Supervisor LLM and deterministic guardrail disagree on a mutating action; holding a no-op this cycle."
	decision.Reasons = compactReasons(append([]string{
		conflict.reason,
		fmt.Sprintf("Guardrail action %q is not risk=safe, so neither side executes until the disagreement resolves", deterministic.RecommendedAction),
	}, outcomeGuardrailReasons(deterministic)...))
	decision.StuckStates = appendStuck(decision.StuckStates, stuck)
	return decision
}

func outcomeGuardrailReasons(deterministic state.SupervisorDecision) []string {
	if deterministic.Outcome == nil {
		return nil
	}
	return []string{outcomeDecisionReason(*deterministic.Outcome)}
}

func redactReasons(reasons []string) []string {
	redacted := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		redacted = append(redacted, RedactSensitive(reason))
	}
	return compactReasons(redacted)
}

type supervisorPolicy struct {
	allowed          map[string]struct{}
	approvalRequired map[string]struct{}
}

func newSupervisorPolicy(cfg *config.Config) supervisorPolicy {
	allowedActions := cfg.Supervisor.AllowedActions
	if allowedActions == nil {
		allowedActions = defaultAllowedActions()
	}
	approvalActions := cfg.Supervisor.ApprovalRequiredActions
	if approvalActions == nil {
		approvalActions = defaultApprovalRequiredActions()
	}
	policy := supervisorPolicy{
		allowed:          make(map[string]struct{}, len(allowedActions)),
		approvalRequired: make(map[string]struct{}, len(approvalActions)),
	}
	for _, action := range allowedActions {
		if canonical := canonicalAction(action); canonical != "" {
			policy.allowed[canonical] = struct{}{}
		}
	}
	for _, action := range approvalActions {
		if canonical := canonicalAction(action); canonical != "" {
			policy.approvalRequired[canonical] = struct{}{}
		}
	}
	// #545: operator-configured approval-gated mutating verbs (merge_pr,
	// close_issue, delete_worktree, change_global_config) live in
	// ApprovalRequired, not ApprovalRequiredActions. Without folding them in
	// here the LLM policy never marks them approval-required and the mint is
	// skipped — the same class of gap that left the deterministic path unable
	// to gate merge_pr.
	for _, action := range cfg.Supervisor.ApprovalRequired {
		if canonical := canonicalAction(action); canonical != "" {
			policy.approvalRequired[canonical] = struct{}{}
		}
	}
	return policy
}

func (p supervisorPolicy) isAllowed(action string) bool {
	_, ok := p.allowed[action]
	return ok
}

func (p supervisorPolicy) requiresApproval(action string) bool {
	_, ok := p.approvalRequired[action]
	return ok
}

func defaultAllowedActions() []string {
	return []string{
		ActionNone,
		ActionWaitForRunningWorker,
		ActionWaitForCapacity,
		ActionWaitForOrderedQueue,
		ActionMonitorOpenPR,
		ActionReviewRetryExhausted,
		ActionCheckOutcomeHealth,
		ActionNotifyRed,
		ActionSpawnWorker,
		ActionSpawnRepairWorker,
		ActionLabelIssueReady,
		ActionOpenChildIssue,
		ActionPreflightFailed,
		ActionUnblockIssue,
	}
}

func defaultApprovalRequiredActions() []string {
	return []string{
		ActionReviewRetryExhausted,
		ActionSpawnWorker,
		ActionSpawnRepairWorker,
		ActionLabelIssueReady,
		ActionOpenChildIssue,
	}
}

func canonicalAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case ActionNone:
		return ActionNone
	case ActionWaitForRunningWorker:
		return ActionWaitForRunningWorker
	case ActionWaitForCapacity:
		return ActionWaitForCapacity
	case ActionWaitForOrderedQueue:
		return ActionWaitForOrderedQueue
	case ActionMonitorOpenPR:
		return ActionMonitorOpenPR
	case ActionReviewRetryExhausted:
		return ActionReviewRetryExhausted
	case ActionCheckOutcomeHealth:
		return ActionCheckOutcomeHealth
	case ActionNotifyRed:
		return ActionNotifyRed
	case ActionSpawnWorker:
		return ActionSpawnWorker
	case ActionSpawnRepairWorker:
		return ActionSpawnRepairWorker
	case ActionLabelIssueReady, "add_ready_label":
		return ActionLabelIssueReady
	case ActionOpenChildIssue, "create_issue", "create_child_issue":
		return ActionOpenChildIssue
	case ActionPreflightFailed:
		return ActionPreflightFailed
	case ActionUnblockIssue, "unblock", "remove_blocked_label_and_label_ready":
		return ActionUnblockIssue
	case ActionMergePR:
		return ActionMergePR
	case config.SupervisorActionCloseIssue:
		return config.SupervisorActionCloseIssue
	case config.SupervisorActionCloseIssueBatch:
		return config.SupervisorActionCloseIssueBatch
	case config.SupervisorActionDeleteWorktree:
		return config.SupervisorActionDeleteWorktree
	case config.SupervisorActionChangeGlobalConfig:
		return config.SupervisorActionChangeGlobalConfig
	default:
		return ""
	}
}

func riskRank(risk string) int {
	switch strings.ToLower(strings.TrimSpace(risk)) {
	case RiskSafe:
		return 0
	case RiskMutating:
		return 1
	case RiskApprovalGated:
		return 2
	default:
		return -1
	}
}

func targetsAgree(got, want *state.SupervisorTarget) bool {
	if want == nil {
		return got == nil || (got.Issue == 0 && got.PR == 0 && strings.TrimSpace(got.Session) == "")
	}
	if got == nil {
		return false
	}
	return got.Issue == want.Issue && got.PR == want.PR && strings.TrimSpace(got.Session) == strings.TrimSpace(want.Session)
}

func copyTarget(target *state.SupervisorTarget) *state.SupervisorTarget {
	if target == nil {
		return nil
	}
	copy := *target
	copy.Session = strings.TrimSpace(copy.Session)
	return &copy
}
