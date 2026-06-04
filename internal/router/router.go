package router

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

const defaultRouterPrompt = `Given this GitHub issue, choose the best AI coding backend and classify the task type.
Available backends: {{BACKENDS}}
Task types: refactor, bugfix, test, vision, design, docs, infra

Issue: #{{NUMBER}} — {{TITLE}}
{{BODY}}

Return JSON only: {"backend": "<name>", "task_type": "<task type>", "reason": "<one sentence>"}`

const (
	TaskTypeRefactor = "refactor"
	TaskTypeBugfix   = "bugfix"
	TaskTypeTest     = "test"
	TaskTypeVision   = "vision"
	TaskTypeDesign   = "design"
	TaskTypeDocs     = "docs"
	TaskTypeInfra    = "infra"
)

// routerResponse is the expected JSON response from the router model.
type routerResponse struct {
	Backend  string `json:"backend"`
	TaskType string `json:"task_type"`
	Reason   string `json:"reason"`
}

// Decision is the structured router result used by auto-routing.
type Decision struct {
	Backend  string
	TaskType string
	Reason   string
}

// Router selects the best backend for a given issue using an LLM call.
type Router struct {
	cfg *config.Config

	// RouteFn overrides the default Route method (used in tests).
	RouteFn func(issue github.Issue) (string, string, error)

	// DecisionFn overrides the default RouteDecision method (used in tests
	// that need task_type-aware routing).
	DecisionFn func(issue github.Issue) (Decision, error)
}

// New creates a new Router.
func New(cfg *config.Config) *Router {
	return &Router{cfg: cfg}
}

// Route calls the router model to decide which backend to use for the issue.
// Returns the backend name and a short reason for the decision.
// On any error (model call, parse, unknown backend), returns the configured
// default backend and a non-nil error so callers can distinguish a real
// auto-routed pick from a silent fallback (#427).
func (r *Router) Route(issue github.Issue) (backendName string, reason string, err error) {
	decision, err := r.RouteDecision(issue)
	return decision.Backend, decision.Reason, err
}

// RouteDecision calls the router model and returns the structured backend
// decision including the task taxonomy classification.
func (r *Router) RouteDecision(issue github.Issue) (Decision, error) {
	prompt := r.buildPrompt(issue)

	output, err := r.callModel(prompt)
	if err != nil {
		log.Printf("[router] model call failed: %v — falling back to default", err)
		return Decision{Backend: r.cfg.Model.Default, Reason: "router error"}, err
	}

	resp, err := parseResponse(output)
	if err != nil {
		log.Printf("[router] parse response failed: %v — falling back to default", err)
		return Decision{Backend: r.cfg.Model.Default, Reason: "parse error"}, err
	}

	// Validate that the chosen backend exists in config. Return an error so
	// ResolveBackend can attribute the fallback to router_error rather than
	// silently presenting the default backend as if it was task-routed (#427).
	if _, ok := r.cfg.Model.Backends[resp.Backend]; !ok {
		log.Printf("[router] unknown backend %q — falling back to default", resp.Backend)
		return Decision{Backend: r.cfg.Model.Default, TaskType: resp.TaskType, Reason: fmt.Sprintf("unknown backend %q", resp.Backend)}, fmt.Errorf("router picked unknown backend %q", resp.Backend)
	}

	return Decision{Backend: resp.Backend, TaskType: resp.TaskType, Reason: resp.Reason}, nil
}

// buildPrompt constructs the router prompt from the template and issue data.
func (r *Router) buildPrompt(issue github.Issue) string {
	tmpl := r.cfg.Routing.RouterPrompt
	if tmpl == "" {
		tmpl = defaultRouterPrompt
	}

	// Build backends list
	backends := make([]string, 0, len(r.cfg.Model.Backends))
	for name := range r.cfg.Model.Backends {
		backends = append(backends, name)
	}

	prompt := tmpl
	prompt = strings.ReplaceAll(prompt, "{{BACKENDS}}", strings.Join(backends, ", "))
	prompt = strings.ReplaceAll(prompt, "{{NUMBER}}", fmt.Sprint(issue.Number))
	prompt = strings.ReplaceAll(prompt, "{{TITLE}}", issue.Title)
	prompt = strings.ReplaceAll(prompt, "{{BODY}}", issue.Body)

	return prompt
}

// callModel executes the router model CLI and returns the raw output.
func (r *Router) callModel(prompt string) (string, error) {
	backendName := r.cfg.Routing.RouterModel
	backend, ok := r.cfg.Model.Backends[backendName]
	if !ok {
		return "", fmt.Errorf("router backend %q not found in model.backends", backendName)
	}

	cmdSpec := strings.TrimSpace(backend.Cmd)
	if cmdSpec == "" {
		cmdSpec = backendName
	}

	cmdPath, args := splitRouterCmd(cmdSpec)
	if cmdPath == "" {
		return "", fmt.Errorf("router backend %q has empty command", backendName)
	}
	args = append(args, backend.ExtraArgs...)
	args = append(args, "-p", prompt)
	if r.cfg.Routing.RouterModelName != "" && !hasModelArg(args) {
		args = append(args, "--model", r.cfg.Routing.RouterModelName)
	}

	log.Printf("[router] calling %s for backend %s", cmdPath, backendName)
	out, err := exec.Command(cmdPath, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("exec %s: %w\n%s", cmdPath, err, out)
	}

	return strings.TrimSpace(string(out)), nil
}

func splitRouterCmd(cmd string) (binary string, prefixArgs []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

func hasModelArg(args []string) bool {
	for _, arg := range args {
		if arg == "--model" || arg == "-m" || strings.HasPrefix(arg, "--model=") {
			return true
		}
	}
	return false
}

// parseResponse extracts a JSON object from the model output.
// The model may include extra text around the JSON, so we find and extract it.
func parseResponse(output string) (routerResponse, error) {
	// Try direct unmarshal first
	if resp, err := decodeRouterResponse(output); err == nil {
		return resp, nil
	}

	// Try to extract JSON from the output (model may include surrounding text)
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		jsonStr := output[start : end+1]
		if resp, err := decodeRouterResponse(jsonStr); err == nil {
			return resp, nil
		}
	}

	return routerResponse{}, fmt.Errorf("no valid JSON with backend field in output: %s", truncate(output, 200))
}

func normalizeTaskType(taskType string) string {
	taskType = strings.ToLower(strings.TrimSpace(taskType))
	if IsTaskType(taskType) {
		return taskType
	}
	return ""
}

func IsTaskType(taskType string) bool {
	switch taskType {
	case TaskTypeRefactor, TaskTypeBugfix, TaskTypeTest, TaskTypeVision, TaskTypeDesign, TaskTypeDocs, TaskTypeInfra:
		return true
	default:
		return false
	}
}

func decodeRouterResponse(output string) (routerResponse, error) {
	var resp routerResponse
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return resp, err
	}
	if strings.TrimSpace(resp.Backend) == "" {
		return resp, fmt.Errorf("missing backend")
	}
	resp.Backend = strings.TrimSpace(resp.Backend)
	rawTaskType := resp.TaskType
	resp.TaskType = normalizeTaskType(resp.TaskType)
	if strings.TrimSpace(rawTaskType) != "" && resp.TaskType == "" {
		return resp, fmt.Errorf("invalid task_type %q", rawTaskType)
	}
	return resp, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
