package router

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

func makeIssue(number int, title string, labels ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title}
	for _, l := range labels {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return issue
}

func TestBackendFromLabels_ModelLabel(t *testing.T) {
	issue := makeIssue(1, "Fix bug", "enhancement", "model:codex")
	got := BackendFromLabels(issue)
	if got != "codex" {
		t.Errorf("BackendFromLabels() = %q, want %q", got, "codex")
	}
}

func TestBackendFromLabels_NoModelLabel(t *testing.T) {
	issue := makeIssue(2, "Add feature", "enhancement", "bug")
	got := BackendFromLabels(issue)
	if got != "" {
		t.Errorf("BackendFromLabels() = %q, want empty", got)
	}
}

func TestBackendFromLabels_NoLabels(t *testing.T) {
	issue := makeIssue(3, "Update docs")
	got := BackendFromLabels(issue)
	if got != "" {
		t.Errorf("BackendFromLabels() = %q, want empty", got)
	}
}

func TestBackendFromLabels_MultipleModelLabels_FirstWins(t *testing.T) {
	issue := makeIssue(4, "Complex", "model:gemini", "model:codex")
	got := BackendFromLabels(issue)
	if got != "gemini" {
		t.Errorf("BackendFromLabels() = %q, want %q (first model: label wins)", got, "gemini")
	}
}

func TestBackendFromLabels_EmptyModelValue(t *testing.T) {
	issue := makeIssue(5, "Edge case", "model:", "model:cline")
	got := BackendFromLabels(issue)
	if got != "cline" {
		t.Errorf("BackendFromLabels() = %q, want %q (empty model: should be skipped)", got, "cline")
	}
}

func TestBackendFromLabels_AllKnownBackends(t *testing.T) {
	backends := []string{"claude", "codex", "gemini", "cline"}
	for _, b := range backends {
		issue := makeIssue(10, "Test", "model:"+b)
		got := BackendFromLabels(issue)
		if got != b {
			t.Errorf("BackendFromLabels(model:%s) = %q, want %q", b, got, b)
		}
	}
}

func TestValidateBackend_Known(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
	}
	name, ok := ValidateBackend("codex", cfg)
	if !ok || name != "codex" {
		t.Errorf("ValidateBackend(codex) = (%q, %v), want (%q, true)", name, ok, "codex")
	}
}

func TestValidateBackend_Unknown(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
	}
	name, ok := ValidateBackend("nonexistent", cfg)
	if ok || name != "claude" {
		t.Errorf("ValidateBackend(nonexistent) = (%q, %v), want (%q, false)", name, ok, "claude")
	}
}

func TestResolveBackend_LabelOverride(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(42, "Fix SQL injection", "enhancement", "model:codex")
	name, reason := r.ResolveBackend(issue)
	if name != "codex" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "codex")
	}
	if reason != "label" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "label")
	}
}

func TestResolveBackend_LabelOverride_UnknownBackend(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(43, "Test unknown", "model:nonexistent")
	name, reason := r.ResolveBackend(issue)
	if name != "claude" {
		t.Errorf("ResolveBackend() name = %q, want %q (should fall back to default)", name, "claude")
	}
	if reason != ReasonUnknownPin {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, ReasonUnknownPin)
	}
}

func TestResolveBackend_DefaultFallback(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(44, "Add feature", "enhancement")
	name, reason := r.ResolveBackend(issue)
	if name != "claude" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "claude")
	}
	if reason != "default" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackend_LabelTakesPrecedenceOverAutoRouting(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
				"gemini": {Cmd: "gemini"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:        "auto",
			RouterModel: "claude",
		},
	}
	r := New(cfg)

	// Even with auto-routing enabled, the label should win
	issue := makeIssue(45, "Refactor auth", "model:gemini")
	name, reason := r.ResolveBackend(issue)
	if name != "gemini" {
		t.Errorf("ResolveBackend() name = %q, want %q (label should override auto-routing)", name, "gemini")
	}
	if reason != "label" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "label")
	}
}

