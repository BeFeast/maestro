package aiexecution

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/google/uuid"
)

// NativeContainmentProfile is a finite, installed Linux execution profile.
// The file and tools are owned by root; launch inputs cannot add mounts,
// destinations, credentials, or systemd properties to this profile.
type NativeContainmentProfile struct {
	Version              int                   `json:"version"`
	ProjectID            string                `json:"project_id"`
	UID                  uint32                `json:"uid"`
	GID                  uint32                `json:"gid"`
	Namespace            string                `json:"namespace"`
	NamespaceDev         uint64                `json:"namespace_dev"`
	NamespaceIno         uint64                `json:"namespace_ino"`
	RulesSHA256          string                `json:"rules_sha256"`
	GatewayURL           string                `json:"gateway_url"`
	ForgejoIP            string                `json:"forgejo_ip"`
	ForgejoRepository    string                `json:"forgejo_repository"`
	ForgejoTokenSHA256   string                `json:"forgejo_token_sha256,omitempty"`
	ForgejoCredential    FileProof             `json:"forgejo_credential"`
	ForgejoAuthorization FileProof             `json:"forgejo_authorization"`
	ClaimDir             string                `json:"claim_dir"`
	WorktreeRoot         string                `json:"worktree_root"`
	ScratchRoot          string                `json:"scratch_root"`
	MemoryMaxMB          int                   `json:"memory_max_mb"`
	Maestro              FileProof             `json:"maestro"`
	Harness              FileProof             `json:"harness"`
	Bubblewrap           FileProof             `json:"bubblewrap"`
	SystemdRun           FileProof             `json:"systemd_run"`
	Systemctl            FileProof             `json:"systemctl"`
	Sudo                 FileProof             `json:"sudo"`
	Nsenter              FileProof             `json:"nsenter"`
	Nft                  FileProof             `json:"nft"`
	ReadOnly             []NativeReadOnlyMount `json:"read_only"`
	BuildEnvironment     map[string]string     `json:"build_environment,omitempty"`
}

type NativeReadOnlyMount struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type nativeLaunchEnvelope struct {
	Version         int       `json:"version"`
	Profile         FileProof `json:"profile"`
	ProjectID       string    `json:"project_id"`
	Role            string    `json:"role"`
	NativeSessionID string    `json:"native_session_id"`
	Unit            string    `json:"unit"`
	Worktree        string    `json:"worktree"`
	Scratch         string    `json:"scratch"`
	Arguments       []string  `json:"arguments"`
	Environment     []string  `json:"environment"`
}

type ContainedNativeCommand struct {
	Cmd              *exec.Cmd
	Lease            tmuxsession.ProcessLease
	redactionSecrets []string
}

// RedactionSecrets returns the exact profile-selected snapshot used in the
// stdin envelope. Output filtering must not reread a rotated credential file.
func (c *ContainedNativeCommand) RedactionSecrets() []string {
	return append([]string(nil), c.redactionSecrets...)
}

func NativeAuxiliaryUnit(nativeID string) (string, error) {
	if id, err := uuid.Parse(nativeID); err != nil || id.String() != nativeID {
		return "", Held("native_session_invalid")
	}
	return "maestro-native-" + strings.ReplaceAll(nativeID, "-", "") + ".service", nil
}

func readContainmentProfile(pin FileProof) (NativeContainmentProfile, error) {
	var p NativeContainmentProfile
	b, err := readRootContainmentEvidence(pin)
	if err != nil || len(b) > 128<<10 || digest(b) != pin.SHA256 || DecodeStrict(b, &p) != nil {
		return p, Held("containment_profile_invalid")
	}
	if err := validateContainmentProfile(p); err != nil {
		return p, err
	}
	return p, nil
}

