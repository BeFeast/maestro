package worker

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/tmuxsession"
)

type workerExecutionProof struct {
	Version            int                         `json:"version"`
	Policy             aiexecution.Policy          `json:"policy"`
	ControllerRevision aiexecution.FileProof       `json:"controller_revision"`
	ControllerLease    aiexecution.ControllerLease `json:"controller_lease"`
	Spec               aiexecution.LaunchSpec      `json:"spec"`
	Arguments          []string                    `json:"arguments"`
	RuntimeKey         string                      `json:"runtime_key"`
	ProcessLeaseUnit   string                      `json:"process_lease_unit"`
	Worktree           string                      `json:"worktree"`
	ObserverCredential *aiexecution.FileProof      `json:"observer_credential,omitempty"`
}

func prepareWorkerExecutionProof(cfg *config.Config, n *nativeWorkerLaunch, cmd *exec.Cmd, runnerPath string) error {
	if err := cfg.AIExecution.CheckCurrent(); err != nil {
		return err
	}
	if !cfg.AIExecution.RequireVerifiedRoute {
		return nil
	}
	if n == nil || n.receipt == nil || n.receipt.Acknowledgement == nil {
		return aiexecution.Held("native_registration_required")
	}
	pin, err := cfg.AIExecution.ControllerPin()
	if err != nil {
		return err
	}
	lease, err := cfg.AIExecution.LiveControllerLease()
	if err != nil {
		return err
	}
	configDigest, err := config.AIExecutionConfigDigest(cfg)
	if err != nil {
		return aiexecution.Held("project_config_unobservable")
	}
	r := n.receipt
	args, err := resolveWorkerCommandPath(cmd.Args)
	if err != nil {
		return aiexecution.Held("worker_harness_unobservable")
	}
	model := ""
	for i := 1; i < len(cmd.Args)-1; i++ {
		if cmd.Args[i] == "--model" {
			model = cmd.Args[i+1]
		}
	}
	proof := workerExecutionProof{Version: 2, Policy: cfg.AIExecution, ControllerRevision: pin, ControllerLease: lease, Arguments: args, Spec: aiexecution.LaunchSpec{ProjectID: cfg.ProjectID, Role: r.Request.Role, RoleRunID: r.RoleRunID, Model: model, GatewayScope: r.Request.GatewayScope, ExpectedPolicyVersion: r.Request.ExpectedVersion, Registration: r.Acknowledgement, ProjectConfigSHA256: configDigest}}
	proof.RuntimeKey, proof.ProcessLeaseUnit, proof.Worktree = r.Slot, r.ProcessLeaseUnit, cmd.Dir
	proof.Spec.RuntimeKey = r.Slot
	observer, err := prepareWorkerObserverCredential(proof, runnerPath+".observer.json")
	if err != nil {
		return err
	}
	proof.ObserverCredential = &observer
	b, err := json.Marshal(proof)
	if err != nil {
		return aiexecution.Held("worker_execution_proof_invalid")
	}
	if err := writeFileAtomicMode(filepath.Dir(runnerPath), runnerPath+".execution.json", string(b), 0600); err != nil {
		return aiexecution.Held("worker_execution_proof_unavailable")
	}
	return nil
}

func prepareContainedWorkerCommand(path, sha string, original *exec.Cmd) (*aiexecution.ContainedNativeCommand, error) {
	if err := inspectWorkerExecutionProof(path, sha, original); err != nil {
		return nil, err
	}
	b, err := readOwnedRegularNoFollow(path, 128<<10)
	if err != nil {
		return nil, aiexecution.Held("worker_execution_proof_unavailable")
	}
	sum := sha256.Sum256(b)
	var proof workerExecutionProof
	if hex.EncodeToString(sum[:]) != sha || aiexecution.DecodeStrict(b, &proof) != nil {
		return nil, aiexecution.Held("worker_execution_proof_drift")
	}
	var lease tmuxsession.ProcessLease
	raw, err := base64.RawStdEncoding.DecodeString(os.Getenv(tmuxsession.NativeProcessLeaseEnv))
	if err != nil || len(raw) > 16<<10 || aiexecution.DecodeStrict(raw, &lease) != nil || !lease.HostRunner || lease.Unit != proof.ProcessLeaseUnit || lease.Manager != tmuxsession.ProcessLeaseManagerSystem {
		return nil, aiexecution.Held("containment_process_lease_invalid")
	}
	pin, err := aiexecution.ContainmentProfilePin(proof.Policy, proof.RuntimeKey, proof.Spec.Role)
	if err != nil {
		return nil, err
	}
	gateway, err := aiexecution.GatewayProcessFromPolicy(proof.Policy)
	if err != nil {
		return nil, err
	}
	original.Dir = proof.Worktree
	return aiexecution.PrepareContainedNativeCommand(pin, proof.Spec.ProjectID, proof.Spec.Role, proof.Spec.Registration.Binding.NativeSessionID, gateway, original, lease)
}

