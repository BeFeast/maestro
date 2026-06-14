// Package selfdeploy implements the opt-in post-merge self-deploy of the
// maestro binary (#698).
//
// The orchestrator does not build/install/restart in-process: it would kill
// itself halfway through. Instead Trigger launches scripts/self-deploy.sh in
// a detached transient systemd unit (systemd-run --user), so the script
// survives the maestro unit restarting. The script builds from merged main
// (version-stamped per #682), installs the binary atomically keeping the
// previous one as `.prev`, restarts the configured units — user units via
// `systemctl --user` or, with scope=system, system units via
// `sudo -n systemctl` (#716) — which honors the units' drain semantics via
// their normal stop path, verifies the
// restarted process reports the expected version, rolls back to `.prev` on
// failure, and writes a JSON result file into the state dir. The next
// orchestrator cycle — running the freshly deployed binary on success —
// consumes that file and surfaces the outcome as a supervisor finding.
package selfdeploy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// Result statuses written by scripts/self-deploy.sh.
const (
	StatusDeployed   = "deployed"    // new binary live and verified
	StatusRolledBack = "rolled_back" // verification failed; .prev restored and units restarted
	StatusFailed     = "failed"      // deploy failed and rollback was impossible or also failed
)

// resultFileName lives in the project state dir so both the transient deploy
// unit and the orchestrator agree on the location without extra plumbing.
const resultFileName = "self-deploy-result.json"

// Result is the JSON contract between scripts/self-deploy.sh and the
// orchestrator.
type Result struct {
	Status      string `json:"status"`                 // deployed | rolled_back | failed
	Version     string `json:"version,omitempty"`      // stamped version of the new binary (e.g. "1.4.2+gabc1234")
	PrevVersion string `json:"prev_version,omitempty"` // version reported by the replaced binary
	ExpectedSHA string `json:"expected_sha,omitempty"` // origin/main commit the build was taken from
	PR          int    `json:"pr,omitempty"`           // merged PR that triggered the deploy
	Reason      string `json:"reason,omitempty"`       // failure/rollback reason
	FinishedAt  string `json:"finished_at,omitempty"`  // RFC3339
}

// ResultPath returns the path of the deploy result file inside stateDir.
func ResultPath(stateDir string) string {
	return filepath.Join(stateDir, resultFileName)
}

// ReadResult loads a finished deploy result. Returns (nil, nil) when no
// result file exists — i.e. no deploy has finished since the last consume.
func ReadResult(stateDir string) (*Result, error) {
	data, err := os.ReadFile(ResultPath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read self-deploy result: %w", err)
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("parse self-deploy result: %w", err)
	}
	return &res, nil
}

