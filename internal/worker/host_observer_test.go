package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/aiexecution"
)

func observerTestPolicy(t *testing.T, dir string) aiexecution.Policy {
	t.Helper()
	profile := os.Getenv("MAESTRO_TEST_HOST_OBSERVER_PROFILE")
	if profile == "" {
		t.Skip("set MAESTRO_TEST_HOST_OBSERVER_PROFILE to a root-owned native profile for host boundary integration")
	}
	b, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	m := aiexecution.Manifest{Version: 1, EvidenceKind: "synthetic-no-launch", Runtime: aiexecution.RuntimeExpectation{ManagementKeyEnv: "MAESTRO_TEST_HOST_KEY"}, Containment: map[string]aiexecution.FileProof{"worker": {Path: profile, SHA256: hex.EncodeToString(sum[:])}}}
	b, _ = json.Marshal(m)
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(b)
	p := aiexecution.Policy{RequireVerifiedRoute: true, ManifestPath: path, ManifestSHA256: hex.EncodeToString(sum[:])}
	return p.BindController(p, dir)
}

func TestPreparedHostRunnerKeepsObserverOutsideCleanChildEnvironment(t *testing.T) {
	f, receipt, _ := registeredPrelaunchFixture(t)
	f.cfg.AIExecution = observerTestPolicy(t, f.cfg.StateDir)
	t.Setenv("MAESTRO_TEST_HOST_KEY", "synthetic-private-observer-key")
	runner := filepath.Join(f.cfg.StateDir, "worker-run.sh")
	cmd := exec.Command("claude", "--model", "fixture", "--session-id", receipt.Request.NativeSessionID)
	cmd.Dir = receipt.Worktree
	if err := prepareWorkerExecutionProof(f.cfg, &nativeWorkerLaunch{receipt: receipt}, cmd, runner); err != nil {
		t.Fatal(err)
	}
	proofPath := runner + ".execution.json"
	b, _ := os.ReadFile(proofPath)
	var proof workerExecutionProof
	if err := json.Unmarshal(b, &proof); err != nil {
		t.Fatal(err)
	}
	if proof.Version != 2 || proof.ObserverCredential == nil || bytes.Contains(b, []byte(os.Getenv("MAESTRO_TEST_HOST_KEY"))) {
		t.Fatal("proof exposed or omitted observer credential")
	}
	if st, err := os.Stat(proof.ObserverCredential.Path); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("observer file is not private", err)
	}
	// Exercise generated bash → internal exec entry with the environment stripped
	// in exactly the way the detached sudo/tmux host boundary strips observer.env.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(f.cfg.StateDir, "maestro-test")
	script := "#!/bin/sh\nexec " + shellQuote(executable) + " -test.run=^TestHostObserverExecChild$ -- \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	sha, _ := workerExecutionProofPin(proofPath)
	logFile := filepath.Join(f.cfg.StateDir, "boundary.log")
	script = buildWorkerRunnerScript(proof.Arguments, "", logFile, receipt.Worktree, f.cfg.StateDir, "", helper, nil, proofPath, sha)
	if strings.Contains(script, os.Getenv("MAESTRO_TEST_HOST_KEY")) {
		t.Fatal("runner exposed observer key")
	}
	if err := os.WriteFile(runner, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/bash", runner)
	child.Env = []string{"PATH=/usr/bin:/bin", "MAESTRO_OBSERVER_TEST_CHILD=1"}
	out, err := child.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("OBSERVER_BOUNDARY_OK")) || bytes.Contains(out, []byte(os.Getenv("MAESTRO_TEST_HOST_KEY"))) {
		t.Fatalf("boundary failed: %v %s", err, out)
	}
	// The parent-path variant must also remove an inherited observer variable.
	cmd = exec.Command(proof.Arguments[0], proof.Arguments[1:]...)
	cmd.Env = []string{"MAESTRO_TEST_HOST_KEY=synthetic-private-observer-key", "SAFE=1"}
	err = inspectWorkerProofRoute(proof, cmd)
	var held *aiexecution.Hold
	if !errors.As(err, &held) || held.Code != "source_evidence_not_installed" || strings.Contains(strings.Join(cmd.Env, "\n"), "MAESTRO_TEST_HOST_KEY") {
		t.Fatal("parent path leaked observer", err)
	}
	for _, mode := range []string{"loose_mode", "drift", "symlink", "key_name"} {
		t.Run(mode, func(t *testing.T) {
			original, _ := os.ReadFile(proof.ObserverCredential.Path)
			defer func() {
				_ = os.Remove(proof.ObserverCredential.Path)
				_ = os.WriteFile(proof.ObserverCredential.Path, original, 0600)
			}()
			switch mode {
			case "loose_mode":
				_ = os.Chmod(proof.ObserverCredential.Path, 0644)
			case "drift":
				_ = os.WriteFile(proof.ObserverCredential.Path, []byte("changed"), 0600)
			case "symlink":
				_ = os.Remove(proof.ObserverCredential.Path)
				_ = os.Symlink(proofPath, proof.ObserverCredential.Path)
			case "key_name":
				var c workerObserverCredential
				_ = json.Unmarshal(original, &c)
				c.KeyName = "OTHER"
				changed, _ := json.Marshal(c)
				_ = os.WriteFile(proof.ObserverCredential.Path, changed, 0600)
				copy := *proof.ObserverCredential
				sum := sha256.Sum256(changed)
				copy.SHA256 = hex.EncodeToString(sum[:])
				proof.ObserverCredential = &copy
			}
			if _, err := readWorkerObserverCredential(proof); err == nil {
				t.Fatal("invalid observer accepted")
			}
		})
	}
}

func TestHostObserverExecChild(t *testing.T) {
	if os.Getenv("MAESTRO_OBSERVER_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv("MAESTRO_TEST_HOST_KEY") != "" {
		os.Exit(91)
	}
	args := os.Args
	for len(args) > 0 && args[0] != "_worker-exec" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(92)
	}
	fs := flag.NewFlagSet("_worker-exec", flag.ContinueOnError)
	proof := fs.String("execution-proof", "", "")
	sha := fs.String("execution-proof-sha256", "", "")
	if fs.Parse(args[1:]) != nil {
		os.Exit(93)
	}
	err := RunWorkerWithExecutionProof("", *proof, *sha, fs.Args(), bytes.NewReader(nil), io.Discard)
	var held *aiexecution.Hold
	if !errors.As(err, &held) || held.Code != "source_evidence_not_installed" {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(94)
	}
	fmt.Println("OBSERVER_BOUNDARY_OK")
	os.Exit(0)
}
