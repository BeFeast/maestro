// Package aiexecution fences managed AI leaves. Source fixtures, native-session
// registration and configured gateway URLs do not establish installed coverage.
package aiexecution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/google/uuid"
)

type Policy struct {
	RequireVerifiedRoute  bool   `yaml:"require_verified_route" json:"require_verified_route"`
	ManifestPath          string `yaml:"manifest_path,omitempty" json:"manifest_path,omitempty"`
	ManifestSHA256        string `yaml:"manifest_sha256,omitempty" json:"manifest_sha256,omitempty"`
	revision              *Revision
	generation            uint64
	controllerPin         *FileProof
	controllerLease       *ControllerLease
	controllerUnavailable bool
}

func (p Policy) Validate() error {
	if p.ManifestPath == "" && p.ManifestSHA256 == "" {
		return nil // Explicit strict mode without proof is a runtime hold.
	}
	if !filepath.IsAbs(p.ManifestPath) || !validDigest(p.ManifestSHA256) {
		return fmt.Errorf("ai_execution requires an absolute manifest_path and lowercase SHA-256 manifest_sha256 together")
	}
	return nil
}

type Hold struct{ Code string }

func (h *Hold) Error() string { return "AI execution held: " + h.Code }
func Held(code string) error  { return &Hold{Code: code} }

// AuxiliaryLimiter is supplied only by the existing supported controller. A
// standalone caller cannot silently create a second auxiliary capacity owner.
type AuxiliaryLimiter interface {
	ReserveAuxiliary(stateDir, roleRunID string) (release func(), err error)
}

type LaunchSpec struct {
	RuntimeKey            string
	ProjectID             string
	Role                  string
	RoleRunID             string
	Model                 string
	GatewayScope          string
	ExpectedPolicyVersion int64
	Registration          *admissioncontrol.Acknowledgement
	ProjectConfigSHA256   string
}

type FileProof struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type ProcessProof struct {
	PID           int       `json:"pid"`
	UID           uint32    `json:"uid"`
	BootID        string    `json:"boot_id"`
	StartTicks    string    `json:"start_ticks"`
	CmdlineSHA256 string    `json:"cmdline_sha256"`
	Binary        FileProof `json:"binary"`
}

// Manifest is a reviewed expectation, not self-authenticating capability. The
// hash is pinned outside it, and every observable field is rechecked at launch.
// Evidence references are explicit digests, never booleans meaning "ready".
type Manifest struct {
	Version                 int                  `json:"version"`
	EvidenceKind            string               `json:"evidence_kind"`
	ExpiresAt               time.Time            `json:"expires_at"`
	ProjectID               string               `json:"project_id"`
	ProjectConfigSHA256     string               `json:"project_config_sha256"`
	FleetID                 string               `json:"fleet_id"`
	PolicyVersion           int64                `json:"policy_version"`
	Routes                  map[string]RoleRoute `json:"routes"`
	GatewayURL              string               `json:"gateway_url"`
	Maestro                 FileProof            `json:"maestro"`
	Harness                 FileProof            `json:"harness"`
	Gateway                 ProcessProof         `json:"gateway"`
	Runtime                 RuntimeExpectation   `json:"runtime"`
	Bindings                BindingExpectation   `json:"bindings"`
	GatewayConfig           FileProof            `json:"gateway_config"`
	AuthorityPolicy         FileProof            `json:"authority_policy"`
	ModelAccountPolicy      FileProof            `json:"model_account_policy"`
	EgressPolicy            FileProof            `json:"egress_policy"`
	NativeWireReceipt       FileProof            `json:"native_wire_receipt"`
	FullAPIAdmissionReceipt FileProof            `json:"full_api_admission_receipt"`
	LegacyCIContainment     FileProof            `json:"legacy_ci_containment"`
	Containment             map[string]FileProof `json:"containment"`
}

// Each role keeps its exact requested model and authenticated budget route.
// This package covers one installed gateway instance; no cross-role model or
// principal substitution is implicit.
type RoleRoute struct {
	AdmissionBasis         string `json:"admission_basis,omitempty"`
	Model                  string `json:"model"`
	GatewayScope           string `json:"gateway_scope"`
	BudgetRunID            string `json:"budget_run_id"`
	ManagedPrincipalSHA256 string `json:"managed_principal_sha256"`
	CallerScopeHash        string `json:"caller_scope_hash"`
}

