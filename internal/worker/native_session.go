package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/gofrs/flock"
	"github.com/google/uuid"
)

// NativeRegistrationHold never contains prompt text, raw argv or authority data.
type NativeRegistrationHold struct {
	Code            string
	LaunchUncertain bool
	Slot            string
}

func (h *NativeRegistrationHold) Error() string { return "worker native registration held: " + h.Code }

func NativeHold(err error) (*NativeRegistrationHold, bool) {
	var hold *NativeRegistrationHold
	ok := errors.As(err, &hold)
	return hold, ok
}

type NativeWorkerReceipt struct {
	SchemaVersion       int                                  `json:"schema_version"`
	ProjectID           string                               `json:"project_id"`
	Slot                string                               `json:"slot"`
	Generation          uint64                               `json:"generation"`
	IssueNumber         int                                  `json:"issue_number"`
	RoleRunID           string                               `json:"role_run_id"`
	ParentRoleRunID     string                               `json:"parent_role_run_id,omitempty"`
	Worktree            string                               `json:"worktree"`
	Branch              string                               `json:"branch"`
	Backend             string                               `json:"backend"`
	ConfigDigest        string                               `json:"config_digest"`
	Status              string                               `json:"status"`
	Request             admissioncontrol.RegistrationRequest `json:"request"`
	Acknowledgement     *admissioncontrol.Acknowledgement    `json:"acknowledgement,omitempty"`
	ProcessLeaseUnit    string                               `json:"process_lease_unit"`
	ProcessLeaseManager string                               `json:"process_lease_manager"`
	LogFile             string                               `json:"log_file,omitempty"`
	PID                 int                                  `json:"pid,omitempty"`
	AccountingReady     bool                                 `json:"accounting_ready"`
	OutcomeIntent       *admissioncontrol.SealRequest        `json:"outcome_intent,omitempty"`
	Outcome             *admissioncontrol.NativeOutcome      `json:"outcome,omitempty"`
}

type nativeWorkerLaunch struct {
	dir     string
	receipt *NativeWorkerReceipt
	unlock  func()
	adopt   bool
}

var registerNativeWorker = func(client admissioncontrol.Client, request admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
	return client.Register(request)
}
var persistNativeWorkerReceipt = writeNativeWorkerReceipt

// A launched CLI can exit after an unknown/partial physical send. Neither a
// local exit nor successful OS teardown proves financial settlement. This
// seam opens only from a durably saved exact authority seal/outcome. A network
// result that was not persisted is never sufficient to mint a next generation.
var previousNativeGenerationOutcome = persistedNativeGenerationOutcome

func NativeRoleForPhase(phase state.Phase) string {
	switch phase {
	case state.PhasePlan:
		return "planner"
	case state.PhaseAdvisor:
		return "advisor"
	case state.PhaseValidate:
		return "validator"
	case state.PhaseNone, state.PhaseImplement:
		return "implementer"
	default:
		return ""
	}
}

func nativeRole(cfg *config.Config, sess *state.Session) (string, string) {
	if cfg.WorkerLaunchContext != nil {
		return cfg.WorkerLaunchContext.Role, cfg.WorkerLaunchContext.ParentRoleRunID
	}
	if sess != nil {
		role := NativeRoleForPhase(sess.Phase)
		if sess.Phase == state.PhaseNone && sess.NativeRole == "repair" {
			role = "repair"
		}
		return role, sess.NativeRoleRunID
	}
	return "", ""
}

func nativeClient(cfg *config.Config) (admissioncontrol.Client, error) {
	if cfg == nil {
		return admissioncontrol.Client{}, &NativeRegistrationHold{Code: "configuration_invalid"}
	}
	r := cfg.WorkerNativeSessionRegistration
	if r == nil || !filepath.IsAbs(cfg.StateDir) || !filepath.IsAbs(r.ControlSocket) || r.AuthorityUID == nil ||
		r.ExpectedPolicyVersion <= 0 || !admissioncontrol.Identifier(cfg.ProjectID) ||
		!admissioncontrol.Identifier(r.FleetID) || !admissioncontrol.Identifier(r.GatewayScope) ||
		!admissioncontrol.Identifier(r.BudgetRunID) || r.TTLSeconds <= 0 || r.TTLSeconds > math.MaxInt64-time.Now().Unix() {
		return admissioncontrol.Client{}, &NativeRegistrationHold{Code: "configuration_invalid"}
	}
	return admissioncontrol.Client{SocketPath: r.ControlSocket, AuthorityUID: *r.AuthorityUID, Timeout: 7 * time.Second}, nil
}

