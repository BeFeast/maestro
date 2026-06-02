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

const defaultRouterPrompt = `Given this GitHub issue, choose the best AI coding backend.
Available backends: {{BACKENDS}}

Issue: #{{NUMBER}} — {{TITLE}}
{{BODY}}

Return JSON only: {"backend": "<name>", "reason": "<one sentence>"}`

// routerResponse is the expected JSON response from the router model.
type routerResponse struct {
	Backend string `json:"backend"`
	Reason  string `json:"reason"`
}

// Router selects the best backend for a given issue using an LLM call.
type Router struct {
	cfg *config.Config

	// RouteFn overrides the default Route method (used in tests).
	RouteFn func(issue github.Issue) (string, string, error)
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
	prompt := r.buildPrompt(issue)

	output, err := r.callModel(prompt)
	if err != nil {
		log.Printf("[router] model call failed: %v — falling back to default", err)
		return r.cfg.Model.Default, "router error", err
	}

	resp, err := parseResponse(output)
	if err != nil {
		log.Printf("[router] parse response failed: %v — falling back to default", err)
		return r.cfg.Model.Default, "parse error", err
	}

	// Validate that the chosen backend exists in config. Return an error so
	// ResolveBackend can attribute the fallback to router_error rather than
	// silently presenting the default backend as if it was task-routed (#427).
	if _, ok := r.cfg.Model.Backends[resp.Backend]; !ok {
		log.Printf("[router] unknown backend %q — falling back to default", resp.Backend)
		return r.cfg.Model.Default, fmt.Sprintf("unknown backend %q", resp.Backend), fmt.Errorf("router picked unknown backend %q", resp.Backend)
	}

	return resp.Backend, resp.Reason, nil
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
	var resp routerResponse

	// Try direct unmarshal first
	if err := json.Unmarshal([]byte(output), &resp); err == nil && resp.Backend != "" {
		return resp, nil
	}

	// Try to extract JSON from the output (model may include surrounding text)
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		jsonStr := output[start : end+1]
		if err := json.Unmarshal([]byte(jsonStr), &resp); err == nil && resp.Backend != "" {
			return resp, nil
		}
	}

	return resp, fmt.Errorf("no valid JSON with backend field in output: %s", truncate(output, 200))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