func workerExecutionProofPin(path string) (string, error) {
	b, err := readOwnedRegularNoFollow(path, 128<<10)
	if err != nil {
		return "", aiexecution.Held("worker_execution_proof_unavailable")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func inspectWorkerExecutionProof(path, sha string, cmd *exec.Cmd) error {
	b, err := readOwnedRegularNoFollow(path, 128<<10)
	if err != nil {
		return aiexecution.Held("worker_execution_proof_unavailable")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != sha {
		return aiexecution.Held("worker_execution_proof_drift")
	}
	var proof workerExecutionProof
	if aiexecution.DecodeStrict(b, &proof) != nil || (proof.Version != 1 && proof.Version != 2) || !proof.Policy.RequireVerifiedRoute {
		return aiexecution.Held("worker_execution_proof_invalid")
	}
	if len(proof.Arguments) == 0 || cmd.Path != proof.Arguments[0] || !reflect.DeepEqual(cmd.Args[1:], proof.Arguments[1:]) {
		return aiexecution.Held("worker_execution_arguments_changed")
	}
	if proof.ControllerLease.Verify() != nil {
		return aiexecution.Held("controller_lease_unavailable")
	}
	if err := aiexecution.VerifyFile(proof.ControllerRevision); err != nil {
		return aiexecution.Held("project_config_changed")
	}
	if err := inspectWorkerProofRoute(proof, cmd); err != nil {
		return err
	}
	if err := proof.ControllerLease.Verify(); err != nil {
		return err
	}
	return aiexecution.VerifyFile(proof.ControllerRevision)
}

type workerObserverCredential struct {
	Version int    `json:"version"`
	KeyName string `json:"key_name"`
	Key     string `json:"key"`
}

func prepareWorkerObserverCredential(proof workerExecutionProof, path string) (aiexecution.FileProof, error) {
	var pin aiexecution.FileProof
	if err := aiexecution.VerifyHostObservationPath(proof.Policy, proof.RuntimeKey, proof.Spec.Role, path); err != nil {
		return pin, err
	}
	name, err := aiexecution.ObservationKeyEnvironment(proof.Policy)
	if err != nil {
		return pin, err
	}
	key := os.Getenv(name)
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return pin, aiexecution.Held("runtime_management_key_unavailable")
	}
	b, err := json.Marshal(workerObserverCredential{Version: 1, KeyName: name, Key: key})
	if err != nil || writeFileAtomicMode(filepath.Dir(path), path, string(b), 0600) != nil {
		return pin, aiexecution.Held("host_observer_credential_unavailable")
	}
	sum := sha256.Sum256(b)
	return aiexecution.FileProof{Path: path, SHA256: hex.EncodeToString(sum[:])}, nil
}

func inspectWorkerProofRoute(proof workerExecutionProof, cmd *exec.Cmd) error {
	if proof.Version == 1 {
		return aiexecution.Inspect(proof.Policy, proof.Spec, cmd)
	}
	credential, err := readWorkerObserverCredential(proof)
	if err != nil {
		return err
	}
	// Parent preflight can inherit observer.env; detached exec normally cannot.
	// Neither path may carry this value into containment or a native process.
	env := cmd.Env[:0]
	for _, entry := range cmd.Env {
		if !strings.HasPrefix(entry, credential.KeyName+"=") {
			env = append(env, entry)
		}
	}
	cmd.Env = env
	return aiexecution.InspectWithObservationKey(proof.Policy, proof.Spec, cmd, credential.Key)
}

func readWorkerObserverCredential(proof workerExecutionProof) (workerObserverCredential, error) {
	var credential workerObserverCredential
	if proof.ObserverCredential == nil {
		return credential, aiexecution.Held("host_observer_credential_unavailable")
	}
	pin := *proof.ObserverCredential
	if err := aiexecution.VerifyHostObservationPath(proof.Policy, proof.RuntimeKey, proof.Spec.Role, pin.Path); err != nil {
		return credential, err
	}
	b, err := readPrivateObserverFile(pin.Path)
	sum := sha256.Sum256(b)
	if err != nil || hex.EncodeToString(sum[:]) != pin.SHA256 || aiexecution.DecodeStrict(b, &credential) != nil || credential.Version != 1 || credential.Key == "" || len(credential.Key) > 4096 || strings.ContainsAny(credential.Key, "\r\n") {
		return workerObserverCredential{}, aiexecution.Held("host_observer_credential_invalid")
	}
	name, err := aiexecution.ObservationKeyEnvironment(proof.Policy)
	if err != nil || credential.KeyName != name {
		return workerObserverCredential{}, aiexecution.Held("host_observer_credential_invalid")
	}
	return credential, nil
}

func readPrivateObserverFile(path string) ([]byte, error) {
	f, err := openPrivateCredentialFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	if err != nil || len(b) > 16<<10 {
		return nil, aiexecution.Held("host_observer_credential_unavailable")
	}
	return b, nil
}

func preflightWorkerExecutionDescriptor(cfg *config.Config, runnerPath string) error {
	b, err := readOwnedRegularNoFollow(runnerPath+".execution.json", 128<<10)
	if err != nil {
		return aiexecution.Held("worker_execution_proof_unavailable")
	}
	var proof workerExecutionProof
	if json.Unmarshal(b, &proof) != nil || len(proof.Arguments) == 0 {
		return aiexecution.Held("worker_execution_proof_invalid")
	}
	return preflightWorkerExecution(cfg, runnerPath, proof.Arguments)
}

func preflightWorkerExecution(cfg *config.Config, runnerPath string, args []string) error {
	if !cfg.AIExecution.RequireVerifiedRoute {
		return cfg.AIExecution.CheckCurrent()
	}
	path := runnerPath + ".execution.json"
	sha, err := workerExecutionProofPin(path)
	if err != nil {
		return err
	}
	credentialPath, err := resolveWorkerCredentialsFile(cfg.StateDir)
	if err != nil {
		return aiexecution.Held("worker_credentials_unobservable")
	}
	credentials, err := readWorkerCredentialsFile(credentialPath)
	if err != nil {
		return aiexecution.Held("worker_credentials_unobservable")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = workerExecEnvironment(os.Environ(), credentials)
	return inspectWorkerExecutionProof(path, sha, cmd)
}
