package aiexecution

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const ClaudeBindingProjection = "managed-claude-bindings-v1"

// BindingExpectation pins the entire finite managed credential/model inventory.
// It proves reviewed selection only. ModelAccountPolicy must independently bind
// these opaque pins/aliases to real account ownership and actual pricing.
type BindingExpectation struct {
	SchemaVersion     int                    `json:"schema_version"`
	ProjectionVersion string                 `json:"projection_version"`
	Credentials       []BindingCredentialPin `json:"credentials"`
}

type BindingCredentialPin struct {
	AuthRef               string            `json:"auth_ref"`
	AccountAlias          string            `json:"account_alias"`
	CredentialKind        string            `json:"credential_kind"`
	CredentialSHA256      string            `json:"credential_sha256,omitempty"`
	BindingMode           string            `json:"binding_mode,omitempty"`
	AccountIdentitySHA256 string            `json:"account_identity_sha256,omitempty"`
	Models                []BindingModelPin `json:"models"`
}

type BindingModelPin struct {
	RouteID     string `json:"route_id"`
	Model       string `json:"model"`
	ModelSHA256 string `json:"model_sha256"`
}

type claudeBindingReceipt struct {
	SchemaVersion          int       `json:"schema_version"`
	ProjectionVersion      string    `json:"projection_version"`
	ObservationScope       string    `json:"observation_scope"`
	ProcessInstanceID      string    `json:"process_instance_id"`
	ObservedAt             time.Time `json:"observed_at"`
	RequestNonce           string    `json:"request_nonce"`
	ConfigApplyComplete    bool      `json:"config_apply_complete"`
	ManagedAdmissionSHA256 string    `json:"managed_admission_sha256"`
	ExecutionConfigSHA256  string    `json:"execution_config_sha256"`
	Inventory              struct {
		SnapshotComplete              bool                      `json:"snapshot_complete"`
		SelectionPinsMatch            bool                      `json:"selection_pins_match"`
		BuiltinExecutor               bool                      `json:"builtin_executor"`
		ExecutorConfigMatches         bool                      `json:"executor_config_matches"`
		ManagerConfigMatches          bool                      `json:"manager_config_matches"`
		NativeTransport               bool                      `json:"native_transport"`
		UnconfiguredClaudeCredentials int                       `json:"unconfigured_claude_credentials"`
		RegistryGeneration            uint64                    `json:"registry_generation"`
		Credentials                   []claudeBindingCredential `json:"credentials"`
	} `json:"inventory"`
}

type claudeBindingCredential struct {
	AuthRef                string               `json:"auth_ref"`
	AccountAlias           string               `json:"account_alias"`
	CredentialKind         string               `json:"credential_kind"`
	CredentialSHA256       string               `json:"credential_sha256"`
	BindingMode            string               `json:"binding_mode,omitempty"`
	AccountIdentitySHA256  string               `json:"account_identity_sha256,omitempty"`
	VerifiedGeneration     uint64               `json:"verified_generation,omitempty"`
	PinMatches             bool                 `json:"pin_matches"`
	AuthActive             bool                 `json:"auth_active"`
	AuthGeneration         uint64               `json:"auth_generation"`
	AuthRegistrationEpoch  uint64               `json:"auth_registration_epoch"`
	ModelRegistrationEpoch uint64               `json:"model_registration_epoch"`
	Models                 []claudeBindingModel `json:"models"`
}

type claudeBindingModel struct {
	RouteID       string `json:"route_id"`
	Model         string `json:"model"`
	ResolvedModel string `json:"resolved_model"`
	Registered    bool   `json:"registered"`
	ModelSHA256   string `json:"model_sha256"`
}

func bindingIdentifier(value string) bool {
	return value != "" && len(value) <= 512 && strings.IndexFunc(value, func(r rune) bool { return r <= 32 || r == 127 }) < 0
}

const OAuthAccountBindingMode = "oauth-account-v1"