// ContainmentProfilePin selects the reviewed finite profile for one worker
// slot or native auxiliary role. Inspect remains required before preparation.
func ContainmentProfilePin(policy Policy, key, role string) (FileProof, error) {
	b, err := os.ReadFile(policy.ManifestPath)
	if err != nil || len(b) > 128<<10 || digest(b) != policy.ManifestSHA256 {
		return FileProof{}, Held("manifest_drift")
	}
	var m Manifest
	if DecodeStrict(b, &m) != nil {
		return FileProof{}, Held("manifest_invalid")
	}
	return selectContainmentProfile(m, key, role)
}

func selectContainmentProfile(m Manifest, key, role string) (FileProof, error) {
	if pin, ok := m.Containment[key]; ok {
		return pin, nil
	}
	// The explicit worker class serializes successive dynamic worker slots in
	// the pilot's single namespace. Its ProjectID is checked at every launch;
	// UUID, unit, registration and generation ownership remain exact.
	workerRole := role == "worker" || role == "planner" || role == "advisor" || role == "implementer" || role == "validator" || role == "repair"
	i := strings.LastIndexByte(key, '-')
	slot := false
	if i > 0 {
		number, err := strconv.ParseUint(key[i+1:], 10, 64)
		slot = err == nil && number > 0
	}
	if workerRole && slot {
		if pin, ok := m.Containment["worker"]; ok {
			return pin, nil
		}
	}
	return FileProof{}, Held("containment_profile_missing")
}

func GatewayProcessFromPolicy(policy Policy) (ProcessProof, error) {
	b, err := os.ReadFile(policy.ManifestPath)
	if err != nil || len(b) > 128<<10 || digest(b) != policy.ManifestSHA256 {
		return ProcessProof{}, Held("manifest_drift")
	}
	var m Manifest
	if DecodeStrict(b, &m) != nil {
		return ProcessProof{}, Held("manifest_invalid")
	}
	return m.Gateway, nil
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}
func VerifyFile(p FileProof) error {
	if !filepath.IsAbs(p.Path) || !validDigest(p.SHA256) {
		return Held("proof_file_invalid")
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return Held("proof_file_unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Held("proof_file_invalid")
	}
	h := sha256.New()
	const maxProofFile = 1 << 30
	if info.Size() > maxProofFile {
		return Held("proof_file_invalid")
	}
	if n, err := io.Copy(h, io.LimitReader(f, maxProofFile+1)); err != nil || n > maxProofFile {
		return Held("proof_file_unavailable")
	}
	if hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
		return Held("proof_file_drift")
	}
	return nil
}

// Inspect rechecks local pins and the gateway's live configuration observation
// at every leaf. Matching config-only receipts cannot prove active account
// routing or kernel containment; those still require their real observers.
func Inspect(policy Policy, spec LaunchSpec, cmd *exec.Cmd) error {
	return inspectWithObservationKey(policy, spec, cmd, nil)
}

// InspectWithObservationKey keeps host observation credentials out of both the
// process-global environment and the native command environment.
func InspectWithObservationKey(policy Policy, spec LaunchSpec, cmd *exec.Cmd, key string) error {
	return inspectWithObservationKey(policy, spec, cmd, &key)
}