func validateContainmentProfile(p NativeContainmentProfile) error {
	// The root-owned, hash-pinned profile is the authority for the repository;
	// only its shape is checked here, never a compiled-in organization.
	if !validNativeForgejoRepository(p.ForgejoRepository) {
		return Held("containment_forgejo_repository_invalid")
	}
	if p.Version != 1 || p.ProjectID == "" || p.UID == 0 || p.GID == 0 || p.NamespaceIno == 0 || !validDigest(p.RulesSHA256) || p.MemoryMaxMB <= 0 {
		return Held("containment_profile_invalid")
	}
	// Every project/profile for the runner UID shares this namespace-keyed
	// claim store. A caller cannot bypass an occupied lock via a second path.
	if p.ClaimDir != filepath.Join("/var/lib/maestro/native-claims", strconv.FormatUint(uint64(p.UID), 10)) {
		return Held("containment_claim_store_invalid")
	}
	for _, path := range []string{p.Namespace, p.ClaimDir, p.WorktreeRoot, p.ScratchRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return Held("containment_profile_invalid")
		}
	}
	u, err := url.Parse(p.GatewayURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return Held("containment_gateway_invalid")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	forge := net.ParseIP(p.ForgejoIP)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || err != nil || port <= 0 || port > 65535 || forge == nil || forge.To4() == nil || forge.IsUnspecified() || forge.IsMulticast() || forge.IsLoopback() {
		return Held("containment_gateway_invalid")
	}
	seen := map[string]bool{}
	for _, m := range p.ReadOnly {
		if !filepath.IsAbs(m.Source) || filepath.Clean(m.Source) != m.Source || !allowedNativeMountTarget(m.Target) || seen[m.Target] {
			return Held("containment_mount_invalid")
		}
		seen[m.Target] = true
	}
	if !seen["/usr"] || !seen["/etc/hosts"] || !seen["/etc/passwd"] {
		return Held("containment_mounts_incomplete")
	}
	for key, value := range p.BuildEnvironment {
		switch key {
		case "pnpm_config_verify_deps_before_run":
			if value != "error" {
				return Held("containment_build_environment_invalid")
			}
		case "pnpm_config_store_dir":
			if value != "/cache/pnpm" {
				return Held("containment_build_environment_invalid")
			}
		case "NEXT_PUBLIC_CLERK_PUBLISHABLE_KEY":
			if !strings.HasPrefix(value, "pk_test_") || len(value) > 512 {
				return Held("containment_build_environment_invalid")
			}
		default:
			return Held("containment_build_environment_invalid")
		}
	}
	return nil
}

func allowedNativeMountTarget(target string) bool {
	switch target {
	case "/usr", "/lib", "/lib64", "/etc/ssl/certs", "/etc/hosts", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/cache/go-mod", "/cache/pnpm", "/cache/npm", "/cache/bun", "/home/native/.cache", "/usr/local/go", "/usr/local/bin/bun", "/usr/local/bin/node", "/usr/local/bin/pnpm", "/opt/node", "/opt/pnpm":
		return true
	}
	return false
}

