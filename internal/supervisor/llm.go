package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
	"github.com/google/uuid"
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
	cfg                 *config.Config
	role                string
	executionContext    context.Context
	projectConfigSHA256 string
	// backendHealth is an optional per-cycle snapshot of the project's
	// BackendHealth gates. When present, candidates in an active cooldown are
	// skipped instead of re-tried every supervise tick — without it the walk
	// re-burned the whole chain down to the last rung (live 2026-07: opencode
	// every cycle while the primaries cooled).
	backendHealth map[string]state.BackendHealth
	// memory tracks consecutive per-backend failures across consults (shared
	// by all withBackendHealth copies). A backend that keeps failing — e.g. a
	// carrier that cannot answer within its attempt timeout — gets skipped for
	// a window instead of billing another doomed generation every cycle. Burn
	// RCA 2026-07-24: without this, a 100%-failing chain was re-walked on every
	// consult for 4.5h.
	memory *supervisorBackendMemory
	// receiptSave is an injected persistence fault seam for offline tests.
	receiptSave func(*ConsultationReceipt) error
}

const (
	supervisorBackendAttemptTimeout = 45 * time.Second
	supervisorBackendTotalTimeout   = 3 * time.Minute
	// supervisorBackendFailureThreshold consecutive failures put a candidate
	// into a skip window; a success resets the count.
	supervisorBackendFailureThreshold = 3
	// supervisorBackendFailureSkipWindow is how long a repeatedly-failing
	// candidate is skipped before it may be probed again.
	supervisorBackendFailureSkipWindow = 10 * time.Minute
)

// supervisorBackendMemory is the supervisor-local failure memory. It is
// deliberately NOT written to state.BackendHealth: a print-mode consult
// timing out says nothing about the backend's fitness for long-form worker
// runs, so it must not gate dispatch.
type supervisorBackendMemory struct {
	mu        sync.Mutex
	failures  map[string]int
	skipUntil map[string]time.Time
}

func newSupervisorBackendMemory() *supervisorBackendMemory {
	return &supervisorBackendMemory{failures: map[string]int{}, skipUntil: map[string]time.Time{}}
}

// shouldSkip reports whether the candidate is inside an active skip window.
func (m *supervisorBackendMemory) shouldSkip(name string, now time.Time) (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.skipUntil[name]
	if !ok || now.After(until) {
		return time.Time{}, false
	}
	return until, true
}

// recordFailure bumps the candidate's consecutive-failure count and opens a
// skip window at the threshold. Returns the window expiry when one opened.
func (m *supervisorBackendMemory) recordFailure(name string, now time.Time) (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[name]++
	if m.failures[name] < supervisorBackendFailureThreshold {
		return time.Time{}, false
	}
	m.failures[name] = 0
	until := now.Add(supervisorBackendFailureSkipWindow)
	m.skipUntil[name] = until
	return until, true
}

func (m *supervisorBackendMemory) recordSuccess(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.failures, name)
	delete(m.skipUntil, name)
}

// supervisorAttemptTimeoutFor resolves the attempt budget for one candidate:
// per-backend override, then supervisor.attempt_timeout_seconds, then the 45s
// default.
func supervisorAttemptTimeoutFor(cfg *config.Config, def config.BackendDef) time.Duration {
	if def.SupervisorAttemptTimeoutSeconds > 0 {
		return time.Duration(def.SupervisorAttemptTimeoutSeconds) * time.Second
	}
	if cfg != nil && cfg.Supervisor.AttemptTimeoutSeconds > 0 {
		return time.Duration(cfg.Supervisor.AttemptTimeoutSeconds) * time.Second
	}
	return supervisorBackendAttemptTimeout
}

func supervisorTotalTimeoutFor(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.Supervisor.TotalTimeoutSeconds > 0 {
		return time.Duration(cfg.Supervisor.TotalTimeoutSeconds) * time.Second
	}
	return supervisorBackendTotalTimeout
}

func NewBackendLLMClient(cfg *config.Config) LLMClient {
	return &backendLLMClient{cfg: cfg, memory: newSupervisorBackendMemory()}
}