func nativeSessionArgsConflict(args []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--session-id", "--resume", "--continue", "--fork-session", "--no-session-persistence", "--":
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg[1:], "rc") {
			return true
		}
	}
	return false
}

func nativeConfigDigest(cfg *config.Config, backend string, backendCfg BackendConfig, role string) string {
	b, _ := json.Marshal(struct {
		Registration           *config.NativeSessionRegistrationConfig
		Project, Backend, Role string
		Config                 BackendConfig
	}{
		cfg.WorkerNativeSessionRegistration, cfg.ProjectID, backend, role, backendCfg})
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

func nativeReceiptDir(stateDir, slot string) string {
	return filepath.Join(stateDir, "worker-native-sessions", slot)
}
func nativeReceiptName(generation uint64) string {
	return "generation-" + strconv.FormatUint(generation, 10) + ".json"
}

func lockNativeWorker(cfg *config.Config, slot string) (string, func(), error) {
	if state.ValidateSlotID(slot) != nil {
		return "", nil, &NativeRegistrationHold{Code: "identity_invalid"}
	}
	root := filepath.Join(cfg.StateDir, "worker-native-sessions")
	dir := nativeReceiptDir(cfg.StateDir, slot)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, &NativeRegistrationHold{Code: "receipt_unavailable"}
	}
	for _, path := range []string{root, dir} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !nativeOwned(info) {
			return "", nil, &NativeRegistrationHold{Code: "receipt_unavailable"}
		}
	}
	for _, path := range []string{cfg.StateDir, root} {
		if err := syncNativeDir(path); err != nil {
			return "", nil, &NativeRegistrationHold{Code: "receipt_persistence_failed"}
		}
	}
	lock := flock.New(filepath.Join(dir, ".lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return "", nil, &NativeRegistrationHold{Code: "generation_in_progress"}
	}
	return dir, func() { _ = lock.Unlock() }, nil
}

func syncNativeDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writeNativeWorkerReceipt(dir string, receipt *NativeWorkerReceipt) error {
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".native-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, nativeReceiptName(receipt.Generation))); err != nil {
		return err
	}
	return syncNativeDir(dir)
}

