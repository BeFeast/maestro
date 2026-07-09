package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestSelfCheckCLIPasses: the real binary's `maestro selfcheck` exits 0 against
// the embedded fixture and prints a PASS line per check plus the OK summary.
// This guards the gate the deploy script depends on: if it ever exits non-zero
// on a healthy build, every self-deploy would roll back.
func TestSelfCheckCLIPasses(t *testing.T) {
	var out bytes.Buffer
	code := runSelfCheck(nil, &out)
	if code != 0 {
		t.Fatalf("runSelfCheck exit = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "[selfcheck] OK") {
		t.Errorf("missing OK summary line:\n%s", got)
	}
	for _, name := range []string{"config", "backend", "prompt", "state"} {
		if !strings.Contains(got, "PASS "+name) {
			t.Errorf("missing PASS line for %q:\n%s", name, got)
		}
	}
	if strings.Contains(got, "FAILED:") {
		t.Errorf("healthy gate must not print a FAILED line:\n%s", got)
	}
}

// TestSelfCheckCLIJSON: --json emits a single parseable report object and exits
// 0 when healthy.
func TestSelfCheckCLIJSON(t *testing.T) {
	var out bytes.Buffer
	code := runSelfCheck([]string{"--json"}, &out)
	if code != 0 {
		t.Fatalf("runSelfCheck --json exit = %d, want 0\n%s", code, out.String())
	}
	var rep struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if !rep.OK || len(rep.Checks) != 4 {
		t.Fatalf("unexpected report: ok=%v checks=%d\n%s", rep.OK, len(rep.Checks), out.String())
	}
}

// TestSelfCheckCLIUsageError: an unknown flag is a usage error (exit 2), not a
// gate failure (exit 1) — so the deploy script does not misattribute a bad
// invocation to a behavioral regression.
func TestSelfCheckCLIUsageError(t *testing.T) {
	var out bytes.Buffer
	if code := runSelfCheck([]string{"--nope"}, &out); code != 2 {
		t.Fatalf("runSelfCheck with unknown flag exit = %d, want 2\n%s", code, out.String())
	}
}