func TestResolveBackend_GeminiAsDefault(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "gemini",
			Backends: map[string]config.BackendDef{
				"gemini": {Cmd: "gemini"},
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	// Issue without model label should use gemini as default
	issue := makeIssue(50, "Add dark mode", "enhancement")
	name, reason := r.ResolveBackend(issue)
	if name != "gemini" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "gemini")
	}
	if reason != "default" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackend_GeminiLabelOverridesDefault(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"gemini": {Cmd: "gemini"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(51, "Build API", "model:gemini", "enhancement")
	name, reason := r.ResolveBackend(issue)
	if name != "gemini" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "gemini")
	}
	if reason != "label" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "label")
	}
}

func TestResolveBackend_NoLabelsManualMode(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"codex": {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "manual"},
	}
	r := New(cfg)

	issue := makeIssue(46, "Something")
	name, reason := r.ResolveBackend(issue)
	if name != "codex" {
		t.Errorf("ResolveBackend() name = %q, want %q (default)", name, "codex")
	}
	if reason != "default" {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackend_AutoRoutingViaRouteFn(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto"},
	}
	r := New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "codex", "simple fix", nil
	}

	issue := makeIssue(47, "Simple fix")
	name, reason := r.ResolveBackend(issue)
	if name != "codex" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "codex")
	}
	if reason != ReasonAuto {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, ReasonAuto)
	}
}

func TestResolveBackend_AutoRoutingTaskTypeMappingWinsBeforeBackendPick(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
				"gemini": {Cmd: "gemini"},
			},
		},
		Routing: config.RoutingConfig{
			Mode: "auto",
			TaskTypeBackends: map[string]string{
				TaskTypeVision: "claude",
			},
		},
	}
	r := New(cfg)
	r.DecisionFn = func(issue github.Issue) (Decision, error) {
		return Decision{Backend: "gemini", TaskType: TaskTypeVision, Reason: "image-heavy UI issue"}, nil
	}

	decision := r.ResolveBackendDecision(makeIssue(658, "Match screenshot"))
	if decision.Backend != "claude" {
		t.Fatalf("Backend = %q, want claude from vision task_type mapping", decision.Backend)
	}
	if decision.Reason != ReasonAuto {
		t.Fatalf("Reason = %q, want %q", decision.Reason, ReasonAuto)
	}
	if decision.TaskType != TaskTypeVision {
		t.Fatalf("TaskType = %q, want %q", decision.TaskType, TaskTypeVision)
	}
}

func TestResolveBackend_AutoRoutingTaskTypeWithoutMappingUsesBackendPick(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
				"gemini": {Cmd: "gemini"},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto"},
	}
	r := New(cfg)
	r.DecisionFn = func(issue github.Issue) (Decision, error) {
		return Decision{Backend: "gemini", TaskType: TaskTypeVision, Reason: "image-heavy UI issue"}, nil
	}

	decision := r.ResolveBackendDecision(makeIssue(659, "Inspect screenshot"))
	if decision.Backend != "gemini" {
		t.Fatalf("Backend = %q, want router backend pick gemini", decision.Backend)
	}
	if decision.TaskType != TaskTypeVision {
		t.Fatalf("TaskType = %q, want %q", decision.TaskType, TaskTypeVision)
	}
}

func TestResolveBackend_ManualModeIgnoresTaskTypeBackends(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{
			Mode: "manual",
			TaskTypeBackends: map[string]string{
				TaskTypeVision: "claude",
			},
		},
	}
	r := New(cfg)
	routerCalled := false
	r.DecisionFn = func(issue github.Issue) (Decision, error) {
		routerCalled = true
		return Decision{Backend: "claude", TaskType: TaskTypeVision, Reason: "vision"}, nil
	}

	decision := r.ResolveBackendDecision(makeIssue(660, "Screenshot task"))
	if decision.Backend != "codex" || decision.Reason != ReasonDefault {
		t.Fatalf("decision = %+v, want codex/default", decision)
	}
	if decision.TaskType != "" {
		t.Fatalf("TaskType = %q, want empty in manual mode", decision.TaskType)
	}
	if routerCalled {
		t.Fatal("manual mode must not call router even when task_type_backends is configured")
	}
}

