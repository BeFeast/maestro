package aiexecution

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func bindingFixture() (Manifest, claudeBindingReceipt) {
	m := Manifest{
		Gateway: ProcessProof{PID: os.Getpid()},
		Routes:  map[string]RoleRoute{"worker": {Model: "fixture-model"}},
		Runtime: RuntimeExpectation{ManagementKeyEnv: "MAESTRO_TEST_BINDING_KEY", ProjectionVersion: RuntimeProjection, ProcessInstanceID: uuid.NewString(), ManagedAdmissionSHA256: strings.Repeat("a", 64), ExecutionConfigSHA256: strings.Repeat("b", 64)},
		Bindings: BindingExpectation{SchemaVersion: 1, ProjectionVersion: ClaudeBindingProjection, Credentials: []BindingCredentialPin{{
			AuthRef: strings.Repeat("c", 64), AccountAlias: "reviewed-account", CredentialKind: "x-api-key", CredentialSHA256: strings.Repeat("d", 64),
			Models: []BindingModelPin{{RouteID: "fixture-route", Model: "fixture-model", ModelSHA256: strings.Repeat("e", 64)}},
		}}},
	}
	r := claudeBindingReceipt{SchemaVersion: 1, ProjectionVersion: ClaudeBindingProjection, ObservationScope: "credential_selection_only", ProcessInstanceID: m.Runtime.ProcessInstanceID, ConfigApplyComplete: true, ManagedAdmissionSHA256: m.Runtime.ManagedAdmissionSHA256, ExecutionConfigSHA256: m.Runtime.ExecutionConfigSHA256}
	r.Inventory.SnapshotComplete, r.Inventory.SelectionPinsMatch, r.Inventory.BuiltinExecutor = true, true, true
	r.Inventory.ExecutorConfigMatches, r.Inventory.ManagerConfigMatches, r.Inventory.NativeTransport = true, true, true
	r.Inventory.RegistryGeneration = 1
	r.Inventory.Credentials = []claudeBindingCredential{{AuthRef: strings.Repeat("c", 64), AccountAlias: "reviewed-account", CredentialKind: "x-api-key", CredentialSHA256: strings.Repeat("d", 64), PinMatches: true, AuthActive: true, AuthGeneration: 1, AuthRegistrationEpoch: 1, ModelRegistrationEpoch: 1,
		Models: []claudeBindingModel{{RouteID: "fixture-route", Model: "fixture-model", ResolvedModel: "fixture-model", Registered: true, ModelSHA256: strings.Repeat("e", 64)}},
	}}
	return m, r
}

