package aiexecution

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dualOAuthBindingFixture mirrors the pinned two-account managed lane: two
// OAuth credentials, each pinned to the same three (route, model) keys, both
// serving. Aliases and digests are synthetic.
func dualOAuthBindingFixture() (Manifest, claudeBindingReceipt) {
	m, r := oauthBindingFixture()
	models := []BindingModelPin{
		{RouteID: "fixture-route-1", Model: "fixture-model-1", ModelSHA256: strings.Repeat("1", 64)},
		{RouteID: "fixture-route-2", Model: "fixture-model-2", ModelSHA256: strings.Repeat("2", 64)},
		{RouteID: "fixture-route-3", Model: "fixture-model", ModelSHA256: strings.Repeat("3", 64)},
	}
	a := m.Bindings.Credentials[0]
	a.AccountAlias, a.Models = "fixture-account-a", append([]BindingModelPin(nil), models...)
	b := a
	b.AuthRef, b.AccountAlias, b.AccountIdentitySHA256 = strings.Repeat("9", 64), "fixture-account-b", strings.Repeat("8", 64)
	b.Models = append([]BindingModelPin(nil), models...)
	m.Bindings.Credentials = []BindingCredentialPin{a, b}
	observed := func(pin BindingCredentialPin, token string) claudeBindingCredential {
		c := r.Inventory.Credentials[0]
		c.AuthRef, c.AccountAlias, c.AccountIdentitySHA256 = pin.AuthRef, pin.AccountAlias, pin.AccountIdentitySHA256
		c.CredentialSHA256, c.VerifiedGeneration = token, 1
		c.Models = nil
		for _, model := range pin.Models {
			c.Models = append(c.Models, claudeBindingModel{RouteID: model.RouteID, Model: model.Model, ResolvedModel: model.Model, Registered: true, ModelSHA256: model.ModelSHA256})
		}
		return c
	}
	r.Inventory.Credentials = []claudeBindingCredential{observed(a, strings.Repeat("4", 64)), observed(b, strings.Repeat("5", 64))}
	r.Inventory.SelectionPinsMatch = true
	return m, r
}

// liveStandby is the observed shape of a quota-blocked account whose access
// token expired while blocked: no proof, a token digest, models registered.
func liveStandby(c *claudeBindingCredential) {
	c.AuthActive, c.PinMatches = false, false
	c.AccountIdentitySHA256, c.VerifiedGeneration = "", 0
}

func coldActive(c *claudeBindingCredential) {
	c.AuthActive, c.PinMatches = true, false
	c.AccountIdentitySHA256, c.VerifiedGeneration = "", 0
	c.CredentialSHA256 = strings.Repeat("6", 64)
}

func validateReceipt(t *testing.T, m Manifest, r claudeBindingReceipt) error {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return ValidateClaudeBindingReceipt(m, raw)
}

func wantHold(t *testing.T, err error, code string) {
	t.Helper()
	var hold *Hold
	if !errors.As(err, &hold) || hold.Code != code {
		t.Fatalf("got %v, want hold %s", err, code)
	}
}