func validCredentialExpectation(p BindingCredentialPin) bool {
	switch p.BindingMode {
	case "":
		return validDigest(p.CredentialSHA256) && p.AccountIdentitySHA256 == "" && (p.CredentialKind == "x-api-key" || p.CredentialKind == "authorization-bearer")
	case OAuthAccountBindingMode:
		return p.CredentialKind == "authorization-bearer" && p.CredentialSHA256 == "" && validDigest(p.AccountIdentitySHA256)
	default:
		return false
	}
}

func credentialObservationMatches(want BindingCredentialPin, got claudeBindingCredential) bool {
	if !validCredentialExpectation(want) || got.BindingMode != want.BindingMode || got.CredentialKind != want.CredentialKind {
		return false
	}
	if want.BindingMode == OAuthAccountBindingMode {
		// The pinned builtin gateway verifies the actual token against the official
		// provider profile. Auth metadata and a changing auth epoch alone are not
		// identity evidence. Each verified token has its own runtime generation.
		return got.AccountIdentitySHA256 == want.AccountIdentitySHA256 && got.VerifiedGeneration > 0 && validDigest(got.CredentialSHA256)
	}
	return got.CredentialSHA256 == want.CredentialSHA256 && got.AccountIdentitySHA256 == "" && got.VerifiedGeneration == 0
}

func validateBindingExpectation(m Manifest) error {
	e := m.Bindings
	if e.SchemaVersion != 1 || e.ProjectionVersion != ClaudeBindingProjection || len(e.Credentials) == 0 || len(e.Credentials) > 64 || len(m.Routes) == 0 {
		return Held("binding_expectation_invalid")
	}
	auths, models := map[string]bool{}, map[string]bool{}
	for _, credential := range e.Credentials {
		if !validDigest(credential.AuthRef) || auths[credential.AuthRef] || !bindingIdentifier(credential.AccountAlias) || !validCredentialExpectation(credential) || len(credential.Models) == 0 || len(credential.Models) > 128 {
			return Held("binding_expectation_invalid")
		}
		auths[credential.AuthRef] = true
		seen := map[string]bool{}
		for _, model := range credential.Models {
			key := model.RouteID + "\x00" + model.Model
			if !bindingIdentifier(model.RouteID) || !bindingIdentifier(model.Model) || !validDigest(model.ModelSHA256) || seen[key] {
				return Held("binding_expectation_invalid")
			}
			seen[key], models[model.Model] = true, true
		}
	}
	for _, route := range m.Routes {
		if !models[route.Model] {
			return Held("binding_role_model_unproven")
		}
	}
	return nil
}

