package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
)

type workerExecutionProof struct {
	Version            int                         `json:"version"`
	Policy             aiexecution.Policy          `json:"policy"`
	ControllerRevision aiexecution.FileProof       `json:"controller_revision"`
	ControllerLease    aiexecution.ControllerLease `json:"controller_lease"`
	Spec               aiexecution.LaunchSpec      `json:"spec"`
	Arguments          []string                    `json:"arguments"`
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
	proof := workerExecutionProof{Version: 1, Policy: cfg.AIExecution, ControllerRevision: pin, ControllerLease: lease, Arguments: args, Spec: aiexecution.LaunchSpec{ProjectID: cfg.ProjectID, Role: r.Request.Role, RoleRunID: r.RoleRunID, Model: model, GatewayScope: r.Request.GatewayScope, ExpectedPolicyVersion: r.Request.ExpectedVersion, Registration: r.Acknowledgement, ProjectConfigSHA256: configDigest}}
	b, err := json.Marshal(proof)
	if err != nil {
		return aiexecution.Held("worker_execution_proof_invalid")
	}
	if err := writeFileAtomicMode(filepath.Dir(runnerPath), runnerPath+".execution.json", string(b), 0600); err != nil {
		return aiexecution.Held("worker_execution_proof_unavailable")
	}
	return nil
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
	if aiexecution.DecodeStrict(b, &proof) != nil || proof.Version != 1 || !proof.Policy.RequireVerifiedRoute {
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
	if err := aiexecution.Inspect(proof.Policy, proof.Spec, cmd); err != nil {
		return err
	}
	if err := proof.ControllerLease.Verify(); err != nil {
		return err
	}
	return aiexecution.VerifyFile(proof.ControllerRevision)
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
