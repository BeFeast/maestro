package aiexecution

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func assertExecutionHold(t *testing.T, err error, code string) {
	t.Helper()
	var hold *Hold
	if !errors.As(err, &hold) || hold.Code != code {
		t.Fatalf("error=%v want %s", err, code)
	}
}

func TestRuntimeObservationBindsNonceProcessPolicyAndConfig(t *testing.T) {
	for _, mode := range []string{"valid", "nonce", "instance", "stale", "partial_reload", "startup_drift", "current_drift", "policy", "caller", "redirect", "unknown_field", "wrong_pid"} {
		t.Run(mode, func(t *testing.T) {
			expected := RuntimeExpectation{ManagementKeyEnv: "MAESTRO_TEST_OBSERVATION_KEY", ProjectionVersion: RuntimeProjection, ProcessInstanceID: uuid.NewString(), StartedAt: time.Now().Add(-time.Hour).UTC(), BuildVersion: "fixture", GitCommit: strings.Repeat("b", 40), ManagedAdmissionSHA256: strings.Repeat("a", 64), ExecutionConfigSHA256: strings.Repeat("c", 64)}
			t.Setenv(expected.ManagementKeyEnv, "synthetic-management-key")
			var sent atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent.Add(1)
				if r.URL.Path != "/v8/management/observability/admission/runtime" || r.Header.Get("Authorization") != "Bearer synthetic-management-key" {
					t.Error("wrong observation request")
				}
				receipt := runtimeReceipt{SchemaVersion: 1, ProjectionVersion: expected.ProjectionVersion, ProcessInstanceID: expected.ProcessInstanceID, StartedAt: expected.StartedAt, ObservedAt: time.Now().UTC(), RequestNonce: r.URL.Query().Get("request_nonce"), ConfigApplyComplete: true}
				if len(receipt.RequestNonce) != 32 {
					t.Error("missing fresh nonce")
				}
				receipt.Build.Version = expected.BuildVersion
				receipt.Build.GitCommit = expected.GitCommit
				receipt.ManagedAdmission.Enabled = true
				receipt.ManagedAdmission.BuiltinOnly = true
				receipt.ManagedAdmission.CallerScopeHash = strings.Repeat("d", 64)
				receipt.ManagedAdmission.PolicyVersion = 3
				receipt.ManagedAdmission.StartupSHA256 = expected.ManagedAdmissionSHA256
				receipt.ManagedAdmission.CurrentSHA256 = expected.ManagedAdmissionSHA256
				receipt.ExecutionConfig.StartupSHA256 = expected.ExecutionConfigSHA256
				receipt.ExecutionConfig.CurrentSHA256 = expected.ExecutionConfigSHA256
				switch mode {
				case "nonce":
					receipt.RequestNonce = strings.Repeat("0", 32)
				case "instance":
					receipt.ProcessInstanceID = uuid.NewString()
				case "stale":
					receipt.ObservedAt = time.Now().Add(-time.Minute)
				case "partial_reload":
					receipt.ConfigApplyComplete = false
				case "startup_drift":
					receipt.ManagedAdmission.StartupSHA256 = strings.Repeat("f", 64)
				case "current_drift":
					receipt.ExecutionConfig.CurrentSHA256 = strings.Repeat("f", 64)
				case "policy":
					receipt.ManagedAdmission.PolicyVersion++
				case "caller":
					receipt.ManagedAdmission.CallerScopeHash = strings.Repeat("f", 64)
				case "redirect":
					w.Header().Set("Location", "/another")
					w.WriteHeader(http.StatusFound)
					return
				}
				w.Header().Set("Cache-Control", "no-store")
				b, _ := json.Marshal(receipt)
				if mode == "unknown_field" {
					b = append([]byte(`{"allow_all":true,`), b[1:]...)
				}
				_, _ = w.Write(b)
			}))
			defer srv.Close()
			m := Manifest{GatewayURL: srv.URL, Gateway: ProcessProof{PID: os.Getpid()}, Runtime: expected, PolicyVersion: 3}
			if mode == "wrong_pid" {
				m.Gateway.PID = -1
			}
			err := observeRuntime(m, strings.Repeat("d", 64))
			if mode == "valid" && err != nil {
				t.Fatal(err)
			}
			if mode != "valid" && err == nil {
				t.Fatal("invalid observation accepted")
			}
			if sent.Load() > 1 || mode == "wrong_pid" && sent.Load() != 0 {
				t.Fatal("observation retried or unowned process queried")
			}
		})
	}
}