// withBackendHealth returns a copy of the client carrying the cycle's
// BackendHealth snapshot, so a concurrent Complete on the shared engine client
// never observes a mutated map. The failure memory pointer is shared — it must
// survive across cycles.
func (c *backendLLMClient) withBackendHealth(health map[string]state.BackendHealth) *backendLLMClient {
	return &backendLLMClient{cfg: c.cfg, backendHealth: health, memory: c.memory, receiptSave: c.receiptSave, role: c.role, executionContext: c.executionContext, projectConfigSHA256: c.projectConfigSHA256}
}

func (c *backendLLMClient) Complete(prompt string) (string, error) {
	result, err := c.CompleteConsultation(newConsultationIdentity(c.cfg, ""), prompt)
	return result.Output, err
}

func (c *backendLLMClient) CompleteConsultation(identity ConsultationIdentity, prompt string) (result ConsultationResult, retErr error) {
	if err := c.cfg.AIExecution.CheckCurrent(); err != nil {
		return result, err
	}
	if c.cfg.Supervisor.RequireAccountingReady && !c.cfg.AIExecution.RequireVerifiedRoute {
		return result, &ConsultationHold{Code: "accounting_route_unsupported"}
	}
	if identity.ID == "" {
		identity = newConsultationIdentity(c.cfg, identity.CycleID)
	}
	if uuid.Validate(identity.ID) != nil || identity.Role != c.executionRole() || identity.ProjectID != c.cfg.ProjectID || routeIdentifier(identity.CycleID) == "" || (identity.ParentRoleRunID != "" && uuid.Validate(identity.ParentRoleRunID) != nil) {
		return result, &ConsultationHold{Code: "consultation_identity_invalid"}
	}
	if strings.TrimSpace(c.cfg.StateDir) == "" {
		return result, &ConsultationHold{Code: "receipt_store_unavailable"}
	}
	store, unlock, err := openConsultationStore(c.cfg.StateDir)
	if err != nil {
		var hold *ConsultationHold
		if errors.As(err, &hold) {
			return result, hold
		}
		return result, &ConsultationHold{Code: "receipt_store_unavailable"}
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(store.dir, identity.ID+".json")); err == nil {
		return result, &ConsultationHold{Code: "consultation_identity_reused"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, &ConsultationHold{Code: "receipt_store_unavailable"}
	}
	receipt := newConsultationReceipt(c.cfg, identity)
	result.Receipt = receipt
	save := store.save
	if c.receiptSave != nil {
		save = c.receiptSave
	}
	persist := func() error {
		if err := save(receipt); err != nil {
			return &ConsultationHold{Code: "receipt_persistence_failed"}
		}
		return nil
	}
	if err := persist(); err != nil {
		return result, err
	}
	launchIntentWritten := false
	var releaseAux func()
	if c.cfg.RuntimeAuxiliaryLimiter != nil {
		releaseAux, err = c.cfg.RuntimeAuxiliaryLimiter.ReserveAuxiliary(c.cfg.StateDir, identity.ID)
		if err != nil {
			return result, err
		}
	} else if c.cfg.AIExecution.RequireVerifiedRoute || c.executionRole() == "reviewer" {
		return result, aiexecution.Held("auxiliary_controller_unavailable")
	}
	defer func() {
		if releaseAux != nil && !launchIntentWritten {
			releaseAux()
		}
	}()
	defer func() {
		// A persistence failure after launch must not erase its durable intent.
		var hold *ConsultationHold
		if errors.As(retErr, &hold) && hold.Code == "receipt_persistence_failed" {
			return
		}
		var nativeHold *aiexecution.Hold
		if errors.As(retErr, &nativeHold) && nativeHold.Code == "native_outcome_unverified" {
			return
		}
		end := time.Now().UTC()
		receipt.EndedAt = &end
		if retErr != nil {
			receipt.Status = "failed"
		} else {
			receipt.Status = "succeeded"
		}
		if err := persist(); err != nil {
			result.Output = ""
			retErr = err
			return
		}
		if launchIntentWritten {
			// Final outcome is durable. Cleanup failure must not convert a known
			// success into an apparent model failure that invites another call.
			// A retained marker conservatively holds the next consultation.
			if err := store.finishLaunch(); err != nil {
				log.Printf("[supervisor] consultation outcome persisted; launch marker cleanup incomplete")
			} else {
				launchIntentWritten = false
			}
		}
	}()
	candidates, err := supervisorBackendCandidates(c.cfg, c.backendHealth, time.Now().UTC())
	eligible := map[string]bool{}
	for _, candidate := range candidates {
		eligible[candidate.name] = true
	}
	primary, _, _ := supervisorBackend(c.cfg)
	seen := map[string]bool{}
	for _, name := range append([]string{primary}, c.cfg.Model.FallbackBackends...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] || eligible[name] {
			continue
		}
		seen[name] = true
		reason := "disabled_or_missing"
		if gate, ok := c.backendHealth[name]; ok && gate.State == state.BackendHealthCooldown {
			reason = "cooldown"
		}
		receipt.Candidates = append(receipt.Candidates, CandidateReceipt{Backend: routeIdentifier(name), Status: reason})
	}
	if err != nil {
		return result, err
	}
	promptFile, err := os.CreateTemp(c.cfg.StateDir, "supervisor-prompt-*.md")
	if err != nil {
		return result, fmt.Errorf("create supervisor prompt file: %w", err)
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if _, err := promptFile.WriteString(prompt); err != nil {
		_ = promptFile.Close()
		return result, err
	}
	if err := promptFile.Close(); err != nil {
		return result, err
	}
	worktree := c.cfg.LocalPath
	if strings.TrimSpace(worktree) == "" {
		worktree = "."
	}
	deadline := time.Now().Add(supervisorTotalTimeoutFor(c.cfg))
	var failed, skipped []string
	for _, candidate := range candidates {
		if candidate.def.IsMetered() && !c.cfg.Supervisor.AllowMeteredBackend {
			receipt.Candidates = append(receipt.Candidates, CandidateReceipt{Backend: routeIdentifier(candidate.name), Status: "metered_policy_denied"})
			continue
		}
		if _, skip := c.memory.shouldSkip(candidate.name, time.Now()); skip {
			skipped = append(skipped, candidate.name)
			receipt.Candidates = append(receipt.Candidates, CandidateReceipt{Backend: routeIdentifier(candidate.name), Status: "failure_memory"})
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			receipt.Candidates = append(receipt.Candidates, CandidateReceipt{Backend: routeIdentifier(candidate.name), Status: "deadline"})
			continue
		}
		timeout := supervisorAttemptTimeoutFor(c.cfg, candidate.def)
		if remaining < timeout {
			timeout = remaining
		}
		cmd, stdin, buildErr := buildSupervisorBackend(candidate.name, candidate.def, c.cfg, promptPath, worktree)
		if buildErr != nil {
			receipt.Candidates = append(receipt.Candidates, CandidateReceipt{Backend: routeIdentifier(candidate.name), Status: "build_failed"})
			failed = append(failed, candidate.name)
			continue
		}
		fallback := ""
		if len(failed) > 0 {
			fallback = "previous_candidate_failed"
		} else if candidate.name != primary {
			fallback = "primary_skipped"
		}
		intent := uuid.NewString()
		invocation := invocationForCommand(candidate, cmd, intent, len(receipt.Invocations)+1, fallback)
		if c.executionRole() == "reviewer" && (invocation.EffectiveCLIModel == nil || *invocation.EffectiveCLIModel != c.cfg.Supervisor.Model) {
			if stdin != nil {
				_ = stdin.Close()
			}
			return result, aiexecution.Held("reviewer_model_unproven")
		}
		candidateStatus := "launch_intent"
		if c.cfg.Supervisor.NativeSessionRegistration != nil {
			if err := prepareNativeSession(c.cfg, identity, &invocation, cmd); err != nil {
				if stdin != nil {
					_ = stdin.Close()
				}
				return result, err
			}
			candidateStatus = "registration_intent"
		}
		receipt.PlannedInvocation = &invocation
		receipt.Candidates = append(receipt.Candidates, CandidateReceipt{Backend: routeIdentifier(candidate.name), Status: candidateStatus, IntentID: intent})
		index := len(receipt.Candidates) - 1
		if err := persist(); err != nil {
			if stdin != nil {
				_ = stdin.Close()
			}
			return result, err
		}
		if invocation.NativeSession != nil {
			if err := store.beginRegistration(identity, invocation.NativeSession); err != nil {
				if stdin != nil {
					_ = stdin.Close()
				}
				return result, &ConsultationHold{Code: "receipt_persistence_failed"}
			}
			if err := registerNativeSession(c.cfg, invocation.NativeSession, time.Until(deadline)); err != nil {
				if stdin != nil {
					_ = stdin.Close()
				}
				return result, err
			}
			configDigest, digestErr := config.AIExecutionConfigDigest(c.cfg)
			if c.projectConfigSHA256 != "" {
				configDigest = c.projectConfigSHA256
				digestErr = nil
			}
			if digestErr != nil {
				return result, aiexecution.Held("project_config_unobservable")
			}
			if err := aiexecution.Inspect(c.cfg.AIExecution, aiexecution.LaunchSpec{
				ProjectID: identity.ProjectID, Role: identity.Role, RoleRunID: identity.ID, Model: c.cfg.Supervisor.Model,
				GatewayScope:          c.cfg.Supervisor.NativeSessionRegistration.GatewayScope,
				ExpectedPolicyVersion: c.cfg.Supervisor.NativeSessionRegistration.ExpectedPolicyVersion,
				Registration:          invocation.NativeSession.Acknowledgement,
				ProjectConfigSHA256:   configDigest,
			}, cmd); err != nil {
				if stdin != nil {
					_ = stdin.Close()
				}
				receipt.Candidates[index].Status = "readiness_held"
				if err := persist(); err != nil {
					return result, err
				}
				if err := store.finishRegistration(); err != nil {
					return result, &ConsultationHold{Code: "receipt_persistence_failed"}
				}
				return result, err
			}
			receipt.Candidates[index].Status = "launch_intent"
			if err := persist(); err != nil {
				if stdin != nil {
					_ = stdin.Close()
				}
				return result, err
			}
		}
		if c.cfg.AIExecution.RequireVerifiedRoute && invocation.NativeSession == nil {
			if stdin != nil {
				_ = stdin.Close()
			}
			return result, aiexecution.Held("native_registration_required")
		}
		if err := store.beginLaunch(identity, intent); err != nil {
			if stdin != nil {
				_ = stdin.Close()
			}
			return result, &ConsultationHold{Code: "receipt_persistence_failed"}
		}
		launchIntentWritten = true
		if invocation.NativeSession != nil {
			if err := store.finishRegistration(); err != nil {
				if stdin != nil {
					_ = stdin.Close()
				}
				return result, &ConsultationHold{Code: "receipt_persistence_failed"}
			}
			remaining = time.Until(deadline)
			if remaining <= 0 || invocation.NativeSession.Request.ExpiresAt <= time.Now().Unix() {
				if stdin != nil {
					_ = stdin.Close()
				}
				return result, &ConsultationHold{Code: "registration_launch_deadline"}
			}
			if remaining < timeout {
				timeout = remaining
			}
		}
		invocation.StartedAt = time.Now().UTC()
		if err := c.cfg.AIExecution.CheckCurrent(); err != nil {
			if stdin != nil {
				_ = stdin.Close()
			}
			return result, err
		}
		out, launched, status, runErr := outputWithTimeoutReceiptContext(c.executionContext, cmd, timeout)
		if stdin != nil {
			_ = stdin.Close()
		}
		invocation.EndedAt = time.Now().UTC()
		invocation.Status = status
		if invocation.NativeSession != nil && launched {
			checkpoint, err := store.saveNativeOutput(identity, invocation, out)
			if err != nil {
				return result, &ConsultationHold{Code: "receipt_persistence_failed"}
			}
			invocation.OutputCheckpoint = checkpoint
		}
		receipt.PlannedInvocation = nil
		receipt.Candidates[index].Status = status
		if launched {
			receipt.Invocations = append(receipt.Invocations, invocation)
		}
		// Persist the completed call BEFORE deciding whether another candidate may run.
		if err := persist(); err != nil {
			return result, err
		}
		if invocation.NativeSession != nil && launched {
			// Every local outcome, including exit 0, is financially unresolved
			// until the authority outcome bridge proves a terminal result for
			// this exact role-run. Keep its durable marker and auxiliary permit.
			return result, aiexecution.Held("native_outcome_unverified")
		}
		if runErr == nil {
			c.memory.recordSuccess(candidate.name)
			result.Output = strings.TrimSpace(string(out))
			return result, nil
		}
		if invocation.NativeSession != nil {
			// The failed process outcome is synced above. Clear only this known
			// completed launch before recording the next candidate's registration.
			if err := store.finishLaunch(); err != nil {
				return result, &ConsultationHold{Code: "receipt_persistence_failed"}
			}
			launchIntentWritten = false
		}
		failed = append(failed, candidate.name)
		c.memory.recordFailure(candidate.name, time.Now())
	}
	if len(failed) == 0 && len(skipped) > 0 {
		return result, fmt.Errorf("run supervisor backends: all candidates inside failure-memory skip windows (%s); deterministic guardrail proceeds", strings.Join(skipped, ", "))
	}
	return result, fmt.Errorf("run supervisor backends: all bounded candidates failed (%s)", strings.Join(failed, ", "))
}

type supervisorBackendCandidate struct {
	name string
	def  config.BackendDef
}

func supervisorBackendCandidates(cfg *config.Config, health map[string]state.BackendHealth, now time.Time) ([]supervisorBackendCandidate, error) {
	primary, def, err := supervisorBackend(cfg)
	if err != nil {
		return nil, err
	}
	names := append([]string{primary}, cfg.Model.FallbackBackends...)
	seen := make(map[string]struct{}, len(names))
	out := make([]supervisorBackendCandidate, 0, len(names))
	var cooling []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		candidate, ok := cfg.Model.Backends[name]
		if !ok || !candidate.IsEnabled() {
			continue
		}
		// Skip candidates in an active BackendHealth cooldown instead of
		// re-proving the outage with a live call every tick. The gate clears
		// itself: ReconcileBackendHealth drops elapsed entries, and an entry
		// whose RetryAfter has passed is eligible again here.
		if gate, gated := health[name]; gated && gate.State == state.BackendHealthCooldown {
			if gate.RetryAfter == nil || now.Before(*gate.RetryAfter) {
				cooling = append(cooling, name)
				continue
			}
		}
		if name == primary {
			candidate = def
		}
		out = append(out, supervisorBackendCandidate{name: name, def: candidate})
	}
	if len(out) == 0 {
		if len(cooling) > 0 {
			return nil, fmt.Errorf("supervisor backend candidates all cooling down (%s); skipping LLM this cycle", strings.Join(cooling, ", "))
		}
		return nil, fmt.Errorf("supervisor has no enabled backend candidates")
	}
	if len(cooling) > 0 {
		log.Printf("[supervisor] skipping cooling-down backend candidate(s) %s for this cycle", strings.Join(cooling, ", "))
	}
	return out, nil
}

