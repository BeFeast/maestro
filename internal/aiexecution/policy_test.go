package aiexecution

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/google/uuid"
)

func pinManifest(t *testing.T, m Manifest) Policy {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return Policy{RequireVerifiedRoute: true, ManifestPath: path, ManifestSHA256: digest(b)}
}
func TestInstalledManifestPinBindingAndExpiry(t *testing.T) {
	for _, mode := range []string{"source", "expiry", "model", "role", "role_run", "registration", "native_id", "harness", "pin", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			m := Manifest{Version: 1, EvidenceKind: "installed", ExpiresAt: time.Now().Add(time.Hour), ProjectID: uuid.NewString(), FleetID: "fleet", PolicyVersion: 3, Routes: map[string]RoleRoute{"reviewer": {BudgetRunID: "budget", GatewayScope: "scope", Model: "requested"}}}
			spec := LaunchSpec{ProjectID: m.ProjectID, Role: "reviewer", RoleRunID: uuid.NewString(), Model: m.Routes["reviewer"].Model, GatewayScope: m.Routes["reviewer"].GatewayScope, ExpectedPolicyVersion: m.PolicyVersion, Registration: &admissioncontrol.Acknowledgement{RegistrationVersion: 1, Binding: admissioncontrol.Binding{ProjectID: m.ProjectID, FleetID: m.FleetID, RunID: m.Routes["reviewer"].BudgetRunID, Role: "reviewer", GatewayScope: m.Routes["reviewer"].GatewayScope, ExpiresAt: time.Now().Add(time.Minute).Unix(), NativeSessionID: uuid.NewString()}}}
			m.ProjectConfigSHA256 = strings.Repeat("a", 64)
			spec.ProjectConfigSHA256 = m.ProjectConfigSHA256
			want := "native_harness_unsupported"
			switch mode {
			case "source":
				m.EvidenceKind = "source_evidence"
				want = "source_evidence_not_installed"
			case "expiry":
				m.ExpiresAt = time.Now().Add(-time.Second)
				want = "proof_expired"
			case "model":
				spec.Model = "substituted"
				want = "route_binding_mismatch"
			case "role":
				spec.Role = "unknown-child"
				want = "role_unsupported"
			case "role_run":
				spec.RoleRunID = "unowned"
				want = "role_run_invalid"
			case "registration":
				spec.Registration.Revoked = true
				want = "registration_binding_mismatch"
			case "native_id":
				spec.Registration.Binding.NativeSessionID = "unowned"
				want = "native_session_invalid"
			case "pin":
				want = "manifest_drift"
			case "oversized":
				want = "manifest_unavailable"
			}
			p := pinManifest(t, m)
			if mode == "pin" {
				p.ManifestSHA256 = strings.Repeat("0", 64)
			}
			if mode == "oversized" {
				if err := os.WriteFile(p.ManifestPath, make([]byte, 129<<10), 0600); err != nil {
					t.Fatal(err)
				}
			}
			assertExecutionHold(t, Inspect(p, spec, exec.Command("unsupported")), want)
		})
	}
	if err := Inspect(Policy{}, LaunchSpec{}, nil); err != nil {
		t.Fatal("legacy changed", err)
	}
	assertExecutionHold(t, Inspect(Policy{RequireVerifiedRoute: true}, LaunchSpec{}, nil), "manifest_pin_required")
}