func inspectWithObservationKey(policy Policy, spec LaunchSpec, cmd *exec.Cmd, observationKey *string) error {
	if err := policy.CheckCurrent(); err != nil {
		return err
	}
	if !policy.RequireVerifiedRoute {
		return nil
	}
	if !filepath.IsAbs(policy.ManifestPath) || !validDigest(policy.ManifestSHA256) {
		return Held("manifest_pin_required")
	}
	f, err := os.Open(policy.ManifestPath)
	if err != nil {
		return Held("manifest_unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return Held("manifest_unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	if err != nil || len(b) > 128<<10 {
		return Held("manifest_unavailable")
	}
	if digest(b) != policy.ManifestSHA256 {
		return Held("manifest_drift")
	}
	var m Manifest
	if DecodeStrict(b, &m) != nil || m.Version != 1 {
		return Held("manifest_invalid")
	}
	stripHostObservationEnvironment(cmd, m.Runtime.ManagementKeyEnv)
	if m.EvidenceKind != "installed" {
		return Held("source_evidence_not_installed")
	}
	if m.ExpiresAt.IsZero() || !time.Now().Before(m.ExpiresAt) {
		return Held("proof_expired")
	}
	route, roleOK := m.Routes[spec.Role]
	if !roleOK {
		return Held("role_unsupported")
	}
	if spec.ProjectID == "" || spec.ProjectID != m.ProjectID || spec.GatewayScope == "" || spec.GatewayScope != route.GatewayScope || spec.Model == "" || spec.Model != route.Model || !validDigest(spec.ProjectConfigSHA256) || spec.ProjectConfigSHA256 != m.ProjectConfigSHA256 {
		return Held("route_binding_mismatch")
	}
	if id, err := uuid.Parse(spec.RoleRunID); err != nil || id.String() != spec.RoleRunID {
		return Held("role_run_invalid")
	}
	ack := spec.Registration
	if ack == nil || ack.Revoked || ack.RegistrationVersion <= 0 || !admissioncontrol.ValidAdmissionBasis(route.AdmissionBasis) || ack.Binding.AdmissionBasis != route.AdmissionBasis || spec.ExpectedPolicyVersion != m.PolicyVersion || ack.Binding.ProjectID != m.ProjectID || ack.Binding.FleetID != m.FleetID || ack.Binding.RunID != route.BudgetRunID || ack.Binding.GatewayScope != route.GatewayScope || ack.Binding.Role != spec.Role || ack.Binding.ExpiresAt <= time.Now().Unix() {
		return Held("registration_binding_mismatch")
	}
	if id, err := uuid.Parse(ack.Binding.NativeSessionID); err != nil || id.String() != ack.Binding.NativeSessionID {
		return Held("native_session_invalid")
	}
	if cmd == nil || filepath.Base(cmd.Path) != "claude" {
		return Held("native_harness_unsupported")
	}
	if err := VerifyFile(m.Harness); err != nil {
		return err
	}
	got, err := filepath.EvalSymlinks(cmd.Path)
	if err != nil {
		return Held("harness_unobservable")
	}
	want, err := filepath.EvalSymlinks(m.Harness.Path)
	if err != nil || got != want {
		return Held("harness_binding_mismatch")
	}
	modelCount, sessionCount := 0, 0
	for i := 1; i < len(cmd.Args); i++ {
		name, value, equals := strings.Cut(cmd.Args[i], "=")
		switch name {
		case "--resume", "--continue", "--fork-session", "--no-session-persistence", "--", "-m":
			return Held("native_arguments_invalid")
		}
		if strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "--") && strings.ContainsAny(name[1:], "rc") {
			return Held("native_arguments_invalid")
		}
		if name != "--model" && name != "--session-id" {
			continue
		}
		if !equals {
			if i+1 >= len(cmd.Args) {
				return Held("native_arguments_invalid")
			}
			i++
			value = cmd.Args[i]
		}
		if name == "--model" {
			modelCount++
			if value != route.Model {
				return Held("model_argument_mismatch")
			}
		} else {
			sessionCount++
			if value != ack.Binding.NativeSessionID {
				return Held("native_session_mismatch")
			}
		}
	}
	if modelCount != 1 || sessionCount != 1 {
		return Held("native_arguments_unproven")
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	values := map[string]string{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			if _, exists := values[k]; exists {
				return Held("environment_ambiguous")
			}
			values[k] = v
		}
	}
	if values["ANTHROPIC_BASE_URL"] != m.GatewayURL || !validDigest(route.ManagedPrincipalSHA256) || digest([]byte(values["ANTHROPIC_AUTH_TOKEN"])) != route.ManagedPrincipalSHA256 || values["ANTHROPIC_AUTH_TOKEN"] == "" {
		return Held("managed_gateway_environment_mismatch")
	}
	for _, p := range []FileProof{m.Maestro, m.Gateway.Binary, m.GatewayConfig, m.AuthorityPolicy, m.ModelAccountPolicy, m.EgressPolicy, m.NativeWireReceipt, m.FullAPIAdmissionReceipt, m.LegacyCIContainment} {
		if err := VerifyFile(p); err != nil {
			return err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return Held("maestro_binary_unobservable")
	}
	actual, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return Held("maestro_binary_unobservable")
	}
	expected, err := filepath.EvalSymlinks(m.Maestro.Path)
	if err != nil || actual != expected {
		return Held("maestro_binary_mismatch")
	}
	if err := VerifyFile(FileProof{Path: "/proc/self/exe", SHA256: m.Maestro.SHA256}); err != nil {
		return Held("maestro_loaded_binary_mismatch")
	}
	if err := inspectProcess(m.Gateway); err != nil {
		return err
	}
	observerKey := os.Getenv(m.Runtime.ManagementKeyEnv)
	if observationKey != nil {
		observerKey = *observationKey
	}
	if err := observeRuntimeWithKey(m, route.CallerScopeHash, observerKey); err != nil {
		return err
	}
	if err := observeClaudeBindingsWithKey(m, observerKey); err != nil {
		return err
	}
	if err := inspectProcess(m.Gateway); err != nil {
		return err
	}
	key := spec.RuntimeKey
	if key == "" {
		key = spec.Role
	}
	pin, err := selectContainmentProfile(m, key, spec.Role)
	if err != nil {
		return err
	}
	profile, err := inspectContainmentProfile(pin, spec.ProjectID, m.Gateway)
	if err != nil {
		return err
	}
	if profile.Harness != m.Harness || profile.Maestro != m.Maestro {
		return Held("containment_manifest_binary_mismatch")
	}
	// The caller must now use PrepareContainedNativeCommand; readiness itself
	// does not attest to a process that has not yet entered its OS lease.
	return nil
}

func stripHostObservationEnvironment(cmd *exec.Cmd, name string) {
	if cmd == nil || name == "" {
		return
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, name+"=") {
			clean = append(clean, entry)
		}
	}
	cmd.Env = clean
}

