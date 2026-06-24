// Package selfdeploy implements the opt-in post-merge self-deploy of the
// maestro binary (#698).
//
// The orchestrator does not build/install/restart in-process: it would kill
// itself halfway through. Instead Trigger launches scripts/self-deploy.sh in
// a detached transient systemd unit, so the script survives the maestro unit
// restarting. The launcher scope matches the unit scope (#742): `systemd-run
// --user` for user scope, or `sudo -n systemd-run --uid=<user>` in the system
// manager for scope=system (whose orchestrator service env lacks the user bus).
// The script builds from merged main
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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// ErrDebounced reports that a self-deploy request was dropped because another
// deploy already fired within the debounce window — not that the trigger
// failed. The daemon's centralized RequestSelfDeploy returns it so N flows
// merging PRs near-simultaneously launch exactly ONE deploy of ONE unit (#758):
// the first request in a wave deploys, the rest see ErrDebounced. Callers must
// treat it as a benign skip (no failure finding, no "deploy started" notice),
// distinct from a real launcher failure (systemd-run rejected the unit).
var ErrDebounced = errors.New("self-deploy: debounced (recent trigger within window)")

// Result statuses written by scripts/self-deploy.sh.
const (
	StatusDeployed   = "deployed"    // new binary live and verified
	StatusRolledBack = "rolled_back" // verification failed; .prev restored and units restarted
	StatusFailed     = "failed"      // deploy failed and rollback was impossible or also failed
)

// resultFileName lives in the project state dir so both the transient deploy
// unit and the orchestrator agree on the location without extra plumbing.
const resultFileName = "self-deploy-result.json"

// triggerMarkerName records when the orchestrator last launched a deploy, so a
// burst of merges — or a run-loop restarted by its own deploy — can debounce
// re-triggers instead of stacking deploys that bounce the fleet mid-verify
// (#722). It lives in the state dir alongside the result file so it survives the
// orchestrator unit restarting.
const triggerMarkerName = "self-deploy-last-trigger.json"

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

// triggerMarker is the JSON persisted by RecordTrigger / read by LastTrigger.
type triggerMarker struct {
	TriggeredAt time.Time `json:"triggered_at"`
	PR          int       `json:"pr,omitempty"`
}

// triggerMarkerPath returns the path of the last-trigger marker inside stateDir.
func triggerMarkerPath(stateDir string) string {
	return filepath.Join(stateDir, triggerMarkerName)
}

// RecordTrigger persists the time (and PR) of a self-deploy trigger so a
// re-evaluating — or restarted — orchestrator can debounce the same wave of
// merges (#722). A blank stateDir is a no-op so callers without a state dir
// (e.g. tests) do not write into the working directory.
func RecordTrigger(stateDir string, prNumber int, now time.Time) error {
	if strings.TrimSpace(stateDir) == "" {
		return nil
	}
	data, err := json.Marshal(triggerMarker{TriggeredAt: now.UTC(), PR: prNumber})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	tmp := triggerMarkerPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, triggerMarkerPath(stateDir))
}

// LastTrigger reports when a self-deploy was last triggered from stateDir, the
// PR that triggered it, and whether a usable marker existed. A missing or
// malformed marker reports ok=false (no debounce — fail open toward deploying).
func LastTrigger(stateDir string) (when time.Time, pr int, ok bool) {
	if strings.TrimSpace(stateDir) == "" {
		return time.Time{}, 0, false
	}
	data, err := os.ReadFile(triggerMarkerPath(stateDir))
	if err != nil {
		return time.Time{}, 0, false
	}
	var m triggerMarker
	if err := json.Unmarshal(data, &m); err != nil || m.TriggeredAt.IsZero() {
		return time.Time{}, 0, false
	}
	return m.TriggeredAt, m.PR, true
}

// BuildSHA recovers the git commit SHA a maestro binary was built from out of
// its version string, so the orchestrator can tell whether the running binary
// still matches origin/main (#751). Two stamp formats are understood:
//
//   - release / self-deploy builds: "<semver>+g<shortsha>" — the
//     "-X main.version=<VERSION>+g<shortsha>" stamp from #682
//     (e.g. "1.4.2+gabc1234" -> "abc1234");
//   - local checkout builds: "dev-<vcsrevision>[-dirty]" — the resolveVersion
//     VCS fallback (e.g. "dev-abc123456789" -> "abc123456789").
//
// Returns "" when no SHA can be recovered (the bare "dev" sentinel, a Go
// pseudo-version, or an empty string), in which case drift cannot be
// determined and callers must not deploy on a guess.
func BuildSHA(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return ""
	}
	// "<semver>+g<shortsha>": take the segment after the last "+g".
	if i := strings.LastIndex(v, "+g"); i >= 0 {
		return strings.TrimSpace(v[i+2:])
	}
	// "dev-<rev>[-dirty]": strip the prefix and any "-dirty" suffix.
	if rest, ok := strings.CutPrefix(v, "dev-"); ok {
		rest = strings.TrimSuffix(rest, "-dirty")
		return strings.TrimSpace(rest)
	}
	return ""
}