func TestRoute_UsesBackendCommandPrefixArgs(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	cliPath := filepath.Join(dir, "router-cli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\nprintf '{\"backend\":\"gemini\",\"reason\":\"docs task\"}'\n"
	if err := os.WriteFile(cliPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake router cli: %v", err)
	}
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: cliPath + " --temperature 0"},
				"gemini": {Cmd: "gemini"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:            "auto",
			RouterModel:     "claude",
			RouterModelName: "router-model",
		},
	}

	name, reason, err := New(cfg).Route(github.Issue{Number: 47, Title: "Update docs", Body: "Small docs cleanup"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if name != "gemini" || reason != "docs task" {
		t.Fatalf("Route() = (%q, %q), want (gemini, docs task)", name, reason)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	want := []string{"--temperature", "0", "-p"}
	if len(args) < len(want) {
		t.Fatalf("args = %v, want prefix %v", args, want)
	}
	for i, arg := range want {
		if args[i] != arg {
			t.Fatalf("args[%d] = %q, want %q; all args=%v", i, args[i], arg, args)
		}
	}
	if !containsArgPair(args, "--model", "router-model") {
		t.Fatalf("args = %v, want router model argument", args)
	}
}

func TestRoute_DoesNotAppendRouterModelWhenCommandPinsModel(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	cliPath := filepath.Join(dir, "router-cli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\nprintf '{\"backend\":\"codex\",\"reason\":\"implementation\"}'\n"
	if err := os.WriteFile(cliPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake router cli: %v", err)
	}
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: cliPath + " --model pinned-router --effort high"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:            "auto",
			RouterModel:     "claude",
			RouterModelName: "default-router-model",
		},
	}

	if _, _, err := New(cfg).Route(github.Issue{Number: 48, Title: "Implement feature"}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if countArg(args, "--model") != 1 {
		t.Fatalf("args = %v, want exactly one --model from backend command", args)
	}
	if containsArg(args, "default-router-model") {
		t.Fatalf("args = %v, router_model_name should not be appended when cmd pins --model", args)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, key string, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func countArg(args []string, want string) int {
	count := 0
	for _, arg := range args {
		if arg == want {
			count++
		}
	}
	return count
}

// --- ResolveBackendForRole tests ---

func TestResolveBackendForRole_UsesRoleBackend(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":       {Cmd: "claude"},
				"gemini-flash": {Cmd: "gemini-flash"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:           "manual",
			PlannerBackend: "gemini-flash",
		},
	}
	r := New(cfg)
	issue := makeIssue(100, "Plan feature")

	name, reason := r.ResolveBackendForRole(issue, RolePlanner)
	if name != "gemini-flash" {
		t.Errorf("ResolveBackendForRole(planner) name = %q, want %q", name, "gemini-flash")
	}
	if reason != "role" {
		t.Errorf("ResolveBackendForRole(planner) reason = %q, want %q", reason, "role")
	}
}

func TestResolveBackendForRole_ImplementerBackend(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "gemini-flash",
			Backends: map[string]config.BackendDef{
				"gemini-flash": {Cmd: "gemini-flash"},
				"claude":       {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:                  "manual",
			ImplementationBackend: "claude",
		},
	}
	r := New(cfg)
	issue := makeIssue(101, "Implement feature")

	name, reason := r.ResolveBackendForRole(issue, RoleImplementer)
	if name != "claude" {
		t.Errorf("ResolveBackendForRole(implementer) name = %q, want %q", name, "claude")
	}
	if reason != "role" {
		t.Errorf("ResolveBackendForRole(implementer) reason = %q, want %q", reason, "role")
	}
}

func TestResolveBackendForRole_ValidatorBackend(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "gemini-flash",
			Backends: map[string]config.BackendDef{
				"gemini-flash": {Cmd: "gemini-flash"},
				"claude":       {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:             "manual",
			ValidatorBackend: "claude",
		},
	}
	r := New(cfg)
	issue := makeIssue(102, "Validate code")

	name, reason := r.ResolveBackendForRole(issue, RoleValidator)
	if name != "claude" {
		t.Errorf("ResolveBackendForRole(validator) name = %q, want %q", name, "claude")
	}
	if reason != "role" {
		t.Errorf("ResolveBackendForRole(validator) reason = %q, want %q", reason, "role")
	}
}

func TestResolveBackendForRole_FallsBackToIssueLevel(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:           "manual",
			PlannerBackend: "codex",
			// No implementation_backend set — should fall back to default
		},
	}
	r := New(cfg)
	issue := makeIssue(103, "Implement feature")

	name, reason := r.ResolveBackendForRole(issue, RoleImplementer)
	if name != "claude" {
		t.Errorf("ResolveBackendForRole(implementer) name = %q, want %q (should fall back to default)", name, "claude")
	}
	if reason != "default" {
		t.Errorf("ResolveBackendForRole(implementer) reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackendForRole_LabelOverridesRoleBackend(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":       {Cmd: "claude"},
				"codex":        {Cmd: "codex"},
				"gemini-flash": {Cmd: "gemini-flash"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:           "manual",
			PlannerBackend: "gemini-flash",
		},
	}
	r := New(cfg)
	issue := makeIssue(104, "Plan feature", "model:codex")

	name, reason := r.ResolveBackendForRole(issue, RolePlanner)
	if name != "codex" {
		t.Errorf("ResolveBackendForRole(planner) name = %q, want %q (label should override role config)", name, "codex")
	}
	if reason != "label" {
		t.Errorf("ResolveBackendForRole(planner) reason = %q, want %q", reason, "label")
	}
}

func TestResolveBackendForRole_InvalidRoleBackendFallsToIssueLevel(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:           "manual",
			PlannerBackend: "nonexistent",
		},
	}
	r := New(cfg)
	issue := makeIssue(105, "Plan feature")

	name, reason := r.ResolveBackendForRole(issue, RolePlanner)
	if name != "claude" {
		t.Errorf("ResolveBackendForRole(planner) name = %q, want %q (invalid role backend should fall through)", name, "claude")
	}
	if reason != "default" {
		t.Errorf("ResolveBackendForRole(planner) reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackendForRole_UnknownRoleFallsToIssueLevel(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":       {Cmd: "claude"},
				"gemini-flash": {Cmd: "gemini-flash"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:           "manual",
			PlannerBackend: "gemini-flash",
		},
	}
	r := New(cfg)
	issue := makeIssue(106, "Unknown role")

	name, reason := r.ResolveBackendForRole(issue, "unknown-role")
	if name != "claude" {
		t.Errorf("ResolveBackendForRole(unknown) name = %q, want %q (unknown role should fall back)", name, "claude")
	}
	if reason != "default" {
		t.Errorf("ResolveBackendForRole(unknown) reason = %q, want %q", reason, "default")
	}
}

func TestResolveBackendForRole_AllRolesConfigured(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":       {Cmd: "claude"},
				"gemini-flash": {Cmd: "gemini-flash"},
				"codex":        {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:                  "manual",
			PlannerBackend:        "gemini-flash",
			ImplementationBackend: "claude",
			ValidatorBackend:      "codex",
		},
	}
	r := New(cfg)
	issue := makeIssue(107, "Full pipeline")

	tests := []struct {
		role   string
		wantBe string
	}{
		{RolePlanner, "gemini-flash"},
		{RoleImplementer, "claude"},
		{RoleValidator, "codex"},
	}
	for _, tt := range tests {
		name, reason := r.ResolveBackendForRole(issue, tt.role)
		if name != tt.wantBe {
			t.Errorf("ResolveBackendForRole(%s) name = %q, want %q", tt.role, name, tt.wantBe)
		}
		if reason != "role" {
			t.Errorf("ResolveBackendForRole(%s) reason = %q, want %q", tt.role, reason, "role")
		}
	}
}