func inspectProcess(p ProcessProof) error {
	if p.PID <= 0 || p.StartTicks == "" || p.BootID == "" || !validDigest(p.CmdlineSHA256) {
		return Held("gateway_instance_invalid")
	}
	root := "/proc/" + strconv.Itoa(p.PID)
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) != p.BootID {
		return Held("gateway_instance_drift")
	}
	stat, err := os.ReadFile(root + "/stat")
	if err != nil {
		return Held("gateway_instance_unobservable")
	}
	cut := bytes.LastIndexByte(stat, ')')
	if cut < 0 {
		return Held("gateway_instance_unobservable")
	}
	fields := strings.Fields(string(stat[cut+1:]))
	if len(fields) < 20 || fields[19] != p.StartTicks {
		return Held("gateway_instance_drift")
	}
	cmdline, err := os.ReadFile(root + "/cmdline")
	if err != nil || digest(cmdline) != p.CmdlineSHA256 {
		return Held("gateway_instance_drift")
	}
	status, err := os.ReadFile(root + "/status")
	if err != nil {
		return Held("gateway_instance_unobservable")
	}
	uid := ""
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fs := strings.Fields(line)
			if len(fs) == 5 && fs[1] == fs[2] && fs[2] == fs[3] && fs[3] == fs[4] {
				uid = fs[1]
			}
		}
	}
	if uid != strconv.FormatUint(uint64(p.UID), 10) {
		return Held("gateway_uid_mismatch")
	}
	expected, err := filepath.EvalSymlinks(p.Binary.Path)
	if err != nil {
		return Held("gateway_binary_unobservable")
	}
	actual, err := os.Readlink(root + "/exe")
	if err != nil || actual != expected {
		return Held("gateway_binary_mismatch")
	}
	if err := VerifyFile(FileProof{root + "/exe", p.Binary.SHA256}); err != nil {
		return err
	}
	return nil
}
