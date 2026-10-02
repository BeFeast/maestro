package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/daemonresources"
)

func TestCandidatePreflightCLI(t *testing.T) {
	root := t.TempDir()
	makeSide := func(name string) map[string]any {
		o := daemonresources.Defaults(root + "/" + name + "/maestro.db")
		o.Port = 0
		o.ApprovalsDB, o.StateDB, o.WebhookDB, o.EmergencyDB = o.Store, o.Store, o.Store, o.Store
		return map[string]any{"options": o, "projects": []any{}}
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "stable": makeSide("stable"), "candidate": makeSide("candidate")})
	var out, errOut bytes.Buffer
	code := runCandidatePreflight(nil, bytes.NewReader(data), &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), `"activation_authorized": false`) {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	for _, bad := range []string{`{}`, string(data) + `{}`, `{"version":1,"unknown":true}`} {
		out.Reset()
		errOut.Reset()
		if code := runCandidatePreflight(nil, strings.NewReader(bad), &out, &errOut); code != 2 || out.Len() != 0 {
			t.Fatalf("invalid input %q: code=%d out=%s", bad, code, out.String())
		}
	}
	// A declared shared store is a reportable failure, not a command/parser error.
	shared := fmt.Sprintf(`{"version":1,"stable":{"options":{"store":%q},"projects":[]},"candidate":{"options":{"store":%q},"projects":[]}}`, root+"/same.db", root+"/same.db")
	out.Reset()
	errOut.Reset()
	if code := runCandidatePreflight(nil, strings.NewReader(shared), &out, &errOut); code != 1 || !strings.Contains(out.String(), `"status": "overlap"`) {
		t.Fatalf("overlap: code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
}