func TestResolveBackendForRole_WithAutoRouting(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{
			Mode: "auto",
			// No role-specific backends — should fall through to auto-routing
		},
	}
	r := New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "codex", "auto-routed", nil
	}

	issue := makeIssue(108, "Auto routed")
	name, reason := r.ResolveBackendForRole(issue, RolePlanner)
	if name != "codex" {
		t.Errorf("ResolveBackendForRole(planner) name = %q, want %q (should fall through to auto-routing)", name, "codex")
	}
	if reason != ReasonAuto {
		t.Errorf("ResolveBackendForRole(planner) reason = %q, want %q", reason, ReasonAuto)
	}
}

func TestResolveBackendForRole_RoleBackendOverridesAutoRouting(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude":       {Cmd: "claude"},
				"codex":        {Cmd: "codex"},
				"gemini-flash": {Cmd: "gemini-flash"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:           "auto",
			PlannerBackend: "gemini-flash",
		},
	}
	r := New(cfg)
	routerCalled := false
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		routerCalled = true
		return "codex", "auto-routed", nil
	}

	issue := makeIssue(109, "Plan with role")
	name, reason := r.ResolveBackendForRole(issue, RolePlanner)
	if name != "gemini-flash" {
		t.Errorf("ResolveBackendForRole(planner) name = %q, want %q (role config should override auto-routing)", name, "gemini-flash")
	}
	if reason != "role" {
		t.Errorf("ResolveBackendForRole(planner) reason = %q, want %q", reason, "role")
	}
	if routerCalled {
		t.Error("auto-router should not be called when role backend is configured")
	}
}

