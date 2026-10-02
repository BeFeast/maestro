package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRetiredReviewScriptCannotInvokeInferenceOrPublish(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "llm-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []string{"false", "true"} {
		cmd := exec.Command("/bin/bash", script, "42", "fixture/repo")
		cmd.Env = []string{"PATH=" + t.TempDir(), "LLM_REVIEW_ENABLED=" + enabled, "ANTHROPIC_API_KEY=synthetic-secret", "LLM_REVIEW_STREAMS=llm-review-opus,llm-review-terra,llm-review-cursor"}
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "retired direct-inference runner") || strings.Contains(string(output), "synthetic-secret") {
			t.Fatalf("output=%s error=%v", output, err)
		}
	}
}

func TestRetiredReviewWorkflowHasNoInferenceCredentialsOrAutomaticTrigger(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "llm-review.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 {
		t.Fatal("unexpected automatic review trigger")
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Fatal("retirement notice is not manual-only")
	}
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if step["env"] != nil || step["uses"] != nil {
				t.Fatal("retired workflow gained credentials/actions")
			}
			run, _ := step["run"].(string)
			if !strings.HasPrefix(run, "printf ") || strings.ContainsAny(run, ";&|`$") {
				t.Fatalf("retired workflow executes more than its notice: %s", run)
			}
		}
	}
}
