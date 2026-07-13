package selfcheck

import (
	"encoding/json"
	"testing"
)

// TestRunPassesAgainstEmbeddedFixture is the load-bearing guard on the gate
// itself: the real binary's `maestro selfcheck` MUST pass against the embedded
// held-out fixture, otherwise every self-deploy would roll back. If a future
// change to the fixture or the invariants breaks this, the gate is broken.
func TestRunPassesAgainstEmbeddedFixture(t *testing.T) {
	rep := Run()
	if !rep.OK {
		t.Fatalf("Run() against the embedded fixture failed: %s", rep.JSON())
	}
	wantChecks := map[string]bool{CheckConfig: false, CheckBackend: false, CheckPrompt: false, CheckState: false, CheckDelivery: false}
	for _, c := range rep.Checks {
		if _, known := wantChecks[c.Name]; !known {
			t.Errorf("unexpected check %q", c.Name)
		}
		wantChecks[c.Name] = true
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
	}
	for name, seen := range wantChecks {
		if !seen {
			t.Errorf("check %q was not run", name)
		}
	}
	if names := rep.FailedNames(); len(names) != 0 {
		t.Errorf("FailedNames = %v, want none", names)
	}
}

// TestRunIsDeterministic: two back-to-back runs against the embedded fixture
// must produce identical reports. The gate is only trustworthy if its verdict
// does not depend on wall clock, map iteration order, or ambient state.
func TestRunIsDeterministic(t *testing.T) {
	a, b := Run().JSON(), Run().JSON()
	if a != b {
		t.Fatalf("Run() is not deterministic:\nfirst:\n%s\nsecond:\n%s", a, b)
	}
}

// TestGateHasTeeth_BrokenConfig proves a deliberately-broken fixture — a routing
// tier pointing at a backend that is not declared — fails the config invariant.
// This is the AC9 proof at the evaluator layer: broken fixture behavior makes
// the gate fail, and self-deploy wires a failing gate to rollback.
func TestGateHasTeeth_BrokenConfig(t *testing.T) {
	broken := []byte(`
repo: broken/fixture
model:
  default: codex
  backends:
    codex:
      cmd: codex
routing:
  mode: policy
  tiers:
    standard:
      backend: does-not-exist
  policy:
    default_tier: standard
`)
	rep := RunWithConfig(broken)
	if rep.OK {
		t.Fatalf("gate passed a config whose tier references an undeclared backend: %s", rep.JSON())
	}
	if !failed(rep, CheckConfig) {
		t.Errorf("expected the %q check to fail; report: %s", CheckConfig, rep.JSON())
	}
	// Backend/prompt depend on a parsed config, so they must be marked failed
	// too (not silently skipped-as-passing) when config does not parse.
	if !failed(rep, CheckBackend) || !failed(rep, CheckPrompt) {
		t.Errorf("dependent checks should fail when config does not parse: %s", rep.JSON())
	}
}

// TestGateHasTeeth_BrokenPrompt proves the prompt invariant is falsifiable
// independent of config parsing: this fixture parses and validates cleanly, but
// points the supervisor prompt at a path that does not exist, so prompt
// assembly errors and the gate fails naming the prompt check — while config,
// backend, and state still pass.
func TestGateHasTeeth_BrokenPrompt(t *testing.T) {
	broken := []byte(`
repo: broken/fixture
supervisor:
  prompt: /nonexistent/maestro-selfcheck-prompt-does-not-exist.txt
model:
  default: gemini
  backends:
    gemini:
      cmd: gemini
    codex:
      cmd: codex
    claude:
      cmd: claude
routing:
  mode: policy
  tiers:
    standard:
      backend: codex
      rank: 0
  policy:
    default_tier: standard
`)
	rep := RunWithConfig(broken)
	if rep.OK {
		t.Fatalf("gate passed with an unreadable supervisor prompt: %s", rep.JSON())
	}
	if !failed(rep, CheckPrompt) {
		t.Fatalf("expected the %q check to fail; report: %s", CheckPrompt, rep.JSON())
	}
	// The prompt failure must be isolated: config parsed, and backend + state
	// are independent of the prompt path, so they should still pass.
	for _, name := range []string{CheckConfig, CheckBackend, CheckState} {
		if failed(rep, name) {
			t.Errorf("check %q unexpectedly failed alongside the prompt check: %s", name, rep.JSON())
		}
	}
}

// TestJSONShape confirms the machine-readable report parses and carries the
// per-check outcomes self-deploy names on failure.
func TestJSONShape(t *testing.T) {
	var rep Report
	if err := json.Unmarshal([]byte(Run().JSON()), &rep); err != nil {
		t.Fatalf("selfcheck JSON does not round-trip: %v", err)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("report has no checks")
	}
}

func failed(rep Report, name string) bool {
	for _, n := range rep.FailedNames() {
		if n == name {
			return true
		}
	}
	return false
}