func TestResolveBackend_AutoRoutingErrorFallsToDefaultWithRouterErrorReason(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto"},
	}
	r := New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "", "", fmt.Errorf("network error")
	}

	// #427: when routing.mode=auto is configured but the router call fails,
	// the fallback to the default backend must surface a router_error reason
	// (not the bare "default" used in manual mode) so operators can see that
	// auto-routing failed silently rather than being unconfigured.
	issue := makeIssue(48, "Fix bug")
	name, reason := r.ResolveBackend(issue)
	if name != "claude" {
		t.Errorf("ResolveBackend() name = %q, want %q (should fall back to default)", name, "claude")
	}
	if reason != ReasonRouterError {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, ReasonRouterError)
	}
}

// #427: command strings with arguments must reach the router CLI through
// splitRouterCmd, and a router that picks an unknown backend must surface
// router_error so the dashboard does not present the default as auto-routed.
func TestRoute_UnknownBackendFromCommandReturnsRouterError(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "router-cli")
	// The fake CLI accepts any --foo arg and prints a backend that isn't in config.
	script := "#!/bin/sh\nprintf '{\"backend\":\"nonexistent\",\"reason\":\"pick\"}'\n"
	if err := os.WriteFile(cliPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake router cli: %v", err)
	}
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: cliPath + " --temperature 0"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{
			Mode:        "auto",
			RouterModel: "claude",
		},
	}

	r := New(cfg)
	name, reason := r.ResolveBackend(github.Issue{Number: 99, Title: "Anything"})
	if name != "claude" {
		t.Fatalf("ResolveBackend() name = %q, want claude (default fallback)", name)
	}
	if reason != ReasonRouterError {
		t.Fatalf("ResolveBackend() reason = %q, want %q (silent unknown-backend fallback must surface router_error)", reason, ReasonRouterError)
	}
}

// #427: an auto-routed call that returns an empty backend without error is
// still a silent fallback — operators need the router_error signal too.
func TestResolveBackend_AutoRoutingEmptyBackendUsesRouterErrorReason(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
		Routing: config.RoutingConfig{Mode: "auto"},
	}
	r := New(cfg)
	r.RouteFn = func(issue github.Issue) (string, string, error) {
		return "", "", nil
	}

	name, reason := r.ResolveBackend(makeIssue(49, "Empty route"))
	if name != "claude" {
		t.Errorf("ResolveBackend() name = %q, want %q", name, "claude")
	}
	if reason != ReasonRouterError {
		t.Errorf("ResolveBackend() reason = %q, want %q", reason, ReasonRouterError)
	}
}