func readNativeWorkerReceipt(dir string, generation uint64) (*NativeWorkerReceipt, error) {
	path := filepath.Join(dir, nativeReceiptName(generation))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !nativeOwned(info) || info.Size() > 65536 {
		return nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r NativeWorkerReceipt
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&r)
	var trailing any
	if decodeErr == nil && decoder.Decode(&trailing) != io.EOF {
		decodeErr = fmt.Errorf("trailing data")
	}
	canonical, _ := json.Marshal(&r)
	if !bytes.Equal(b, canonical) {
		decodeErr = fmt.Errorf("noncanonical receipt")
	}
	if decodeErr != nil || r.SchemaVersion != 1 || r.Generation != generation || r.AccountingReady ||
		!r.Request.Valid() || uuid.Validate(r.RoleRunID) != nil || (r.ParentRoleRunID != "" && uuid.Validate(r.ParentRoleRunID) != nil) ||
		r.ProjectID != r.Request.ProjectID || r.RoleRunID == r.Request.NativeSessionID || r.IssueNumber <= 0 || r.ProcessLeaseUnit == "" || r.ProcessLeaseManager == "" {
		return nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	switch r.Status {
	case "registration_intent", "registered", "registration_reconciled_not_launched", "launch_intent", "launched":
	default:
		return nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	if r.Status != "registration_intent" && (r.Acknowledgement == nil || r.Acknowledgement.Binding != r.Request.Binding || r.Acknowledgement.Revoked || r.Acknowledgement.RegistrationVersion <= 0 || r.Acknowledgement.RegistrationVersion > r.Request.ExpectedVersion) {
		return nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	if (r.Status == "launch_intent" || r.Status == "launched") && !filepath.IsAbs(r.LogFile) {
		return nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	if err := validateNativeWorkerOutcome(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func prepareNativeWorker(cfg *config.Config, sess *state.Session, slot, backend string, backendCfg BackendConfig, generation uint64, issue int, worktree, branch string) (*nativeWorkerLaunch, error) {
	if cfg.WorkerNativeSessionRegistration == nil {
		if sess != nil && sess.NativeRoleRunID != "" {
			return nil, &NativeRegistrationHold{Code: "configuration_removed"}
		}
		if _, err := os.Lstat(nativeReceiptDir(cfg.StateDir, slot)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return nil, &NativeRegistrationHold{Code: "configuration_removed", LaunchUncertain: true}
		}
		return nil, nil
	}
	client, err := nativeClient(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.RemoteRunner.Enabled || resolveBackendKind(backend, backendCfg) != config.BackendKindClaude {
		return nil, &NativeRegistrationHold{Code: "harness_unsupported"}
	}
	cmd, _, err := BuildWorkerCmd(backend, backendCfg, "native-owned-prompt", worktree)
	if err != nil || filepath.Base(cmd.Path) != "claude" {
		return nil, &NativeRegistrationHold{Code: "harness_unsupported"}
	}
	if nativeSessionArgsConflict(cmd.Args[1:]) {
		return nil, &NativeRegistrationHold{Code: "argument_conflict"}
	}
	role, parent := nativeRole(cfg, sess)
	switch role {
	case "planner", "advisor", "implementer", "validator", "repair":
	default:
		return nil, &NativeRegistrationHold{Code: "role_context_missing"}
	}
	if generation == 0 || issue <= 0 || (parent != "" && uuid.Validate(parent) != nil) {
		return nil, &NativeRegistrationHold{Code: "identity_invalid"}
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return nil, err
	}
	owner := &nativeWorkerLaunch{dir: dir, unlock: unlock}
	ok := false
	defer func() {
		if !ok {
			unlock()
		}
	}()
	lease, err := workerProcessLease(cfg, slot, generation)
	if err != nil {
		return nil, &NativeRegistrationHold{Code: "process_lease_invalid"}
	}
	configDigest := nativeConfigDigest(cfg, backend, backendCfg, role)
	previous, err := readNativeWorkerReceipt(dir, generation)
	if err == nil {
		if previous.OutcomeIntent != nil {
			return nil, &NativeRegistrationHold{Code: "native_generation_sealed", LaunchUncertain: true}
		}
		if previous.ProjectID != cfg.ProjectID || previous.Slot != slot || previous.IssueNumber != issue || previous.Worktree != worktree || previous.Branch != branch || previous.Backend != backend || previous.ConfigDigest != configDigest || previous.Request.Role != role || previous.ParentRoleRunID != parent || previous.ProcessLeaseUnit != lease.Unit || previous.ProcessLeaseManager != lease.Manager {
			return nil, &NativeRegistrationHold{Code: "identity_conflict", LaunchUncertain: previous.Status == "launch_intent" || previous.Status == "launched"}
		}
		if previous.Status == "launch_intent" || previous.Status == "launched" {
			owner.receipt, owner.adopt = previous, true
			ok = true
			return owner, nil
		}
		return nil, &NativeRegistrationHold{Code: "unresolved_registration"}
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	if generation == 1 && tmuxSessionExists(TmuxSessionName(slot)) {
		return nil, &NativeRegistrationHold{Code: "unregistered_process_exists", LaunchUncertain: true, Slot: slot}
	}
	if generation > 1 {
		prior, err := readNativeWorkerReceipt(dir, generation-1)
		if err != nil || prior.Status != "launched" || prior.ProjectID != cfg.ProjectID || prior.Slot != slot {
			return nil, &NativeRegistrationHold{Code: "unresolved_previous_generation", LaunchUncertain: true}
		}
		if parent != prior.RoleRunID {
			return nil, &NativeRegistrationHold{Code: "ancestry_conflict"}
		}
		parent = prior.RoleRunID
		if err := previousNativeGenerationOutcome(cfg, prior); err != nil {
			prior, err = reconcileNativeWorkerOutcomeLocked(cfg, dir, prior)
			if err != nil || previousNativeGenerationOutcome(cfg, prior) != nil {
				return nil, &NativeRegistrationHold{Code: "previous_outcome_unknown"}
			}
		}
	}
	r := cfg.WorkerNativeSessionRegistration
	receipt := &NativeWorkerReceipt{SchemaVersion: 1, ProjectID: cfg.ProjectID, Slot: slot, Generation: generation, IssueNumber: issue,
		RoleRunID: uuid.NewString(), ParentRoleRunID: parent, Worktree: worktree, Branch: branch, Backend: backend, ConfigDigest: configDigest,
		Status: "registration_intent", ProcessLeaseUnit: lease.Unit, ProcessLeaseManager: lease.Manager,
		Request: admissioncontrol.RegistrationRequest{Binding: admissioncontrol.Binding{GatewayScope: r.GatewayScope,
			NativeSessionID: uuid.NewString(), FleetID: r.FleetID, ProjectID: cfg.ProjectID, RunID: r.BudgetRunID, Role: role, ExpiresAt: time.Now().Unix() + r.TTLSeconds}, ExpectedVersion: r.ExpectedPolicyVersion}}
	if err := persistNativeWorkerReceipt(dir, receipt); err != nil {
		return nil, &NativeRegistrationHold{Code: "receipt_persistence_failed"}
	}
	ack, err := registerNativeWorker(client, receipt.Request)
	if err != nil {
		var hold *admissioncontrol.Hold
		if errors.As(err, &hold) {
			return nil, &NativeRegistrationHold{Code: hold.Code}
		}
		return nil, &NativeRegistrationHold{Code: "authority_unavailable"}
	}
	if ack.Binding != receipt.Request.Binding || ack.Revoked || ack.RegistrationVersion <= 0 || ack.RegistrationVersion > receipt.Request.ExpectedVersion || ack.Binding.ExpiresAt <= time.Now().Unix() {
		return nil, &NativeRegistrationHold{Code: "acknowledgement_invalid"}
	}
	receipt.Acknowledgement = &ack
	receipt.Status = "registered"
	if err := persistNativeWorkerReceipt(dir, receipt); err != nil {
		return nil, &NativeRegistrationHold{Code: "receipt_persistence_failed"}
	}
	owner.receipt = receipt
	ok = true
	return owner, nil
}

func (n *nativeWorkerLaunch) close() {
	if n != nil && n.unlock != nil {
		n.unlock()
		n.unlock = nil
	}
}
func (n *nativeWorkerLaunch) command(cmd *exec.Cmd) error {
	if n == nil {
		return nil
	}
	if n.adopt {
		return &NativeRegistrationHold{Code: "unresolved_launch", LaunchUncertain: true}
	}
	if filepath.Base(cmd.Path) != "claude" || nativeSessionArgsConflict(cmd.Args[1:]) {
		return &NativeRegistrationHold{Code: "argument_conflict"}
	}
	cmd.Args = append(cmd.Args, "--session-id", n.receipt.Request.NativeSessionID)
	return nil
}
func (n *nativeWorkerLaunch) beginLaunch(logFile string) error {
	if n == nil {
		return nil
	}
	if n.adopt || n.receipt.Status != "registered" || n.receipt.Request.ExpiresAt <= time.Now().Unix() {
		return &NativeRegistrationHold{Code: "launch_not_authorized", LaunchUncertain: n.adopt}
	}
	if !filepath.IsAbs(logFile) {
		return &NativeRegistrationHold{Code: "log_identity_invalid"}
	}
	n.receipt.LogFile = logFile
	n.receipt.Status = "launch_intent"
	if err := persistNativeWorkerReceipt(n.dir, n.receipt); err != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	return nil
}
func (n *nativeWorkerLaunch) complete(pid int, lease tmuxsession.ProcessLease) error {
	if n == nil {
		return nil
	}
	if pid <= 0 || lease.Unit != n.receipt.ProcessLeaseUnit || lease.Manager != n.receipt.ProcessLeaseManager {
		return &NativeRegistrationHold{Code: "process_identity_conflict", LaunchUncertain: true}
	}
	n.receipt.PID = pid
	n.receipt.Status = "launched"
	if err := persistNativeWorkerReceipt(n.dir, n.receipt); err != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	return nil
}
func (n *nativeWorkerLaunch) stamp(sess *state.Session) {
	if n == nil || sess == nil {
		return
	}
	sess.NativeSessionID = n.receipt.Request.NativeSessionID
	sess.NativeRoleRunID = n.receipt.RoleRunID
	sess.NativeParentRoleRunID = n.receipt.ParentRoleRunID
	sess.NativeRole = n.receipt.Request.Role
	sess.NativeReceiptDir = n.dir
	sess.NativeRegistrationHold = ""
	if n.receipt.LogFile != "" {
		sess.LogFile = n.receipt.LogFile
	}
}

// adoptExisting validates the exact process lease and canonical workspace before
// accepting an uncertain prior launch. It never launches or rewrites a runner.
func (n *nativeWorkerLaunch) adoptExisting(cfg *config.Config, slot string) (int, tmuxsession.ProcessLease, error) {
	if n == nil || !n.adopt {
		return 0, tmuxsession.ProcessLease{}, nil
	}
	lease, err := workerProcessLease(cfg, slot, n.receipt.Generation)
	if err != nil {
		return 0, lease, err
	}
	terminal, terminalErr := nativeWorkerTerminated(n.dir, n.receipt)
	if terminalErr != nil || terminal {
		return 0, lease, &NativeRegistrationHold{Code: "launch_terminal_or_unknown", LaunchUncertain: !terminal}
	}
	pid, path, err := TmuxPaneIdentity(TmuxSessionName(slot))
	if err != nil || !sameCleanPath(path, n.receipt.Worktree) || validateExactWorktreeIdentity(cfg.LocalPath, n.receipt.Worktree, n.receipt.Branch) != nil {
		return 0, lease, &NativeRegistrationHold{Code: "unresolved_launch", LaunchUncertain: true}
	}
	active, err := workerProcessLeaseActive(lease)
	if err != nil || !active {
		return 0, lease, &NativeRegistrationHold{Code: "unresolved_launch", LaunchUncertain: true}
	}
	owned, err := workerProcessLeaseAnchored(lease, pid)
	if err != nil || !owned {
		return 0, lease, &NativeRegistrationHold{Code: "unresolved_launch", LaunchUncertain: true}
	}
	scratch, err := recoverWorkerScratchLease(cfg, slot, lease)
	if err != nil {
		return 0, lease, &NativeRegistrationHold{Code: "scratch_identity_unknown", LaunchUncertain: true}
	}
	attachWorkerScratchReceipt(&lease, scratch)
	if err := n.complete(pid, lease); err != nil {
		return 0, lease, err
	}
	return pid, lease, nil
}

// NativePendingSlots extends the existing fleet limiter with durable launch
// uncertainty. Registration-only receipts consume no process capacity.
func NativePendingSlots(stateDir string, sessions map[string]*state.Session) ([]string, error) {
	for slot, sess := range sessions {
		if sess != nil && sess.NativeRoleRunID != "" {
			if _, err := NativeSessionProcessTerminal(stateDir, slot, sess); err != nil {
				return nil, err
			}
		}
	}
	root := filepath.Join(stateDir, "worker-native-sessions")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !nativeOwned(info) {
		return nil, fmt.Errorf("invalid native receipt root")
	}
	dirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, dir := range dirs {
		if dir.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("invalid native receipt directory")
		}
		if !dir.IsDir() {
			continue
		}
		if state.ValidateSlotID(dir.Name()) != nil {
			return nil, fmt.Errorf("invalid native slot receipt")
		}
		info, err := dir.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0077 != 0 || !nativeOwned(info) {
			return nil, fmt.Errorf("invalid native receipt directory")
		}
		files, err := os.ReadDir(filepath.Join(root, dir.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if !strings.HasPrefix(file.Name(), "generation-") || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			generation, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(file.Name(), "generation-"), ".json"), 10, 64)
			if err != nil || generation == 0 || file.Name() != nativeReceiptName(generation) {
				return nil, fmt.Errorf("invalid native receipt generation")
			}
			r, err := readNativeWorkerReceipt(filepath.Join(root, dir.Name()), generation)
			if err != nil {
				return nil, err
			}
			if r.Slot != dir.Name() {
				return nil, fmt.Errorf("native slot identity conflict")
			}
			if r.Status != "launch_intent" && r.Status != "launched" {
				continue
			}
			terminal, err := nativeWorkerTerminated(filepath.Join(root, dir.Name()), r)
			if err != nil {
				return nil, err
			}
			if terminal {
				continue
			}
			pending = append(pending, dir.Name())
			break
		}
	}
	return pending, nil
}

// finishError converts post-registration setup failures into stable holds. Once
// launch intent exists, no caller may release ownership or invent a new run.
func (n *nativeWorkerLaunch) finishError(err *error) {
	if n == nil || err == nil || *err == nil {
		return
	}
	uncertain := n.receipt.Status == "launch_intent" || n.receipt.Status == "launched"
	if h, ok := NativeHold(*err); ok {
		h.LaunchUncertain = h.LaunchUncertain || uncertain
		h.Slot = n.receipt.Slot
		return
	}
	*err = &NativeRegistrationHold{Code: "setup_failed", LaunchUncertain: uncertain, Slot: n.receipt.Slot}
}

func (n *nativeWorkerLaunch) adoptSession(cfg *config.Config, slot string, sess *state.Session) (bool, error) {
	if n == nil || !n.adopt {
		return false, nil
	}
	pid, lease, err := n.adoptExisting(cfg, slot)
	if err != nil {
		return true, err
	}
	if sess.WorkerGeneration+1 != n.receipt.Generation {
		return true, &NativeRegistrationHold{Code: "generation_conflict", LaunchUncertain: true}
	}
	beginSessionAttempt(cfg, sess, n.receipt.Backend, "native_launch_adopted", "runtime_state_lost", time.Now())
	sess.PID = pid
	sess.TmuxSession = TmuxSessionName(slot)
	sess.Worktree = n.receipt.Worktree
	sess.Branch = n.receipt.Branch
	setSessionProcessLease(sess, lease)
	n.stamp(sess)
	return true, nil
}

type nativeTermination struct {
	RoleRunID       string `json:"role_run_id"`
	NativeSessionID string `json:"native_session_id"`
	Generation      uint64 `json:"generation"`
	Unit            string `json:"unit"`
	Manager         string `json:"manager"`
}

func terminationFor(r *NativeWorkerReceipt) nativeTermination {
	return nativeTermination{r.RoleRunID, r.Request.NativeSessionID, r.Generation, r.ProcessLeaseUnit, r.ProcessLeaseManager}
}
func nativeWorkerTerminated(dir string, r *NativeWorkerReceipt) (bool, error) {
	path := filepath.Join(dir, nativeReceiptName(r.Generation)+".terminated")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !nativeOwned(info) || info.Size() > 4096 {
		return false, fmt.Errorf("invalid native terminal receipt")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var t nativeTermination
	if json.Unmarshal(b, &t) != nil || t != terminationFor(r) {
		return false, fmt.Errorf("native terminal identity conflict")
	}
	return true, nil
}

// Called only after the exact OS lease was proven terminated. A separate
// immutable receipt avoids racing an in-flight next-generation registration.
func markNativeWorkerTerminated(sess *state.Session) error {
	if sess == nil || sess.NativeRoleRunID == "" {
		return nil
	}
	r, err := readNativeWorkerReceipt(sess.NativeReceiptDir, sess.WorkerGeneration)
	if err != nil || r.RoleRunID != sess.NativeRoleRunID || r.Request.NativeSessionID != sess.NativeSessionID || r.ProcessLeaseUnit != sess.ProcessLeaseUnit || r.ProcessLeaseManager != sess.ProcessLeaseManager {
		return &NativeRegistrationHold{Code: "terminal_identity_conflict", LaunchUncertain: true}
	}
	b, _ := json.Marshal(terminationFor(r))
	f, err := os.CreateTemp(sess.NativeReceiptDir, ".terminal-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(sess.NativeReceiptDir, nativeReceiptName(r.Generation)+".terminated")); err != nil {
		return err
	}
	return syncNativeDir(sess.NativeReceiptDir)
}

// ReconcileNativeWorkerRegistration repeats only the saved registration. It
// never starts/adopts a process and never promotes reconciliation into launch
// authority. A held generation cannot be erased or rebound to new identity.
func ReconcileNativeWorkerRegistration(cfg *config.Config, slot string, generation uint64) (*NativeWorkerReceipt, error) {
	client, err := nativeClient(cfg)
	if err != nil {
		return nil, err
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := readNativeWorkerReceipt(dir, generation)
	if err != nil {
		return nil, err
	}
	c := cfg.WorkerNativeSessionRegistration
	def, ok := cfg.Model.Backends[r.Backend]
	if !ok {
		return nil, &NativeRegistrationHold{Code: "backend_unknown"}
	}
	backendCfg := workerBackendConfig(def)
	backendCfg.TokenBudget = cfg.WorkerMaxTokens
	if r.ConfigDigest != nativeConfigDigest(cfg, r.Backend, backendCfg, r.Request.Role) {
		return nil, &NativeRegistrationHold{Code: "identity_conflict"}
	}
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.Request.GatewayScope != c.GatewayScope || r.Request.FleetID != c.FleetID || r.Request.RunID != c.BudgetRunID || r.Request.ExpectedVersion != c.ExpectedPolicyVersion {
		return nil, &NativeRegistrationHold{Code: "identity_conflict"}
	}
	if r.Status == "launch_intent" || r.Status == "launched" {
		return nil, &NativeRegistrationHold{Code: "unresolved_launch", LaunchUncertain: true}
	}
	ack, err := registerNativeWorker(client, r.Request)
	if err != nil {
		return nil, &NativeRegistrationHold{Code: "authority_unavailable"}
	}
	if ack.Binding != r.Request.Binding || ack.Revoked || ack.RegistrationVersion <= 0 || ack.RegistrationVersion > r.Request.ExpectedVersion || ack.Binding.ExpiresAt <= time.Now().Unix() {
		return nil, &NativeRegistrationHold{Code: "acknowledgement_invalid"}
	}
	r.Acknowledgement = &ack
	r.Status = "registration_reconciled_not_launched"
	if err := persistNativeWorkerReceipt(dir, r); err != nil {
		return nil, &NativeRegistrationHold{Code: "receipt_persistence_failed"}
	}
	return r, nil
}

func stampNativeHold(slot string, sess *state.Session, err error) bool {
	hold, ok := NativeHold(err)
	if !ok {
		return false
	}
	hold.Slot = slot
	if sess != nil {
		sess.NativeRegistrationHold = hold.Code
	}
	return true
}

func nativeOwned(info os.FileInfo) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid())
}

// ValidateNativeWorkerRuntime is a read-only guard for the existing runtime
// projection/CAS repair paths. Pane cwd alone cannot prove a native generation.
func ValidateNativeWorkerRuntime(cfg *config.Config, slot string, sess *state.Session, pid int) error {
	if cfg == nil || sess == nil || sess.NativeRoleRunID == "" || sess.NativeReceiptDir != nativeReceiptDir(cfg.StateDir, slot) {
		return &NativeRegistrationHold{Code: "native_identity_missing", LaunchUncertain: true}
	}
	r, err := readNativeWorkerReceipt(sess.NativeReceiptDir, sess.WorkerGeneration)
	if err != nil {
		return err
	}
	if r.OutcomeIntent != nil {
		return &NativeRegistrationHold{Code: "native_generation_sealed", LaunchUncertain: true}
	}
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.RoleRunID != sess.NativeRoleRunID || r.Request.NativeSessionID != sess.NativeSessionID || r.Worktree != sess.Worktree || r.Branch != sess.Branch || r.ProcessLeaseUnit != sess.ProcessLeaseUnit || r.ProcessLeaseManager != sess.ProcessLeaseManager || r.Status != "launched" {
		return &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	terminal, err := nativeWorkerTerminated(sess.NativeReceiptDir, r)
	if err != nil || terminal {
		return &NativeRegistrationHold{Code: "native_identity_terminal", LaunchUncertain: true}
	}
	if validateExactWorktreeIdentity(cfg.LocalPath, r.Worktree, r.Branch) != nil {
		return &NativeRegistrationHold{Code: "native_workspace_conflict", LaunchUncertain: true}
	}
	lease := tmuxsession.ProcessLease{Unit: r.ProcessLeaseUnit, Manager: r.ProcessLeaseManager}
	active, err := workerProcessLeaseActive(lease)
	if err != nil || !active {
		return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	owned, err := workerProcessLeaseAnchored(lease, pid)
	if err != nil || !owned {
		return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	return nil
}

// NativeSessionProcessTerminal validates only durable local OS termination.
// This never grants financial recovery or deletion of raw worker evidence.
func NativeSessionProcessTerminal(stateDir, slot string, sess *state.Session) (bool, error) {
	if sess == nil || sess.NativeRoleRunID == "" {
		return false, nil
	}
	if sess.NativeReceiptDir != nativeReceiptDir(stateDir, slot) {
		return false, &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	r, err := readNativeWorkerReceipt(sess.NativeReceiptDir, sess.WorkerGeneration)
	if err != nil {
		return false, err
	}
	if r.Slot != slot || r.RoleRunID != sess.NativeRoleRunID || r.Request.NativeSessionID != sess.NativeSessionID {
		return false, &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	return nativeWorkerTerminated(sess.NativeReceiptDir, r)
}

var nativeWorkerPaneAbsent = func(name string) (bool, error) {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && (strings.Contains(string(out), "no server running on ") || (strings.Contains(string(out), "error connecting to ") && strings.Contains(string(out), "No such file or directory"))) {
			return true, nil
		}
		return false, fmt.Errorf("native pane absence unknown")
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == name {
			return false, nil
		}
	}
	return true, nil
}

// ReconcileNativeWorkerTermination observes a previously launched exact OS
// lease without signalling anything. Financial holds survive local termination.
// launch_intent is never released by absence: dispatch may not have happened yet.
func ReconcileNativeWorkerTermination(cfg *config.Config, slot string, sess *state.Session) error {
	if sess == nil || sess.NativeRoleRunID == "" {
		return nil
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return err
	}
	defer unlock()
	terminal, err := NativeSessionProcessTerminal(cfg.StateDir, slot, sess)
	if err != nil {
		return err
	}
	if !terminal {
		r, err := readNativeWorkerReceipt(dir, sess.WorkerGeneration)
		if err != nil {
			return err
		}
		if r.ProjectID != cfg.ProjectID || r.Status != "launched" || r.ProcessLeaseUnit != sess.ProcessLeaseUnit || r.ProcessLeaseManager != sess.ProcessLeaseManager {
			return &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
		}
		lease, has, err := sessionProcessLease(sess)
		if err != nil || !has {
			return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
		}
		active, err := workerProcessLeaseActive(lease)
		if err != nil || active {
			return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
		}
		absent, err := nativeWorkerPaneAbsent(TmuxSessionName(slot))
		if err != nil || !absent {
			return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
		}
		if err := markNativeWorkerTerminated(sess); err != nil {
			return err
		}
	}
	sess.PID = 0
	sess.TmuxSession = ""
	clearSessionProcessLease(sess)
	if sess.Status == state.StatusRunning {
		sess.Status = state.StatusDead
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
	}
	return nil
}

func nativeWorkerDestructiveOutcome(cfg *config.Config, slot string, sess *state.Session) error {
	if sess == nil || sess.NativeRoleRunID == "" {
		return nil
	}
	r, err := readNativeWorkerReceipt(sess.NativeReceiptDir, sess.WorkerGeneration)
	if err != nil {
		return err
	}
	if r.Slot != slot || r.RoleRunID != sess.NativeRoleRunID || r.Request.NativeSessionID != sess.NativeSessionID {
		return &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	if cfg != nil && (r.ProjectID != cfg.ProjectID || sess.NativeReceiptDir != nativeReceiptDir(cfg.StateDir, slot)) {
		return &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	if err := previousNativeGenerationOutcome(cfg, r); err != nil {
		return &NativeRegistrationHold{Code: "previous_outcome_unknown"}
	}
	return nil
}