// PrepareContainedNativeCommand runs outside the network/mount sandbox. It
// observes the host listener and actual kernel policy before systemd launches
// the same owned service into the namespace. No secret is added to argv/disk.
func PrepareContainedNativeCommand(pin FileProof, projectID, role, nativeID string, gateway ProcessProof, original *exec.Cmd, lease tmuxsession.ProcessLease) (*ContainedNativeCommand, error) {
	p, err := inspectContainmentProfile(pin, projectID, gateway)
	if err != nil {
		return nil, err
	}
	if p.ProjectID != projectID || p.UID != uint32(os.Getuid()) || original == nil || uuid.Validate(nativeID) != nil || lease.Manager != tmuxsession.ProcessLeaseManagerSystem || !strings.HasSuffix(lease.Unit, ".service") {
		return nil, Held("containment_launch_binding_invalid")
	}
	forgejoToken := ""
	if role != "supervisor" && role != "reviewer" {
		if err := verifyNativeForgejoAuthorization(p); err != nil {
			return nil, err
		}
		forgejoToken, err = readNativeForgejoCredential(p)
		if err != nil {
			return nil, err
		}
	}
	worktree := original.Dir
	if role == "supervisor" || role == "reviewer" {
		worktree = filepath.Join(p.WorktreeRoot, nativeID)
		if err := os.Mkdir(worktree, 0700); err != nil {
			return nil, Held("containment_workspace_identity_in_use")
		}
	}
	if worktree == "" {
		worktree, err = os.Getwd()
		if err != nil {
			return nil, Held("containment_worktree_unavailable")
		}
	}
	if !containedPath(p.WorktreeRoot, worktree) || verifyOwnedPath(worktree, p.UID, true) != nil {
		return nil, Held("containment_worktree_unsafe")
	}
	// Full clones keep all Git metadata within the mounted tree. Linked
	// worktrees and object alternates can otherwise require host paths.
	if st, err := os.Lstat(filepath.Join(worktree, ".git")); err == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
		return nil, Held("containment_git_metadata_external")
	}
	if role != "supervisor" && role != "reviewer" {
		if err := verifyNativeGitGuards(worktree, p.UID); err != nil {
			return nil, err
		}
	}
	scratch := filepath.Join(p.ScratchRoot, nativeID)
	if err := os.Mkdir(scratch, 0700); err != nil {
		// A reused native ID must reconcile its existing OS/authority outcome.
		return nil, Held("containment_scratch_identity_in_use")
	}
	for _, dir := range []string{"tmp", "tmp/go", "go-build", "cache"} {
		if err := os.MkdirAll(filepath.Join(scratch, dir), 0700); err != nil {
			return nil, Held("containment_scratch_unavailable")
		}
	}
	args, err := containedNativeArguments(original.Args, nativeID, role)
	if err != nil {
		return nil, err
	}
	// The public child endpoint is separately observed as belonging to the
	// same gateway PID; the host management observer remains loopback-only.
	childInput := make([]string, 0, len(original.Env)+1)
	for _, entry := range original.Env {
		if strings.HasPrefix(entry, "FORGEJO_TOKEN=") || strings.HasPrefix(entry, "MAESTRO_FORGEJO_REPOSITORY_TOKEN=") {
			continue
		}
		childInput = append(childInput, entry)
	}
	if forgejoToken != "" {
		childInput = append(childInput, "FORGEJO_TOKEN="+forgejoToken)
	}
	for i, entry := range childInput {
		if strings.HasPrefix(entry, "ANTHROPIC_BASE_URL=") {
			childInput[i] = "ANTHROPIC_BASE_URL=" + p.GatewayURL
		}
	}
	env, err := containedNativeEnvironment(p, childInput, role)
	if err != nil {
		return nil, err
	}
	envelope := nativeLaunchEnvelope{Version: 1, Profile: pin, ProjectID: projectID, Role: role, NativeSessionID: nativeID, Unit: lease.Unit, Worktree: worktree, Scratch: scratch, Arguments: args, Environment: env}
	frame, err := encodeNativeEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	serviceArgs, err := tmuxsession.NativeProcessServiceArgs(lease, int(p.UID), int(p.GID), p.Namespace, p.MemoryMaxMB, []string{p.Maestro.Path, "_native-monitor"})
	if err != nil {
		return nil, Held("containment_process_lease_invalid")
	}
	cmd := exec.Command(p.Sudo.Path, append([]string{"-n", p.SystemdRun.Path}, serviceArgs...)...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/bin", "LANG=C.UTF-8"}
	input := original.Stdin
	if input == nil {
		input = strings.NewReader("")
	}
	cmd.Stdin = io.MultiReader(bytes.NewReader(frame), input)
	contained := &ContainedNativeCommand{Cmd: cmd, Lease: lease}
	if forgejoToken != "" {
		contained.redactionSecrets = []string{forgejoToken}
	}
	return contained, nil
}