// observeClaudeBindings observes only the gateway instance already pinned by
// Inspect. The nonce, process identity and both config digests must match the
// independently observed configuration-only receipt. No provider request runs.
func observeClaudeBindings(m Manifest) error {
	if err := validateBindingExpectation(m); err != nil {
		return err
	}
	id, err := uuid.Parse(m.Runtime.ProcessInstanceID)
	if err != nil || id.String() != m.Runtime.ProcessInstanceID || m.Runtime.ProjectionVersion != RuntimeProjection || !validDigest(m.Runtime.ManagedAdmissionSHA256) || !validDigest(m.Runtime.ExecutionConfigSHA256) {
		return Held("binding_runtime_expectation_invalid")
	}
	u, err := url.Parse(m.GatewayURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return Held("binding_observation_route_unsupported")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port <= 0 || port > 65535 {
		return Held("binding_observation_route_unsupported")
	}
	if err := inspectListener(m.Gateway.PID, ip, port); err != nil {
		return err
	}
	keyName := m.Runtime.ManagementKeyEnv
	if keyName == "" || strings.IndexFunc(keyName, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') }) >= 0 {
		return Held("binding_management_key_unavailable")
	}
	key := os.Getenv(keyName)
	if key == "" {
		return Held("binding_management_key_unavailable")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Held("binding_nonce_unavailable")
	}
	nonce := hex.EncodeToString(random[:])
	u.Path, u.RawQuery = "/v8/management/observability/admission/claude-bindings", "request_nonce="+nonce
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return Held("binding_observation_unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	response, err := client.Do(req)
	if err != nil {
		return Held("binding_observation_unavailable")
	}
	defer response.Body.Close()
	noStore := false
	for _, directive := range strings.Split(strings.ToLower(response.Header.Get("Cache-Control")), ",") {
		noStore = noStore || strings.TrimSpace(directive) == "no-store"
	}
	if response.StatusCode != http.StatusOK || !noStore {
		return Held("binding_observation_rejected")
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || len(b) > 256<<10 {
		return Held("binding_observation_invalid")
	}
	var receipt claudeBindingReceipt
	if DecodeStrict(b, &receipt) != nil {
		return Held("binding_observation_invalid")
	}
	if receipt.SchemaVersion != 1 || receipt.ProjectionVersion != ClaudeBindingProjection || receipt.ObservationScope != "credential_selection_only" || receipt.RequestNonce != nonce || receipt.ProcessInstanceID != m.Runtime.ProcessInstanceID || receipt.ObservedAt.Before(start.Add(-time.Second)) || receipt.ObservedAt.After(time.Now().Add(time.Second)) {
		return Held("binding_observation_binding_mismatch")
	}
	if err := verifyClaudeBindingInventory(m, receipt); err != nil {
		return err
	}
	return inspectListener(m.Gateway.PID, ip, port)
}

func verifyClaudeBindingInventory(m Manifest, receipt claudeBindingReceipt) error {
	if !receipt.ConfigApplyComplete || receipt.ManagedAdmissionSHA256 != m.Runtime.ManagedAdmissionSHA256 || receipt.ExecutionConfigSHA256 != m.Runtime.ExecutionConfigSHA256 {
		return Held("binding_config_drift")
	}
	i := receipt.Inventory
	if !i.SnapshotComplete || !i.SelectionPinsMatch || !i.BuiltinExecutor || !i.ExecutorConfigMatches || !i.ManagerConfigMatches || !i.NativeTransport || i.RegistryGeneration == 0 || i.UnconfiguredClaudeCredentials < 0 {
		return Held("binding_inventory_incomplete")
	}
	expected := map[string]BindingCredentialPin{}
	for _, credential := range m.Bindings.Credentials {
		expected[credential.AuthRef] = credential
	}
	if len(i.Credentials) != len(expected) {
		return Held("binding_credential_set_mismatch")
	}
	for _, got := range i.Credentials {
		want, ok := expected[got.AuthRef]
		if !ok || got.AccountAlias != want.AccountAlias || !credentialObservationMatches(want, got) || !got.PinMatches || !got.AuthActive || got.AuthGeneration == 0 || got.AuthRegistrationEpoch == 0 || got.ModelRegistrationEpoch == 0 {
			return Held("binding_credential_mismatch")
		}
		delete(expected, got.AuthRef)
		models := map[string]BindingModelPin{}
		for _, model := range want.Models {
			models[model.RouteID+"\x00"+model.Model] = model
		}
		if len(got.Models) != len(models) {
			return Held("binding_model_set_mismatch")
		}
		for _, observed := range got.Models {
			key := observed.RouteID + "\x00" + observed.Model
			approved, ok := models[key]
			if !ok || !observed.Registered || observed.ResolvedModel != observed.Model || observed.ModelSHA256 != approved.ModelSHA256 {
				return Held("binding_model_mismatch")
			}
			delete(models, key)
		}
	}
	return nil
}

// ValidateClaudeBindingReceipt shares the launch-time inventory contract with
// offline provisioning tools. The caller must independently verify freshness,
// nonce, authenticated transport and PID ownership of the observed endpoint.
func ValidateClaudeBindingReceipt(m Manifest, raw []byte) error {
	if err := validateBindingExpectation(m); err != nil {
		return err
	}
	var receipt claudeBindingReceipt
	if DecodeStrict(raw, &receipt) != nil {
		return Held("binding_observation_invalid")
	}
	if receipt.SchemaVersion != 1 || receipt.ProjectionVersion != ClaudeBindingProjection || receipt.ObservationScope != "credential_selection_only" || receipt.ProcessInstanceID != m.Runtime.ProcessInstanceID {
		return Held("binding_observation_binding_mismatch")
	}
	return verifyClaudeBindingInventory(m, receipt)
}
