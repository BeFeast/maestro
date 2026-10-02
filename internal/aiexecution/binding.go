package aiexecution

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const ClaudeBindingProjection = "managed-claude-bindings-v1"

// BindingExpectation pins the entire finite managed credential/model inventory:
// the configured credential set must equal the pinned set, and every pinned
// (route, model) key needs at least one selectable credential whose identity
// proof matches its pin. Credentials that are not selectable stay pinned as
// standbys. It proves reviewed selection only. ModelAccountPolicy must
// independently bind these opaque pins/aliases to real account ownership and
// actual pricing.
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
	return observeClaudeBindingsWithKey(m, os.Getenv(m.Runtime.ManagementKeyEnv))
}

func observeClaudeBindingsWithKey(m Manifest, key string) error {
	_, err := observeClaudeBindingsReport(m, key)
	return err
}

// observeClaudeBindingsReport observes the managed lane once. When the only
// remaining obstacle is a currently selectable OAuth credential whose token has
// no identity proof yet (the gateway keys proofs by the exact token digest, so
// every token rotation starts unverified), it asks the gateway's exempt
// control-plane verifier once, rate limited per credential set, and observes
// again. The verifier response is never evidence: only the re-observed
// inventory is evaluated, under the same predicate.
//
// Every request here carries the management key, so the pinned gateway PID is
// proven to own the loopback listener immediately before each of them, and
// once more after the last response.
func observeClaudeBindingsReport(m Manifest, key string) (bindingVerdict, error) {
	var none bindingVerdict
	if err := validateBindingExpectation(m); err != nil {
		return none, err
	}
	id, err := uuid.Parse(m.Runtime.ProcessInstanceID)
	if err != nil || id.String() != m.Runtime.ProcessInstanceID || m.Runtime.ProjectionVersion != RuntimeProjection || !validDigest(m.Runtime.ManagedAdmissionSHA256) || !validDigest(m.Runtime.ExecutionConfigSHA256) {
		return none, Held("binding_runtime_expectation_invalid")
	}
	u, err := url.Parse(m.GatewayURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return none, Held("binding_observation_route_unsupported")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port <= 0 || port > 65535 {
		return none, Held("binding_observation_route_unsupported")
	}
	proveListener := func() error { return inspectListener(m.Gateway.PID, ip, port) }
	if err := proveListener(); err != nil {
		return none, err
	}
	keyName := m.Runtime.ManagementKeyEnv
	if keyName == "" || strings.IndexFunc(keyName, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') }) >= 0 {
		return none, Held("binding_management_key_unavailable")
	}
	if key == "" {
		return none, Held("binding_management_key_unavailable")
	}
	// The proof above precedes the first keyed GET; key validation in between
	// is local.
	verdict, err := fetchClaudeBindings(m, *u, key)
	if err != nil && verdict.unverifiedActiveOAuth && holdCode(err) == "binding_credential_unverified" && claudeIdentityVerification.allow(bindingCredentialSetKey(m), time.Now()) {
		// The verify POST and the second GET both carry the key: the listener
		// may have changed hands during either earlier exchange.
		if listenerErr := proveListener(); listenerErr != nil {
			return none, listenerErr
		}
		requestClaudeIdentityVerification(*u, key)
		if listenerErr := proveListener(); listenerErr != nil {
			return none, listenerErr
		}
		verdict, err = fetchClaudeBindings(m, *u, key)
	}
	if err != nil {
		return verdict, err
	}
	return verdict, proveListener()
}

func fetchClaudeBindings(m Manifest, u url.URL, key string) (bindingVerdict, error) {
	var none bindingVerdict
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return none, Held("binding_nonce_unavailable")
	}
	nonce := hex.EncodeToString(random[:])
	u.Path, u.RawQuery = "/v8/management/observability/admission/claude-bindings", "request_nonce="+nonce
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return none, Held("binding_observation_unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	response, err := client.Do(req)
	if err != nil {
		return none, Held("binding_observation_unavailable")
	}
	defer response.Body.Close()
	noStore := false
	for _, directive := range strings.Split(strings.ToLower(response.Header.Get("Cache-Control")), ",") {
		noStore = noStore || strings.TrimSpace(directive) == "no-store"
	}
	if response.StatusCode != http.StatusOK || !noStore {
		return none, Held("binding_observation_rejected")
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || len(b) > 256<<10 {
		return none, Held("binding_observation_invalid")
	}
	var receipt claudeBindingReceipt
	if DecodeStrict(b, &receipt) != nil {
		return none, Held("binding_observation_invalid")
	}
	if receipt.SchemaVersion != 1 || receipt.ProjectionVersion != ClaudeBindingProjection || receipt.ObservationScope != "credential_selection_only" || receipt.RequestNonce != nonce || receipt.ProcessInstanceID != m.Runtime.ProcessInstanceID || receipt.ObservedAt.Before(start.Add(-time.Second)) || receipt.ObservedAt.After(time.Now().Add(time.Second)) {
		return none, Held("binding_observation_binding_mismatch")
	}
	return evaluateClaudeBindingInventory(m, receipt)
}

// claudeIdentityVerifyInterval bounds explicit verification requests per
// gateway instance and credential set, shared by every role in this process.
var claudeIdentityVerifyInterval = 60 * time.Second

type identityVerificationLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var claudeIdentityVerification = &identityVerificationLimiter{}

func (l *identityVerificationLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	for k, at := range l.last {
		if now.Sub(at) >= claudeIdentityVerifyInterval {
			delete(l.last, k)
		}
	}
	if at, ok := l.last[key]; ok && now.Sub(at) < claudeIdentityVerifyInterval {
		return false
	}
	l.last[key] = now
	return true
}

func bindingCredentialSetKey(m Manifest) string {
	refs := make([]string, 0, len(m.Bindings.Credentials))
	for _, credential := range m.Bindings.Credentials {
		refs = append(refs, credential.AuthRef)
	}
	sort.Strings(refs)
	return digest([]byte(m.Runtime.ProcessInstanceID + "\x00" + strings.Join(refs, "\x00")))
}

// requestClaudeIdentityVerification calls the gateway's exempt control-plane
// verifier (no body, no query, bounded official profile lookups, no
// inference). The gateway verifies and caches each present credential
// independently and answers 503 while any configured one stays unverified, so
// the status is deliberately ignored here.
func requestClaudeIdentityVerification(u url.URL, key string) {
	u.Path, u.RawQuery = "/v8/management/admission/claude-identity/verify", ""
	req, err := http.NewRequest(http.MethodPost, u.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
}

func holdCode(err error) string {
	var hold *Hold
	if errors.As(err, &hold) {
		return hold.Code
	}
	return ""
}

// bindingVerdict carries the facts an accepted (or held) inventory leaves for
// the caller. It is never evidence by itself.
type bindingVerdict struct {
	// unverifiedActiveOAuth: the hold is an auth_active OAuth credential
	// without an identity proof for its current token.
	unverifiedActiveOAuth bool
	// unverifiedStandby counts inactive OAuth credentials awaiting an identity
	// proof. The launch predicate accepts them, provisioning does not; see
	// evaluateClaudeBindingInventory and ValidateClaudeBindingReceipt.
	unverifiedStandby int
}

func verifyClaudeBindingInventory(m Manifest, receipt claudeBindingReceipt) error {
	_, err := evaluateClaudeBindingInventory(m, receipt)
	return err
}

// awaitingIdentityProof is the only credential state in which the pinned
// gateway cannot compare a credential with the pin it enforces: an OAuth
// credential whose current token has no identity proof. The gateway caches a
// proof under (auth ID, exact token digest, configured account identity) and
// stores it only when the provider profile matches that configured identity,
// so a reported account_identity_sha256 is always the enforced pin and always
// comes with pin_matches. An exact key is static configuration whose
// comparison needs no proof, so it never awaits one, standby or not.
func awaitingIdentityProof(want BindingCredentialPin, got claudeBindingCredential) bool {
	return want.BindingMode == OAuthAccountBindingMode && !got.PinMatches && got.AccountIdentitySHA256 == "" && got.VerifiedGeneration == 0 &&
		(got.CredentialSHA256 == "" || validDigest(got.CredentialSHA256))
}

// credentialAbsent is the projection of a configured credential slot with no
// loaded auth at all: the configured reference, alias and route keys only, no
// kind, token, proof, auth generation or registration, no registered model.
func credentialAbsent(got claudeBindingCredential) bool {
	if got.CredentialKind != "" || got.CredentialSHA256 != "" || got.BindingMode != "" || got.AccountIdentitySHA256 != "" || got.VerifiedGeneration != 0 ||
		got.PinMatches || got.AuthActive || got.AuthGeneration != 0 || got.AuthRegistrationEpoch != 0 {
		return false
	}
	for _, model := range got.Models {
		if model.Registered || model.ResolvedModel != "" || model.ModelSHA256 != "" {
			return false
		}
	}
	return true
}

func bindingModelKey(routeID, model string) string { return routeID + "\x00" + model }

func verifyBindingModels(want BindingCredentialPin, got claudeBindingCredential) error {
	models := map[string]BindingModelPin{}
	for _, model := range want.Models {
		models[bindingModelKey(model.RouteID, model.Model)] = model
	}
	if len(got.Models) != len(models) {
		return Held("binding_model_set_mismatch")
	}
	for _, observed := range got.Models {
		key := bindingModelKey(observed.RouteID, observed.Model)
		approved, ok := models[key]
		if !ok || !observed.Registered || observed.ResolvedModel != observed.Model || observed.ModelSHA256 != approved.ModelSHA256 {
			return Held("binding_model_mismatch")
		}
		delete(models, key)
	}
	return nil
}

// evaluateClaudeBindingInventory admits a lane whose whole configured
// credential set is the pinned one and in which every pinned (route, model)
// key is served by at least one credential that is selectable now
// (auth_active) and whose identity proof matches its pin. A credential that is
// not selectable (quota block, expired token, cooldown) may stay as a standby
// with the approved model definitions, so it can never re-enter selection with
// an unreviewed model. A selectable credential without a proof holds. The
// gateway additionally verifies the identity of every managed physical
// attempt against its enforced pin before any upstream byte, so a standby
// that returns to selection cannot be used unverified.
//
// Pin equality. Whenever the gateway can compare a credential with the pin it
// enforces, it must report the match (pin_matches) and the observed fields
// must equal the manifest pin: every exact key and every OAuth credential that
// carries an identity proof, active or not. Only an OAuth credential awaiting
// a proof for its current token cannot be compared, because the projection
// exposes the enforced account identity only through a proof. For that
// standby, manifest pin == enforced pin rests on two facts instead. The
// enforced pins belong to the immutable startup admission snapshot of the
// manifest's exact gateway process instance; that snapshot's canonical digest
// is managed_admission_sha256, which must equal the manifest's, with the
// configuration apply complete and without drift. And ValidateClaudeBindingReceipt,
// the provisioning contract, accepts a manifest only against a receipt of
// that instance and digest in which every credential proved its pin. A pin
// proved once cannot change while the instance and digest stay the same, and
// a change of either holds every launch (binding_observation_binding_mismatch,
// binding_config_drift).
//
// Known limits of the projection, both availability-only: auth_active is one
// conjunction over all of a credential's pinned routes while the gateway blocks
// per model, so an unverified standby blocked on some pinned routes only is
// still selectable on the others; and a standby whose current token failed
// verification looks the same as one whose token expired. In both cases the
// gateway holds the attempts it would route there (managed_admission_hold)
// instead of sending them. An absent pinned credential (no loaded auth) is
// not tolerated: what it would register is unknown (binding_credential_absent).
func evaluateClaudeBindingInventory(m Manifest, receipt claudeBindingReceipt) (bindingVerdict, error) {
	var v bindingVerdict
	if !receipt.ConfigApplyComplete || receipt.ManagedAdmissionSHA256 != m.Runtime.ManagedAdmissionSHA256 || receipt.ExecutionConfigSHA256 != m.Runtime.ExecutionConfigSHA256 {
		return v, Held("binding_config_drift")
	}
	i := receipt.Inventory
	// selection_pins_match is cross-checked below instead of required here.
	if !i.SnapshotComplete || !i.BuiltinExecutor || !i.ExecutorConfigMatches || !i.ManagerConfigMatches || !i.NativeTransport || i.RegistryGeneration == 0 || i.UnconfiguredClaudeCredentials < 0 {
		return v, Held("binding_inventory_incomplete")
	}
	expected := map[string]BindingCredentialPin{}
	for _, credential := range m.Bindings.Credentials {
		expected[credential.AuthRef] = credential
	}
	if len(i.Credentials) != len(expected) {
		return v, Held("binding_credential_set_mismatch")
	}
	// Slot pins, pin equality and model definitions hold for every
	// credential, standby included.
	pins := make([]BindingCredentialPin, len(i.Credentials))
	verified := make([]bool, len(i.Credentials))
	for k, got := range i.Credentials {
		want, ok := expected[got.AuthRef]
		if ok && got.AccountAlias == want.AccountAlias && credentialAbsent(got) {
			return v, Held("binding_credential_absent")
		}
		if !ok || got.AccountAlias != want.AccountAlias || got.CredentialKind != want.CredentialKind || got.BindingMode != want.BindingMode ||
			!validCredentialExpectation(want) || got.AuthGeneration == 0 || got.AuthRegistrationEpoch == 0 || got.ModelRegistrationEpoch == 0 {
			return v, Held("binding_credential_mismatch")
		}
		delete(expected, got.AuthRef) // a duplicate auth_ref finds no pin
		pins[k] = want
		// A comparable credential must match, and a claimed match must agree
		// with the observed fields.
		verified[k] = got.PinMatches && credentialObservationMatches(want, got)
		if !verified[k] && !awaitingIdentityProof(want, got) {
			return v, Held("binding_credential_mismatch")
		}
		if err := verifyBindingModels(want, got); err != nil {
			return v, err
		}
	}
	for k, got := range i.Credentials {
		if got.AuthActive && !verified[k] {
			v.unverifiedActiveOAuth = true // only an OAuth credential can await a proof
			return v, Held("binding_credential_unverified")
		}
	}
	served := map[string]bool{}
	allServing := true
	for k, got := range i.Credentials {
		serving := got.AuthActive && verified[k]
		allServing = allServing && serving
		if !verified[k] {
			v.unverifiedStandby++
		}
		if serving {
			for _, model := range pins[k].Models {
				served[bindingModelKey(model.RouteID, model.Model)] = true
			}
		}
	}
	// For the pinned gateway build, once every other conjunct was checked
	// above, selection_pins_match is exactly "every credential serves".
	if i.SelectionPinsMatch && !allServing {
		return v, Held("binding_observation_invalid")
	}
	if !i.SelectionPinsMatch && allServing {
		return v, Held("binding_inventory_incomplete")
	}
	for _, credential := range m.Bindings.Credentials {
		for _, model := range credential.Models {
			if !served[bindingModelKey(model.RouteID, model.Model)] {
				return v, Held("binding_route_unserved")
			}
		}
	}
	return v, nil
}

// ValidateClaudeBindingReceipt is the provisioning contract for a manifest's
// binding pins. It applies the launch predicate and additionally requires
// every pinned credential, standby included, to prove its pin in this
// receipt: a manifest must not be installed with an account identity that was
// never compared with the pin the gateway enforces under the manifest's
// process instance and managed admission digest. The launch predicate relies
// on that comparison for a standby awaiting a proof (see
// evaluateClaudeBindingInventory). The caller must independently verify
// freshness, nonce, authenticated transport and PID ownership of the observed
// endpoint.
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
	v, err := evaluateClaudeBindingInventory(m, receipt)
	if err != nil {
		return err
	}
	if v.unverifiedStandby > 0 {
		return Held("binding_credential_unverified")
	}
	return nil
}