func buildSupervisorBackend(name string, def config.BackendDef, cfg *config.Config, promptPath, worktree string) (*exec.Cmd, *os.File, error) {
	backendCfg := worker.BackendConfig{
		Cmd: def.Cmd, ExtraArgs: def.ExtraArgs, PromptMode: def.PromptMode, Provider: def.Provider,
		Model: cfg.Supervisor.Model, Effort: cfg.Supervisor.Effort, TempDir: cfg.Supervisor.EffectiveTempDir(),
	}
	cmd, stdinFile, err := worker.BuildSupervisorCmd(name, backendCfg, promptPath, worktree)
	if err != nil {
		return nil, nil, err
	}
	var stdin *os.File
	if stdinFile != "" {
		stdin, err = os.Open(stdinFile)
		if err != nil {
			return nil, nil, err
		}
		cmd.Stdin = stdin
	}
	return cmd, stdin, nil
}

func outputWithTimeout(cmd *exec.Cmd, timeout time.Duration) ([]byte, error) {
	out, _, _, err := outputWithTimeoutReceipt(cmd, timeout)
	return out, err
}

func outputWithTimeoutReceipt(cmd *exec.Cmd, timeout time.Duration) ([]byte, bool, string, error) {
	return outputWithTimeoutReceiptContext(context.Background(), cmd, timeout)
}

type boundedNativeOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *boundedNativeOutput) Bytes() []byte { return b.buffer.Bytes() }
func (b *boundedNativeOutput) Len() int      { return b.buffer.Len() }

func (b *boundedNativeOutput) Write(p []byte) (int, error) {
	const maxOutput = 4 << 20
	n := len(p)
	remaining := maxOutput - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func outputWithTimeoutReceiptContext(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) ([]byte, bool, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, "cancelled", err
	}
	var out boundedNativeOutput
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return nil, false, "start_failed", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if out.overflow {
			return out.Bytes(), true, "output_limit", fmt.Errorf("native output exceeded bounded receipt limit")
		}
		status := "succeeded"
		if err != nil {
			status = "failed"
		}
		return out.Bytes(), true, status, err
	case <-ctx.Done():
		worker.ForceKillProcessTree(cmd.Process.Pid)
		<-done
		return out.Bytes(), true, "cancelled", ctx.Err()
	case <-timer.C:
		// This is already a hard attempt deadline. Do not spend the worker
		// reaper's two-second SIGTERM grace period here or the bounded fallback
		// chain can overrun its advertised total deadline.
		worker.ForceKillProcessTree(cmd.Process.Pid)
		<-done
		return out.Bytes(), true, "timed_out", fmt.Errorf("timed out after %s", timeout.Round(time.Second))
	}
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

	// #837 / hands-off 2026-07-22: short-circuit EVERY risk=safe deterministic
	// decision, including label_issue_ready with planned mutations. validateLLMDecision
	// + resolveGuardrailConflict already force agreement with the safe guardrail, so
	// the LLM can only reword the summary. Waiting on a hung supervisor backend
	// (live ok-player: safe label path never returned, live→0) freezes the control
	// loop for nothing. Mutating / approval-gated decisions still consult the LLM.
	// Escape hatch: supervisor.always_consult_llm=true.
	if !e.cfg.Supervisor.AlwaysConsultLLM && deterministic.Risk == RiskSafe {
		log.Printf("[supervisor] supervise: safe decision, LLM skipped (action=%s risk=%s mutations=%d)", deterministic.RecommendedAction, deterministic.Risk, len(deterministic.Mutations))
		return deterministic, nil
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
	// Thread the cycle's BackendHealth gates into the backend walk so a
	// cooling-down candidate is skipped, not re-tried per tick. Custom LLM
	// clients (tests, remote implementations) pass through unchanged.
	if backendClient, ok := client.(*backendLLMClient); ok {
		client = backendClient.withBackendHealth(st.BackendHealth)
	}
	var output string
	if currentErr := e.cfg.AIExecution.CheckCurrent(); currentErr != nil {
		err = currentErr
	} else if e.cfg.Supervisor.RequireAccountingReady && !e.cfg.AIExecution.RequireVerifiedRoute {
		err = &ConsultationHold{Code: "accounting_route_unsupported"}
	} else if _, native := client.(*backendLLMClient); e.cfg.AIExecution.RequireVerifiedRoute && !native {
		err = aiexecution.Held("custom_completer_unsupported")
	} else if rich, ok := client.(ConsultationClient); ok {
		identity := newConsultationIdentity(e.cfg, deterministic.ID)
		var result ConsultationResult
		result, err = rich.CompleteConsultation(identity, prompt)
		output = result.Output
		deterministic.ConsultationID = identity.ID
	} else if e.cfg.AIExecution.RequireVerifiedRoute {
		err = aiexecution.Held("custom_completer_unsupported")
	} else {
		output, err = client.Complete(prompt)
	}
	if err != nil {
		// The deterministic guardrail already selected the only action the LLM
		// is allowed to agree with. Provider failure must not freeze the project
		// control loop: preserve that decision and make the degraded route visible.
		log.Printf("[supervisor] all model backends unavailable; continuing with deterministic guardrail: %v", err)
		deterministic.ErrorClass = ErrorClassSupervisorBackend
		var hold *ConsultationHold
		if errors.As(err, &hold) {
			deterministic.ErrorClass = hold.Code
		}
		var executionHold *aiexecution.Hold
		if errors.As(err, &executionHold) {
			deterministic.ErrorClass = executionHold.Code
		}
		deterministic.Reasons = append(deterministic.Reasons, "Supervisor model backends were unavailable; deterministic guardrail executed without model synthesis.")
		return deterministic, nil
	}
	llmDecision, err := ParseLLMDecision(output)
	if err != nil {
		return state.SupervisorDecision{}, err
	}
	decision, err := validateLLMDecision(llmDecision, deterministic, policy)
	decision.ConsultationID = deterministic.ConsultationID
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

func (c *backendLLMClient) executionRole() string {
	if c.role != "" {
		return c.role
	}
	return "supervisor"
}