func TestBindingAcceptsConsistentStandbyWhileAnotherAccountServes(t *testing.T) {
	cases := map[string]func(*Manifest, *claudeBindingReceipt){
		"expired token standby": func(_ *Manifest, r *claudeBindingReceipt) {
			liveStandby(&r.Inventory.Credentials[1])
			r.Inventory.SelectionPinsMatch = false
		},
		"quota standby keeps its proof": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[1].AuthActive = false
			r.Inventory.SelectionPinsMatch = false
		},
		"serving account covers a partially pinned standby": func(m *Manifest, r *claudeBindingReceipt) {
			m.Bindings.Credentials[1].Models = m.Bindings.Credentials[1].Models[:2]
			r.Inventory.Credentials[1].Models = r.Inventory.Credentials[1].Models[:2]
			liveStandby(&r.Inventory.Credentials[1])
			r.Inventory.SelectionPinsMatch = false
		},
		"standby on either slot": func(_ *Manifest, r *claudeBindingReceipt) {
			liveStandby(&r.Inventory.Credentials[0])
			r.Inventory.SelectionPinsMatch = false
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m, r := dualOAuthBindingFixture()
			mutate(&m, &r)
			if err := validateReceipt(t, m, r); err != nil {
				t.Fatalf("serving lane with a consistent standby held: %v", err)
			}
		})
	}
	t.Run("exact key standby", func(t *testing.T) {
		m, r := bindingFixture()
		second := m.Bindings.Credentials[0]
		second.AuthRef, second.AccountAlias, second.CredentialSHA256 = strings.Repeat("9", 64), "fixture-account-b", strings.Repeat("8", 64)
		m.Bindings.Credentials = append(m.Bindings.Credentials, second)
		standby := r.Inventory.Credentials[0]
		standby.AuthRef, standby.AccountAlias, standby.CredentialSHA256 = second.AuthRef, second.AccountAlias, second.CredentialSHA256
		standby.AuthActive, standby.PinMatches = false, false
		r.Inventory.Credentials = append(r.Inventory.Credentials, standby)
		r.Inventory.SelectionPinsMatch = false
		if err := validateReceipt(t, m, r); err != nil {
			t.Fatalf("exact key standby held: %v", err)
		}
		r.Inventory.Credentials[1].CredentialSHA256 = strings.Repeat("7", 64)
		wantHold(t, validateReceipt(t, m, r), "binding_credential_mismatch")
	})
}