// nativeForgejoAuthorizationEvidence is the root-owned, hash-pinned
// attestation the reviewed R9 provisioner writes after server-side negative
// probes. The credential hash by itself proves no repository scope,
// main-branch protection, or merge prohibition; these fields do.
type nativeForgejoAuthorizationEvidence struct {
	Version                int       `json:"version"`
	Repository             string    `json:"repository"`
	CredentialSHA256       string    `json:"credential_sha256"`
	WorkerLogin            string    `json:"worker_login"`
	ObservedAt             time.Time `json:"observed_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	Admin                  bool      `json:"admin"`
	RepositoryOnly         bool      `json:"repository_only"`
	MainPushDenied         bool      `json:"main_push_denied"`
	MergeDenied            bool      `json:"merge_denied"`
	PolicyFilesWriteDenied bool      `json:"policy_files_write_denied"`
	ServerProbe            FileProof `json:"server_probe"`
}

// readNativeForgejoAuthorization reads and strictly decodes the evidence the
// profile pins. It verifies the file (root ownership, pinned digest, exact
// schema), not the claims — verifyNativeForgejoAuthorization owns those.
func readNativeForgejoAuthorization(p NativeContainmentProfile) (nativeForgejoAuthorizationEvidence, error) {
	var e nativeForgejoAuthorizationEvidence
	b, err := readRootContainmentEvidence(p.ForgejoAuthorization)
	if err != nil || DecodeStrict(b, &e) != nil {
		return nativeForgejoAuthorizationEvidence{}, Held("containment_forgejo_authorization_unverified")
	}
	return e, nil
}

func verifyNativeForgejoAuthorization(p NativeContainmentProfile) error {
	e, err := readNativeForgejoAuthorization(p)
	if err != nil || e.Version != 1 || e.Repository != p.ForgejoRepository || e.CredentialSHA256 != p.ForgejoTokenSHA256 || e.WorkerLogin == "" || e.Admin || !e.RepositoryOnly || !e.MainPushDenied || !e.MergeDenied || !e.PolicyFilesWriteDenied || e.ObservedAt.IsZero() || e.ObservedAt.After(time.Now()) || !time.Now().Before(e.ExpiresAt) {
		return Held("containment_forgejo_authorization_unverified")
	}
	if _, err := readRootContainmentEvidence(e.ServerProbe); err != nil {
		return Held("containment_forgejo_authorization_unverified")
	}
	return nil
}

// NativeForgejoAuthorization is the merge-relevant subset of the
// forgejo-authorization evidence bound to one containment profile. It carries
// no secret: CredentialSHA256 is the same domain-separated digest the profile
// pins (NativeForgejoCredentialSHA256), so a caller can tell whether the
// credential it acts with IS the attested worker actor without ever reading
// the worker's token.
type NativeForgejoAuthorization struct {
	ProfileKey       string
	Repository       string
	WorkerLogin      string
	CredentialSHA256 string
	MergeDenied      bool
}

// NativeForgejoAuthorizationReport is the result of reading every bound
// forgejo-authorization the policy manifest pins.
type NativeForgejoAuthorizationReport struct {
	// Authorizations lists the evidence provably bound to its profile's
	// repository and credential.
	Authorizations []NativeForgejoAuthorization
	// Unverified lists the profile keys whose profile or pinned evidence could
	// not be read, is not bound to that profile, or has expired. None of them
	// is ever reported as a denial or an allowance.
	Unverified []string
}

// loadNativeForgejoAuthorizationEvidence reads one pinned containment profile
// and the forgejo-authorization evidence it pins, with the same root-ownership
// and digest checks as the launch path. hasEvidence is false (and err nil)
// for a profile that pins no forgejo-authorization at all. A package variable
// so tests can stand in for the root-owned files; production never reassigns
// it.
var loadNativeForgejoAuthorizationEvidence = func(pin FileProof) (p NativeContainmentProfile, e nativeForgejoAuthorizationEvidence, hasEvidence bool, err error) {
	p, err = readContainmentProfile(pin)
	if err != nil {
		return p, e, false, err
	}
	if strings.TrimSpace(p.ForgejoAuthorization.Path) == "" {
		return p, e, false, nil
	}
	e, err = readNativeForgejoAuthorization(p)
	return p, e, err == nil, err
}

// NativeForgejoAuthorizations reports the forgejo-authorization evidence bound
// to every containment profile the policy manifest pins (#1247). The
// orchestrator consults it before any merge call: when its own forge
// credential hashes to an attested worker actor with merge_denied=true, the
// forge will answer 405 for every head, so the merge must not be attempted.
//
// This is a read of what is provably bound, not an admission check: a profile
// whose root-owned evidence cannot be verified, whose evidence is not bound to
// that profile's repository and credential, or whose attestation has expired
// is reported as Unverified rather than guessed. Launch verification
// (verifyNativeForgejoAuthorization) stays the strict gate. A manifest that is
// missing, oversized, or drifted from its pin is an error, because then
// nothing at all is bound.
func NativeForgejoAuthorizations(policy Policy) (NativeForgejoAuthorizationReport, error) {
	return nativeForgejoAuthorizationsAt(policy, time.Now())
}

func nativeForgejoAuthorizationsAt(policy Policy, now time.Time) (NativeForgejoAuthorizationReport, error) {
	m, err := readPinnedManifest(policy)
	if err != nil {
		return NativeForgejoAuthorizationReport{}, err
	}
	keys := make([]string, 0, len(m.Containment))
	for key := range m.Containment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var report NativeForgejoAuthorizationReport
	for _, key := range keys {
		p, e, hasEvidence, err := loadNativeForgejoAuthorizationEvidence(m.Containment[key])
		if err != nil {
			report.Unverified = append(report.Unverified, key)
			continue
		}
		if !hasEvidence {
			continue
		}
		if e.Version != 1 || e.Repository != p.ForgejoRepository || e.CredentialSHA256 == "" || e.CredentialSHA256 != p.ForgejoTokenSHA256 || !now.Before(e.ExpiresAt) {
			report.Unverified = append(report.Unverified, key)
			continue
		}
		report.Authorizations = append(report.Authorizations, NativeForgejoAuthorization{
			ProfileKey:       key,
			Repository:       e.Repository,
			WorkerLogin:      e.WorkerLogin,
			CredentialSHA256: e.CredentialSHA256,
			MergeDenied:      e.MergeDenied,
		})
	}
	return report, nil
}

// NativeForgejoCredentialSHA256 is the domain-separated digest a containment
// profile pins for its Forgejo token (forgejo_token_sha256) and the
// authorization evidence repeats as credential_sha256. The prefix keeps a
// plain SHA-256 of the token (as a leaked log line or rainbow table would
// compute it) from matching the pin.
func NativeForgejoCredentialSHA256(token string) string {
	return digest([]byte("maestro-native-forgejo:v1\x00" + token))
}

// readPinnedManifest reads the policy manifest exactly as the launch path does:
// bounded size, digest-pinned, strict schema.
func readPinnedManifest(policy Policy) (Manifest, error) {
	b, err := os.ReadFile(policy.ManifestPath)
	if err != nil || len(b) > 128<<10 || digest(b) != policy.ManifestSHA256 {
		return Manifest{}, Held("manifest_drift")
	}
	var m Manifest
	if DecodeStrict(b, &m) != nil {
		return Manifest{}, Held("manifest_invalid")
	}
	return m, nil
}

func inspectContainmentProfile(pin FileProof, projectID string, gateway ProcessProof) (NativeContainmentProfile, error) {
	p, err := readContainmentProfile(pin)
	if err != nil {
		return p, err
	}
	if p.ProjectID != projectID || p.UID != uint32(os.Getuid()) {
		return p, Held("containment_launch_binding_invalid")
	}
	for _, proof := range []FileProof{p.Maestro, p.Harness, p.Bubblewrap, p.SystemdRun, p.Systemctl, p.Sudo, p.Nsenter, p.Nft} {
		if verifyOwnedPath(proof.Path, 0, false) != nil {
			return p, Held("containment_executable_unsafe")
		}
		if err := VerifyFile(proof); err != nil {
			return p, err
		}
	}
	if VerifyFile(FileProof{Path: "/proc/self/exe", SHA256: p.Maestro.SHA256}) != nil {
		return p, Held("containment_launcher_drift")
	}
	if err := observeContainmentNetwork(p); err != nil {
		return p, err
	}
	u, _ := url.Parse(p.GatewayURL)
	port, _ := strconv.Atoi(u.Port())
	if err := inspectListener(gateway.PID, net.ParseIP(u.Hostname()), port); err != nil {
		return p, err
	}
	return p, nil
}

func containedPath(root, path string) bool {
	return filepath.IsAbs(root) && filepath.IsAbs(path) && filepath.Clean(path) == path && strings.HasPrefix(path, filepath.Clean(root)+string(filepath.Separator))
}

func containedNativeArguments(args []string, nativeID, role string) ([]string, error) {
	if len(args) < 2 {
		return nil, Held("containment_arguments_invalid")
	}
	out := []string{"/runtime/claude"}
	model, session := 0, 0
	seen := map[string]bool{}
	tools := "Bash,Read,Edit,Write,Glob,Grep"
	aux := role == "supervisor" || role == "reviewer"
	if aux {
		tools = ""
	} else {
		switch role {
		case "worker", "planner", "advisor", "implementer", "validator", "repair":
		default:
			return nil, Held("containment_role_unsupported")
		}
	}
	for i := 1; i < len(args); i++ {
		name, value, eq := strings.Cut(args[i], "=")
		if seen[name] {
			return nil, Held("containment_arguments_invalid")
		}
		seen[name] = true
		switch name {
		case "--bare":
			if eq {
				return nil, Held("containment_arguments_invalid")
			}
			continue
		case "-p", "--print", "--dangerously-skip-permissions", "--verbose":
			if eq {
				return nil, Held("containment_arguments_invalid")
			}
			if aux && name == "--dangerously-skip-permissions" {
				return nil, Held("containment_arguments_unsupported")
			}
			out = append(out, name)
		case "--model", "--session-id", "--effort", "--output-format", "--max-turns", "--tools", "--permission-mode", "--permission-prompts":
			if !eq {
				i++
				if i >= len(args) {
					return nil, Held("containment_arguments_invalid")
				}
				value = args[i]
			}
			if name == "--permission-mode" || name == "--permission-prompts" {
				if !aux || name == "--permission-mode" && value != "dontAsk" || name == "--permission-prompts" && value != "none" {
					return nil, Held("containment_arguments_unsupported")
				}
				continue
			}
			if name == "--session-id" {
				session++
				if value != nativeID {
					return nil, Held("native_session_mismatch")
				}
			}
			if name == "--model" {
				model++
				if value == "" {
					return nil, Held("containment_arguments_invalid")
				}
			}
			if name == "--tools" {
				if value != tools {
					return nil, Held("containment_tools_unsupported")
				}
				continue
			}
			if name == "--max-turns" && aux {
				if value != "1" {
					return nil, Held("containment_arguments_unsupported")
				}
				continue
			}
			out = append(out, name, value)
		default:
			return nil, Held("containment_arguments_unsupported")
		}
	}
	if model != 1 || session != 1 || seen["-p"] && seen["--print"] {
		return nil, Held("containment_arguments_invalid")
	}
	out = append(out, "--bare", "--tools", tools)
	if aux {
		// Tool-free auxiliary calls deny anything that would prompt. Do not
		// inherit the CLI's auto permission mode or permit a bypass override.
		out = append(out, "--max-turns", "1", "--permission-mode", "dontAsk", "--permission-prompts", "none")
	}
	return out, nil
}

func containedNativeEnvironment(p NativeContainmentProfile, original []string, role string) ([]string, error) {
	values := map[string]string{}
	for _, entry := range original {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, exists := values[k]; exists {
			return nil, Held("environment_ambiguous")
		}
		values[k] = v
	}
	if values["ANTHROPIC_AUTH_TOKEN"] == "" || values["ANTHROPIC_BASE_URL"] != p.GatewayURL {
		return nil, Held("managed_gateway_environment_mismatch")
	}
	out := []string{
		"PATH=/usr/local/go/bin:/opt/node/bin:/opt/pnpm/bin:/usr/local/bin:/usr/bin:/bin", "HOME=/home/native", "USER=native", "LOGNAME=native", "SHELL=/bin/bash", "LANG=C.UTF-8", "TZ=UTC",
		"TMPDIR=/tmp", "TMP=/tmp", "TEMP=/tmp", "GOTMPDIR=/tmp/go", "GOCACHE=/scratch/go-build", "GOMODCACHE=/cache/go-mod", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"XDG_CONFIG_HOME=/home/native/.config", "XDG_CACHE_HOME=/scratch/cache", "XDG_DATA_HOME=/home/native/.local/share", "BUN_INSTALL_CACHE_DIR=/cache/bun",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1",
		"ANTHROPIC_BASE_URL=" + p.GatewayURL, "ANTHROPIC_AUTH_TOKEN=" + values["ANTHROPIC_AUTH_TOKEN"],
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS=32000",
		"npm_config_cache=/cache/npm", "npm_config_offline=true", "npm_config_ignore_scripts=true",
	}
	for key, value := range p.BuildEnvironment {
		out = append(out, key+"="+value)
	}
	if role != "supervisor" && role != "reviewer" {
		token := values["FORGEJO_TOKEN"]
		if !validDigest(p.ForgejoTokenSHA256) || NativeForgejoCredentialSHA256(token) != p.ForgejoTokenSHA256 || token == "" {
			return nil, Held("containment_forgejo_credential_unverified")
		}
		out = append(out, "FORGEJO_TOKEN="+token, "FORGEJO_REPOSITORY="+p.ForgejoRepository,
			"GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1=!/runtime/maestro _native-forgejo-credential",
			"GIT_CONFIG_KEY_2=credential.useHttpPath", "GIT_CONFIG_VALUE_2=true",
			"GIT_AUTHOR_NAME=Maestro", "GIT_AUTHOR_EMAIL=maestro@localhost", "GIT_COMMITTER_NAME=Maestro", "GIT_COMMITTER_EMAIL=maestro@localhost")
	}
	return out, nil
}

func encodeNativeEnvelope(e nativeLaunchEnvelope) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil || len(b) > 64<<10 {
		return nil, Held("containment_envelope_invalid")
	}
	frame := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	return frame, nil
}

func readNativeEnvelope(input io.Reader) (nativeLaunchEnvelope, error) {
	var e nativeLaunchEnvelope
	var prefix [4]byte
	if _, err := io.ReadFull(input, prefix[:]); err != nil {
		return e, Held("containment_envelope_unavailable")
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > 64<<10 {
		return e, Held("containment_envelope_invalid")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(input, b); err != nil || DecodeStrict(b, &e) != nil || e.Version != 1 {
		return e, Held("containment_envelope_invalid")
	}
	return e, nil
}

func (c *ContainedNativeCommand) StopAndVerify() error {
	if err := tmuxsession.TerminateProcessLease(c.Lease); err != nil {
		return Held("containment_unresolved")
	}
	active, err := tmuxsession.ProcessLeaseActive(c.Lease)
	if err != nil || active {
		return Held("containment_unresolved")
	}
	return nil
}

func (p NativeContainmentProfile) String() string {
	return fmt.Sprintf("native profile %s uid=%d netns=%d", p.ProjectID, p.UID, p.NamespaceIno)
}