// ClearResult removes the result file so a result is consumed exactly once.
func ClearResult(stateDir string) error {
	err := os.Remove(ResultPath(stateDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// TriggerCommand builds the detached systemd-run invocation for a deploy
// after the given merged PR. Split from Trigger so tests can assert the
// argv without systemd.
func TriggerCommand(cfg *config.Config, prNumber int, now time.Time) (string, []string, error) {
	if cfg == nil {
		return "", nil, fmt.Errorf("self-deploy: nil config")
	}
	localPath := strings.TrimSpace(cfg.LocalPath)
	if localPath == "" {
		return "", nil, fmt.Errorf("self-deploy: local_path is required")
	}
	script := cfg.SelfDeploy.EffectiveScript(localPath)
	if script == "" {
		return "", nil, fmt.Errorf("self-deploy: no deploy script configured")
	}
	if _, err := os.Stat(script); err != nil {
		return "", nil, fmt.Errorf("self-deploy: script %s: %w", script, err)
	}
	bin := strings.TrimSpace(cfg.SelfDeploy.BinPath)
	if bin == "" {
		exe, err := os.Executable()
		if err != nil || strings.TrimSpace(exe) == "" {
			return "", nil, fmt.Errorf("self-deploy: bin_path not set and running binary not resolvable: %w", err)
		}
		bin = exe
	}

	// One unit per trigger: a stale failed unit from a previous deploy must
	// not block the next one, and --collect garbage-collects it either way.
	unitName := fmt.Sprintf("maestro-self-deploy-pr%d-%d", prNumber, now.Unix())
	timeoutSec := cfg.SelfDeploy.EffectiveTimeoutMinutes() * 60

	args := []string{
		"--user",
		"--collect",
		"--unit=" + unitName,
		"--property=WorkingDirectory=" + localPath,
		// Hard backstop well past the script's own deadline so a wedged
		// deploy cannot linger forever; the script handles its own
		// timeouts (and rollback) inside timeoutSec.
		"--property=RuntimeMaxSec=" + strconv.Itoa(timeoutSec*2),
	}
	// The transient unit gets the user manager's default environment, which
	// usually lacks the Go toolchain dirs the orchestrator unit configures.
	// Hand our PATH down so the build step finds the same tools.
	if path := strings.TrimSpace(os.Getenv("PATH")); path != "" {
		args = append(args, "--setenv=PATH="+path)
	}
	args = append(args,
		"/bin/bash", script,
		"--repo-dir", localPath,
		"--bin", bin,
		"--units", strings.Join(cfg.SelfDeploy.EffectiveUnits(), ","),
		"--result-file", ResultPath(cfg.StateDir),
		"--timeout-seconds", strconv.Itoa(timeoutSec),
		"--pr", strconv.Itoa(prNumber),
		// #716: select the systemd unit scope. Default "user" keeps the
		// pre-migration behavior; "system" restarts system units via
		// `sudo -n systemctl` (e.g. the Loki fleet, where maestro runs as
		// User=god system units rather than per-user units).
		"--scope", cfg.SelfDeploy.EffectiveScope(),
	)
	// #711: when bin_path is root-owned, the unprivileged deploy user cannot
	// stage/rename into it. install_via_sudo escalates the file ops with
	// passwordless `sudo -n` so atomic-rename install + rollback still work.
	if cfg.SelfDeploy.InstallViaSudo {
		args = append(args, "--install-via-sudo")
	}
	if url := cfg.SelfDeploy.EffectiveHealthURL(cfg.Server); url != "" {
		args = append(args, "--health-url", url)
		if env := cfg.SelfDeploy.EffectiveHealthTokenEnv(cfg.Server); env != "" {
			args = append(args, "--health-token-env", env)
		}
	}
	return "systemd-run", args, nil
}

// Trigger starts the deploy script in a detached transient unit and returns
// once systemd has accepted the job. It does NOT wait for the deploy: the
// orchestrator is typically restarted by it.
func Trigger(cfg *config.Config, prNumber int) error {
	name, args, err := TriggerCommand(cfg, prNumber, time.Now().UTC())
	if err != nil {
		return err
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("self-deploy: systemd-run: %w\n%s", err, out)
	}
	return nil
}

// Finding converts a deploy result into the supervisor finding the
// orchestrator records in state (#698 — "surfaces the result as a supervisor
// finding: deployed version / rollback reason").
func Finding(res *Result, project string, now time.Time) state.SupervisorDecision {
	d := state.SupervisorDecision{
		ID:               "self-deploy-" + now.Format("20060102T150405.000000000Z"),
		CreatedAt:        now,
		Project:          project,
		Risk:             "safe",
		Confidence:       1.0,
		RequiresApproval: false,
	}
	if res == nil {
		d.Status = "failed"
		d.Summary = "self-deploy: empty result"
		d.RecommendedAction = "inspect the self-deploy transient unit logs (journalctl --user -u 'maestro-self-deploy-*')"
		return d
	}
	prSuffix := ""
	if res.PR > 0 {
		prSuffix = fmt.Sprintf(" after PR #%d", res.PR)
	}
	switch res.Status {
	case StatusDeployed:
		d.Status = "succeeded"
		d.Summary = fmt.Sprintf("self-deploy: deployed maestro v%s%s", res.Version, prSuffix)
		d.RecommendedAction = "none"
	case StatusRolledBack:
		d.Status = "failed"
		d.Summary = fmt.Sprintf("self-deploy: rolled back to previous binary%s — %s", prSuffix, res.Reason)
		d.RecommendedAction = "inspect the failed deploy (journalctl --user -u 'maestro-self-deploy-*'), fix the regression, re-merge"
	default:
		d.Status = "failed"
		d.Summary = fmt.Sprintf("self-deploy: failed%s — %s", prSuffix, res.Reason)
		d.RecommendedAction = "verify the maestro units and binary by hand (systemctl --user status; maestro version), then redeploy manually"
	}
	if res.Version != "" {
		d.Reasons = append(d.Reasons, "new version: "+res.Version)
	}
	if res.PrevVersion != "" {
		d.Reasons = append(d.Reasons, "previous version: "+res.PrevVersion)
	}
	if res.ExpectedSHA != "" {
		d.Reasons = append(d.Reasons, "built from origin/main @ "+res.ExpectedSHA)
	}
	if res.Reason != "" && res.Status != StatusDeployed {
		d.Reasons = append(d.Reasons, "reason: "+res.Reason)
	}
	return d
}