func TestBindingHoldsUnservedUnprovenOrInconsistentLane(t *testing.T) {
	standbyB := func(r *claudeBindingReceipt) {
		liveStandby(&r.Inventory.Credentials[1])
		r.Inventory.SelectionPinsMatch = false
	}
	cases := []struct {
		name   string
		code   string
		mutate func(*Manifest, *claudeBindingReceipt)
	}{
		{"all accounts blocked", "binding_route_unserved", func(_ *Manifest, r *claudeBindingReceipt) {
			liveStandby(&r.Inventory.Credentials[0])
			standbyB(r)
		}},
		{"serving account misses a pinned key", "binding_route_unserved", func(m *Manifest, r *claudeBindingReceipt) {
			m.Bindings.Credentials[0].Models = m.Bindings.Credentials[0].Models[:2]
			r.Inventory.Credentials[0].Models = r.Inventory.Credentials[0].Models[:2]
			standbyB(r)
		}},
		{"active account proves another identity", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].PinMatches = false
			r.Inventory.Credentials[0].AccountIdentitySHA256 = strings.Repeat("a", 64)
			r.Inventory.SelectionPinsMatch = false
		}},
		{"active account unverified beside a healthy one", "binding_credential_unverified", func(_ *Manifest, r *claudeBindingReceipt) {
			coldActive(&r.Inventory.Credentials[0])
			r.Inventory.SelectionPinsMatch = false
		}},
		{"active account unverified beside a standby", "binding_credential_unverified", func(_ *Manifest, r *claudeBindingReceipt) {
			coldActive(&r.Inventory.Credentials[0])
			standbyB(r)
		}},
		{"unverified pin claim on active account", "binding_credential_unverified", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].PinMatches = false
			standbyB(r)
		}},
		{"extra inactive credential", "binding_credential_set_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			extra := r.Inventory.Credentials[1]
			extra.AuthRef = strings.Repeat("7", 64)
			liveStandby(&extra)
			r.Inventory.Credentials = append(r.Inventory.Credentials, extra)
			r.Inventory.SelectionPinsMatch = false
		}},
		{"extra active credential", "binding_credential_set_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			extra := r.Inventory.Credentials[1]
			extra.AuthRef = strings.Repeat("7", 64)
			r.Inventory.Credentials = append(r.Inventory.Credentials, extra)
		}},
		{"duplicate auth ref as standby", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[1] = r.Inventory.Credentials[0]
			liveStandby(&r.Inventory.Credentials[1])
			r.Inventory.SelectionPinsMatch = false
		}},
		{"unknown auth ref as standby", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[1].AuthRef = strings.Repeat("7", 64)
			standbyB(r)
		}},
		{"missing standby", "binding_credential_set_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials = r.Inventory.Credentials[:1]
			r.Inventory.SelectionPinsMatch = false
		}},
		{"absent standby auth", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			c := &r.Inventory.Credentials[1]
			*c = claudeBindingCredential{AuthRef: c.AuthRef, AccountAlias: c.AccountAlias, ModelRegistrationEpoch: 1, Models: c.Models}
			for k := range c.Models {
				c.Models[k].ResolvedModel, c.Models[k].Registered, c.Models[k].ModelSHA256 = "", false, ""
			}
			r.Inventory.SelectionPinsMatch = false
		}},
		{"standby model digest differs", "binding_model_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].Models[0].ModelSHA256 = strings.Repeat("f", 64)
		}},
		{"standby model resolves elsewhere", "binding_model_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].Models[1].ResolvedModel = "unreviewed-upstream"
		}},
		{"standby model unregistered", "binding_model_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].Models[2].Registered = false
		}},
		{"standby route changed", "binding_model_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].Models[2].RouteID = "unreviewed-route"
		}},
		{"standby extra model", "binding_model_set_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].Models = append(r.Inventory.Credentials[1].Models, r.Inventory.Credentials[1].Models[0])
		}},
		{"standby missing model", "binding_model_set_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].Models = r.Inventory.Credentials[1].Models[1:]
		}},
		{"standby alias drift", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].AccountAlias = "other-account"
		}},
		{"standby kind drift", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].CredentialKind = "x-api-key"
		}},
		{"standby mode drift", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].BindingMode = ""
		}},
		{"standby auth generation zero", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].AuthGeneration = 0
		}},
		{"standby auth epoch zero", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].AuthRegistrationEpoch = 0
		}},
		{"standby model epoch zero", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].ModelRegistrationEpoch = 0
		}},
		{"standby shows another identity", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].AccountIdentitySHA256, r.Inventory.Credentials[1].VerifiedGeneration = strings.Repeat("a", 64), 3
		}},
		{"standby proof generation without identity", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].VerifiedGeneration = 3
		}},
		{"standby identity without proof generation", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].AccountIdentitySHA256 = strings.Repeat("8", 64)
		}},
		{"standby malformed token digest", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.Credentials[1].CredentialSHA256 = "not-a-digest"
		}},
		{"aggregate claims all serve while one is blocked", "binding_observation_invalid", func(_ *Manifest, r *claudeBindingReceipt) {
			liveStandby(&r.Inventory.Credentials[1])
		}},
		{"aggregate denies selection while all serve", "binding_inventory_incomplete", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.SelectionPinsMatch = false
		}},
		{"active missing proof", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].VerifiedGeneration = 0
			standbyB(r)
		}},
		{"active metadata only", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].AccountIdentitySHA256 = ""
			standbyB(r)
		}},
		{"active unknown outgoing token", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].CredentialSHA256 = ""
			standbyB(r)
		}},
		{"active invalid outgoing token digest", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].CredentialSHA256 = "not-a-digest"
			standbyB(r)
		}},
		{"active wrong mode", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].BindingMode = ""
			standbyB(r)
		}},
		{"active wrong kind", "binding_credential_mismatch", func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].CredentialKind = "x-api-key"
			standbyB(r)
		}},
		{"snapshot with standby", "binding_inventory_incomplete", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.SnapshotComplete = false
		}},
		{"executor with standby", "binding_inventory_incomplete", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.BuiltinExecutor = false
		}},
		{"executor config with standby", "binding_inventory_incomplete", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.ExecutorConfigMatches = false
		}},
		{"manager config with standby", "binding_inventory_incomplete", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.ManagerConfigMatches = false
		}},
		{"transport with standby", "binding_inventory_incomplete", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.NativeTransport = false
		}},
		{"registry with standby", "binding_inventory_incomplete", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.Inventory.RegistryGeneration = 0
		}},
		{"partial config apply with standby", "binding_config_drift", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.ConfigApplyComplete = false
		}},
		{"config digest drift with standby", "binding_config_drift", func(_ *Manifest, r *claudeBindingReceipt) {
			standbyB(r)
			r.ExecutionConfigSHA256 = strings.Repeat("f", 64)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, r := dualOAuthBindingFixture()
			tc.mutate(&m, &r)
			wantHold(t, validateReceipt(t, m, r), tc.code)
		})
	}
}