// MainAdvanced reports whether origin/main (mainHeadSHA, a full commit SHA)
// points at a different commit than the one the running binary was built from
// (recovered from runningVersion via BuildSHA). It is the storm guard for the
// observe-merge self-deploy (#751 AC3): a reconcile that sees many
// already-merged historical PRs must NOT redeploy unless main is genuinely
// ahead of the live binary — once a deploy lands, the binary matches main and
// the drift clears. The build stamp carries a short SHA while mainHeadSHA is a
// full commit, so the match is a case-insensitive prefix check. Returns false
// (no deploy) whenever either SHA is unknown, so an undeterminable version
// never triggers a spurious deploy.
func MainAdvanced(runningVersion, mainHeadSHA string) bool {
	head := strings.ToLower(strings.TrimSpace(mainHeadSHA))
	built := strings.ToLower(BuildSHA(runningVersion))
	if head == "" || built == "" {
		return false
	}
	if len(built) > len(head) {
		return built != head
	}
	return !strings.HasPrefix(head, built)
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

	scope := cfg.SelfDeploy.EffectiveScope()

	// #742: the OUTER launcher scope must match the unit scope. #716 made only
	// the inner unit restart scope-aware; the outer launcher stayed on
	// `systemd-run --user`. On a system-scope host the orchestrator runs as a
	// system service (e.g. User=god) whose process environment lacks
	// XDG_RUNTIME_DIR/DBUS_SESSION_BUS_ADDRESS, so `systemd-run --user` cannot
	// reach the user bus and exits 1 — silently dropping the deploy. For
	// scope=system, launch the detached unit in the always-reachable system
	// manager via `sudo -n systemd-run` (non-interactive, mirroring the script's
	// privileged ops), running it as the invoking user via --uid so the build/git
	// steps keep the same uid as the user-scope path (running as root would trip
	// git's dubious-ownership guard on the user-owned checkout). Keep
	// `systemd-run --user` for the legacy user scope.
	launcher := "systemd-run"
	var args []string
	if scope == config.SelfDeployScopeSystem {
		launcher = "sudo"
		args = []string{"-n", "systemd-run", "--uid=" + deployRunAsUser()}
	} else {
		args = []string{"--user"}
	}
	args = append(args,
		"--collect",
		"--unit="+unitName,
		"--property=WorkingDirectory="+localPath,
		// Hard backstop well past the script's own deadline so a wedged
		// deploy cannot linger forever; the script handles its own
		// timeouts (and rollback) inside timeoutSec.
		"--property=RuntimeMaxSec="+strconv.Itoa(timeoutSec*2),
	)
	// The transient unit gets the systemd manager's default environment, which
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
		// #716: select the systemd unit scope inside the script. Default "user"
		// keeps the pre-migration behavior; "system" restarts system units via
		// `sudo -n systemctl` (e.g. the Loki fleet, where maestro runs as
		// User=god system units rather than per-user units).
		"--scope", scope,
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
	return launcher, args, nil
}

// deployRunAsUser returns the username the system-scope transient unit should
// run as — the invoking user — so the build/git steps keep the same uid as the
// user-scope path instead of running as root (which would trip git's
// dubious-ownership guard on the user-owned checkout, and pollute root's build
// cache). Falls back to the USER/LOGNAME env, then to the numeric uid.
func deployRunAsUser() string {
	if u, err := user.Current(); err == nil {
		if name := strings.TrimSpace(u.Username); name != "" {
			return name
		}
		if uid := strings.TrimSpace(u.Uid); uid != "" {
			return uid
		}
	}
	for _, k := range []string{"USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return strconv.Itoa(os.Getuid())
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
		return fmt.Errorf("self-deploy: %s: %w\n%s", name, err, out)
	}
	return nil
}

// TriggerFailedFinding converts a failed self-deploy *trigger* — the detached
// unit never launched (e.g. systemd-run/sudo rejected the request) — into a
// supervisor finding so a merge that silently failed to deploy surfaces as
// fleet attention instead of a lone log line (#742). Until acted on, the fleet
// keeps running the previous binary, so this must not be invisible. The next
// merge retries automatically (the trigger marker is only recorded on success).
func TriggerFailedFinding(prNumber int, triggerErr error, project string, now time.Time) state.SupervisorDecision {
	reason := "unknown error"
	if triggerErr != nil {
		reason = triggerErr.Error()
	}
	prSuffix := ""
	if prNumber > 0 {
		prSuffix = fmt.Sprintf(" after PR #%d", prNumber)
	}
	return state.SupervisorDecision{
		ID:               "self-deploy-trigger-failed-" + now.Format("20060102T150405.000000000Z"),
		CreatedAt:        now,
		Project:          project,
		Risk:             "safe",
		Confidence:       1.0,
		RequiresApproval: false,
		Status:           "failed",
		Summary:          fmt.Sprintf("self-deploy: trigger failed%s — merged change NOT deployed; fleet still on the previous binary", prSuffix),
		RecommendedAction: "the detached deploy unit never launched: check `sudo -n systemd-run`/`systemd-run --user` from the orchestrator's service context, " +
			"then redeploy by hand (a later merge retries automatically)",
		Reasons: []string{"trigger error: " + reason},
	}
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