func TestClaudeBindingObservationRejectsDriftAndIncompleteEvidence(t *testing.T) {
	for _, mode := range []string{"valid", "unmanaged_neighbors", "wrong_pid", "nonce", "instance", "stale", "future", "config_only", "projection", "schema", "partial_reload", "managed_digest", "execution_digest", "snapshot", "selection", "executor", "executor_config", "manager_config", "transport", "registry", "credential_swap", "credential_kind", "auth_ref", "account_alias", "inactive", "unmatched_pin", "auth_epoch", "model_epoch", "missing_credential", "extra_credential", "duplicate_credential", "model_digest", "model_resolution", "model_unregistered", "route", "missing_model", "extra_model", "duplicate_model", "unknown_field", "duplicate_field", "oversized", "redirect", "no_cache_control", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			m, base := bindingFixture()
			if mode == "duplicate_credential" {
				second := m.Bindings.Credentials[0]
				second.AuthRef = strings.Repeat("f", 64)
				m.Bindings.Credentials = append(m.Bindings.Credentials, second)
			}
			if mode == "duplicate_model" {
				second := m.Bindings.Credentials[0].Models[0]
				second.RouteID = "second-route"
				m.Bindings.Credentials[0].Models = append(m.Bindings.Credentials[0].Models, second)
			}
			t.Setenv(m.Runtime.ManagementKeyEnv, "synthetic-management-key")
			var sent atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				sent.Add(1)
				if req.URL.Path != "/v8/management/observability/admission/claude-bindings" || req.Header.Get("Authorization") != "Bearer synthetic-management-key" || len(req.URL.Query()) != 1 {
					t.Error("wrong binding observation request")
				}
				r := base
				r.RequestNonce, r.ObservedAt = req.URL.Query().Get("request_nonce"), time.Now().UTC()
				if len(r.RequestNonce) != 32 || strings.ToLower(r.RequestNonce) != r.RequestNonce {
					t.Error("invalid nonce")
				}
				switch mode {
				case "unmanaged_neighbors":
					r.Inventory.UnconfiguredClaudeCredentials = 8
				case "nonce":
					r.RequestNonce = strings.Repeat("0", 32)
				case "instance":
					r.ProcessInstanceID = uuid.NewString()
				case "stale":
					r.ObservedAt = time.Now().Add(-time.Minute)
				case "future":
					r.ObservedAt = time.Now().Add(time.Minute)
				case "config_only":
					r.ObservationScope = "configuration_only"
				case "projection":
					r.ProjectionVersion = RuntimeProjection
				case "schema":
					r.SchemaVersion = 2
				case "partial_reload":
					r.ConfigApplyComplete = false
				case "managed_digest":
					r.ManagedAdmissionSHA256 = strings.Repeat("f", 64)
				case "execution_digest":
					r.ExecutionConfigSHA256 = strings.Repeat("f", 64)
				case "snapshot":
					r.Inventory.SnapshotComplete = false
				case "selection":
					r.Inventory.SelectionPinsMatch = false
				case "executor":
					r.Inventory.BuiltinExecutor = false
				case "executor_config":
					r.Inventory.ExecutorConfigMatches = false
				case "manager_config":
					r.Inventory.ManagerConfigMatches = false
				case "transport":
					r.Inventory.NativeTransport = false
				case "registry":
					r.Inventory.RegistryGeneration = 0
				case "credential_swap":
					r.Inventory.Credentials[0].CredentialSHA256 = strings.Repeat("f", 64)
				case "credential_kind":
					r.Inventory.Credentials[0].CredentialKind = "authorization-bearer"
				case "auth_ref":
					r.Inventory.Credentials[0].AuthRef = strings.Repeat("f", 64)
				case "account_alias":
					r.Inventory.Credentials[0].AccountAlias = "other-account"
				case "inactive":
					r.Inventory.Credentials[0].AuthActive = false
				case "unmatched_pin":
					r.Inventory.Credentials[0].PinMatches = false
				case "auth_epoch":
					r.Inventory.Credentials[0].AuthRegistrationEpoch = 0
				case "model_epoch":
					r.Inventory.Credentials[0].ModelRegistrationEpoch = 0
				case "missing_credential":
					r.Inventory.Credentials = nil
				case "extra_credential", "duplicate_credential":
					r.Inventory.Credentials = append(r.Inventory.Credentials, r.Inventory.Credentials[0])
				case "model_digest":
					r.Inventory.Credentials[0].Models[0].ModelSHA256 = strings.Repeat("f", 64)
				case "model_resolution":
					r.Inventory.Credentials[0].Models[0].ResolvedModel = "unreviewed-upstream"
				case "model_unregistered":
					r.Inventory.Credentials[0].Models[0].Registered = false
				case "route":
					r.Inventory.Credentials[0].Models[0].RouteID = "unreviewed-route"
				case "missing_model":
					r.Inventory.Credentials[0].Models = nil
				case "extra_model", "duplicate_model":
					r.Inventory.Credentials[0].Models = append(r.Inventory.Credentials[0].Models, r.Inventory.Credentials[0].Models[0])
				case "redirect":
					w.Header().Set("Location", "/redirect")
					w.WriteHeader(http.StatusFound)
					return
				case "unauthorized":
					w.WriteHeader(http.StatusUnauthorized)
					return
				case "oversized":
					w.Header().Set("Cache-Control", "no-store")
					_, _ = w.Write([]byte(strings.Repeat(" ", (256<<10)+1)))
					return
				}
				if mode != "no_cache_control" {
					w.Header().Set("Cache-Control", "no-store")
				}
				b, _ := json.Marshal(r)
				if mode == "unknown_field" {
					b = append([]byte(`{"account_ownership_verified":true,`), b[1:]...)
				}
				if mode == "duplicate_field" {
					b = append([]byte(`{"schema_version":1,`), b[1:]...)
				}
				_, _ = w.Write(b)
			}))
			defer srv.Close()
			m.GatewayURL = srv.URL
			if mode == "wrong_pid" {
				m.Gateway.PID = -1
			}
			err := observeClaudeBindings(m)
			good := mode == "valid" || mode == "unmanaged_neighbors"
			if good && err != nil {
				t.Fatal(err)
			}
			if !good && err == nil {
				t.Fatal("invalid active binding accepted")
			}
			if sent.Load() > 1 || mode == "wrong_pid" && sent.Load() != 0 {
				t.Fatal("retried or queried an unowned gateway")
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-management-key") {
				t.Fatal("secret disclosed")
			}
		})
	}
}

func TestClaudeBindingExpectationRequiresReviewedFiniteInventory(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate_auth", "duplicate_model", "uncovered_role", "unsupported_kind", "invalid_digest"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := bindingFixture()
			switch mode {
			case "missing":
				m.Bindings.Credentials = nil
			case "duplicate_auth":
				m.Bindings.Credentials = append(m.Bindings.Credentials, m.Bindings.Credentials[0])
			case "duplicate_model":
				m.Bindings.Credentials[0].Models = append(m.Bindings.Credentials[0].Models, m.Bindings.Credentials[0].Models[0])
			case "uncovered_role":
				m.Routes["reviewer"] = RoleRoute{Model: "other-model"}
			case "unsupported_kind":
				m.Bindings.Credentials[0].CredentialKind = "metadata-account-uuid"
			case "invalid_digest":
				m.Bindings.Credentials[0].CredentialSHA256 = "known"
			}
			if err := observeClaudeBindings(m); err == nil {
				t.Fatal("unreviewed expectation accepted")
			}
		})
	}
}