// legacyBindingAccepts is the predicate before standby support, kept only to
// prove that the new predicate accepts a strict superset.
func legacyBindingAccepts(m Manifest, receipt claudeBindingReceipt) bool {
	if !receipt.ConfigApplyComplete || receipt.ManagedAdmissionSHA256 != m.Runtime.ManagedAdmissionSHA256 || receipt.ExecutionConfigSHA256 != m.Runtime.ExecutionConfigSHA256 {
		return false
	}
	i := receipt.Inventory
	if !i.SnapshotComplete || !i.SelectionPinsMatch || !i.BuiltinExecutor || !i.ExecutorConfigMatches || !i.ManagerConfigMatches || !i.NativeTransport || i.RegistryGeneration == 0 || i.UnconfiguredClaudeCredentials < 0 {
		return false
	}
	expected := map[string]BindingCredentialPin{}
	for _, credential := range m.Bindings.Credentials {
		expected[credential.AuthRef] = credential
	}
	if len(i.Credentials) != len(expected) {
		return false
	}
	for _, got := range i.Credentials {
		want, ok := expected[got.AuthRef]
		if !ok || got.AccountAlias != want.AccountAlias || !credentialObservationMatches(want, got) || !got.PinMatches || !got.AuthActive || got.AuthGeneration == 0 || got.AuthRegistrationEpoch == 0 || got.ModelRegistrationEpoch == 0 {
			return false
		}
		delete(expected, got.AuthRef)
		if verifyBindingModels(want, got) != nil {
			return false
		}
	}
	return true
}

func TestBindingPredicateOnlyAddsConsistentStandbys(t *testing.T) {
	mutations := map[string]func(*Manifest, *claudeBindingReceipt){
		"valid":          func(*Manifest, *claudeBindingReceipt) {},
		"neighbors":      func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.UnconfiguredClaudeCredentials = 8 },
		"partial_reload": func(_ *Manifest, r *claudeBindingReceipt) { r.ConfigApplyComplete = false },
		"managed_digest": func(_ *Manifest, r *claudeBindingReceipt) { r.ManagedAdmissionSHA256 = strings.Repeat("f", 64) },
		"snapshot":       func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.SnapshotComplete = false },
		"selection":      func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.SelectionPinsMatch = false },
		"executor":       func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.BuiltinExecutor = false },
		"registry":       func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.RegistryGeneration = 0 },
		"credential_swap": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].CredentialSHA256 = strings.Repeat("f", 64)
		},
		"credential_kind": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].CredentialKind = "other-kind" },
		"auth_ref": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].AuthRef = strings.Repeat("f", 64)
		},
		"account_alias": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].AccountAlias = "other-account" },
		"inactive":      func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].AuthActive = false },
		"standby_a": func(_ *Manifest, r *claudeBindingReceipt) {
			liveStandby(&r.Inventory.Credentials[0])
			r.Inventory.SelectionPinsMatch = false
		},
		"unmatched_pin": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].PinMatches = false },
		"auth_epoch":    func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].AuthRegistrationEpoch = 0 },
		"model_epoch":   func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].ModelRegistrationEpoch = 0 },
		"missing":       func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials = r.Inventory.Credentials[1:] },
		"extra": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials = append(r.Inventory.Credentials, r.Inventory.Credentials[0])
		},
		"model_digest": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].Models[0].ModelSHA256 = strings.Repeat("f", 64)
		},
		"model_resolution": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].Models[0].ResolvedModel = "unreviewed"
		},
		"model_registered": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].Models[0].Registered = false },
		"missing_model":    func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].Models = nil },
		"identity": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].AccountIdentitySHA256 = strings.Repeat("a", 64)
		},
		"proof": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].VerifiedGeneration = 0 },
		"standby_b": func(_ *Manifest, r *claudeBindingReceipt) {
			liveStandby(&r.Inventory.Credentials[len(r.Inventory.Credentials)-1])
			r.Inventory.SelectionPinsMatch = false
		},
	}
	fixtures := map[string]func() (Manifest, claudeBindingReceipt){"exact": bindingFixture, "oauth": oauthBindingFixture, "dual": dualOAuthBindingFixture}
	widened := 0
	for fixtureName, fixture := range fixtures {
		for name, mutate := range mutations {
			m, r := fixture()
			mutate(&m, &r)
			legacy, current := legacyBindingAccepts(m, r), verifyClaudeBindingInventory(m, r) == nil
			if legacy && !current {
				t.Errorf("%s/%s: previously accepted inventory is now held", fixtureName, name)
			}
			if !legacy && current {
				widened++
				if !strings.Contains(name, "standby") || fixtureName != "dual" {
					t.Errorf("%s/%s: newly accepted inventory is not a consistent standby of a served lane", fixtureName, name)
				}
			}
		}
	}
	if widened != 2 {
		t.Fatalf("expected exactly the two dual-account standbys to be newly accepted, got %d", widened)
	}
}

type bindingTestServer struct {
	t        *testing.T
	mu       sync.Mutex
	gets     atomic.Int64
	posts    atomic.Int64
	receipts []claudeBindingReceipt // successive GET answers; the last one repeats
	verify   int
	onVerify func()
}

func (s *bindingTestServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer synthetic-management-key" {
		s.t.Error("wrong observer credential")
	}
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/v8/management/admission/claude-identity/verify":
		s.posts.Add(1)
		body, _ := io.ReadAll(req.Body)
		if req.URL.RawQuery != "" || len(body) != 0 || req.ContentLength > 0 {
			s.t.Error("verification request carried input")
		}
		if s.onVerify != nil {
			s.onVerify()
		}
		w.Header().Set("Cache-Control", "no-store")
		status := s.verify
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"verified":true}`))
	case req.Method == http.MethodGet && req.URL.Path == "/v8/management/observability/admission/claude-bindings":
		n := int(s.gets.Add(1))
		s.mu.Lock()
		r := s.receipts[len(s.receipts)-1]
		if n <= len(s.receipts) {
			r = s.receipts[n-1]
		}
		s.mu.Unlock()
		r.RequestNonce, r.ObservedAt = req.URL.Query().Get("request_nonce"), time.Now().UTC()
		w.Header().Set("Cache-Control", "no-store")
		b, _ := json.Marshal(r)
		_, _ = w.Write(b)
	default:
		s.t.Errorf("unexpected gateway request %s %s", req.Method, req.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func resetIdentityVerificationLimiter(t *testing.T) {
	t.Helper()
	claudeIdentityVerification.mu.Lock()
	claudeIdentityVerification.last = nil
	claudeIdentityVerification.mu.Unlock()
}

func TestBindingObservationVerifiesRotatedTokenOnceThenReobserves(t *testing.T) {
	rotated := func() (Manifest, claudeBindingReceipt, claudeBindingReceipt) {
		m, verified := dualOAuthBindingFixture()
		liveStandby(&verified.Inventory.Credentials[1])
		verified.Inventory.SelectionPinsMatch = false
		cold := verified
		cold.Inventory.Credentials = append([]claudeBindingCredential(nil), verified.Inventory.Credentials...)
		coldActive(&cold.Inventory.Credentials[0])
		verified.Inventory.Credentials[0].CredentialSHA256 = cold.Inventory.Credentials[0].CredentialSHA256
		return m, cold, verified
	}
	t.Run("verified after one request", func(t *testing.T) {
		resetIdentityVerificationLimiter(t)
		m, cold, verified := rotated()
		t.Setenv(m.Runtime.ManagementKeyEnv, "synthetic-management-key")
		s := &bindingTestServer{t: t, receipts: []claudeBindingReceipt{cold, verified}}
		srv := httptest.NewServer(s)
		defer srv.Close()
		m.GatewayURL = srv.URL
		if err := observeClaudeBindings(m); err != nil {
			t.Fatalf("rotated token was not re-observed after verification: %v", err)
		}
		if s.gets.Load() != 2 || s.posts.Load() != 1 {
			t.Fatalf("gets=%d posts=%d, want 2 and 1", s.gets.Load(), s.posts.Load())
		}
	})
	t.Run("verification response is not evidence", func(t *testing.T) {
		resetIdentityVerificationLimiter(t)
		m, cold, _ := rotated()
		t.Setenv(m.Runtime.ManagementKeyEnv, "synthetic-management-key")
		s := &bindingTestServer{t: t, receipts: []claudeBindingReceipt{cold}}
		srv := httptest.NewServer(s)
		defer srv.Close()
		m.GatewayURL = srv.URL
		err := observeClaudeBindings(m)
		wantHold(t, err, "binding_credential_unverified")
		if s.gets.Load() != 2 || s.posts.Load() != 1 {
			t.Fatalf("gets=%d posts=%d, want 2 and 1", s.gets.Load(), s.posts.Load())
		}
		if strings.Contains(err.Error(), "synthetic-management-key") {
			t.Fatal("secret disclosed")
		}
	})
	t.Run("failed verification is rate limited per credential set", func(t *testing.T) {
		resetIdentityVerificationLimiter(t)
		m, cold, _ := rotated()
		t.Setenv(m.Runtime.ManagementKeyEnv, "synthetic-management-key")
		s := &bindingTestServer{t: t, receipts: []claudeBindingReceipt{cold}, verify: http.StatusServiceUnavailable}
		srv := httptest.NewServer(s)
		defer srv.Close()
		m.GatewayURL = srv.URL
		for i := 0; i < 3; i++ {
			wantHold(t, observeClaudeBindings(m), "binding_credential_unverified")
		}
		if s.posts.Load() != 1 || s.gets.Load() != 4 {
			t.Fatalf("gets=%d posts=%d, want 4 and 1", s.gets.Load(), s.posts.Load())
		}
	})
	t.Run("only an unverified selectable OAuth credential asks", func(t *testing.T) {
		resetIdentityVerificationLimiter(t)
		m, r := bindingFixture() // exact key mode
		r.Inventory.Credentials[0].PinMatches = false
		r.Inventory.SelectionPinsMatch = false
		dm, dr := dualOAuthBindingFixture()
		liveStandby(&dr.Inventory.Credentials[0])
		liveStandby(&dr.Inventory.Credentials[1])
		dr.Inventory.SelectionPinsMatch = false
		for _, tc := range []struct {
			m    Manifest
			r    claudeBindingReceipt
			code string
		}{{m, r, "binding_credential_unverified"}, {dm, dr, "binding_route_unserved"}} {
			t.Setenv(tc.m.Runtime.ManagementKeyEnv, "synthetic-management-key")
			s := &bindingTestServer{t: t, receipts: []claudeBindingReceipt{tc.r}}
			srv := httptest.NewServer(s)
			tc.m.GatewayURL = srv.URL
			wantHold(t, observeClaudeBindings(tc.m), tc.code)
			srv.Close()
			if s.posts.Load() != 0 || s.gets.Load() != 1 {
				t.Fatalf("gets=%d posts=%d, want one observation and no verification", s.gets.Load(), s.posts.Load())
			}
		}
	})
}

func selfGatewayProof(t *testing.T) ProcessProof {
	t.Helper()
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skip("procfs unavailable")
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	cmdline, _ := os.ReadFile("/proc/self/cmdline")
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Skip("executable unobservable")
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Skip("executable unreadable")
	}
	return ProcessProof{PID: os.Getpid(), UID: uint32(os.Getuid()), BootID: strings.TrimSpace(string(boot)), StartTicks: fields[19], CmdlineSHA256: digest(cmdline), Binary: FileProof{Path: exe, SHA256: digest(b)}}
}

func readinessPolicy(t *testing.T, m Manifest) Policy {
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

func TestObserveBindingReadinessIsAPureProbe(t *testing.T) {
	gateway := selfGatewayProof(t)
	standbyLane := func() (Manifest, claudeBindingReceipt) {
		m, r := dualOAuthBindingFixture()
		liveStandby(&r.Inventory.Credentials[1])
		r.Inventory.SelectionPinsMatch = false
		m.Version, m.EvidenceKind, m.ExpiresAt, m.Gateway = 1, "installed", time.Now().Add(time.Hour), gateway
		return m, r
	}
	run := func(t *testing.T, mutate func(*Manifest, *claudeBindingReceipt, *Policy)) (BindingReadiness, error, *bindingTestServer) {
		resetIdentityVerificationLimiter(t)
		m, r := standbyLane()
		t.Setenv(m.Runtime.ManagementKeyEnv, "synthetic-management-key")
		s := &bindingTestServer{t: t, receipts: []claudeBindingReceipt{r}}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		m.GatewayURL = srv.URL
		var policy Policy
		if mutate != nil {
			mutate(&m, &s.receipts[0], &policy)
		}
		if policy.ManifestPath == "" {
			want := policy.RequireVerifiedRoute || mutate == nil
			policy = readinessPolicy(t, m)
			if !want {
				policy.RequireVerifiedRoute = false
			}
		}
		report, err := ObserveBindingReadinessReport(policy)
		if err != nil && strings.Contains(err.Error(), "synthetic-management-key") {
			t.Fatal("secret disclosed")
		}
		return report, err, s
	}
	t.Run("consistent standby lane is ready", func(t *testing.T) {
		report, err, s := run(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.gets.Load() != 1 || s.posts.Load() != 0 || report.UnverifiedStandbyCredentials != 1 {
			t.Fatalf("gets=%d posts=%d unverified standbys=%d", s.gets.Load(), s.posts.Load(), report.UnverifiedStandbyCredentials)
		}
	})
	t.Run("not required", func(t *testing.T) {
		_, err, s := run(t, func(*Manifest, *claudeBindingReceipt, *Policy) {})
		if err != nil || s.gets.Load() != 0 {
			t.Fatalf("err=%v gets=%d", err, s.gets.Load())
		}
	})
	t.Run("all accounts blocked", func(t *testing.T) {
		_, err, _ := run(t, func(_ *Manifest, r *claudeBindingReceipt, p *Policy) {
			liveStandby(&r.Inventory.Credentials[0])
			p.RequireVerifiedRoute = true
		})
		wantHold(t, err, "binding_route_unserved")
	})
	for name, tc := range map[string]struct {
		code   string
		mutate func(*Manifest)
	}{
		"wrong gateway process": {"gateway_instance_drift", func(m *Manifest) { m.Gateway.StartTicks = "0" }},
		"expired manifest":      {"proof_expired", func(m *Manifest) { m.ExpiresAt = time.Now().Add(-time.Minute) }},
		"source evidence":       {"source_evidence_not_installed", func(m *Manifest) { m.EvidenceKind = "source" }},
	} {
		t.Run(name, func(t *testing.T) {
			_, err, s := run(t, func(m *Manifest, _ *claudeBindingReceipt, p *Policy) {
				tc.mutate(m)
				p.RequireVerifiedRoute = true
			})
			wantHold(t, err, tc.code)
			if s.gets.Load() != 0 {
				t.Fatal("observed an unproven gateway")
			}
		})
	}
	t.Run("manifest drift", func(t *testing.T) {
		_, err, s := run(t, func(m *Manifest, _ *claudeBindingReceipt, p *Policy) {
			*p = readinessPolicy(t, *m)
			p.ManifestSHA256 = strings.Repeat("0", 64)
		})
		wantHold(t, err, "manifest_drift")
		if s.gets.Load() != 0 {
			t.Fatal("observed with a drifted manifest")
		}
	})
}
