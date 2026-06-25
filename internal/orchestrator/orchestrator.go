package orchestrator

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mission"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/selfdeploy"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/befeast/maestro/internal/versioning"
	"github.com/befeast/maestro/internal/worker"
)

const (
	minProjectGraphQLRemaining = 100
	projectBoardSweepInterval  = 30 * time.Minute
	projectBoardSweepRetry     = 10 * time.Minute
	pipelineFullLabel          = "pipeline:full"
)

// Orchestrator coordinates all agent sessions
type Orchestrator struct {
	cfg                   *config.Config
	notifier              *notify.Notifier
	gh                    *github.Client
	router                *router.Router
	repo                  string
	binaryVersion         string
	promptBase            string
	bugPromptBase         string
	enhancementPromptBase string
	pidAliveFn            func(pid int) bool
	tmuxSessionExistsFn   func(name string) bool
	listOpenPRsFn         func() ([]github.PR, error)
	remoteBranchExistsFn  func(branch string) (bool, error)
	createPRFn            func(title, body, base, head string) (int, error)
	updatePRBodyFn        func(prNumber int, body string) error
	amendHeadFn           func(worktreePath, branch string, attribution []state.BackendAttribution, now time.Time) error
	hasOpenPRForIssueFn   func(issueNumber int) (bool, error)
	hasMergedPRForIssueFn func(issueNumber int) (bool, error)
	isPRMergedFn          func(prNumber int) (bool, error)

	// Testing hooks for checkSessions
	captureTmuxFn             func(session string) (string, error)
	tmuxCaptureFn             func(session string) (string, error)
	isIssueClosedFn           func(issueNumber int) (bool, error)
	addIssueLabelFn           func(number int, label string) error
	isRateLimitedFn           func(logFile string) bool
	rateLimitResetFromLogFn   func(logFile string) *time.Time
	authFailureFromLogFn      func(logFile string) (bool, string)
	modelUnavailableFromLogFn func(logFile string) (bool, string)
	// workerRespawnFn / respawnWorkerFn: used by respawnWorker() for dead-worker fallback (tests set one or the other)
	workerRespawnFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error
	respawnWorkerFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error
	getIssueFn      func(number int) (github.Issue, error)
	// saveCheckpointFn / respawnInPlaceFn: used for soft token threshold checkpoint+respawn
	saveCheckpointFn func(sess *state.Session) (string, error)
	respawnInPlaceFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error

	// Testing hooks for pipeline phase transitions
	workerStartPhaseFn func(cfg *config.Config, sess *state.Session, slotName, prompt, backendName string) error

	// Testing hooks for startNewWorkers
	listOpenIssuesFn func(labels []string) ([]github.Issue, error)
	workerStartFn    func(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase, backend string) (string, error)

	// Cached project board metadata and sweep cadence.
	projectField           *github.ProjectField
	projectDiscoverRetryAt time.Time
	projectRateCheckedAt   time.Time
	projectRateAllowed     bool
	projectRateRetryAt     time.Time
	projectItemsSweepAt    time.Time
	projectItemsSweepRetry time.Time

	// Mission processor (nil when missions disabled)
	missionProc *mission.Processor

	// Config hot-reload channel (nil = disabled, safe in select)
	configReloadCh <-chan *config.Config

	// Restart-required signal. Some config fields (model.default, routing.*) cannot
	// be hot-applied because the backend router is built once at startup. When such a
	// field changes during a config reload we do not apply it; instead we raise this
	// persistent flag so a long-running daemon surfaces "restart required" in
	// `maestro status` and the Fleet API instead of silently ignoring the change.
	restartRequired       bool
	restartRequiredReason string

	// Testing hooks for autoMergePRs / mergeReadyPR
	ghPRCIStatusFn               func(prNumber int) (string, error)
	ghPRMergeStatusFn            func(prNumber int) (mergeable string, mergeStateStatus string, err error)
	ghPRGreptileApprovedFn       func(prNumber int) (approved bool, pending bool, err error)
	ghPRReviewGateVerdictFn      func(prNumber int, streams []string) (github.ReviewGateVerdict, error)
	ghPRHasCriticalReviewFn      func(prNumber int) (bool, error)
	ghUpdateBranchFn             func(prNumber int) error
	ghMarkPRReadyFn              func(prNumber int) error
	ghMergePRFn                  func(prNumber int) error
	ghClosePRFn                  func(prNumber int, comment string) error
	ghPRChecksOutputFn           func(prNumber int) (string, error)
	ghCollectPRReviewFeedbackFn  func(prNumber int) (string, error)
	ghCloseIssueFn               func(number int, comment string) error
	ghPRHeadSHAFn                func(prNumber int) (string, error)
	ghCommentPRFn                func(prNumber int, body string) error
	ghPRChangedFilesFn           func(prNumber int) ([]string, error)
	ghPRVisualEvidenceAttachedFn func(prNumber int) (bool, error)
	runVisualCaptureFn           func(v config.VerifyVisualConfig, worktreePath string) ([]string, error)
	workerStopFn                 func(cfg *config.Config, slotName string, sess *state.Session) error
	selfDeployStartFn            func(prNumber int) error
	mainHeadSHAFn                func() (string, error)
	rebaseWorktreeFn             func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error
	outcomeCheckFn               func(context.Context, outcome.Brief) outcome.HealthCheckResult
	syncProjectFn                func(issueNumber int, status github.ProjectStatus) bool
	listNonDoneProjectItemsFn    func(pf *github.ProjectField) ([]github.ProjectItem, error)
	rateLimitFn                  func() (github.RateLimitStatus, error)
}

// New creates a new Orchestrator
func New(cfg *config.Config) *Orchestrator {
	n := notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
	if cfg.Telegram.DigestMode {
		n.SetDigestMode(true)
		log.Printf("[orch] digest mode enabled — notifications will be batched per cycle")
	}
	gh := github.New(cfg.Repo)
	o := &Orchestrator{
		cfg:      cfg,
		notifier: n,
		gh:       gh,
		router:   router.New(cfg),
		repo:     cfg.Repo,
	}
	if cfg.Missions.Enabled {
		o.missionProc = mission.NewProcessor(cfg, gh)
		log.Printf("[orch] mission mode enabled (max_children=%d, labels=%v)", cfg.Missions.MaxChildren, cfg.Missions.Labels)
	}
	return o
}

func (o *Orchestrator) pidAlive(pid int) bool {
	if o.pidAliveFn != nil {
		return o.pidAliveFn(pid)
	}
	return worker.IsAlive(pid)
}

func (o *Orchestrator) tmuxSessionExists(name string) bool {
	if o.tmuxSessionExistsFn != nil {
		return o.tmuxSessionExistsFn(name)
	}
	if name == "" {
		return false
	}
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func (o *Orchestrator) listOpenPRs() ([]github.PR, error) {
	if o.listOpenPRsFn != nil {
		return o.listOpenPRsFn()
	}
	return o.gh.ListOpenPRs()
}

func (o *Orchestrator) remoteBranchExists(branch string) (bool, error) {
	if o.remoteBranchExistsFn != nil {
		return o.remoteBranchExistsFn(branch)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" || o.cfg == nil || strings.TrimSpace(o.cfg.LocalPath) == "" {
		return false, nil
	}
	out, err := exec.Command("git", "-C", o.cfg.LocalPath, "ls-remote", "--exit-code", "--heads", "origin", branch).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			return false, nil
		}
		return false, fmt.Errorf("git ls-remote --heads origin %s: %w\n%s", branch, err, out)
	}
	return true, nil
}

func (o *Orchestrator) createPR(title, body, base, head string) (int, error) {
	if o.createPRFn != nil {
		return o.createPRFn(title, body, base, head)
	}
	return o.gh.CreatePR(title, body, base, head)
}

func (o *Orchestrator) updatePRBody(prNumber int, body string) error {
	if o.updatePRBodyFn != nil {
		return o.updatePRBodyFn(prNumber, body)
	}
	if o.gh == nil {
		return fmt.Errorf("no github client configured for PR body update")
	}
	return o.gh.UpdatePRBody(prNumber, body)
}

func (o *Orchestrator) hasOpenPRForIssue(issueNumber int) (bool, error) {
	if o.hasOpenPRForIssueFn != nil {
		return o.hasOpenPRForIssueFn(issueNumber)
	}
	return o.gh.HasOpenPRForIssue(issueNumber)
}

func (o *Orchestrator) hasMergedPRForIssue(issueNumber int) (bool, error) {
	if o.hasMergedPRForIssueFn != nil {
		return o.hasMergedPRForIssueFn(issueNumber)
	}
	return o.gh.HasMergedPRForIssue(issueNumber)
}

func (o *Orchestrator) isPRMerged(prNumber int) (bool, error) {
	if o.isPRMergedFn != nil {
		return o.isPRMergedFn(prNumber)
	}
	return o.gh.IsPRMerged(prNumber)
}

// SetBinaryVersion records the running binary's resolved version (e.g.
// "1.4.2+gabc1234"), set once at startup by cmd/maestro from resolveVersion().
// The observe-merge self-deploy (#751) compares its stamped SHA against
// origin/main to decide whether the live binary lags. Left unset (e.g. in
// tests), the drift-based trigger stays off.
func (o *Orchestrator) SetBinaryVersion(v string) {
	o.binaryVersion = strings.TrimSpace(v)
}

// SetSelfDeployStartFn installs a custom self-deploy launcher, replacing the
// default in-process selfdeploy.Trigger. The daemon (#758) wires this to its
// centralized, cross-flow-debounced RequestSelfDeploy so N flows merging PRs
// near-simultaneously launch exactly ONE deploy of ONE unit instead of each
// flow firing its own — a thundering herd that bounces the fleet mid-verify
// (#722). A launcher that returns selfdeploy.ErrDebounced signals the request
// was dropped by the central debounce, which the trigger paths treat as a
// benign skip rather than a failure.
func (o *Orchestrator) SetSelfDeployStartFn(fn func(prNumber int) error) {
	o.selfDeployStartFn = fn
}

// mainHeadSHA returns origin/main's current head commit SHA.
func (o *Orchestrator) mainHeadSHA() (string, error) {
	if o.mainHeadSHAFn != nil {
		return o.mainHeadSHAFn()
	}
	if o.gh == nil {
		return "", fmt.Errorf("no github client configured for main head sha")
	}
	return o.gh.BranchHeadSHA("main")
}

func (o *Orchestrator) prCIStatus(prNumber int) (string, error) {
	if o.ghPRCIStatusFn != nil {
		return o.ghPRCIStatusFn(prNumber)
	}
	return o.gh.PRCIStatus(prNumber)
}

// prMergeStatus returns GitHub's per-PR mergeable verdict together with the
// raw mergeable_state ("clean" / "unstable" / "blocked" / "behind" / "dirty"
// / "" / "unknown" / "draft" / "has_hooks"). It mirrors the supervisor's
// prMergeStateReader and is consulted by autoMergePRs when the aggregate
// PRCIStatus sticks at "pending" — GitHub's own required-checks verdict is
// the authoritative override for legacy commit-status drift (#424).
func (o *Orchestrator) prMergeStatus(prNumber int) (string, string, error) {
	if o.ghPRMergeStatusFn != nil {
		return o.ghPRMergeStatusFn(prNumber)
	}
	if o.gh == nil {
		return "", "", fmt.Errorf("no github client configured for merge-status")
	}
	return o.gh.PRMergeStatus(prNumber)
}

func (o *Orchestrator) prGreptileApproved(prNumber int) (bool, bool, error) {
	if o.ghPRGreptileApprovedFn != nil {
		return o.ghPRGreptileApprovedFn(prNumber)
	}
	return o.gh.PRGreptileApproved(prNumber)
}

func (o *Orchestrator) prReviewGateVerdict(prNumber int) (github.ReviewGateVerdict, error) {
	streams := o.cfg.EffectiveReviewGateStreams()
	if len(streams) == 0 {
		return github.ReviewGateVerdict{Passed: true}, nil
	}
	if len(streams) == 1 && streams[0] == "greptile" {
		approved, pending, err := o.prGreptileApproved(prNumber)
		if err != nil {
			return github.ReviewGateVerdict{}, err
		}
		return github.ReviewGateVerdict{
			Passed:  approved && !pending,
			Pending: pending,
			Streams: []github.ReviewStreamVerdict{{
				Name:    "greptile",
				Passed:  approved,
				Pending: pending,
			}},
		}, nil
	}
	if o.ghPRReviewGateVerdictFn != nil {
		return o.ghPRReviewGateVerdictFn(prNumber, streams)
	}
	return o.gh.PRReviewGateVerdict(prNumber, streams)
}

func (o *Orchestrator) prHeadSHA(prNumber int) (string, error) {
	if o.ghPRHeadSHAFn != nil {
		return o.ghPRHeadSHAFn(prNumber)
	}
	if o.gh == nil {
		return "", fmt.Errorf("no github client configured for head SHA")
	}
	return o.gh.PRHeadSHA(prNumber)
}

func (o *Orchestrator) commentPR(prNumber int, body string) error {
	if o.ghCommentPRFn != nil {
		return o.ghCommentPRFn(prNumber, body)
	}
	if o.gh == nil {
		return fmt.Errorf("no github client configured for PR comment")
	}
	return o.gh.CommentPR(prNumber, body)
}

func (o *Orchestrator) prHasCriticalReview(prNumber int) (bool, error) {
	if o.ghPRHasCriticalReviewFn != nil {
		return o.ghPRHasCriticalReviewFn(prNumber)
	}
	if o.gh == nil {
		// Fail safe: cannot determine criticality -> caller must NOT auto-merge.
		return false, fmt.Errorf("no github client configured for critical-review check")
	}
	return o.gh.PRHasCriticalReviewOnHead(prNumber)
}

func (o *Orchestrator) updateBranch(prNumber int) error {
	if o.ghUpdateBranchFn != nil {
		return o.ghUpdateBranchFn(prNumber)
	}
	if o.gh == nil {
		return fmt.Errorf("no github client configured for update-branch")
	}
	return o.gh.UpdateBranch(prNumber)
}

func (o *Orchestrator) markPRReady(prNumber int) error {
	if o.ghMarkPRReadyFn != nil {
		return o.ghMarkPRReadyFn(prNumber)
	}
	if o.gh == nil {
		return fmt.Errorf("no github client configured for mark-pr-ready")
	}
	return o.gh.MarkPRReady(prNumber)
}

func (o *Orchestrator) mergePR(prNumber int) error {
	if o.ghMergePRFn != nil {
		return o.ghMergePRFn(prNumber)
	}
	return o.gh.MergePR(prNumber)
}

func (o *Orchestrator) closePR(prNumber int, comment string) error {
	if o.ghClosePRFn != nil {
		return o.ghClosePRFn(prNumber, comment)
	}
	if o.gh == nil {
		return fmt.Errorf("no github client configured for close-pr")
	}
	return o.gh.ClosePR(prNumber, comment)
}

func (o *Orchestrator) prChecksOutput(prNumber int) (string, error) {
	if o.ghPRChecksOutputFn != nil {
		return o.ghPRChecksOutputFn(prNumber)
	}
	return o.gh.PRChecksOutput(prNumber)
}

func (o *Orchestrator) collectPRReviewFeedback(prNumber int) (string, error) {
	if o.ghCollectPRReviewFeedbackFn != nil {
		return o.ghCollectPRReviewFeedbackFn(prNumber)
	}
	return o.gh.CollectPRReviewFeedback(prNumber)
}

func (o *Orchestrator) closeIssue(number int, comment string) error {
	if o.ghCloseIssueFn != nil {
		return o.ghCloseIssueFn(number, comment)
	}
	if o.gh == nil {
		return fmt.Errorf("no github client configured for close-issue")
	}
	return o.gh.CloseIssue(number, comment)
}

func (o *Orchestrator) stopWorker(slotName string, sess *state.Session) error {
	if o.workerStopFn != nil {
		return o.workerStopFn(o.cfg, slotName, sess)
	}
	return worker.Stop(o.cfg, slotName, sess)
}

func (o *Orchestrator) getIssue(number int) (github.Issue, error) {
	if o.getIssueFn != nil {
		return o.getIssueFn(number)
	}
	return o.gh.GetIssue(number)
}

func (o *Orchestrator) respawnWorker(slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	// Support both hook names for test compatibility (respawnWorkerFn = branch, workerRespawnFn = HEAD)
	if o.respawnWorkerFn != nil {
		return o.respawnWorkerFn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	if o.workerRespawnFn != nil {
		return o.workerRespawnFn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	return worker.Respawn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
}

func (o *Orchestrator) saveCheckpoint(sess *state.Session) (string, error) {
	if o.saveCheckpointFn != nil {
		return o.saveCheckpointFn(sess)
	}
	return worker.SaveCheckpoint(sess)
}

func (o *Orchestrator) checkOutcome(ctx context.Context) outcome.HealthCheckResult {
	if o.outcomeCheckFn != nil {
		return o.outcomeCheckFn(ctx, o.cfg.Outcome)
	}
	return outcome.Checker{}.Check(ctx, o.cfg.Outcome)
}

func (o *Orchestrator) rateLimit() (github.RateLimitStatus, error) {
	if o.rateLimitFn != nil {
		return o.rateLimitFn()
	}
	return o.gh.RateLimit()
}

func (o *Orchestrator) respawnInPlace(slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	if o.respawnInPlaceFn != nil {
		return o.respawnInPlaceFn(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	return worker.RespawnInPlace(o.cfg, slotName, sess, o.repo, issue, promptBase, backendName)
}

func (o *Orchestrator) rebaseWorktree(worktreePath, branch string) error {
	if o.rebaseWorktreeFn != nil {
		return o.rebaseWorktreeFn(worktreePath, branch, o.cfg.AutoResolveFiles, o.cfg.AutoRestoreFiles)
	}
	return worker.RebaseWorktree(worktreePath, branch, o.cfg.AutoResolveFiles, o.cfg.AutoRestoreFiles)
}

func (o *Orchestrator) captureTmux(session string) (string, error) {
	if o.tmuxCaptureFn != nil {
		return o.tmuxCaptureFn(session)
	}
	if o.captureTmuxFn != nil {
		return o.captureTmuxFn(session)
	}
	return tmuxCapture(session)
}

func (o *Orchestrator) isIssueClosed(number int) (bool, error) {
	if o.isIssueClosedFn != nil {
		return o.isIssueClosedFn(number)
	}
	if o.gh == nil {
		return false, fmt.Errorf("no github client configured for issue-closed check")
	}
	return o.gh.IsIssueClosed(number)
}

func (o *Orchestrator) addIssueLabel(number int, label string) error {
	if o.addIssueLabelFn != nil {
		return o.addIssueLabelFn(number, label)
	}
	return o.gh.AddIssueLabel(number, label)
}

func (o *Orchestrator) isRateLimited(logFile string) bool {
	if o.isRateLimitedFn != nil {
		return o.isRateLimitedFn(logFile)
	}
	return worker.IsRateLimited(logFile)
}

// rateLimitResetFromLog reads a dead worker's log file and parses the
// provider-stated reset time ("try again at ...") if present. It returns nil
// when the log is unreadable or carries no parseable reset hint.
//
// Per issue #663, a nil result from this function is the orchestrator's signal
// that the rate-limit detection is LOW-confidence: the classifier matched a
// pattern but the provider did not state when the limit resets, so the match
// may well be a stale prompt-context echo or a transient tool/exec error.
// Callers that switch backends on rate-limit signals MUST treat nil as "do
// not fall back" and let the ordinary worker-died path handle the session.
func (o *Orchestrator) rateLimitResetFromLog(logFile string) *time.Time {
	if o.rateLimitResetFromLogFn != nil {
		return o.rateLimitResetFromLogFn(logFile)
	}
	if logFile == "" {
		return nil
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		return nil
	}
	if reset, ok := worker.ParseRateLimitReset(string(data)); ok {
		return &reset
	}
	return nil
}

// providerRateLimitFromLog returns the high-confidence rate-limit verdict for
// a dead worker's log. It reports hit=true only when the classifier matches
// AND a provider-stated reset window is parseable — a positive rate-limit
// signal in the sense of issue #663. A classifier match without a reset hint
// is treated as low-confidence and reported as hit=false, so the caller falls
// through to the ordinary worker-died handling instead of triggering a false
// backend fallback. The non-nil resetAt is also returned so callers can
// stamp BackendHealth.RetryAfter without re-reading the log.
func (o *Orchestrator) providerRateLimitFromLog(logFile string) (hit bool, resetAt *time.Time) {
	if !o.isRateLimited(logFile) {
		return false, nil
	}
	reset := o.rateLimitResetFromLog(logFile)
	if reset == nil {
		return false, nil
	}
	return true, reset
}

// backendAuthFailureWindow bounds how long after spawn an auth-error log
// signature is trusted as a backend credential failure (#693). An
// unauthenticated CLI dies on its first API call — within a couple of
// minutes of spawn (live case: claude 401 killed every worker ~2 minutes
// in) — so an auth signature in a longer-lived worker's log is more likely
// incidental work content (the worker reading or writing auth-related code)
// than a backend outage, and is left to the ordinary retry path.
const backendAuthFailureWindow = 10 * time.Minute

// backendAuthFailureFromLog reports whether a dead worker died because its
// backend could not authenticate (e.g. "Failed to authenticate. API Error:
// 401 Invalid authentication credentials"). It returns hit=true only when
// the worker died within backendAuthFailureWindow of its spawn AND the log
// tail carries a known auth-failure signature. Such a death is a backend
// failure, not a work failure: callers must gate the backend, preserve the
// per-issue retry budget, and respawn the attempt on a fallback backend.
func (o *Orchestrator) backendAuthFailureFromLog(sess *state.Session, now time.Time) (bool, string) {
	if sess == nil {
		return false, ""
	}
	if sess.StartedAt.IsZero() || now.Sub(sess.StartedAt) > backendAuthFailureWindow {
		return false, ""
	}
	if o.authFailureFromLogFn != nil {
		return o.authFailureFromLogFn(sess.LogFile)
	}
	return worker.IsAuthFailure(sess.LogFile)
}

// backendModelUnavailableFromLog reports whether a dead worker died because
// its backend's configured model is unavailable — pulled, renamed, or not
// accessible to the account (#713; live: claude CLI "There's an issue with
// the selected model (claude-fable-5). It may not exist or you may not have
// access to it."). Like the auth-failure detector it is trusted only within
// the early-death window: an unusable model dies on the CLI's first API call,
// while the same signature in a longer-lived worker's log is more likely
// incidental work content than a backend outage.
func (o *Orchestrator) backendModelUnavailableFromLog(sess *state.Session, now time.Time) (bool, string) {
	if sess == nil {
		return false, ""
	}
	if sess.StartedAt.IsZero() || now.Sub(sess.StartedAt) > backendAuthFailureWindow {
		return false, ""
	}
	if o.modelUnavailableFromLogFn != nil {
		return o.modelUnavailableFromLogFn(sess.LogFile)
	}
	return worker.IsModelUnavailable(sess.LogFile)
}

// classifyBackendFailure inspects a dead worker's log for any hard backend
// failure that must not burn the per-issue retry budget. It returns the gating
// reason (state.BackendBlockAuthFailure or state.BackendBlockModelUnavailable)
// and the matched signature label. Auth is checked first: a 401 and a model
// 404 can co-occur in a noisy log, and a credential outage is the more common
// recovery path (re-login / cred-sync) than a config change. hit=false leaves
// the death to the ordinary retry path.
func (o *Orchestrator) classifyBackendFailure(sess *state.Session, now time.Time) (hit bool, reason string, pattern string) {
	if ok, label := o.backendAuthFailureFromLog(sess, now); ok {
		return true, state.BackendBlockAuthFailure, label
	}
	if ok, label := o.backendModelUnavailableFromLog(sess, now); ok {
		return true, state.BackendBlockModelUnavailable, label
	}
	return false, "", ""
}

func (o *Orchestrator) workerLogFile(slotName string, sess *state.Session) string {
	if sess == nil {
		return ""
	}
	if logFile := strings.TrimSpace(sess.LogFile); logFile != "" {
		return logFile
	}
	if o != nil && o.cfg != nil && strings.TrimSpace(o.cfg.StateDir) != "" {
		return filepath.Join(state.LogDir(o.cfg.StateDir), slotName+".log")
	}
	return ""
}

func (o *Orchestrator) updateTokensUsedFromOutput(slotName string, sess *state.Session, output string) bool {
	if sess == nil {
		return false
	}
	// #730: Pi-backed sessions stream a newline-delimited JSON event
	// stream that carries provider/model + usage + cost. Parse it
	// directly so model/tokens/cost_usd get attributed instead of the
	// generic token regex (which sees none of the JSON shapes).
	kind := o.backendKindForSession(sess)
	if kind == config.BackendKindPi {
		return o.updatePiUsageFromOutput(slotName, sess, output)
	}
	// #737: a claude session with usage_stream opted in carries its usage on a
	// side-channel slot.jsonl (the human-readable slot.log has no parseable
	// token total), so the passed `output` (tmux pane / slot.log) is ignored
	// in favour of the raw NDJSON the stream-splitter appended. Without the
	// opt-in, claude stays on the generic text parser (legacy behaviour).
	if kind == config.BackendKindClaude && o.sessionUsageStream(sess) {
		return o.updateClaudeUsageFromJSONL(slotName, sess)
	}
	// #738: a codex session with usage_stream opted in carries its usage on a
	// side-channel slot.jsonl (`codex exec --json`); the human-readable slot.log
	// has no parseable total, so the passed `output` is ignored in favour of the
	// raw NDJSON. Without the opt-in, codex stays on the legacy text parser
	// (the two-line "tokens used\n89,655" regex below).
	if kind == config.BackendKindCodex && o.sessionUsageStream(sess) {
		return o.updateCodexUsageFromJSONL(slotName, sess)
	}
	tokens := worker.ParseTokensFromOutput(output)
	if tokens <= sess.TokensUsedAttempt {
		return false
	}
	delta := tokens - sess.TokensUsedAttempt
	sess.TokensUsedAttempt = tokens
	sess.TokensUsedTotal += delta
	log.Printf("[orch] %s tokens_used updated: attempt=%d total=%d", slotName, tokens, sess.TokensUsedTotal)
	return true
}

// updatePiUsageFromOutput parses a Pi --mode json event stream from the
// worker log and stamps model/tokens/cost_usd onto the session. Tokens use
// the run-total (sum across turns); model is the provider-reported model;
// cost is Pi's self-reported cost.total. Returns true when anything changed.
func (o *Orchestrator) updatePiUsageFromOutput(slotName string, sess *state.Session, output string) bool {
	usage, ok := worker.ParsePiUsage(output)
	if !ok {
		return false
	}
	changed := false
	// #730: Pi re-parses the full appended slot log on each call, so
	// usage.TotalTokens is the cumulative run total across every attempt.
	// On a respawn, the runner keeps appending to the same slot log while
	// TokensUsedAttempt resets to 0 — comparing against TokensUsedAttempt
	// would re-add the prior attempts' tokens. Use a separate monotonic
	// watermark (UsageTokensWatermark) that persists across respawns so only
	// the new attempt's tokens are counted.
	if usage.TotalTokens > sess.UsageTokensWatermark {
		delta := usage.TotalTokens - sess.UsageTokensWatermark
		sess.UsageTokensWatermark = usage.TotalTokens
		sess.TokensUsedAttempt += delta
		sess.TokensUsedTotal += delta
		// #739: stamp the cache-aware split so the cost panel can price each
		// dimension. The full-log parse is cumulative, so assigning (not
		// accumulating) tracks the run total and is respawn-safe alongside the
		// watermark guard above.
		sess.TokensInput = usage.Input
		sess.TokensOutput = usage.Output
		sess.TokensCacheRead = usage.CacheRead
		sess.TokensCacheWrite = usage.CacheWrite
		changed = true
	}
	if strings.TrimSpace(usage.Model) != "" && strings.TrimSpace(sess.Model) == "" {
		sess.Model = usage.Model
		changed = true
	}
	// #730: cost uses the same monotonic watermark as tokens — the full-log
	// parse returns the cumulative run cost, so guard with `>` instead of a
	// first-non-zero freeze (which would never update after the first turn).
	if usage.CostUSD > sess.CostUSDBackend {
		sess.CostUSDBackend = usage.CostUSD
		changed = true
	}
	if changed {
		log.Printf("[orch] %s pi usage: model=%s tokens=%d cost=$%.4f (total=%d)",
			slotName, usage.Model, usage.TotalTokens, usage.CostUSD, sess.TokensUsedTotal)
	}
	return changed
}

// workerJSONLFile returns the side-channel NDJSON path the claude
// stream-splitter appends to (slot.jsonl), derived from the worker log path
// (slot.log). Empty when no log path can be resolved.
func (o *Orchestrator) workerJSONLFile(slotName string, sess *state.Session) string {
	logFile := o.workerLogFile(slotName, sess)
	if logFile == "" {
		return ""
	}
	return worker.JSONLPathForLog(logFile)
}

// updateClaudeUsageFromJSONL parses the claude stream-json side-channel
// (slot.jsonl) and stamps model/tokens/cost_usd onto the session (#737).
// Like the Pi path it re-parses the full appended jsonl on each call, so
// ParseClaudeUsage returns the cumulative run total across every attempt;
// the monotonic UsageTokensWatermark persists across respawns so only the
// new attempt's tokens are added (no double-count on retry/respawn). Cost
// prefers the backend-reported total_cost_usd. Returns true when anything
// changed; false (usage stays 0) when the jsonl is absent — the documented
// degradation when the stream-splitter was unavailable.
func (o *Orchestrator) updateClaudeUsageFromJSONL(slotName string, sess *state.Session) bool {
	jsonlPath := o.workerJSONLFile(slotName, sess)
	if jsonlPath == "" {
		return false
	}
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		// No side-channel stream (splitter disabled/unavailable, or the
		// worker has not emitted a frame yet) — leave tokens at 0.
		return false
	}
	usage, ok := worker.ParseClaudeUsage(string(data))
	if !ok {
		return false
	}
	changed := false
	if usage.TotalTokens > sess.UsageTokensWatermark {
		delta := usage.TotalTokens - sess.UsageTokensWatermark
		sess.UsageTokensWatermark = usage.TotalTokens
		sess.TokensUsedAttempt += delta
		sess.TokensUsedTotal += delta
		// #739: stamp the cache-aware split (input/output/cache_read/cache_write)
		// so the cost panel can price the cache-read discount. The full-jsonl
		// parse is cumulative, so assigning the run totals is respawn-safe
		// alongside the watermark guard.
		sess.TokensInput = usage.Input
		sess.TokensOutput = usage.Output
		sess.TokensCacheRead = usage.CacheRead
		sess.TokensCacheWrite = usage.CacheWrite
		changed = true
	}
	if strings.TrimSpace(usage.Model) != "" && strings.TrimSpace(sess.Model) == "" {
		sess.Model = usage.Model
		changed = true
	}
	// total_cost_usd is the run-cumulative the full-jsonl parse sums across
	// every result frame, so guard with `>` (mirrors the Pi cost fix in #732):
	// a first-non-zero freeze would never update past the first attempt.
	if usage.CostUSD > sess.CostUSDBackend {
		sess.CostUSDBackend = usage.CostUSD
		changed = true
	}
	if changed {
		log.Printf("[orch] %s claude usage: model=%s tokens=%d cost=$%.4f (total=%d)",
			slotName, usage.Model, usage.TotalTokens, usage.CostUSD, sess.TokensUsedTotal)
	}
	return changed
}

// updateCodexUsageFromJSONL parses the codex `exec --json` side-channel
// (slot.jsonl) and stamps tokens onto the session (#738). codex emits one
// terminal turn.completed usage event per `codex exec` invocation; the
// splitter appends each attempt's frames to the same slot.jsonl, so
// ParseCodexUsage sums them to the cumulative run total. The monotonic
// UsageTokensWatermark persists across respawns so a forced retry never
// double-counts (mirrors the claude/Pi paths). codex reports no USD, so cost
// stays virtual: CostUSDBackend is left at 0 and sessionCostEstimate supplies
// the dollar figure from the configured pricing block. codex --json carries no
// model name, so sess.Model is left as configured. Returns true when tokens
// changed; false (tokens stay 0) when the jsonl is absent — the documented
// degradation when the stream-splitter was unavailable.
func (o *Orchestrator) updateCodexUsageFromJSONL(slotName string, sess *state.Session) bool {
	jsonlPath := o.workerJSONLFile(slotName, sess)
	if jsonlPath == "" {
		return false
	}
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		// No side-channel stream (splitter disabled/unavailable, or the
		// worker has not emitted a turn.completed yet) — leave tokens at 0.
		return false
	}
	usage, ok := worker.ParseCodexUsage(string(data))
	if !ok {
		return false
	}
	if usage.TotalTokens <= sess.UsageTokensWatermark {
		return false
	}
	delta := usage.TotalTokens - sess.UsageTokensWatermark
	sess.UsageTokensWatermark = usage.TotalTokens
	sess.TokensUsedAttempt += delta
	sess.TokensUsedTotal += delta
	log.Printf("[orch] %s codex usage: input=%d output=%d cache_read=%d tokens=%d (total=%d)",
		slotName, usage.Input, usage.Output, usage.CacheRead, usage.TotalTokens, sess.TokensUsedTotal)
	return true
}

// sessionUsageStream reports whether the session's backend opted into
// structured usage capture (usage_stream). Only then do the claude (#737) and
// codex (#738) paths read the slot.jsonl side channel; otherwise they stay on
// the generic text parser.
func (o *Orchestrator) sessionUsageStream(sess *state.Session) bool {
	if sess == nil || o == nil || o.cfg == nil {
		return false
	}
	if def, ok := o.cfg.Model.Backends[strings.TrimSpace(sess.Backend)]; ok {
		return def.UsageStream
	}
	return false
}

// backendKindForSession resolves the CLI exec-path kind for a session's
// backend from the configured backend def, so Pi (and other provider-mapped
// backends) get their first-class usage parser instead of the generic one.
func (o *Orchestrator) backendKindForSession(sess *state.Session) string {
	if sess == nil {
		return ""
	}
	name := strings.TrimSpace(sess.Backend)
	var provider, cmd string
	if o != nil && o.cfg != nil {
		if def, ok := o.cfg.Model.Backends[name]; ok {
			provider = def.Provider
			cmd = def.Cmd
		}
	}
	return config.ResolveBackendKind(name, provider, cmd)
}

func (o *Orchestrator) updateTokensUsedFromWorkerLog(slotName string, sess *state.Session) bool {
	logFile := o.workerLogFile(slotName, sess)
	if logFile == "" {
		return false
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		log.Printf("[orch] warn: could not read worker log for token capture %s (%s): %v", slotName, logFile, err)
		return false
	}
	return o.updateTokensUsedFromOutput(slotName, sess, string(data))
}

// runAfterRunHook executes the after_run hook for a session (best-effort, never fatal).
func (o *Orchestrator) runAfterRunHook(sess *state.Session) {
	if o.cfg.Hooks.AfterRun == "" {
		return
	}
	env := worker.HookEnv{
		IssueID:       fmt.Sprintf("%s#%d", o.cfg.Repo, sess.IssueNumber),
		IssueNumber:   sess.IssueNumber,
		WorkspacePath: sess.Worktree,
	}
	if err := worker.RunHook(o.cfg, "after_run", o.cfg.Hooks.AfterRun, env); err != nil {
		log.Printf("[orch] after_run hook failed for issue #%d: %v", sess.IssueNumber, err)
	}
}

// nextFallbackBackend returns the next untried backend from the fallback list.
// It skips backends that are already in sess.TriedBackends or match the current backend.
// Returns "" if no fallback is available.
func (o *Orchestrator) nextFallbackBackend(sess *state.Session) string {
	fallbacks := o.cfg.Model.FallbackBackends
	if len(fallbacks) == 0 {
		return ""
	}

	tried := make(map[string]bool, len(sess.TriedBackends)+1)
	tried[sess.Backend] = true
	for _, b := range sess.TriedBackends {
		tried[b] = true
	}

	for _, fb := range fallbacks {
		if !tried[fb] {
			// Verify the backend exists in config
			if _, ok := o.cfg.Model.Backends[fb]; ok {
				return fb
			}
		}
	}
	return ""
}

// ensureProjectField discovers the project board field if not cached.
func (o *Orchestrator) ensureProjectField() {
	if !o.cfg.GitHubProjects.Enabled || o.cfg.GitHubProjects.ProjectNumber == 0 {
		return
	}
	if o.projectField != nil {
		return
	}
	if !o.projectGraphQLBudgetAvailable() {
		return
	}
	if !o.projectDiscoverRetryAt.IsZero() && time.Now().Before(o.projectDiscoverRetryAt) {
		return
	}
	pf, err := o.gh.DiscoverProject(o.cfg.GitHubProjects.ProjectNumber)
	if err != nil {
		o.projectDiscoverRetryAt = time.Now().Add(10 * time.Minute)
		log.Printf("[orch] discover project: %v (backing off until %s)", err, o.projectDiscoverRetryAt.Format(time.RFC3339))
		return
	}
	o.projectField = pf
	o.projectDiscoverRetryAt = time.Time{}
}

func (o *Orchestrator) projectGraphQLBudgetAvailable() bool {
	now := time.Now()
	if !o.projectRateCheckedAt.IsZero() && now.Sub(o.projectRateCheckedAt) < time.Minute {
		return o.projectRateAllowed
	}
	if !o.projectRateRetryAt.IsZero() && now.Before(o.projectRateRetryAt) {
		o.projectRateCheckedAt = now
		o.projectRateAllowed = false
		return false
	}
	rate, err := o.rateLimit()
	o.projectRateCheckedAt = now
	if err != nil {
		o.projectRateAllowed = false
		o.projectRateRetryAt = now.Add(time.Minute)
		log.Printf("[projects] could not read GitHub rate budget; skipping ProjectV2 sync until %s: %v", o.projectRateRetryAt.Format(time.RFC3339), err)
		return false
	}
	if rate.GraphQL.Remaining <= minProjectGraphQLRemaining {
		retryAt := now.Add(10 * time.Minute)
		if rate.GraphQL.Reset > 0 {
			resetAt := time.Unix(int64(rate.GraphQL.Reset), 0)
			if resetAt.After(now) {
				retryAt = resetAt
			}
		}
		o.projectRateAllowed = false
		o.projectRateRetryAt = retryAt
		log.Printf("[projects] GraphQL budget too low for ProjectV2 sync (remaining=%d <= %d, used=%d/%d); backing off until %s",
			rate.GraphQL.Remaining, minProjectGraphQLRemaining, rate.GraphQL.Used, rate.GraphQL.Limit, retryAt.Format(time.RFC3339))
		return false
	}
	o.projectRateAllowed = true
	o.projectRateRetryAt = time.Time{}
	return true
}

// syncProject syncs an issue's status to the configured GitHub Project board.
// No-op if github_projects is not enabled.
func (o *Orchestrator) syncProject(issueNumber int, status github.ProjectStatus) bool {
	if !o.cfg.GitHubProjects.Enabled || o.cfg.GitHubProjects.ProjectNumber == 0 {
		return false
	}
	if o.syncProjectFn != nil {
		if !o.projectGraphQLBudgetAvailable() {
			return false
		}
		return o.syncProjectFn(issueNumber, status)
	}
	o.ensureProjectField()
	if o.projectField == nil {
		return false
	}
	candidates := github.ProjectStatusCandidates(status)
	if len(candidates) == 0 {
		candidates = []string{string(status)}
	}
	if !github.HasProjectStatusCandidate(o.projectField, candidates) {
		log.Printf("[projects] none of statuses %v found in project; treating issue #%d status=%q as handled", candidates, issueNumber, status)
		return true
	}
	if !o.projectGraphQLBudgetAvailable() {
		return false
	}
	return o.gh.TrySyncIssueStatusOneOf(o.projectField, issueNumber, candidates...)
}

func (o *Orchestrator) listNonDoneProjectItems(pf *github.ProjectField) ([]github.ProjectItem, error) {
	if o.listNonDoneProjectItemsFn != nil {
		return o.listNonDoneProjectItemsFn(pf)
	}
	return o.gh.ListNonDoneProjectItems(pf)
}

func (o *Orchestrator) projectBoardSweepDue(now time.Time) bool {
	if !o.projectItemsSweepRetry.IsZero() && now.Before(o.projectItemsSweepRetry) {
		return false
	}
	if !o.projectItemsSweepAt.IsZero() && now.Sub(o.projectItemsSweepAt) < projectBoardSweepInterval {
		return false
	}
	return true
}

// projectStatusForSession returns the ProjectStatus that should mirror this
// session on the GitHub Project board, plus whether a sync should happen.
// Sessions that have no clear board-level status (transient/unknown) return
// false so the existing board state is left alone.
//
// Mapping:
//   - running               => In Progress
//   - queued / pr_open      => In Review
//   - code_landed           => Deploying or Live Verification (depending on
//     whether the outcome contract requires deploy)
//   - done                  => Done
//   - retry_exhausted /
//     conflict_failed /
//     failed                => Blocked
//   - dead with no retry    => Blocked
//   - dead awaiting retry   => In Progress (work is still active)
func projectStatusForSession(sess *state.Session, requiresDeploy bool) (github.ProjectStatus, bool) {
	if sess == nil {
		return "", false
	}
	switch sess.Status {
	case state.StatusRunning:
		return github.ProjectStatusInProgress, true
	case state.StatusQueued, state.StatusPROpen:
		return github.ProjectStatusInReview, true
	case state.StatusCodeLanded:
		return codeLandedProjectStatusForSession(sess, requiresDeploy), true
	case state.StatusDone:
		return github.ProjectStatusDone, true
	case state.StatusRetryExhausted, state.StatusConflictFailed, state.StatusFailed:
		return github.ProjectStatusBlocked, true
	case state.StatusDead:
		if sess.NextRetryAt != nil {
			return github.ProjectStatusInProgress, true
		}
		return github.ProjectStatusBlocked, true
	default:
		return "", false
	}
}

// reconcileProjectBoard reconciles the GitHub Project board against Maestro
// state so the board never contradicts the runtime:
//   - every active session (running/queued/pr_open/code_landed/blocked) has its
//     status mirrored on the board so a live worker is always visible as
//     active work (no manual "panoptikon-project-sync" timer needed);
//   - ANY item whose underlying issue is closed moves to Done, regardless of
//     its current board Status — including NO STATUS / unset items and
//     externally-closed (merge-keyword or manual) issues the reconcile never
//     transitioned itself. Closed wins over the no-status branch below, so
//     stranded closed items are backfilled instead of bounced to Todo (#741);
//   - items with no Status that are still open fall back to Todo so they show
//     up in the backlog.
//
// "Done" is only set on the board when the underlying GitHub issue is closed —
// merging a PR alone keeps the item in In Progress until runtime/deploy
// verification closes the issue (see markCodeLanded). This preserves the
// merge-then-verify policy required by ops.
func (o *Orchestrator) reconcileProjectBoard(s *state.State) bool {
	if !o.cfg.GitHubProjects.Enabled || o.cfg.GitHubProjects.ProjectNumber == 0 {
		return false
	}
	o.ensureProjectField()
	if o.projectField == nil {
		return false
	}

	changed := o.reconcileSessionsToProjectBoard(s)

	now := time.Now().UTC()
	if !o.projectBoardSweepDue(now) {
		return changed
	}
	if !o.projectGraphQLBudgetAvailable() {
		return changed
	}

	items, err := o.listNonDoneProjectItems(o.projectField)
	if err != nil {
		o.projectItemsSweepRetry = now.Add(projectBoardSweepRetry)
		log.Printf("[orch] reconcile project board: %v", err)
		return changed
	}
	o.projectItemsSweepAt = now
	o.projectItemsSweepRetry = time.Time{}

	for _, item := range items {
		if item.IssueClosed {
			log.Printf("[orch] reconcile: issue #%d is closed, moving to Done", item.IssueNumber)
			if o.syncProject(item.IssueNumber, github.ProjectStatusDone) {
				s.MarkProjectStatusSynced(item.IssueNumber, string(github.ProjectStatusDone), time.Now().UTC())
				changed = true
			}
		} else if !item.HasStatus {
			log.Printf("[orch] reconcile: issue #%d has no status, setting to Todo", item.IssueNumber)
			if o.syncProject(item.IssueNumber, github.ProjectStatusTodo) {
				s.MarkProjectStatusSynced(item.IssueNumber, string(github.ProjectStatusTodo), time.Now().UTC())
				changed = true
			}
		}
	}
	return changed
}

// reconcileSessionsToProjectBoard pushes each Maestro session's status onto the
// project board so the board mirrors the runtime even when an earlier sync was
// missed (project discovery failed, network blip, board reset, …).
func (o *Orchestrator) reconcileSessionsToProjectBoard(s *state.State) bool {
	if s == nil {
		return false
	}
	changed := false
	// Pick the freshest session per issue so a running worker always wins over
	// an older terminal record for the same issue.
	freshest := make(map[int]*state.Session, len(s.Sessions))
	for _, sess := range s.Sessions {
		if sess == nil || sess.IssueNumber <= 0 {
			continue
		}
		current, ok := freshest[sess.IssueNumber]
		if !ok {
			freshest[sess.IssueNumber] = sess
			continue
		}
		if sessionRecency(sess).After(sessionRecency(current)) {
			freshest[sess.IssueNumber] = sess
		}
	}
	for issue, sess := range freshest {
		status, ok := projectStatusForSession(sess, o.cfg.Outcome.RequiresDeploy)
		if !ok {
			continue
		}
		// Done is only set elsewhere (closed issue or merge-then-close
		// reconciliation), so skip Done here even if the session is marked
		// done — the closed-issue pass below will confirm it.
		if status == github.ProjectStatusDone {
			continue
		}
		if s.ProjectStatusSynced(issue, string(status)) {
			continue
		}
		if o.syncProject(issue, status) {
			s.MarkProjectStatusSynced(issue, string(status), time.Now().UTC())
			changed = true
		}
	}
	return changed
}

func codeLandedProjectStatus(requiresDeploy bool) github.ProjectStatus {
	if requiresDeploy {
		return github.ProjectStatusDeploying
	}
	return github.ProjectStatusLiveVerify
}

func codeLandedProjectStatusForSession(sess *state.Session, requiresDeploy bool) github.ProjectStatus {
	if requiresDeploy && sess != nil && sess.DeploymentFinishedAt != nil {
		return github.ProjectStatusLiveVerify
	}
	return codeLandedProjectStatus(requiresDeploy)
}

func sessionRecency(sess *state.Session) time.Time {
	if sess == nil {
		return time.Time{}
	}
	candidates := []time.Time{sess.LastOutputChangedAt, sess.StartedAt}
	if sess.FinishedAt != nil {
		candidates = append(candidates, *sess.FinishedAt)
	}
	var latest time.Time
	for _, t := range candidates {
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

func readLastLines(path string, limit int) (string, error) {
	if limit <= 0 {
		return "", nil
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty log file path")
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lines := make([]string, 0, limit)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "(log file is empty)", nil
	}
	return strings.Join(lines, "\n"), nil
}

func tmuxCapture(session string) (string, error) {
	if strings.TrimSpace(session) == "" {
		return "", fmt.Errorf("empty tmux session")
	}
	out, err := exec.Command("tmux", "capture-pane", "-t", session, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func hashOutput(output string) string {
	const tailLines = 50

	lines := strings.Split(output, "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	tail := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(tail))
	return fmt.Sprintf("%x", sum[:])
}

func countSilentTimeoutKillsForIssue(s *state.State, issueNumber int) int {
	count := 0
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNumber && sess.LastNotifiedStatus == "silent_timeout" {
			count++
		}
	}
	return count
}

// retryBackoffMs computes the exponential backoff delay for a retry attempt.
// Formula: min(10000 * 2^(attempt-1), maxRetryBackoffMs).
// attempt is 1-based (the first retry is attempt 1).
func retryBackoffMs(attempt, maxRetryBackoffMs int) int {
	if attempt <= 0 {
		attempt = 1
	}
	delay := 10000 // 10 seconds base
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > maxRetryBackoffMs {
			return maxRetryBackoffMs
		}
	}
	if delay > maxRetryBackoffMs {
		return maxRetryBackoffMs
	}
	return delay
}

// appendCIFailureContext appends a section to the worker prompt describing
// what went wrong in the previous CI run, so the new worker can fix it.
func appendCIFailureContext(promptBase, ciOutput string, attempt int) string {
	return fmt.Sprintf(`%s

---

## IMPORTANT: Previous CI Failure (Attempt %d)

A previous worker created a PR for this issue, but CI failed. The PR has been closed.
You are a retry worker — please fix the CI failures described below.

**CI output from the failed run:**
`+"```"+`
%s
`+"```"+`

Focus on fixing the CI failures while still implementing the issue requirements.
`, promptBase, attempt, ciOutput)
}

// appendReviewFeedbackContext appends a section to the worker prompt with
// code review findings from the previous failed attempt.
func appendReviewFeedbackContext(promptBase, feedback string) string {
	return fmt.Sprintf(`%s

### Code Review Findings

The following code review comments were left on the previous PR. Address ALL of these issues:

%s

IMPORTANT: Address ALL code review findings above before creating a new PR.
Do NOT repeat the same mistakes.
`, promptBase, feedback)
}

// appendRebaseConflictContext appends a section to the worker prompt with
// auto-rebase failure details so the retry worker can update the same PR branch.
func appendRebaseConflictContext(promptBase, feedback string) string {
	return fmt.Sprintf(`%s

### Rebase Conflict

Maestro tried to update the existing PR branch against origin/main, but git rebase hit conflicts.
You are a retry worker running in the same worktree and branch.

Resolve the conflicts, keep the PR focused on the original issue, run validation, commit the fix, and push to the existing PR branch.
Do NOT open a second PR.

Rebase failure details:
`+"```"+`
%s
`+"```"+`
`, promptBase, feedback)
}

// canRetryIssue returns true if the session can be retried, considering
// both the session's retry count and the global max_retries_per_issue config.
// When max_retries_per_issue is 0 (unlimited), retries are always allowed.
func (o *Orchestrator) canRetryIssue(s *state.State, sess *state.Session) bool {
	maxRetries := o.cfg.MaxRetriesPerIssue
	if maxRetries <= 0 {
		return true // unlimited retries
	}
	totalAttempts := s.FailedAttemptsForIssue(sess.IssueNumber) + sess.RetryCount
	return totalAttempts < maxRetries
}

func (o *Orchestrator) maintenanceRetryBudget() int {
	if o == nil || o.cfg == nil {
		return 1
	}
	max := o.cfg.Supervisor.ReviewRepair.EffectiveMaxRetries()
	if max <= 0 {
		return 1
	}
	return max
}

func (o *Orchestrator) canRetryPRMaintenance(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	return sess.MaintenanceRetryCount < o.maintenanceRetryBudget()
}

func pendingRetryReservations(s *state.State) int {
	now := time.Now().UTC()
	count := 0
	for _, sess := range s.Sessions {
		if sess.Status == state.StatusDead && sess.NextRetryAt != nil && !now.Before(*sess.NextRetryAt) {
			count++
		}
	}
	return count
}

// respawnDueRetries checks dead sessions with a scheduled retry time and
// respawns them when the backoff period has elapsed.
func (o *Orchestrator) respawnDueRetries(s *state.State, slots int) {
	if slots <= 0 {
		if pending := pendingRetryReservations(s); pending > 0 {
			log.Printf("[orch] retry queue has %d pending session(s), but no worker slots are available", pending)
		}
		return
	}

	slotNames := make([]string, 0, len(s.Sessions))
	for slotName := range s.Sessions {
		slotNames = append(slotNames, slotName)
	}
	sort.Strings(slotNames)

	respawned := 0
	for _, slotName := range slotNames {
		if respawned >= slots {
			log.Printf("[orch] retry queue still has pending session(s), but retry slots are exhausted")
			return
		}

		sess := s.Sessions[slotName]
		if sess.Status != state.StatusDead {
			continue
		}
		if sess.NextRetryAt == nil {
			continue
		}
		if time.Now().UTC().Before(*sess.NextRetryAt) {
			log.Printf("[orch] worker %s retry %d waiting until %s",
				slotName, sess.RetryCount, sess.NextRetryAt.Format(time.RFC3339))
			continue
		}

		// Backoff elapsed — respawn the worker
		log.Printf("[orch] worker %s backoff elapsed, respawning (retry %d)", slotName, sess.RetryCount)
		sess.NextRetryAt = nil

		issue, err := o.getIssue(sess.IssueNumber)
		if err != nil {
			log.Printf("[orch] fetch issue #%d for retry: %v — marking as failed", sess.IssueNumber, err)
			sess.Status = state.StatusFailed
			now := time.Now().UTC()
			sess.FinishedAt = &now
			state.MarkWorkerEnded(sess, now)
			o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) retry failed (could not fetch issue)",
				slotName, sess.IssueNumber, sess.IssueTitle)
			continue
		}

		promptBase := o.selectPrompt(issue)

		// If this is a CI failure retry, include CI output and review feedback
		// in the prompt so the new worker knows what went wrong.
		if sess.CIFailureOutput != "" {
			promptBase = appendCIFailureContext(promptBase, sess.CIFailureOutput, sess.RetryCount)
			sess.CIFailureOutput = "" // consumed — don't persist stale output
		}
		if sess.PreviousAttemptFeedback != "" {
			if sess.PreviousAttemptFeedbackKind == "rebase_conflict" {
				promptBase = appendRebaseConflictContext(promptBase, sess.PreviousAttemptFeedback)
			} else {
				if sess.PreviousAttemptFeedbackKind == state.RetryReasonReviewFeedback {
					sess.RetryReason = state.RetryReasonReviewFeedback
				}
				promptBase = appendReviewFeedbackContext(promptBase, sess.PreviousAttemptFeedback)
			}
			sess.PreviousAttemptFeedback = "" // consumed — don't persist stale feedback
			sess.PreviousAttemptFeedbackKind = ""
		}

		var respawnErr error
		if sess.PRNumber != 0 && sess.Worktree != "" {
			respawnErr = o.respawnInPlace(slotName, sess, issue, promptBase, sess.Backend)
		} else {
			respawnErr = o.respawnWorker(slotName, sess, issue, promptBase, sess.Backend)
		}
		if respawnErr != nil {
			log.Printf("[orch] respawn worker %s: %v — marking as failed", slotName, respawnErr)
			sess.Status = state.StatusFailed
			now := time.Now().UTC()
			sess.FinishedAt = &now
			state.MarkWorkerEnded(sess, now)
			o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) respawn failed: %v",
				slotName, sess.IssueNumber, sess.IssueTitle, respawnErr)
			continue
		}

		o.notifier.Sendf("🔄 maestro: retrying worker %s for issue #%d: %s (attempt %d)",
			slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount)
		respawned++
	}
}

// LoadPromptBase reads the worker prompt template from config or a provided path.
// Priority: 1) explicit promptPath arg, 2) cfg.WorkerPrompt, 3) built-in fallback.
// Also loads optional bug_prompt and enhancement_prompt from config.
func (o *Orchestrator) LoadPromptBase(promptPath string) error {
	if promptPath == "" {
		promptPath = o.cfg.WorkerPrompt
	}
	if promptPath == "" {
		log.Printf("[orch] warn: no worker_prompt configured, using built-in fallback")
		o.promptBase = "You are a coding agent. Implement the given issue in the provided worktree."
		return nil
	}
	data, err := os.ReadFile(promptPath)
	if err != nil {
		log.Printf("[orch] warn: could not read prompt base from %s: %v", promptPath, err)
		o.promptBase = "You are a coding agent. Implement the given issue in the provided worktree."
		return nil
	}
	o.promptBase = string(data)
	log.Printf("[orch] loaded prompt base from %s (%d bytes)", promptPath, len(data))

	// Load optional per-issue-type prompts
	if o.cfg.BugPrompt != "" {
		if data, err := os.ReadFile(o.cfg.BugPrompt); err != nil {
			log.Printf("[orch] warn: could not read bug_prompt from %s: %v", o.cfg.BugPrompt, err)
		} else {
			o.bugPromptBase = string(data)
			log.Printf("[orch] loaded bug_prompt from %s (%d bytes)", o.cfg.BugPrompt, len(data))
		}
	}
	if o.cfg.EnhancementPrompt != "" {
		if data, err := os.ReadFile(o.cfg.EnhancementPrompt); err != nil {
			log.Printf("[orch] warn: could not read enhancement_prompt from %s: %v", o.cfg.EnhancementPrompt, err)
		} else {
			o.enhancementPromptBase = string(data)
			log.Printf("[orch] loaded enhancement_prompt from %s (%d bytes)", o.cfg.EnhancementPrompt, len(data))
		}
	}

	return nil
}

// selectPrompt returns the appropriate prompt template for an issue based on its labels.
// Priority: bug label → bug_prompt, enhancement label → enhancement_prompt, fallback → worker_prompt.
func (o *Orchestrator) selectPrompt(issue github.Issue) string {
	if o.bugPromptBase != "" && github.HasLabel(issue, []string{"bug"}) {
		return o.bugPromptBase
	}
	if o.enhancementPromptBase != "" && github.HasLabel(issue, []string{"enhancement"}) {
		return o.enhancementPromptBase
	}
	return o.promptBase
}

// RunOnce executes one orchestration cycle
func (o *Orchestrator) RunOnce() error {
	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	log.Printf("[orch] === cycle start — %d sessions in state ===", len(s.Sessions))

	// Step 0: Surface a finished self-deploy (#698) as a supervisor finding.
	// Persist immediately: the result file is already consumed, so the
	// finding must not depend on the rest of the cycle succeeding.
	if o.consumeSelfDeployResult(s) {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after self-deploy result: %w", err)
		}
	}

	// Step 1: Reconcile stale running sessions before scheduling/slot calculation.
	reconciled := o.reconcileRunningSessions(s)

	// Persist immediately when reconciliation changes state, so slot calculation
	// always sees healed state on disk.
	if reconciled {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after reconcile: %w", err)
		}
	}

	// Step 2: Check running sessions for dead processes / stale / closed issues
	o.checkSessions(s)

	// Step 2b: Respawn dead sessions whose backoff has elapsed
	retrySlots := availableSlots(o.cfg, s, len(s.ActiveSessions()))
	o.respawnDueRetries(s, retrySlots)

	// Step 3: Auto-merge green PRs
	o.autoMergePRs(s)

	// Step 3b: Reconcile already-landed code with the live outcome gate. This
	// covers sessions that reached code_landed before the verifier was available
	// or where the verifier passed on a later cycle.
	o.reconcileCodeLandedSessions(s)

	// Step 3c: Observe-merge self-deploy (#751). A PR merged outside the
	// orchestrator's own merge path (GitHub UI, manual `gh pr merge`, or the
	// approval-gate executor) advances origin/main but never reaches
	// maybeSelfDeployAfterMerge, so without this the fleet binary silently lags
	// main. Runs after autoMergePRs so a same-cycle orchestrator merge has
	// already recorded its trigger and debounces this drift-based path. No-op
	// unless self_deploy is enabled and main is genuinely ahead of the running
	// binary.
	o.maybeSelfDeployOnMainAdvance(s)

	// Step 4: Rebase conflicting PRs
	o.rebaseConflicts(s)

	// Step 4b: Process missions (decompose new epics, update progress)
	if o.missionProc != nil {
		issues, err := o.listOpenIssues(o.cfg.IssueLabels)
		if err != nil {
			log.Printf("[orch] list issues for missions: %v", err)
		} else {
			o.missionProc.ProcessMissions(s, issues)
		}
	}

	// Step 4b2: Accrue session token usage into the per-backend quota
	// windows and gate backends whose estimated subscription usage crossed
	// the dispatch threshold (#704). Runs before the fresh-dispatch step so
	// startNewWorkers sees the quota_pressure cooldowns this cycle.
	o.reconcileBackendQuota(s, time.Now().UTC())

	// Step 4c: Clear stale BackendHealth cooldown entries (#600) — backends
	// that have since produced PR evidence, whose RetryAfter has elapsed,
	// or whose cooldown is older than the max-cooldown TTL. The selector
	// already ignores elapsed RetryAfter, but the MC BACKENDS panel reads
	// the raw map; without this the panel reports working backends as
	// "auto-recovery pending" indefinitely.
	state.ReconcileBackendHealth(s, time.Now().UTC())

	// Save after all checks/reconciliation
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// Step 5: Start new workers for available slots
	active := len(s.ActiveSessions())
	slots := availableSlots(o.cfg, s, active)
	if reserved := pendingRetryReservations(s); reserved > 0 && slots > 0 {
		if reserved > slots {
			reserved = slots
		}
		slots -= reserved
		log.Printf("[orch] reserving %d worker slot(s) for scheduled retries", reserved)
	}
	log.Printf("[orch] active=%d max=%d available_slots=%d", active, o.cfg.MaxParallel, slots)

	if slots > 0 {
		o.startNewWorkers(s, slots)
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after workers: %w", err)
		}
	}

	// Step 6: Reconcile project board — mirror Maestro session state onto the
	// board (active workers visible as In Progress, open PRs as In Review,
	// blocked sessions as Blocked) and move externally-closed issues to Done.
	if o.reconcileProjectBoard(s) {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after project reconcile: %w", err)
		}
	}

	// Flush digest buffer (no-op if digest mode is off or buffer is empty)
	if err := o.notifier.Flush(); err != nil {
		log.Printf("[orch] digest flush: %v", err)
	}

	log.Printf("[orch] === cycle done ===")
	return nil
}

// SetConfigReloadCh sets the channel used to receive hot-reloaded configs.
// A nil channel is safe (select case is never chosen).
func (o *Orchestrator) SetConfigReloadCh(ch <-chan *config.Config) {
	o.configReloadCh = ch
}

// Run loops with the given interval; if once=true, runs once and returns.
// The context can be used to stop the loop (e.g. for multi-project shutdown).
// An optional refreshCh triggers an immediate poll cycle when a value is received.
func (o *Orchestrator) Run(ctx context.Context, interval time.Duration, once bool, refreshCh <-chan struct{}) error {
	// Sweep visual-QA Chrome leftovers from a previous run before doing any
	// work. Workers run inside a shared, detached tmux server that survives a
	// unit restart, so a `systemctl restart` can leave orphaned headless Chrome
	// (+ crashpad) processes and their temp dirs behind; clean them on startup.
	worker.SweepStaleVisualQA()

	if !once {
		// The long-running daemon just (re)started — that is the "restart" the
		// restart-required banner asks for. Reconcile any stale restart_required
		// flag persisted by a previous process into this process's reality (the
		// in-memory signal is false on a clean start) so the banner does not
		// survive the very restart it requested. A genuine post-start config
		// change still re-raises the signal via reloadConfig.
		o.clearStaleRestartRequired()

		// Full ProjectV2 item sweeps are expensive and only repair board drift.
		// Do not run one immediately after every daemon restart; session-state
		// mirroring still runs, and the broad sweep can wait for the normal
		// throttle window.
		o.deferProjectBoardSweep(time.Now().UTC())

		// Graceful drain (#541) is a one-shot "drain then restart" request. A
		// fresh daemon must start in normal (non-drained) mode, so clear any
		// leftover drain flag before the first cycle. Only the long-running
		// daemon clears it — a `run --once` reconcile tick must not lift a
		// drain that an operator is mid-way through.
		o.clearSpawnDrainOnStartup()

		// #497: bound state.Sessions growth — sweep terminal sessions
		// older than the retention window at daemon startup, before the
		// first poll cycle. Idempotent: a no-op when nothing is past the
		// floors. Failures are logged inside the helper.
		o.compactTerminalSessionsOnStartup()
	}
	if err := o.RunOnce(); err != nil {
		log.Printf("[orch] run error: %v", err)
	}
	if once {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[orch] shutting down (%s)", o.repo)
			return nil
		case <-ticker.C:
			if err := o.RunOnce(); err != nil {
				log.Printf("[orch] run error: %v", err)
			}
		case <-refreshCh:
			log.Printf("[orch] refresh triggered via API")
			if err := o.RunOnce(); err != nil {
				log.Printf("[orch] run error: %v", err)
			}
		case newCfg := <-o.configReloadCh:
			o.reloadConfig(newCfg, &ticker)
		}
	}
}

func (o *Orchestrator) deferProjectBoardSweep(now time.Time) {
	if o == nil || !o.projectItemsSweepAt.IsZero() {
		return
	}
	o.projectItemsSweepAt = now.UTC()
}

// clearSpawnDrainOnStartup lifts any leftover graceful-drain flag (#541) so a
// freshly started daemon begins in normal mode. A no-op when no drain is set.
func (o *Orchestrator) clearSpawnDrainOnStartup() {
	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		log.Printf("[orch] drain: load state to clear drain flag: %v", err)
		return
	}
	if !s.DrainActive() {
		return
	}
	s.ClearSpawnDrain(time.Now().UTC())
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		log.Printf("[orch] drain: clear drain flag on startup: %v", err)
		return
	}
	log.Printf("[orch] drain flag cleared on startup — resuming normal spawns")
}

// compactTerminalSessionsOnStartup applies cfg.SessionRetention to the
// persisted state once at daemon startup so the long-running supervise loop
// does not have to wait a full cycle before bounding state.Sessions (#497).
// Idempotent: a no-op when retention is disabled or nothing falls outside
// both retention floors. Failures are logged; the daemon still starts.
func (o *Orchestrator) compactTerminalSessionsOnStartup() {
	if o == nil || o.cfg == nil {
		return
	}
	if !o.cfg.SessionRetention.IsEnabled() {
		return
	}
	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		log.Printf("[orch] compact sessions: load state: %v", err)
		return
	}
	policy := state.SessionRetentionPolicy{
		KeepLast:    o.cfg.SessionRetention.EffectiveKeepLast(),
		MinAge:      o.cfg.SessionRetention.EffectiveMinAge(),
		ArchiveFile: o.cfg.SessionRetention.EffectiveArchiveFile(o.cfg.StateDir),
	}
	res, err := s.CompactSessions(policy, time.Now().UTC())
	if err != nil {
		log.Printf("[orch] compact sessions: %v", err)
		return
	}
	if res.Removed == 0 {
		return
	}
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		log.Printf("[orch] compact sessions: save: %v", err)
		return
	}
	log.Printf("[orch] startup compaction removed %d terminal session(s) (archived=%d)", res.Removed, res.Archived)
}

// reloadConfig applies non-destructive config changes at runtime.
// Fields that require a restart (repo, model.default) are logged as warnings.
func (o *Orchestrator) reloadConfig(newCfg *config.Config, ticker **time.Ticker) {
	old := o.cfg
	var changed []string

	// Restart-required fields — warn only, do not apply
	if newCfg.Repo != old.Repo {
		log.Printf("[orch] config reload: repo changed (%s → %s) — requires restart", old.Repo, newCfg.Repo)
	}

	// model.default and routing.* select the backend router, which is constructed
	// once at startup and is not safe to re-init mid-flight. Detect a change against
	// the live (startup) config, do NOT apply it, and raise a persistent
	// restart-required signal so the operator sees it in `maestro status` / Fleet API
	// rather than only a one-time log line. Keep o.cfg.Model/Routing unchanged so the
	// warning keeps firing on every subsequent reload while the config still differs.
	if newCfg.Model.Default != old.Model.Default {
		o.markRestartRequired(fmt.Sprintf("model.default changed (%s → %s)", old.Model.Default, newCfg.Model.Default))
		log.Printf("[orch] config reload: model.default changed (%s → %s) — requires restart (not applied)", old.Model.Default, newCfg.Model.Default)
	}
	if !reflect.DeepEqual(newCfg.Routing, old.Routing) {
		o.markRestartRequired(fmt.Sprintf("routing.* changed (router_model %s → %s, mode %s → %s)",
			old.Routing.RouterModel, newCfg.Routing.RouterModel, old.Routing.Mode, newCfg.Routing.Mode))
		log.Printf("[orch] config reload: routing.* changed (router_model %s → %s, mode %s → %s) — requires restart (not applied)",
			old.Routing.RouterModel, newCfg.Routing.RouterModel, old.Routing.Mode, newCfg.Routing.Mode)
	}

	// Hot-reloadable fields
	if newCfg.MaxParallel != old.MaxParallel {
		changed = append(changed, fmt.Sprintf("max_parallel: %d→%d", old.MaxParallel, newCfg.MaxParallel))
		o.cfg.MaxParallel = newCfg.MaxParallel
	}
	if newCfg.MaxRuntimeMinutes != old.MaxRuntimeMinutes {
		changed = append(changed, fmt.Sprintf("max_runtime_minutes: %d→%d", old.MaxRuntimeMinutes, newCfg.MaxRuntimeMinutes))
		o.cfg.MaxRuntimeMinutes = newCfg.MaxRuntimeMinutes
	}
	if newCfg.MaxRetriesPerIssue != old.MaxRetriesPerIssue {
		changed = append(changed, fmt.Sprintf("max_retries_per_issue: %d→%d", old.MaxRetriesPerIssue, newCfg.MaxRetriesPerIssue))
		o.cfg.MaxRetriesPerIssue = newCfg.MaxRetriesPerIssue
	}
	if newCfg.WorkerSilentTimeoutMinutes != old.WorkerSilentTimeoutMinutes {
		changed = append(changed, fmt.Sprintf("worker_silent_timeout_minutes: %d→%d", old.WorkerSilentTimeoutMinutes, newCfg.WorkerSilentTimeoutMinutes))
		o.cfg.WorkerSilentTimeoutMinutes = newCfg.WorkerSilentTimeoutMinutes
	}
	if newCfg.WorkerMaxTokens != old.WorkerMaxTokens {
		changed = append(changed, fmt.Sprintf("worker_max_tokens: %d→%d", old.WorkerMaxTokens, newCfg.WorkerMaxTokens))
		o.cfg.WorkerMaxTokens = newCfg.WorkerMaxTokens
	}
	if !strSliceEqual(newCfg.IssueLabels, old.IssueLabels) {
		changed = append(changed, fmt.Sprintf("issue_labels: %v→%v", old.IssueLabels, newCfg.IssueLabels))
		o.cfg.IssueLabels = newCfg.IssueLabels
	}
	if !strSliceEqual(newCfg.ExcludeLabels, old.ExcludeLabels) {
		changed = append(changed, fmt.Sprintf("exclude_labels: %v→%v", old.ExcludeLabels, newCfg.ExcludeLabels))
		o.cfg.ExcludeLabels = newCfg.ExcludeLabels
	}
	if !reflect.DeepEqual(newCfg.Supervisor, old.Supervisor) {
		changed = append(changed, "supervisor policy")
		o.cfg.Supervisor = newCfg.Supervisor
	}
	if newCfg.MergeStrategy != old.MergeStrategy {
		changed = append(changed, fmt.Sprintf("merge_strategy: %s→%s", old.MergeStrategy, newCfg.MergeStrategy))
		o.cfg.MergeStrategy = newCfg.MergeStrategy
	}
	if newCfg.MergeIntervalSeconds != old.MergeIntervalSeconds {
		changed = append(changed, fmt.Sprintf("merge_interval_seconds: %d→%d", old.MergeIntervalSeconds, newCfg.MergeIntervalSeconds))
		o.cfg.MergeIntervalSeconds = newCfg.MergeIntervalSeconds
	}
	if newCfg.ReviewGate != old.ReviewGate {
		changed = append(changed, fmt.Sprintf("review_gate: %s→%s", old.ReviewGate, newCfg.ReviewGate))
		o.cfg.ReviewGate = newCfg.ReviewGate
	}
	if !reflect.DeepEqual(newCfg.ReviewRetrigger, old.ReviewRetrigger) {
		changed = append(changed, "review_retrigger")
		o.cfg.ReviewRetrigger = newCfg.ReviewRetrigger
	}
	if newCfg.AutoRetryReviewFeedback != old.AutoRetryReviewFeedback {
		changed = append(changed, fmt.Sprintf("auto_retry_review_feedback: %v→%v", old.AutoRetryReviewFeedback, newCfg.AutoRetryReviewFeedback))
		o.cfg.AutoRetryReviewFeedback = newCfg.AutoRetryReviewFeedback
	}
	if newCfg.AutoRetryRebaseConflicts != old.AutoRetryRebaseConflicts {
		changed = append(changed, fmt.Sprintf("auto_retry_rebase_conflicts: %v→%v", old.AutoRetryRebaseConflicts, newCfg.AutoRetryRebaseConflicts))
		o.cfg.AutoRetryRebaseConflicts = newCfg.AutoRetryRebaseConflicts
	}
	if newCfg.DeployCmd != old.DeployCmd {
		changed = append(changed, fmt.Sprintf("deploy_cmd: %q→%q", old.DeployCmd, newCfg.DeployCmd))
		o.cfg.DeployCmd = newCfg.DeployCmd
	}
	if newCfg.DeployTimeoutMinutes != old.DeployTimeoutMinutes {
		changed = append(changed, fmt.Sprintf("deploy_timeout_minutes: %d→%d", old.DeployTimeoutMinutes, newCfg.DeployTimeoutMinutes))
		o.cfg.DeployTimeoutMinutes = newCfg.DeployTimeoutMinutes
	}
	if newCfg.AutoRebase != old.AutoRebase {
		changed = append(changed, fmt.Sprintf("auto_rebase: %v→%v", old.AutoRebase, newCfg.AutoRebase))
		o.cfg.AutoRebase = newCfg.AutoRebase
	}

	// Reload prompt files if paths changed
	if newCfg.WorkerPrompt != old.WorkerPrompt {
		changed = append(changed, fmt.Sprintf("worker_prompt: %q→%q", old.WorkerPrompt, newCfg.WorkerPrompt))
		o.cfg.WorkerPrompt = newCfg.WorkerPrompt
		o.LoadPromptBase("")
	}
	if newCfg.BugPrompt != old.BugPrompt {
		changed = append(changed, fmt.Sprintf("bug_prompt: %q→%q", old.BugPrompt, newCfg.BugPrompt))
		o.cfg.BugPrompt = newCfg.BugPrompt
		o.LoadPromptBase("")
	}
	if newCfg.EnhancementPrompt != old.EnhancementPrompt {
		changed = append(changed, fmt.Sprintf("enhancement_prompt: %q→%q", old.EnhancementPrompt, newCfg.EnhancementPrompt))
		o.cfg.EnhancementPrompt = newCfg.EnhancementPrompt
		o.LoadPromptBase("")
	}

	// Poll interval change — reset the ticker
	if newCfg.PollIntervalSeconds != old.PollIntervalSeconds && newCfg.PollIntervalSeconds > 0 {
		newInterval := time.Duration(newCfg.PollIntervalSeconds) * time.Second
		changed = append(changed, fmt.Sprintf("poll_interval_seconds: %d→%d", old.PollIntervalSeconds, newCfg.PollIntervalSeconds))
		o.cfg.PollIntervalSeconds = newCfg.PollIntervalSeconds
		(*ticker).Reset(newInterval)
	}

	// Hot-reload hooks config
	if newCfg.Hooks != old.Hooks {
		changed = append(changed, "hooks")
		o.cfg.Hooks = newCfg.Hooks
	}

	if len(changed) == 0 {
		log.Printf("[orch] config reloaded — no effective changes")
		return
	}
	log.Printf("[orch] config reloaded — changed: %s", strings.Join(changed, ", "))
}

// markRestartRequired records a persistent restart-required signal both in memory
// (so the running daemon keeps emitting the warning) and in the on-disk state file
// (so a separate `maestro status` / Fleet API process can surface it without
// grepping journals). Multiple reasons in a single reload are joined.
func (o *Orchestrator) markRestartRequired(reason string) {
	o.restartRequired = true
	if o.restartRequiredReason == "" {
		o.restartRequiredReason = reason
	} else if !strings.Contains(o.restartRequiredReason, reason) {
		o.restartRequiredReason = o.restartRequiredReason + "; " + reason
	}
	o.persistRestartRequired()
}

// persistRestartRequired mirrors the in-memory restart-required signal into the
// state file. It is best-effort: a load/save failure is logged but never aborts the
// reload (the in-memory warning still fires every cycle).
func (o *Orchestrator) persistRestartRequired() {
	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		log.Printf("[orch] warn: could not load state to persist restart-required signal: %v", err)
		return
	}
	if s.RestartRequired == o.restartRequired && s.RestartRequiredReason == o.restartRequiredReason {
		return
	}
	s.RestartRequired = o.restartRequired
	s.RestartRequiredReason = o.restartRequiredReason
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		log.Printf("[orch] warn: could not save restart-required signal to state: %v", err)
	}
}

// clearStaleRestartRequired reconciles a stale restart-required flag from a
// previous process into the freshly-started daemon's reality. The restart-required
// signal asks the operator to restart the daemon; once the daemon actually (re)starts
// it is loaded from whatever config is now on disk, so the in-memory restartRequired
// is the truth (false on a clean start). Nothing else clears the persisted flag — not
// a fresh start and not a config revert — so without this it survives the very restart
// it requested and produces a false banner in `maestro status` / the Fleet dashboard.
//
// This is only safe to call from the long-running daemon startup (Run with once=false),
// which is the actual "restart" the banner refers to. A genuine post-start config change
// still re-raises the signal through markRestartRequired in reloadConfig, so the real
// signal is preserved. Best-effort: a load/save failure is logged but never aborts start.
func (o *Orchestrator) clearStaleRestartRequired() {
	if o.restartRequired {
		// A real signal was already raised in-process before start completed; keep it.
		return
	}
	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		log.Printf("[orch] warn: could not load state to clear stale restart-required signal: %v", err)
		return
	}
	if !s.RestartRequired && s.RestartRequiredReason == "" {
		return
	}
	log.Printf("[orch] clearing stale restart-required signal from previous process (was: %q)", s.RestartRequiredReason)
	s.RestartRequired = false
	s.RestartRequiredReason = ""
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		log.Printf("[orch] warn: could not save cleared restart-required signal to state: %v", err)
	}
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reconcileRunningSessions self-heals stale "running" sessions.
// If a session is marked running but either its PID is dead/missing OR its tmux session
// is missing, the session is transitioned to a terminal state.
//
// Before marking a session dead, it checks whether the worker already opened a PR
// for its branch. If a PR exists, the session transitions to pr_open instead of dead.
// This prevents the infinite-spawn loop where reconcile kills a session whose worker
// had successfully created a PR before the tmux session was cleaned up.
func (o *Orchestrator) reconcileRunningSessions(s *state.State) bool {
	// Fetch open PRs once — used to rescue sessions where the worker exited
	// after creating a PR (process/tmux gone, but PR is already open on GitHub).
	prs, prErr := o.listOpenPRs()
	branchToPR := make(map[string]github.PR)
	if prErr != nil {
		log.Printf("[orch] reconcile: warn — could not list PRs: %v (will mark stale sessions dead)", prErr)
	} else {
		for _, pr := range prs {
			branchToPR[pr.HeadRefName] = pr
		}
	}

	reconciled := false
	for slotName, sess := range s.Sessions {
		if sess.Status != state.StatusRunning {
			continue
		}

		tmuxName := sess.TmuxSession
		if tmuxName == "" {
			tmuxName = worker.TmuxSessionName(slotName)
		}

		var reasons []string
		if sess.PID <= 0 {
			reasons = append(reasons, "pid missing")
		} else if !o.pidAlive(sess.PID) {
			reasons = append(reasons, fmt.Sprintf("pid %d dead", sess.PID))
		}

		if !o.tmuxSessionExists(tmuxName) {
			reasons = append(reasons, fmt.Sprintf("tmux session %q missing", tmuxName))
		}

		if len(reasons) == 0 {
			continue
		}

		// Worker process/session is gone. Before marking dead, check whether it
		// already opened a PR. If so, transition to pr_open — the worker succeeded.
		// Without this check, reconcile would mark the session dead, causing
		// IssueInProgress to return false and startNewWorkers to spawn a duplicate.
		if pr, found := branchToPR[sess.Branch]; found {
			o.updateTokensUsedFromWorkerLog(slotName, sess)
			o.ensureAttributionTrailerOnPR(slotName, sess, pr)
			o.ensureAttributionTrailerOnBranch(slotName, sess)
			log.Printf("[orch] reconcile: %s running->pr_open (PR #%d already open for branch %q; %s)",
				slotName, pr.Number, sess.Branch, strings.Join(reasons, ", "))
			sess.Status = state.StatusPROpen
			sess.PRNumber = pr.Number
			sess.PID = 0
			sess.TmuxSession = ""
			now := time.Now().UTC()
			sess.FinishedAt = &now
			state.MarkWorkerEnded(sess, now)
			state.MarkPROpened(sess, now)
			reconciled = true
			continue
		}
		if prErr == nil {
			if prNumber, ok := o.tryCreatePRForPushedBranch(slotName, sess, reasons); ok {
				log.Printf("[orch] reconcile: %s running->pr_open (auto-created PR #%d for pushed branch %q; %s)",
					slotName, prNumber, sess.Branch, strings.Join(reasons, ", "))
				reconciled = true
				continue
			}
		}

		// Before marking the session dead, check whether the worker died because
		// the provider hit a rate / usage limit. Without this, the reconcile path
		// races the main exit handler at the start of every cycle and burns the
		// per-issue retry budget on what is really a transient backend block (#466).
		// On rate-limit, attempt the same provider-limit fallover the main loop does
		// (see :worker-died and :log-scanner paths) — select an available fallback
		// backend and respawn the session there. Mark the session Dead only when no
		// fallback is configured / available, or when the respawn itself fails (#506).
		//
		// Per #663, fallback is gated on a positive (parseable-reset) rate-limit
		// signal. A pattern match without a reset hint is too easy to false-positive
		// — a stale prompt-context echo, a codex tools-router error, or a transient
		// stderr line containing "429" — and switching backends on it burns the
		// more expensive fallback and can loop the session to the retry cap.
		if hit, resetAt := o.providerRateLimitFromLog(sess.LogFile); hit {
			now := time.Now().UTC()
			o.updateTokensUsedFromWorkerLog(slotName, sess)
			o.recordProviderLimit(s, slotName, sess, "log_rate_limit_reconcile", resetAt, now)
			selection := o.selectProviderLimitFallback(s, sess, now)
			sess.BackendSelection = &selection
			oldPID := sess.PID
			oldTmux := tmuxName
			resetStr := "unknown"
			if resetAt != nil {
				resetStr = resetAt.Format(time.RFC3339)
			}
			previousBackend := sess.Backend
			nextBackend := selection.SelectedBackend

			if nextBackend == "" {
				sess.PID = 0
				sess.TmuxSession = ""
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				sess.LastNotifiedStatus = "rate_limit"
				sess.Status = state.StatusDead
				reconciled = true
				log.Printf("[orch] reconcile: %s running->dead via provider rate limit on backend=%s reset=%s (no fallback available; %s); pid=%d tmux=%q",
					slotName, previousBackend, resetStr, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) hit provider limit on %s; reset=%s; no fallback available",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, resetStr)
				continue
			}

			issue, fetchErr := o.getIssue(sess.IssueNumber)
			if fetchErr != nil {
				sess.PID = 0
				sess.TmuxSession = ""
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				sess.LastNotifiedStatus = "rate_limit"
				sess.Status = state.StatusDead
				reconciled = true
				log.Printf("[orch] reconcile: %s running->dead via provider rate limit on backend=%s reset=%s (fallback to %s aborted: could not fetch issue #%d: %v; %s); pid=%d tmux=%q",
					slotName, previousBackend, resetStr, nextBackend, sess.IssueNumber, fetchErr, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) hit provider limit on %s; fallback to %s failed (could not fetch issue): %v",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, nextBackend, fetchErr)
				continue
			}

			if previousBackend != "" {
				sess.TriedBackends = append(sess.TriedBackends, previousBackend)
			}
			promptBase := o.selectPrompt(issue)
			if respawnErr := o.respawnWorker(slotName, sess, issue, promptBase, nextBackend); respawnErr != nil {
				sess.PID = 0
				sess.TmuxSession = ""
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				sess.LastNotifiedStatus = "rate_limit"
				sess.Status = state.StatusDead
				reconciled = true
				log.Printf("[orch] reconcile: %s running->dead via provider rate limit on backend=%s reset=%s (fallback respawn on %s failed: %v; %s); pid=%d tmux=%q",
					slotName, previousBackend, resetStr, nextBackend, respawnErr, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) hit provider limit on %s; fallback to %s failed: %v",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, nextBackend, respawnErr)
				continue
			}

			reconciled = true
			log.Printf("[orch] reconcile: %s rate-limited on backend=%s reset=%s — respawned with backend=%s (%s); old pid=%d tmux=%q",
				slotName, previousBackend, resetStr, nextBackend, strings.Join(reasons, ", "), oldPID, oldTmux)
			o.notifier.Sendf("🔄 maestro: worker %s (issue #%d: %s) hit provider limit on %s; reset=%s — respawned on %s",
				slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, resetStr, nextBackend)
			continue
		}

		// Hard backend failure (#693 auth, #713 model unavailable): the worker
		// died early with a credential-outage signature ("Failed to
		// authenticate. API Error: 401") or a model-unavailable signature
		// ("It may not exist or you may not have access to it") in its log
		// tail. Either is a backend outage, not a work failure — respawning on
		// the same backend dies the same way and burns the per-issue retry
		// budget. Gate the backend (short cooldown; credentials recover via
		// re-login / cred-sync, a pulled model recovers via a config swap),
		// keep the budget intact, and respawn the same attempt on the next
		// fallback backend. The gating reason picks the operator copy.
		if hit, reason, pattern := o.classifyBackendFailure(sess, time.Now().UTC()); hit {
			now := time.Now().UTC()
			cp := backendFailureCopyFor(reason)
			o.updateTokensUsedFromWorkerLog(slotName, sess)
			o.recordBackendFailure(s, slotName, sess, reason, pattern, now)
			selection := o.selectBackendFallback(s, sess, now, cp.selectionReason)
			sess.BackendSelection = &selection
			oldPID := sess.PID
			oldTmux := tmuxName
			previousBackend := sess.Backend
			nextBackend := selection.SelectedBackend

			if nextBackend == "" {
				sess.PID = 0
				sess.TmuxSession = ""
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				sess.LastNotifiedStatus = cp.displayToken
				sess.Status = state.StatusDead
				reconciled = true
				log.Printf("[orch] reconcile: %s running->dead via %s on backend=%s signature=%s (no fallback available; %s); pid=%d tmux=%q",
					slotName, cp.noun, previousBackend, pattern, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s (%s); no fallback backend available — %s",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, pattern, cp.remedy)
				continue
			}

			issue, fetchErr := o.getIssue(sess.IssueNumber)
			if fetchErr != nil {
				sess.PID = 0
				sess.TmuxSession = ""
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				sess.LastNotifiedStatus = cp.displayToken
				sess.Status = state.StatusDead
				reconciled = true
				log.Printf("[orch] reconcile: %s running->dead via %s on backend=%s signature=%s (fallback to %s aborted: could not fetch issue #%d: %v; %s); pid=%d tmux=%q",
					slotName, cp.noun, previousBackend, pattern, nextBackend, sess.IssueNumber, fetchErr, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s; fallback to %s failed (could not fetch issue): %v",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, nextBackend, fetchErr)
				continue
			}

			if previousBackend != "" {
				sess.TriedBackends = append(sess.TriedBackends, previousBackend)
			}
			promptBase := o.selectPrompt(issue)
			if respawnErr := o.respawnWorker(slotName, sess, issue, promptBase, nextBackend); respawnErr != nil {
				sess.PID = 0
				sess.TmuxSession = ""
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				sess.LastNotifiedStatus = cp.displayToken
				sess.Status = state.StatusDead
				reconciled = true
				log.Printf("[orch] reconcile: %s running->dead via %s on backend=%s signature=%s (fallback respawn on %s failed: %v; %s); pid=%d tmux=%q",
					slotName, cp.noun, previousBackend, pattern, nextBackend, respawnErr, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s; fallback to %s failed: %v",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, nextBackend, respawnErr)
				continue
			}

			reconciled = true
			log.Printf("[orch] reconcile: %s %s on backend=%s signature=%s — respawned with backend=%s, retry budget preserved (%s); old pid=%d tmux=%q",
				slotName, cp.noun, previousBackend, pattern, nextBackend, strings.Join(reasons, ", "), oldPID, oldTmux)
			o.notifier.Sendf("🔄 maestro: worker %s (issue #%d: %s) backend %s %s (%s) — respawned on %s, retry budget preserved",
				slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, pattern, nextBackend)
			continue
		}

		oldPID := sess.PID
		oldTmux := tmuxName
		o.updateTokensUsedFromWorkerLog(slotName, sess)
		sess.Status = state.StatusDead
		sess.PID = 0
		sess.TmuxSession = ""
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		reconciled = true

		log.Printf("[orch] reconcile: %s running->dead (%s); pid=%d tmux=%q",
			slotName, strings.Join(reasons, ", "), oldPID, oldTmux)
	}
	return reconciled
}

func (o *Orchestrator) tryCreatePRForPushedBranch(slotName string, sess *state.Session, reasons []string) (int, bool) {
	branch := strings.TrimSpace(sess.Branch)
	if branch == "" {
		return 0, false
	}
	exists, err := o.remoteBranchExists(branch)
	if err != nil {
		log.Printf("[orch] reconcile: could not check remote branch %q for %s: %v", branch, slotName, err)
		return 0, false
	}
	if !exists {
		return 0, false
	}

	title := autoCreatedPRTitle(sess)
	o.ensureAttributionTrailerOnBranch(slotName, sess)
	body := state.AppendAttributionTrailer(autoCreatedPRBody(sess, branch, reasons), sess.Attribution, time.Now().UTC())
	prNumber, err := o.createPR(title, body, "main", branch)
	if err != nil {
		log.Printf("[orch] reconcile: could not auto-create PR for %s branch %q: %v", slotName, branch, err)
		return 0, false
	}

	o.updateTokensUsedFromWorkerLog(slotName, sess)
	sess.Status = state.StatusPROpen
	sess.PRNumber = prNumber
	sess.PID = 0
	sess.TmuxSession = ""
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
	state.MarkPROpened(sess, now)
	if o.notifier != nil {
		o.notifier.Sendf("🔀 maestro: worker %s pushed branch %s and exited before opening a PR; auto-created PR #%d for issue #%d (%s)",
			slotName, branch, prNumber, sess.IssueNumber, sess.IssueTitle)
	}
	return prNumber, true
}

func autoCreatedPRTitle(sess *state.Session) string {
	title := strings.TrimSpace(sess.IssueTitle)
	if title == "" {
		title = "Maestro worker result"
	}
	suffix := fmt.Sprintf(" (#%d)", sess.IssueNumber)
	if !strings.Contains(title, suffix) {
		title += suffix
	}
	if len(title) > 180 {
		title = strings.TrimSpace(title[:180-len(suffix)]) + suffix
	}
	return title
}

func autoCreatedPRBody(sess *state.Session, branch string, reasons []string) string {
	reasonText := strings.TrimSpace(strings.Join(reasons, ", "))
	if reasonText == "" {
		reasonText = "worker process exited before PR creation was observed"
	}
	return fmt.Sprintf(`Refs #%d

Maestro auto-created this PR because the worker pushed branch %s but exited before opening a pull request.

Observed worker state: %s.
`, sess.IssueNumber, branch, reasonText)
}

func (o *Orchestrator) ensureAttributionTrailerOnPR(slotName string, sess *state.Session, pr github.PR) {
	if sess == nil || len(sess.Attribution) == 0 {
		return
	}
	body := state.AppendAttributionTrailer(pr.Body, sess.Attribution, time.Now().UTC())
	if body == pr.Body {
		return
	}
	if err := o.updatePRBody(pr.Number, body); err != nil {
		log.Printf("[orch] attribution: could not update PR #%d body for %s: %v", pr.Number, slotName, err)
	}
}

func (o *Orchestrator) ensureAttributionTrailerOnBranch(slotName string, sess *state.Session) {
	if sess == nil || len(sess.Attribution) == 0 {
		return
	}
	if err := o.amendHeadWithAttributionTrailer(sess.Worktree, sess.Branch, sess.Attribution, time.Now().UTC()); err != nil {
		log.Printf("[orch] attribution: could not amend branch %q for %s: %v", sess.Branch, slotName, err)
	}
}

func (o *Orchestrator) amendHeadWithAttributionTrailer(worktreePath, branch string, attribution []state.BackendAttribution, now time.Time) error {
	if o.amendHeadFn != nil {
		return o.amendHeadFn(worktreePath, branch, attribution, now)
	}
	return amendHeadWithAttributionTrailer(worktreePath, branch, attribution, now)
}

func amendHeadWithAttributionTrailer(worktreePath, branch string, attribution []state.BackendAttribution, now time.Time) error {
	worktreePath = strings.TrimSpace(worktreePath)
	branch = strings.TrimSpace(branch)
	if worktreePath == "" || branch == "" || len(attribution) == 0 {
		return nil
	}
	if _, err := os.Stat(worktreePath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	status, err := exec.Command("git", "-C", worktreePath, "status", "--porcelain").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git status: %w\n%s", err, status)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("worktree has uncommitted changes; refusing attribution amend")
	}
	out, err := exec.Command("git", "-C", worktreePath, "log", "-1", "--pretty=%B").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git log -1: %w\n%s", err, out)
	}
	msg := state.AppendAttributionTrailer(string(out), attribution, now)
	if msg == string(out) {
		return nil
	}
	cmd := exec.Command("git", "-C", worktreePath, "commit", "--amend", "-F", "-")
	cmd.Stdin = strings.NewReader(msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit --amend: %w\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", worktreePath, "push", "--force-with-lease", "origin", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git push --force-with-lease origin %s: %w\n%s", branch, err, out)
	}
	return nil
}

// checkSessions inspects all sessions and updates their status
func (o *Orchestrator) checkSessions(s *state.State) {
	// Fetch open PRs once for the whole check cycle
	prs, prErr := o.listOpenPRs()
	branchToPR := make(map[string]github.PR)
	if prErr != nil {
		log.Printf("[orch] list PRs (check): %v", prErr)
	} else {
		for _, pr := range prs {
			branchToPR[pr.HeadRefName] = pr
		}
	}

	for slotName, sess := range s.Sessions {
		switch sess.Status {
		case state.StatusDone, state.StatusCodeLanded, state.StatusDead, state.StatusConflictFailed, state.StatusFailed, state.StatusRetryExhausted:
			if sess.Status != state.StatusDone && sess.Status != state.StatusCodeLanded && prErr == nil {
				if pr, found := branchToPR[sess.Branch]; found {
					// #556: when a retry-exhausted session has already been
					// settled on this exact PR (status+PRNumber+notified),
					// do NOT flip it back to pr_open every cycle. The merge
					// flow already processes retry_exhausted sessions
					// directly (mergeFlowEligibleStatus), so the flip is
					// redundant — and it produces the board flip-flop
					// (`In Review` ↔ `Blocked`) the dogfood log captured,
					// plus a log line per cycle.
					if isSettledRetryExhausted(sess, pr.Number) {
						// Stay in the terminal retry_exhausted state. No
						// log, no project sync, no FinishedAt churn.
						continue
					}
					// A Dead session with a pending NextRetryAt is QUEUED for an
					// in-place respawn (review-feedback / CI-failure / rebase
					// retry — handleReviewFeedbackRetry et al. set Status=Dead +
					// NextRetryAt). Do NOT flip it to pr_open here: this reconcile
					// runs at Step 1, BEFORE respawnDueRetries at Step 2b, which
					// only relaunches sessions still in StatusDead. Flipping to
					// pr_open clears the Dead status, so respawnDueRetries never
					// sees the session and the worker never relaunches — every
					// cycle re-detects the same review feedback and burns one
					// maintenance retry until the budget exhausts and the PR holds,
					// with not a single real fix attempt. Leave it Dead; the retry
					// queue owns the relaunch (#758 in-place-respawn race).
					if sess.Status == state.StatusDead && sess.NextRetryAt != nil {
						continue
					}
					o.ensureAttributionTrailerOnPR(slotName, sess, pr)
					o.ensureAttributionTrailerOnBranch(slotName, sess)
					log.Printf("[orch] session %s %s->pr_open (PR #%d now open for branch %q)", slotName, sess.Status, pr.Number, sess.Branch)
					sess.Status = state.StatusPROpen
					sess.PRNumber = pr.Number
					sess.PID = 0
					sess.TmuxSession = ""
					now := time.Now().UTC()
					sess.FinishedAt = &now
					state.MarkWorkerEnded(sess, now)
					state.MarkPROpened(sess, now)
					o.syncProject(sess.IssueNumber, github.ProjectStatusInReview)
					continue
				}
			}
			// Zombie cleanup: if the underlying issue is closed, transition to done.
			// This prevents conflict_failed/failed/dead/retry_exhausted sessions from lingering
			// indefinitely when their issues are closed externally (#187).
			if sess.Status != state.StatusDone {
				done := false
				closed, err := o.isIssueClosed(sess.IssueNumber)
				if err != nil {
					log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
				} else if closed {
					if o.canMarkDoneForOutcome(s, sess, fmt.Sprintf("issue #%d is closed", sess.IssueNumber)) {
						log.Printf("[orch] issue #%d closed, transitioning zombie session %s from %s to done", sess.IssueNumber, slotName, sess.Status)
						done = true
					}
				}
				if done {
					o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
					sess.Status = state.StatusDone
					if sess.FinishedAt == nil {
						now := time.Now().UTC()
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
					}
				} else if sess.Status != state.StatusCodeLanded && sess.PRNumber > 0 {
					merged, err := o.isPRMerged(sess.PRNumber)
					if err != nil {
						log.Printf("[orch] check PR #%d merged: %v", sess.PRNumber, err)
					} else if merged {
						log.Printf("[orch] PR #%d merged, transitioning zombie session %s from %s to code_landed", sess.PRNumber, slotName, sess.Status)
						o.markCodeLanded(sess, sess.PRNumber)
					}
				}
			}
			// Terminal states — cleanup old worktrees after 1h
			// Use StartedAt as fallback when FinishedAt is nil (orphaned sessions)
			// to preserve the grace period for recently-killed workers.
			nilAndOld := sess.FinishedAt == nil && !sess.StartedAt.IsZero() && time.Since(sess.StartedAt) > 1*time.Hour
			finishedAndOld := sess.FinishedAt != nil && time.Since(*sess.FinishedAt) > 1*time.Hour
			if sess.Worktree != "" && (nilAndOld || finishedAndOld) {
				if _, err := os.Stat(sess.Worktree); err == nil {
					if sess.FinishedAt != nil {
						log.Printf("[orch] cleaning up stale worktree for %s (finished %s ago)", slotName, time.Since(*sess.FinishedAt).Round(time.Minute))
					} else {
						log.Printf("[orch] cleaning up orphaned worktree for %s (started %s ago, no finishedAt)", slotName, time.Since(sess.StartedAt).Round(time.Minute))
					}
					worker.Stop(o.cfg, slotName, sess)
					sess.Worktree = "" // Mark as cleaned
				}
			}
			continue
		}

		// Check if issue is closed for pr_open/queued sessions —
		// free the worker slot when the issue no longer needs work (#187).
		if sess.Status == state.StatusPROpen || sess.Status == state.StatusQueued {
			closed, err := o.isIssueClosed(sess.IssueNumber)
			if err != nil {
				log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
			} else if closed {
				if !o.canMarkDoneForOutcome(s, sess, fmt.Sprintf("issue #%d is closed", sess.IssueNumber)) {
					continue
				}
				log.Printf("[orch] issue #%d closed, transitioning %s from %s to done", sess.IssueNumber, slotName, sess.Status)
				o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
				o.stopWorker(slotName, sess)
				sess.Status = state.StatusDone
				now := time.Now().UTC()
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				continue
			}
		}

		// Check if issue is now closed (only for running sessions)
		if sess.Status == state.StatusRunning {
			closed, err := o.isIssueClosed(sess.IssueNumber)
			if err != nil {
				log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
			} else if closed {
				if !o.canMarkDoneForOutcome(s, sess, fmt.Sprintf("issue #%d is closed", sess.IssueNumber)) {
					continue
				}
				log.Printf("[orch] issue #%d closed, stopping worker %s", sess.IssueNumber, slotName)
				o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
				o.stopWorker(slotName, sess)
				sess.Status = state.StatusDone
				now := time.Now().UTC()
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				continue
			}

			// Check if process is still alive
			if sess.PID > 0 && !o.pidAlive(sess.PID) {
				// Pipeline phase transition: if this is a pipeline session,
				// try to advance to the next phase before falling through
				// to normal dead-worker handling.
				if sess.Phase != state.PhaseNone && o.advancePipeline(slotName, sess) {
					continue
				}

				// Worker process died — run after_run hook
				o.runAfterRunHook(sess)

				// Check if there's an open PR for this branch BEFORE marking dead
				if pr, found := branchToPR[sess.Branch]; found {
					o.updateTokensUsedFromWorkerLog(slotName, sess)
					o.ensureAttributionTrailerOnPR(slotName, sess, pr)
					o.ensureAttributionTrailerOnBranch(slotName, sess)
					log.Printf("[orch] worker %s exited but PR #%d is open — transitioning to pr_open", slotName, pr.Number)
					sess.Status = state.StatusPROpen
					sess.PRNumber = pr.Number
					now := time.Now().UTC()
					state.MarkWorkerEnded(sess, now)
					state.MarkPROpened(sess, now)
					o.notifier.Sendf("🔀 maestro: worker %s completed, PR #%d open for issue #%d (%s)",
						slotName, pr.Number, sess.IssueNumber, sess.IssueTitle)
				} else if hit, resetAt := o.providerRateLimitFromLog(sess.LogFile); hit {
					// Provider capacity limit detected — gate this backend and select a fallback.
					// Per #663, only a high-confidence signal (parseable reset window)
					// triggers backend fallback; a pattern match without a reset hint
					// is too easy to false-positive (stale prompt echo, codex tool-exec
					// errors, transient stderr containing "429") and is left to the
					// ordinary retry/canRetryIssue path below.
					now := time.Now().UTC()
					o.updateTokensUsedFromWorkerLog(slotName, sess)
					o.recordProviderLimit(s, slotName, sess, "log_rate_limit", resetAt, now)
					selection := o.selectProviderLimitFallback(s, sess, now)
					sess.BackendSelection = &selection
					nextBackend := selection.SelectedBackend
					if nextBackend == "" {
						log.Printf("[orch] worker %s (backend=%s) hit provider limit; no fallback backend available", slotName, sess.Backend)
						sess.Status = state.StatusDead
						sess.LastNotifiedStatus = "rate_limit"
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) hit provider limit; no fallback backend available",
							slotName, sess.IssueNumber, sess.IssueTitle)
						continue
					}
					log.Printf("[orch] worker %s (backend=%s) hit rate limit, falling back to %s",
						slotName, sess.Backend, nextBackend)

					issue, err := o.getIssue(sess.IssueNumber)
					if err != nil {
						log.Printf("[orch] fetch issue #%d for fallback: %v — marking as failed", sess.IssueNumber, err)
						sess.Status = state.StatusFailed
						now := time.Now().UTC()
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) rate-limited and fallback failed (could not fetch issue)",
							slotName, sess.IssueNumber, sess.IssueTitle)
						continue
					}

					previousBackend := sess.Backend
					if previousBackend != "" {
						sess.TriedBackends = append(sess.TriedBackends, previousBackend)
					}
					promptBase := o.selectPrompt(issue)
					if err := o.respawnWorker(slotName, sess, issue, promptBase, nextBackend); err != nil {
						log.Printf("[orch] fallback respawn worker %s with %s: %v — marking as failed", slotName, nextBackend, err)
						sess.Status = state.StatusFailed
						now := time.Now().UTC()
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) rate-limited and fallback to %s failed: %v",
							slotName, sess.IssueNumber, sess.IssueTitle, nextBackend, err)
						continue
					}

					o.notifier.Sendf("🔄 maestro: worker %s (issue #%d) rate-limited on %s, switched to %s",
						slotName, sess.IssueNumber, previousBackend, nextBackend)
				} else if hit, reason, pattern := o.classifyBackendFailure(sess, time.Now().UTC()); hit {
					// Hard backend failure: a credential outage (#693, claude
					// CLI "Failed to authenticate. API Error: 401") or a model
					// that is unavailable / no longer accessible (#713, "It may
					// not exist or you may not have access to it"). Either is a
					// backend outage, not a work failure: every retry on the
					// same backend dies the same way and silently burns
					// max_retries_per_issue. Gate the backend, keep the retry
					// budget intact, and respawn the same attempt on the next
					// fallback backend. The gating reason picks the operator copy.
					now := time.Now().UTC()
					cp := backendFailureCopyFor(reason)
					o.updateTokensUsedFromWorkerLog(slotName, sess)
					o.recordBackendFailure(s, slotName, sess, reason, pattern, now)
					selection := o.selectBackendFallback(s, sess, now, cp.selectionReason)
					sess.BackendSelection = &selection
					nextBackend := selection.SelectedBackend
					if nextBackend == "" {
						log.Printf("[orch] worker %s (backend=%s) %s (%s); no fallback backend available", slotName, sess.Backend, cp.noun, pattern)
						sess.Status = state.StatusDead
						sess.LastNotifiedStatus = cp.displayToken
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s (%s); no fallback backend available — %s",
							slotName, sess.IssueNumber, sess.IssueTitle, sess.Backend, cp.desc, pattern, cp.remedy)
						continue
					}
					log.Printf("[orch] worker %s (backend=%s) %s (%s), falling back to %s — retry budget preserved",
						slotName, sess.Backend, cp.desc, pattern, nextBackend)

					issue, err := o.getIssue(sess.IssueNumber)
					if err != nil {
						log.Printf("[orch] fetch issue #%d for %s fallback: %v — marking as dead (%s)", sess.IssueNumber, cp.noun, err, cp.displayToken)
						sess.Status = state.StatusDead
						sess.LastNotifiedStatus = cp.displayToken
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) %s and fallback failed (could not fetch issue)",
							slotName, sess.IssueNumber, sess.IssueTitle, cp.noun)
						continue
					}

					previousBackend := sess.Backend
					if previousBackend != "" {
						sess.TriedBackends = append(sess.TriedBackends, previousBackend)
					}
					promptBase := o.selectPrompt(issue)
					if err := o.respawnWorker(slotName, sess, issue, promptBase, nextBackend); err != nil {
						log.Printf("[orch] %s fallback respawn worker %s with %s: %v — marking as dead (%s)", cp.noun, slotName, nextBackend, err, cp.displayToken)
						sess.Status = state.StatusDead
						sess.LastNotifiedStatus = cp.displayToken
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) %s and fallback to %s failed: %v",
							slotName, sess.IssueNumber, sess.IssueTitle, cp.noun, nextBackend, err)
						continue
					}

					o.notifier.Sendf("🔄 maestro: worker %s (issue #%d) backend %s %s (%s), switched to %s — retry budget preserved",
						slotName, sess.IssueNumber, previousBackend, cp.desc, pattern, nextBackend)
				} else if o.canRetryIssue(s, sess) {
					// Schedule retry with exponential backoff (respects max_retries_per_issue)
					o.updateTokensUsedFromWorkerLog(slotName, sess)
					sess.RetryCount++
					backoffMs := retryBackoffMs(sess.RetryCount, o.cfg.MaxRetryBackoffMs)
					retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
					sess.NextRetryAt = &retryAt
					sess.Status = state.StatusDead
					now := time.Now().UTC()
					sess.FinishedAt = &now
					state.MarkWorkerEnded(sess, now)
					log.Printf("[orch] worker %s (pid=%d) died, scheduling retry %d in %dms",
						slotName, sess.PID, sess.RetryCount, backoffMs)
					o.notifier.Sendf("🔄 maestro: worker %s (issue #%d: %s) died, retry %d scheduled in %ds",
						slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, backoffMs/1000)
				} else {
					// Retry limit reached — mark as permanently failed
					o.updateTokensUsedFromWorkerLog(slotName, sess)
					log.Printf("[orch] worker %s (pid=%d) permanently failed after %d retries", slotName, sess.PID, sess.RetryCount)
					// auto-label blocked disabled
					o.syncProject(sess.IssueNumber, github.ProjectStatusBlocked)
					sess.Status = state.StatusFailed
					now := time.Now().UTC()
					sess.FinishedAt = &now
					state.MarkWorkerEnded(sess, now)
					o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) permanently failed after %d retries.\nCheck log: %s",
						slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, sess.LogFile)
				}
				continue
			}

			// Check if running session has opened a PR (worker still alive)
			if pr, found := branchToPR[sess.Branch]; found {
				if sess.PRNumber == pr.Number {
					// In-place review/CI retries intentionally keep working on an
					// already-open PR. Do not transition back to pr_open until the
					// worker exits, otherwise the fixer is interrupted mid-run.
				} else {
					log.Printf("[orch] worker %s opened PR #%d while still running — transitioning to pr_open", slotName, pr.Number)
					sess.Status = state.StatusPROpen
					sess.PRNumber = pr.Number
					state.MarkPROpened(sess, time.Now().UTC())
					o.notifier.Sendf("🔀 maestro: worker %s opened PR #%d for issue #%d (%s)",
						slotName, pr.Number, sess.IssueNumber, sess.IssueTitle)
					continue
				}
			}

			// Capture tmux pane output for token tracking, rate-limit detection,
			// silent-timeout detection, and token-limit enforcement.
			{
				tmuxName := sess.TmuxSession
				if tmuxName == "" {
					tmuxName = worker.TmuxSessionName(slotName)
				}

				output, err := o.captureTmux(tmuxName)
				if err != nil {
					log.Printf("[orch] warn: tmux capture-pane failed for %s (%s): %v", slotName, tmuxName, err)
				} else {
					// --- Token tracking ---
					o.updateTokensUsedFromOutput(slotName, sess, output)

					// --- Rate-limit detection ---
					// Per #663, the orchestrator only acts on a HIGH-confidence
					// rate-limit signal: the classifier must match AND the provider
					// must have stated a parseable reset window. A bare pattern match
					// without a reset hint is too easy to false-positive — a stale
					// prompt-context echo, a codex tools-router error, or a transient
					// stderr line containing "429" — and killing the worker on it
					// burns the more-expensive fallback for no reason. A live worker
					// that is still progressing despite a low-confidence match is left
					// alone so it can recover (the apertune case from #663).
					if !sess.RateLimitHit && sess.LastNotifiedStatus != "rate_limit" {
						classifyHit, pattern, confidence, resetTime := worker.ClassifyRateLimit(output)
						if classifyHit && confidence != "high" {
							log.Printf("[orch] worker %s rate-limit pattern matched (pattern=%s) but reset window unparseable — treating as low-confidence and letting worker continue (per #663)",
								slotName, pattern)
						}
						if classifyHit && confidence == "high" {
							log.Printf("[orch] worker %s hit rate limit (pattern=%s, reset=%s), stopping",
								slotName, pattern, resetTime.Format(time.RFC3339))
							o.runAfterRunHook(sess)
							if err := o.stopWorker(slotName, sess); err != nil {
								log.Printf("[orch] warn: could not stop rate-limited worker %s: %v", slotName, err)
							}
							now := time.Now().UTC()
							resetAt := &resetTime
							o.recordProviderLimit(s, slotName, sess, pattern, resetAt, now)
							selection := o.selectProviderLimitFallback(s, sess, now)
							sess.BackendSelection = &selection

							// Attempt fallback: respawn with the best currently available backend.
							if fallback := selection.SelectedBackend; fallback != "" {
								issue, fetchErr := o.getIssue(sess.IssueNumber)
								if fetchErr != nil {
									log.Printf("[orch] fetch issue #%d for rate-limit fallback: %v — marking dead", sess.IssueNumber, fetchErr)
									sess.Status = state.StatusDead
									sess.LastNotifiedStatus = "rate_limit"
									now := time.Now().UTC()
									sess.FinishedAt = &now
									state.MarkWorkerEnded(sess, now)
									o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d) hit rate limit (%s); fallback failed (could not fetch issue)",
										slotName, sess.IssueNumber, pattern)
									continue
								}

								previousBackend := sess.Backend
								if previousBackend != "" {
									sess.TriedBackends = append(sess.TriedBackends, previousBackend)
								}
								promptBase := o.selectPrompt(issue)
								if respawnErr := o.respawnWorker(slotName, sess, issue, promptBase, fallback); respawnErr != nil {
									log.Printf("[orch] rate-limit fallback respawn %s: %v — marking dead", slotName, respawnErr)
									sess.Status = state.StatusDead
									sess.LastNotifiedStatus = "rate_limit"
									now := time.Now().UTC()
									sess.FinishedAt = &now
									state.MarkWorkerEnded(sess, now)
									o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d) hit rate limit (%s); fallback to %s failed: %v",
										slotName, sess.IssueNumber, pattern, fallback, respawnErr)
									continue
								}

								log.Printf("[orch] rate-limit fallback: respawned %s with backend %s", slotName, fallback)
								o.notifier.Sendf("🔄 maestro: worker %s (issue #%d) hit rate limit — falling back to %s",
									slotName, sess.IssueNumber, fallback)
								continue
							}

							// No fallback available — mark dead
							sess.Status = state.StatusDead
							sess.LastNotifiedStatus = "rate_limit"
							now = time.Now().UTC()
							sess.FinishedAt = &now
							state.MarkWorkerEnded(sess, now)
							o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d) hit rate limit (%s); no fallback configured",
								slotName, sess.IssueNumber, pattern)
							continue
						}
					}

					// --- Soft token threshold: checkpoint + respawn ---
					if o.cfg.WorkerMaxTokens > 0 && o.cfg.SoftTokenThreshold() > 0 && sess.CheckpointFile == "" {
						softLimit := int(float64(o.cfg.WorkerMaxTokens) * o.cfg.SoftTokenThreshold())
						if sess.TokensUsedAttempt >= softLimit {
							log.Printf("[orch] worker %s hit soft token threshold (%d >= %d), checkpointing",
								slotName, sess.TokensUsedAttempt, softLimit)

							// Save checkpoint
							cpFile, cpErr := o.saveCheckpoint(sess)
							if cpErr != nil {
								log.Printf("[orch] warn: checkpoint save failed for %s: %v — will hit hard limit", slotName, cpErr)
							} else {
								sess.CheckpointFile = cpFile

								// Fetch issue for prompt assembly
								issue, fetchErr := o.getIssue(sess.IssueNumber)
								if fetchErr != nil {
									log.Printf("[orch] fetch issue #%d for checkpoint respawn: %v — will hit hard limit", sess.IssueNumber, fetchErr)
								} else {
									promptBase := o.selectPrompt(issue)
									if respawnErr := o.respawnInPlace(slotName, sess, issue, promptBase, sess.Backend); respawnErr != nil {
										log.Printf("[orch] checkpoint respawn %s failed: %v — will hit hard limit", slotName, respawnErr)
									} else {
										log.Printf("[orch] checkpoint respawn complete for %s", slotName)
										o.notifier.Sendf("🔄 maestro: worker %s (issue #%d) hit soft token threshold (%s tokens) — checkpointed and respawned",
											slotName, sess.IssueNumber, worker.FormatTokens(softLimit))
										continue
									}
								}
							}
						}
					}

					// --- Token limit enforcement ---
					if o.cfg.WorkerMaxTokens > 0 && sess.TokensUsedAttempt > o.cfg.WorkerMaxTokens && sess.LastNotifiedStatus != "token_limit" {
						log.Printf("[orch] worker %s exceeded token limit (%d > %d), killing",
							slotName, sess.TokensUsedAttempt, o.cfg.WorkerMaxTokens)
						o.runAfterRunHook(sess)
						if err := o.stopWorker(slotName, sess); err != nil {
							log.Printf("[orch] warn: could not stop token-limit worker %s: %v", slotName, err)
						}
						now := time.Now().UTC()
						sess.Status = state.StatusDead
						sess.LastNotifiedStatus = "token_limit"
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("⚠️ Worker %s (issue #%d) exceeded token limit: %s tokens used (attempt), %s total",
							slotName, sess.IssueNumber, worker.FormatTokens(sess.TokensUsedAttempt), worker.FormatTokens(sess.TokensUsedTotal))
						continue
					}

					// --- Silent worker detection ---
					if o.cfg.WorkerSilentTimeoutMinutes > 0 {
						hash := hashOutput(output)
						now := time.Now().UTC()

						if sess.LastOutputHash == "" || sess.LastOutputChangedAt.IsZero() || hash != sess.LastOutputHash {
							sess.LastOutputHash = hash
							sess.LastOutputChangedAt = now
						} else {
							timeout := time.Duration(o.cfg.WorkerSilentTimeoutMinutes) * time.Minute
							if time.Since(sess.LastOutputChangedAt) > timeout {
								log.Printf("[orch] worker %s silent for >%dm, killing", slotName, o.cfg.WorkerSilentTimeoutMinutes)
								o.runAfterRunHook(sess)
								if err := o.stopWorker(slotName, sess); err != nil {
									log.Printf("[orch] warn: could not stop silent worker %s: %v", slotName, err)
								}

								// Count previous silent-timeout kills before updating this session,
								// so the current kill is not included in the count.
								prevSilentKills := countSilentTimeoutKillsForIssue(s, sess.IssueNumber)

								sess.Status = state.StatusDead
								sess.LastNotifiedStatus = "silent_timeout"
								sess.FinishedAt = &now
								state.MarkWorkerEnded(sess, now)

								if prevSilentKills > 0 {
									// auto-label blocked disabled
								}

								o.notifier.Sendf("⏱️ maestro: worker %s (issue #%d) killed — no output for %d minutes",
									slotName, sess.IssueNumber, o.cfg.WorkerSilentTimeoutMinutes)
								continue
							}
						}
					}
				}
			}

			// Check if worker exceeded max runtime — hard fail (no retry) with diagnostics
			maxMinutes := o.cfg.MaxRuntimeMinutes
			if sess.LongRunning {
				maxMinutes *= 2
			}
			maxRuntime := time.Duration(maxMinutes) * time.Minute
			age := time.Since(sess.StartedAt)
			if age > maxRuntime {
				log.Printf("[orch] worker %s exceeded max runtime (%dm), killing and marking failed", slotName, maxMinutes)

				logTail, err := readLastLines(sess.LogFile, 20)
				if err != nil {
					log.Printf("[orch] warn: could not read log tail for %s (%s): %v", slotName, sess.LogFile, err)
					logTail = fmt.Sprintf("(could not read log file %s: %v)", sess.LogFile, err)
				}

				o.runAfterRunHook(sess)
				if err := o.stopWorker(slotName, sess); err != nil {
					log.Printf("[orch] warn: could not stop timed-out worker %s: %v", slotName, err)
				}
				// auto-label blocked disabled
				o.syncProject(sess.IssueNumber, github.ProjectStatusBlocked)
				sess.Status = state.StatusFailed
				now := time.Now().UTC()
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)

				o.notifier.Sendf("⏱️ maestro: worker %s (issue #%d: %s) timed out after %d min.\nLast log lines:\n%s",
					slotName, sess.IssueNumber, sess.IssueTitle, maxMinutes, logTail)
			}
		}
	}
}

// autoMergePRs checks open PRs and merges ones with green CI
func (o *Orchestrator) autoMergePRs(s *state.State) {
	prs, err := o.listOpenPRs()
	if err != nil {
		log.Printf("[orch] list PRs: %v", err)
		return
	}

	// Build branch/number → PR maps
	branchToPR := make(map[string]github.PR)
	numberToPR := make(map[int]github.PR)
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
		numberToPR[pr.Number] = pr
	}

	type mergeCandidate struct {
		slotName string
		sess     *state.Session
		pr       github.PR
	}

	ready := make([]mergeCandidate, 0)

	for slotName, sess := range s.Sessions {
		if !mergeFlowEligibleStatus(sess) {
			continue
		}

		pr, found := mergeFlowPRForSession(sess, branchToPR, numberToPR)
		if !found {
			if sess.Status == state.StatusRetryExhausted {
				if sess.PRNumber == 0 {
					// #577: worker exhausted retries without ever producing a PR
					// (e.g. the issue was already implemented by a prior merge via
					// `Refs #N`, so the worker found zero diff). Without action the
					// orchestrator would log "waiting for reconciliation" forever
					// and the dynamic-wave queue would halt at max_parallel=1.
					// Reconcile once: either close the issue (when a merged PR
					// already closes it and auto-close is allowed) or label it
					// blocked so the supervisor's dynamic-wave drops it and picks
					// the next eligible candidate.
					o.reconcileNoPRRetryExhausted(slotName, sess)
					continue
				}
				log.Printf("[orch] retry_exhausted session %s records PR #%d, but no open PR was found — waiting for reconciliation", slotName, sess.PRNumber)
				continue
			}
			log.Printf("[orch] no open PR found for branch %s (slot %s) — assuming merged/closed", sess.Branch, slotName)
			if !o.canMarkDoneForOutcome(s, sess, fmt.Sprintf("PR for branch %s is no longer open", sess.Branch)) {
				if sess.PRNumber > 0 {
					o.markCodeLanded(sess, sess.PRNumber)
				}
				continue
			}
			sess.Status = state.StatusDone
			now := time.Now().UTC()
			sess.FinishedAt = &now
			state.MarkWorkerEnded(sess, now)
			continue
		}

		if sess.PRNumber == 0 {
			sess.PRNumber = pr.Number
		}
		o.ensureAttributionTrailerOnPR(slotName, sess, pr)
		o.ensureAttributionTrailerOnBranch(slotName, sess)

		// #705: opt-in visual evidence for UI-affecting PRs. One-shot,
		// advisory — posts a warning comment when evidence is missing but
		// never blocks or delays the merge flow below.
		o.ensureVisualEvidence(slotName, sess, pr)

		// Check CI
		ciStatus, err := o.prCIStatus(pr.Number)
		if err != nil {
			log.Printf("[orch] CI status for PR #%d: %v", pr.Number, err)
			continue
		}

		// #424: the aggregate PRCIStatus can stick at "pending" long after
		// every required check has gone green (a common cause is a legacy
		// commit-status used by some review bots that never resolves).
		// GitHub's own per-PR mergeable_state already encodes the
		// required-check verdict, so "clean" or "unstable" overrides the
		// stale aggregate and lets autoMergePRs converge instead of looping.
		if ciStatus == "pending" {
			if mergeable, mergeState, mErr := o.prMergeStatus(pr.Number); mErr == nil && mergeable == "MERGEABLE" {
				switch mergeState {
				case "clean", "unstable":
					log.Printf("[orch] PR #%d (%s) aggregate CI=pending but mergeable_state=%s — treating as success (#424)", pr.Number, sess.Branch, mergeState)
					ciStatus = "success"
				}
			}
		}

		log.Printf("[orch] PR #%d (%s) CI=%s", pr.Number, sess.Branch, ciStatus)

		switch ciStatus {
		case "success":
			// Reset CI-failure notification state when CI goes green. Keep
			// review retry-exhausted markers so actionable feedback does not
			// re-notify on every orchestration cycle.
			if sess.LastNotifiedStatus == "ci_failure" || sess.LastNotifiedStatus == "ci_retry_exhausted" {
				sess.LastNotifiedStatus = ""
			}
			sess.NotifiedCIFail = false // backward compat

			if o.cfg.AutoRetryReviewFeedback {
				reviewFeedback, err := o.collectPRReviewFeedback(pr.Number)
				if err != nil {
					log.Printf("[orch] warn: could not collect review feedback for PR #%d: %v", pr.Number, err)
				} else if strings.TrimSpace(reviewFeedback) != "" {
					// #556: once we've already marked this PR retry-exhausted
					// for review feedback, do NOT re-emit "scheduling retry"
					// or re-sync the project board every poll. The session
					// has reached a stable terminal state that needs an
					// operator decision, not another refused retry cycle.
					if isSettledRetryExhausted(sess, pr.Number) {
						// #565 convergence: the worker honestly exhausted its
						// review-feedback retries. If no CRITICAL (P0) finding
						// remains on head, do not dead-end — merge the green PR;
						// residual P1/P2/P3 stay as advisory comments on the PR.
						// A P0 on head still hard-blocks (operator/repair).
						if o.cfg.MergeExhaustedNonCriticalReviewEnabled() {
							hasCritical, cerr := o.prHasCriticalReview(pr.Number)
							if cerr != nil {
								log.Printf("[orch] convergence: critical-review check PR #%d failed: %v", pr.Number, cerr)
							} else if !hasCritical {
								log.Printf("[orch] convergence-merge: PR #%d retry-exhausted on review feedback but no P0 on head — merging green PR with residual advisory findings (#565)", pr.Number)
								ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr})
								continue
							}
							log.Printf("[orch] PR #%d retry-exhausted with a P0 finding on head — holding for operator/repair", pr.Number)
						}
						continue
					}
					log.Printf("[orch] PR #%d has review feedback; scheduling retry", pr.Number)
					o.handleReviewFeedbackRetry(s, slotName, sess, pr, reviewFeedback)
					continue
				}
			}

			if o.reviewGate() == "none" {
				ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr})
				continue
			}

			reviewVerdict, err := o.prReviewGateVerdict(pr.Number)
			if err != nil {
				log.Printf("[orch] review gate check PR #%d: %v", pr.Number, err)
				continue // skip this cycle, try next
			}
			if reviewVerdict.Pending {
				log.Printf("[orch] PR #%d waiting for review gate (%s)", pr.Number, reviewVerdict.Summary())
				o.maybeRetriggerStalePendingReview(sess, pr, reviewVerdict)
				continue // not ready yet
			}
			clearReviewPendingTracking(sess)
			if !reviewVerdict.Passed {
				log.Printf("[orch] PR #%d blocked by review gate (%s)", pr.Number, reviewVerdict.Summary())
				// auto-label blocked disabled
				continue
			}

			ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr})
		case "failure":
			if sess.Status == state.StatusQueued {
				sess.Status = state.StatusPROpen
			}
			// Auto-retry on CI failure: close the PR, capture CI output, and schedule retry
			if sess.LastNotifiedStatus != "ci_failure" && sess.LastNotifiedStatus != "ci_retry_exhausted" {
				sess.NotifiedCIFail = true // backward compat

				o.handleCIFailureRetry(s, slotName, sess, pr)
			}
		case "pending":
			if sess.Status == state.StatusQueued {
				sess.Status = state.StatusPROpen
			}
		}
	}

	// #697: a green draft carrying an explicit WIP/Partial marker is a
	// deliberate draft — never auto-ready or merge it. Filter here (not in
	// mergeReadyPR) so a marked draft cannot occupy the single sequential-mode
	// merge slot and starve other ready PRs.
	kept := ready[:0]
	for _, candidate := range ready {
		if candidate.pr.IsDraft && prHasDeliberateDraftMarker(candidate.pr) {
			log.Printf("[orch] PR #%d is a draft with an explicit WIP/Partial marker — skipping auto-ready/merge (#697)", candidate.pr.Number)
			continue
		}
		kept = append(kept, candidate)
	}
	ready = kept

	if len(ready) == 0 {
		return
	}

	sort.Slice(ready, func(i, j int) bool {
		return ready[i].pr.Number < ready[j].pr.Number
	})

	strategy := o.mergeStrategy()
	if strategy == "parallel" {
		for _, candidate := range ready {
			o.mergeReadyPR(s, candidate.slotName, candidate.sess, candidate.pr)
		}
		return
	}

	interval := o.mergeInterval()
	if !s.LastMergeAt.IsZero() {
		sinceLastMerge := time.Since(s.LastMergeAt)
		if sinceLastMerge < interval {
			log.Printf("[orch] sequential merge mode: waiting for merge interval (%s remaining), skipping %d ready PR(s)", (interval - sinceLastMerge).Round(time.Second), len(ready))
			return
		}
	}

	candidate := ready[0]
	o.mergeReadyPR(s, candidate.slotName, candidate.sess, candidate.pr)
	if len(ready) > 1 {
		log.Printf("[orch] sequential merge mode: deferring %d additional ready PR(s) to next cycle", len(ready)-1)
	}
}

// greptileRetriggerComment is the PR comment that asks Greptile to (re)run
// its review — the same comment an operator posts manually to un-wedge a PR.
const greptileRetriggerComment = "@greptile review"

// maybeRetriggerStalePendingReview self-heals the Greptile webhook-miss wedge
// (#691): a PR sits CI=green with zero Greptile review signal on the current
// head and the review gate loops greptile=pending forever. Server-side
// update-branch re-arms the wedge by resetting the gate on the new head.
// When the greptile stream has been pending on the same head SHA for longer
// than review_retrigger.pending_minutes, post "@greptile review" on the PR —
// at most once per review_retrigger.cooldown_minutes — so the review re-runs
// without operator intervention. One info log line per re-trigger.
func (o *Orchestrator) maybeRetriggerStalePendingReview(sess *state.Session, pr github.PR, verdict github.ReviewGateVerdict) {
	if o.cfg == nil || !o.cfg.ReviewRetrigger.Active() {
		return
	}
	if !greptileReviewStreamPending(verdict) {
		return // a non-greptile stream is pending; "@greptile review" won't help
	}
	head, err := o.prHeadSHA(pr.Number)
	if err != nil {
		log.Printf("[orch] review re-trigger: head SHA for PR #%d: %v", pr.Number, err)
		return
	}
	now := time.Now().UTC()
	if sess.ReviewPendingSince == nil || sess.ReviewPendingHeadSHA != head {
		// First pending observation on this head — start the clock. A new
		// head (push or server-side update-branch) restarts it.
		sess.ReviewPendingHeadSHA = head
		sess.ReviewPendingSince = &now
		return
	}
	pendingFor := now.Sub(*sess.ReviewPendingSince)
	if pendingFor < o.cfg.ReviewRetrigger.EffectivePendingFor() {
		return
	}
	if sess.ReviewRetriggerAt != nil && now.Sub(*sess.ReviewRetriggerAt) < o.cfg.ReviewRetrigger.EffectiveCooldown() {
		return
	}
	if err := o.commentPR(pr.Number, greptileRetriggerComment); err != nil {
		log.Printf("[orch] review re-trigger: comment on PR #%d: %v", pr.Number, err)
		return
	}
	sess.ReviewRetriggerAt = &now
	log.Printf("[orch] review re-trigger: PR #%d greptile=pending for %s on head %s with no review — posted %q (#691)",
		pr.Number, pendingFor.Round(time.Second), shortHeadSHA(head), greptileRetriggerComment)
}

// greptileReviewStreamPending reports whether the greptile stream is the one
// holding the review gate at pending.
func greptileReviewStreamPending(verdict github.ReviewGateVerdict) bool {
	for _, stream := range verdict.Streams {
		if stream.Name == "greptile" {
			return stream.Pending
		}
	}
	return false
}

// clearReviewPendingTracking resets the #691 pending clock once the review
// gate resolves (passed or blocked by findings) so a later pending phase on
// the same head starts a fresh window.
func clearReviewPendingTracking(sess *state.Session) {
	sess.ReviewPendingHeadSHA = ""
	sess.ReviewPendingSince = nil
}

func shortHeadSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

func mergeFlowEligibleStatus(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	switch sess.Status {
	case state.StatusPROpen, state.StatusQueued:
		return true
	case state.StatusRetryExhausted:
		return sess.PRNumber > 0 || strings.TrimSpace(sess.Branch) != ""
	default:
		return false
	}
}

// isSettledRetryExhausted reports whether a session has already been marked
// retry_exhausted for this exact PR and notified. Used to suppress the
// retry_exhausted ↔ pr_open flip-flop (#556) and idempotent re-emission of
// "scheduling retry" / project board sync calls.
func isSettledRetryExhausted(sess *state.Session, prNumber int) bool {
	if sess == nil || sess.Status != state.StatusRetryExhausted {
		return false
	}
	if prNumber <= 0 || sess.PRNumber != prNumber {
		return false
	}
	switch sess.LastNotifiedStatus {
	case "review_retry_exhausted", "ci_retry_exhausted", "rebase_conflict_retry_exhausted":
		return true
	}
	return false
}

// prHasDeliberateDraftMarker reports whether a draft PR carries an explicit
// WIP/Partial marker, meaning the draft is deliberate and must NOT be
// auto-readied for merge (#697). The recognised markers are:
//
//   - title: a `[WIP]` or `[Partial]` token, or a `WIP:` / `WIP ` /
//     `Partial:` / `Draft:` prefix (case-insensitive)
//   - body: a literal `maestro:partial` or `maestro:wip` token — the TDD
//     worker prompt's Partial flow embeds `<!-- maestro:partial -->`
//
// Only consulted for PRs with IsDraft=true; a non-draft PR mentioning WIP
// keeps its normal merge flow.
func prHasDeliberateDraftMarker(pr github.PR) bool {
	title := strings.ToLower(strings.TrimSpace(pr.Title))
	if strings.Contains(title, "[wip]") || strings.Contains(title, "[partial]") {
		return true
	}
	for _, prefix := range []string{"wip:", "wip ", "partial:", "draft:"} {
		if strings.HasPrefix(title, prefix) {
			return true
		}
	}
	if title == "wip" || title == "partial" {
		return true
	}
	body := strings.ToLower(pr.Body)
	return strings.Contains(body, "maestro:partial") || strings.Contains(body, "maestro:wip")
}

func mergeFlowPRForSession(sess *state.Session, byBranch map[string]github.PR, byNumber map[int]github.PR) (github.PR, bool) {
	if sess == nil {
		return github.PR{}, false
	}
	if sess.PRNumber > 0 {
		if pr, ok := byNumber[sess.PRNumber]; ok {
			return pr, true
		}
	}
	if strings.TrimSpace(sess.Branch) != "" {
		if pr, ok := byBranch[sess.Branch]; ok {
			return pr, true
		}
	}
	return github.PR{}, false
}

// noPRReconciledStatus marks a retry_exhausted session whose worker never
// produced a PR as having been reconciled by autoMergePRs. The marker keeps
// the reconciliation idempotent so subsequent cycles do not re-label, re-close,
// or re-notify, and so the "waiting for reconciliation" log stops firing.
const noPRReconciledStatus = "no_pr_reconciled"

// reconcileNoPRRetryExhausted handles the #577 deadlock: a worker exhausted
// retries without opening a PR (often because the issue was already
// implemented by a prior merge via `Refs #N`), so the merge flow has nothing
// to advance, the session is terminal, and at max_parallel=1 the dynamic-wave
// queue halts. This helper runs once per session: it auto-closes the issue
// when a merged PR already closes it (and close_issue is a configured safe
// action), otherwise it adds the configured blocked label so the supervisor
// drops the issue from the wave and selects the next eligible candidate.
func (o *Orchestrator) reconcileNoPRRetryExhausted(slotName string, sess *state.Session) {
	if sess == nil || sess.IssueNumber <= 0 {
		return
	}
	if sess.LastNotifiedStatus == noPRReconciledStatus {
		// Already reconciled — suppress duplicate side-effects and the
		// "waiting for reconciliation" log spam.
		return
	}

	merged, err := o.hasMergedPRForIssue(sess.IssueNumber)
	if err != nil {
		// Don't mark reconciled on transient GitHub failure; try again next
		// cycle. Log so operators can correlate with API errors.
		log.Printf("[orch] no-PR retry_exhausted: could not check merged PR for issue #%d (slot %s): %v", sess.IssueNumber, slotName, err)
		return
	}

	if merged {
		// The work was already landed by another PR (closing-keyword link).
		// Auto-close when the operator has granted close_issue as a safe
		// action without an approval gate; otherwise surface it as a
		// close-candidate via notification.
		comment := fmt.Sprintf("Maestro: closing this issue because worker session %s exhausted retries without producing a PR (the work appears to be implemented by an already-merged PR).", slotName)
		if o.supervisorActionAllowed(config.SupervisorActionCloseIssue) && !o.supervisorActionRequiresApproval(config.SupervisorActionCloseIssue) {
			if cerr := o.closeIssue(sess.IssueNumber, comment); cerr != nil {
				log.Printf("[orch] no-PR retry_exhausted: auto-close failed for issue #%d (slot %s): %v", sess.IssueNumber, slotName, cerr)
				if o.notifier != nil {
					o.notifier.Sendf("⚠️ maestro: issue #%d retry_exhausted with no PR (work appears merged); auto-close failed: %v", sess.IssueNumber, cerr)
				}
				return
			}
			log.Printf("[orch] no-PR retry_exhausted: auto-closed issue #%d (slot %s) — merged PR already implements it", sess.IssueNumber, slotName)
			if o.notifier != nil {
				o.notifier.Sendf("✅ maestro: closed issue #%d after retry_exhausted with no PR (already implemented by a merged PR)", sess.IssueNumber)
			}
			o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
		} else {
			log.Printf("[orch] no-PR retry_exhausted: issue #%d (slot %s) has a merged PR — surfaced as operator close-candidate", sess.IssueNumber, slotName)
			if o.notifier != nil {
				o.notifier.Sendf("⚠️ maestro: issue #%d retry_exhausted with no PR; a merged PR already implements it — operator close-candidate", sess.IssueNumber)
			}
		}
	} else {
		// No merged PR detected — apply the configured blocked label so the
		// supervisor's dynamic-wave skips this issue on the next cycle and
		// the run-loop advances to the next eligible candidate.
		if blockedLabel := strings.TrimSpace(o.cfg.Supervisor.BlockedLabel); blockedLabel != "" {
			if lerr := o.addIssueLabel(sess.IssueNumber, blockedLabel); lerr != nil {
				log.Printf("[orch] no-PR retry_exhausted: could not add %q label to issue #%d (slot %s): %v", blockedLabel, sess.IssueNumber, slotName, lerr)
				if o.notifier != nil {
					o.notifier.Sendf("⚠️ maestro: issue #%d retry_exhausted with no PR; could not apply %q label: %v", sess.IssueNumber, blockedLabel, lerr)
				}
				return
			}
			log.Printf("[orch] no-PR retry_exhausted: labelled issue #%d %q (slot %s, no PR produced and no merged PR detected)", sess.IssueNumber, blockedLabel, slotName)
		} else {
			log.Printf("[orch] no-PR retry_exhausted: issue #%d (slot %s) produced no PR and no merged PR detected — operator review required (no blocked_label configured)", sess.IssueNumber, slotName)
		}
		if o.notifier != nil {
			o.notifier.Sendf("⚠️ maestro: issue #%d retry_exhausted with no PR — needs operator review", sess.IssueNumber)
		}
		o.syncProject(sess.IssueNumber, github.ProjectStatusBlocked)
	}

	// Mark the session reconciled so future cycles short-circuit. The status
	// stays retry_exhausted (terminal) but the marker prevents repeated
	// labels, close attempts, or notifications.
	sess.LastNotifiedStatus = noPRReconciledStatus
}

// handleReviewFeedbackRetry schedules a retry worker with review feedback in
// its prompt. When the PR worktree is still available, keep the PR open and
// respawn in place so the fixer pushes updates to the same PR.
func (o *Orchestrator) handleReviewFeedbackRetry(s *state.State, slotName string, sess *state.Session, pr github.PR, reviewFeedback string) {
	maxRetries := o.maintenanceRetryBudget()
	if !o.canRetryPRMaintenance(sess) {
		// #556: when the session is already settled retry-exhausted on
		// this PR, short-circuit so the orchestrator does not re-emit
		// the retry-limit log, re-sync the project board, or churn
		// FinishedAt on every poll. The caller in mergeFlow already
		// guards this path; we re-check here so any direct callers stay
		// idempotent too.
		if isSettledRetryExhausted(sess, pr.Number) {
			return
		}
		log.Printf("[orch] review feedback on PR #%d — maintenance retry limit reached (%d/%d) for issue #%d",
			pr.Number, sess.MaintenanceRetryCount, maxRetries, sess.IssueNumber)
		alreadyNotified := sess.LastNotifiedStatus == "review_retry_exhausted"
		s.MarkIssueRetryExhausted(sess.IssueNumber)
		o.syncProject(sess.IssueNumber, github.ProjectStatusBlocked)
		sess.Status = state.StatusRetryExhausted
		sess.NextRetryAt = nil
		sess.LastNotifiedStatus = "review_retry_exhausted"
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		if !alreadyNotified {
			o.notifier.Sendf("💀 maestro: review feedback on PR #%d (issue #%d: %s) — maintenance retry limit exhausted (%d attempts)",
				pr.Number, sess.IssueNumber, sess.IssueTitle, sess.MaintenanceRetryCount)
		}
		return
	}

	if sess.Worktree == "" {
		closeComment := fmt.Sprintf("Code review feedback detected, but the PR worktree is unavailable — maestro is closing this PR and respawning a worker to address it (attempt %d).\n\nReview feedback:\n\n%s",
			sess.RetryCount+1, reviewFeedback)
		if err := o.closePR(pr.Number, closeComment); err != nil {
			log.Printf("[orch] warn: could not close PR #%d after review feedback: %v — skipping retry", pr.Number, err)
			return
		}
		log.Printf("[orch] closed PR #%d due to review feedback (worktree unavailable)", pr.Number)
		sess.PRNumber = 0
	} else {
		log.Printf("[orch] keeping PR #%d open and respawning %s in place to address review feedback", pr.Number, slotName)
		sess.PRNumber = pr.Number
	}

	sess.CIFailureOutput = ""
	sess.PreviousAttemptFeedback = reviewFeedback
	sess.PreviousAttemptFeedbackKind = state.RetryReasonReviewFeedback
	sess.RetryReason = state.RetryReasonReviewFeedback

	sess.MaintenanceRetryCount++
	backoffMs := retryBackoffMs(sess.MaintenanceRetryCount, o.cfg.MaxRetryBackoffMs)
	retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
	sess.NextRetryAt = &retryAt
	sess.Status = state.StatusDead
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)

	log.Printf("[orch] review feedback on PR #%d — scheduling maintenance retry %d/%d in %dms for issue #%d",
		pr.Number, sess.MaintenanceRetryCount, maxRetries, backoffMs, sess.IssueNumber)
	o.notifier.Sendf("🔄 maestro: review feedback on PR #%d (issue #%d: %s), in-place maintenance retry %d/%d scheduled in %ds",
		pr.Number, sess.IssueNumber, sess.IssueTitle, sess.MaintenanceRetryCount, maxRetries, backoffMs/1000)
}

// handleCIFailureRetry closes the failed PR, captures CI output, cleans up,
// and schedules a retry for the worker (respecting max_retries_per_issue).
func (o *Orchestrator) handleCIFailureRetry(s *state.State, slotName string, sess *state.Session, pr github.PR) {
	maxRetries := o.cfg.MaxRetriesPerIssue
	totalAttempts := s.FailedAttemptsForIssue(sess.IssueNumber) + sess.RetryCount

	if maxRetries > 0 && totalAttempts >= maxRetries {
		log.Printf("[orch] CI failure on PR #%d — retry limit reached (%d/%d) for issue #%d",
			pr.Number, totalAttempts, maxRetries, sess.IssueNumber)
		alreadyNotified := sess.LastNotifiedStatus == "ci_retry_exhausted"
		// auto-label blocked disabled
		s.MarkIssueRetryExhausted(sess.IssueNumber)
		o.syncProject(sess.IssueNumber, github.ProjectStatusBlocked)
		sess.Status = state.StatusRetryExhausted
		sess.NextRetryAt = nil
		sess.LastNotifiedStatus = "ci_retry_exhausted"
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		if !alreadyNotified {
			o.notifier.Sendf("💀 maestro: CI failing on PR #%d (issue #%d: %s) — retry limit exhausted (%d attempts)",
				pr.Number, sess.IssueNumber, sess.IssueTitle, totalAttempts)
		}
		return
	}

	// Capture CI failure output before closing the PR
	ciOutput, err := o.prChecksOutput(pr.Number)
	if err != nil {
		log.Printf("[orch] warn: could not capture CI output for PR #%d: %v", pr.Number, err)
		ciOutput = "(CI output unavailable)"
	}

	// Collect Greptile review feedback before closing the PR
	reviewFeedback, err := o.collectPRReviewFeedback(pr.Number)
	if err != nil {
		log.Printf("[orch] warn: could not collect review feedback for PR #%d: %v", pr.Number, err)
	}

	// Close the failed PR with an explanation
	closeComment := fmt.Sprintf("CI failed — maestro is closing this PR and respawning a new worker to retry (attempt %d).\n\nCI output:\n```\n%s\n```",
		sess.RetryCount+1, ciOutput)
	if err := o.closePR(pr.Number, closeComment); err != nil {
		log.Printf("[orch] warn: could not close PR #%d: %v — skipping retry", pr.Number, err)
		return
	}
	log.Printf("[orch] closed PR #%d due to CI failure", pr.Number)

	// Clean up the worktree
	o.stopWorker(slotName, sess)
	sess.Worktree = ""

	// Store CI failure output and review feedback for the next worker
	sess.CIFailureOutput = ciOutput
	sess.PreviousAttemptFeedback = reviewFeedback
	if strings.TrimSpace(reviewFeedback) != "" {
		sess.PreviousAttemptFeedbackKind = "review_feedback"
	} else {
		sess.PreviousAttemptFeedbackKind = ""
	}

	// Schedule retry with exponential backoff
	sess.RetryCount++
	backoffMs := retryBackoffMs(sess.RetryCount, o.cfg.MaxRetryBackoffMs)
	retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
	sess.NextRetryAt = &retryAt
	sess.Status = state.StatusDead
	sess.PRNumber = 0
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)

	log.Printf("[orch] CI failure on PR #%d — scheduling retry %d in %dms for issue #%d",
		pr.Number, sess.RetryCount, backoffMs, sess.IssueNumber)
	o.notifier.Sendf("🔄 maestro: CI failed on PR #%d (issue #%d: %s), retry %d scheduled in %ds",
		pr.Number, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, backoffMs/1000)
}

func (o *Orchestrator) reviewGate() string {
	switch strings.ToLower(strings.TrimSpace(o.cfg.ReviewGate)) {
	case "none", "off", "disabled":
		return "none"
	default:
		return "greptile"
	}
}

func (o *Orchestrator) mergeStrategy() string {
	strategy := strings.ToLower(strings.TrimSpace(o.cfg.MergeStrategy))
	if strategy == "parallel" {
		return "parallel"
	}
	return "sequential"
}

func (o *Orchestrator) mergeInterval() time.Duration {
	seconds := o.cfg.MergeIntervalSeconds
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func (o *Orchestrator) markCodeLanded(sess *state.Session, prNumber int) {
	if prNumber > 0 {
		sess.PRNumber = prNumber
	}
	sess.DeploymentFinishedAt = nil
	o.syncProject(sess.IssueNumber, codeLandedProjectStatus(o.cfg.Outcome.RequiresDeploy))
	sess.Status = state.StatusCodeLanded
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
}

func (o *Orchestrator) markDeploymentFinished(sess *state.Session) {
	if sess == nil {
		return
	}
	now := time.Now().UTC()
	sess.DeploymentFinishedAt = &now
	if sess.Status == state.StatusCodeLanded {
		o.syncProject(sess.IssueNumber, github.ProjectStatusLiveVerify)
	}
}

func (o *Orchestrator) markDoneAfterOutcomePass(sess *state.Session, prNumber int) bool {
	if sess == nil {
		return false
	}
	if prNumber > 0 {
		sess.PRNumber = prNumber
	}
	// #443: issue-specific completion gates. Healthz alone is not enough to
	// close UI/design work that requires live visual or deployment-status
	// verification. When the configured gates match this issue (by label
	// or body marker), hold the session in code_landed and move the
	// Project item to Live Verification so an operator can drive the
	// last-mile check instead of Maestro silently closing.
	if o.completionGatesRequireLiveVerification(sess) {
		o.holdForLiveVerification(sess, prNumber)
		return false
	}
	sess.Status = state.StatusDone
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
	if o.closeVerifiedIssueIfAllowed(sess, prNumber) {
		o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
		return true
	}
	o.syncProject(sess.IssueNumber, github.ProjectStatusAwaitingClose)
	return false
}

// completionGatesRequireLiveVerification returns true when the supervisor
// completion-gates config marks this issue as needing live verification
// (by label or body marker). Falls back to false when no gates are
// configured (legacy behaviour), or when the issue lookup fails — gate
// failure must not silently force-close an issue, but a transient
// GitHub read error should also not strand a session in code_landed
// forever, so we surface the read error in the log and proceed with the
// pre-#443 close path.
func (o *Orchestrator) completionGatesRequireLiveVerification(sess *state.Session) bool {
	if o == nil || o.cfg == nil || sess == nil || sess.IssueNumber <= 0 {
		return false
	}
	gates := o.cfg.Supervisor.CompletionGates
	if !gates.Active() {
		return false
	}
	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[orch] completion-gates lookup for issue #%d failed; falling back to legacy close path: %v", sess.IssueNumber, err)
		return false
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, l.Name)
	}
	return gates.IssueRequiresLiveVerification(labels, issue.Body)
}

// holdForLiveVerification keeps sess in code_landed when the completion
// gates require live verification, syncs the Project board to the live-
// verification column, and emits an operator-visible notification so the
// last-mile check is not invisible. Idempotent: repeated calls do not
// re-notify (we only sync the board if the session is still in
// code_landed; the operator can flip to done manually once verified).
func (o *Orchestrator) holdForLiveVerification(sess *state.Session, prNumber int) {
	if sess == nil {
		return
	}
	if prNumber > 0 {
		sess.PRNumber = prNumber
	}
	if sess.Status != state.StatusCodeLanded {
		sess.Status = state.StatusCodeLanded
	}
	// Idempotent (#570): reconcileCodeLandedSessions re-enters this on every
	// tick while the gate holds, so the board sync + operator notification
	// must fire only once. Without this guard an issue held for an hour at a
	// 30s reconcile interval emits ~120 duplicate notifications. The status
	// correction above stays unconditional (drift repair); the side effects
	// below are one-shot.
	if sess.LiveVerificationNotified {
		return
	}
	o.syncProject(sess.IssueNumber, github.ProjectStatusLiveVerify)
	log.Printf("[orch] issue #%d passed healthz but completion gates require live verification; holding in code_landed", sess.IssueNumber)
	if o.notifier != nil {
		if prNumber > 0 {
			o.notifier.Sendf("⏸ maestro: PR #%d merged for issue #%d, but completion gates require live verification; issue not closed", prNumber, sess.IssueNumber)
		} else {
			o.notifier.Sendf("⏸ maestro: issue #%d completion gates require live verification; not closing on healthz alone", sess.IssueNumber)
		}
	}
	sess.LiveVerificationNotified = true
}

func (o *Orchestrator) reconcileCodeLandedSessions(s *state.State) {
	if o == nil || o.cfg == nil || s == nil {
		return
	}

	codeLanded := make([]*state.Session, 0)
	for _, slotName := range sortedStateSessionNames(s) {
		sess := s.Sessions[slotName]
		if sess != nil && sess.Status == state.StatusCodeLanded {
			codeLanded = append(codeLanded, sess)
		}
	}
	if len(codeLanded) == 0 {
		return
	}

	if o.cfg.Outcome.PassRequiredForDoneEnabled() {
		if !o.cfg.Outcome.HasHealthSignal() {
			log.Printf("[orch] %d code_landed session(s) need outcome verification, but no health signal is configured", len(codeLanded))
			return
		}
		result := o.checkOutcome(context.Background())
		s.OutcomeHealth = &result
		if result.State != outcome.HealthHealthy {
			log.Printf("[orch] code_landed reconcile held: outcome verifier is %s: %s", result.State, result.Summary)
			for _, sess := range codeLanded {
				o.syncProject(sess.IssueNumber, github.ProjectStatusLiveVerify)
			}
			return
		}
	}

	for _, sess := range codeLanded {
		if !o.codeLandedPRMerged(sess) {
			continue
		}
		log.Printf("[orch] code_landed session for issue #%d passed outcome reconciliation; marking done", sess.IssueNumber)
		if o.markDoneAfterOutcomePass(sess, sess.PRNumber) {
			now := time.Now().UTC()
			stale := s.MarkCloseIssueApprovalsStaleForVerifiedIssue(sess.IssueNumber, now)
			if stale > 0 {
				log.Printf("[orch] expired %d stale close_issue approval(s) for auto-closed issue #%d", stale, sess.IssueNumber)
			}
			if rstale := s.MarkSpawnRepairWorkerApprovalsStaleForResolvedIssue(sess.IssueNumber, now); rstale > 0 {
				log.Printf("[orch] expired %d stale spawn_repair_worker approval(s) for auto-closed issue #%d", rstale, sess.IssueNumber)
			}
		}
	}
}

func (o *Orchestrator) codeLandedPRMerged(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	if sess.PRNumber > 0 {
		merged, err := o.isPRMerged(sess.PRNumber)
		if err != nil {
			log.Printf("[orch] code_landed reconcile could not check PR #%d: %v", sess.PRNumber, err)
			return false
		}
		if !merged {
			log.Printf("[orch] code_landed session for issue #%d records PR #%d, but GitHub does not report it merged", sess.IssueNumber, sess.PRNumber)
			return false
		}
		return true
	}
	merged, err := o.hasMergedPRForIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[orch] code_landed reconcile could not check merged PR for issue #%d: %v", sess.IssueNumber, err)
		return false
	}
	return merged
}

func (o *Orchestrator) closeVerifiedIssueIfAllowed(sess *state.Session, prNumber int) bool {
	if o == nil || o.cfg == nil || sess == nil || sess.IssueNumber <= 0 {
		return false
	}
	closed, err := o.isIssueClosed(sess.IssueNumber)
	if err != nil {
		log.Printf("[orch] verified issue #%d close precheck skipped: %v", sess.IssueNumber, err)
	} else if closed {
		return true
	}

	comment := "Maestro verified the configured runtime outcome after code landed; closing this issue as done."
	if prNumber > 0 {
		comment = fmt.Sprintf("Maestro verified the configured runtime outcome after PR #%d landed; closing this issue as done.", prNumber)
	}
	if err := o.closeIssue(sess.IssueNumber, comment); err != nil {
		log.Printf("[orch] verified issue #%d was not auto-closed: %v", sess.IssueNumber, err)
		if o.notifier != nil {
			o.notifier.Sendf("⚠️ maestro: verified issue #%d is done but auto-close failed: %v", sess.IssueNumber, err)
		}
		return false
	}
	if o.notifier != nil {
		o.notifier.Sendf("✅ maestro: closed verified issue #%d after code_landed outcome pass", sess.IssueNumber)
	}
	log.Printf("[orch] auto-closed issue #%d after verified merge", sess.IssueNumber)
	return true
}

func (o *Orchestrator) supervisorActionAllowed(action string) bool {
	return o != nil && o.cfg != nil && o.cfg.Supervisor.AllowsSafeAction(action)
}

func (o *Orchestrator) supervisorActionRequiresApproval(action string) bool {
	if o == nil || o.cfg == nil {
		return true
	}
	action = strings.TrimSpace(action)
	for _, configured := range o.cfg.Supervisor.ApprovalRequired {
		if strings.TrimSpace(configured) == action {
			return true
		}
	}
	for _, configured := range o.cfg.Supervisor.ApprovalRequiredActions {
		if strings.TrimSpace(configured) == action {
			return true
		}
	}
	return false
}

func (o *Orchestrator) canMarkDoneForOutcome(s *state.State, sess *state.Session, trigger string) bool {
	if o == nil || o.cfg == nil || !o.cfg.Outcome.PassRequiredForDoneEnabled() {
		return true
	}
	donePRs := 0
	var lastMergeAt time.Time
	if s != nil {
		donePRs = s.DonePRCount()
		lastMergeAt = s.LastMergeAt
	}
	status := outcome.StatusFor(o.cfg.Outcome, donePRs, lastMergeAt, outcomeHealthChecks(s)...)
	if status.HealthState == outcome.HealthHealthy {
		return true
	}
	issue := 0
	if sess != nil {
		issue = sess.IssueNumber
	}
	trigger = strings.TrimSpace(trigger)
	if trigger == "" {
		trigger = fmt.Sprintf("issue #%d reached a done-like state", issue)
	}
	log.Printf("[orch] %s, but outcome health is %s; keeping session out of done until live verification passes", trigger, status.HealthState)
	if sess != nil && sess.Status == state.StatusCodeLanded {
		o.syncProject(sess.IssueNumber, codeLandedProjectStatus(o.cfg.Outcome.RequiresDeploy))
	}
	return false
}

func (o *Orchestrator) canTreatIssueDoneForQueue(s *state.State, issueNumber int, reason string) bool {
	if o.canMarkDoneForOutcome(s, nil, fmt.Sprintf("ordered queue saw issue #%d as done via %s", issueNumber, reason)) {
		return true
	}
	log.Printf("[orch] ordered queue will not advance past issue #%d: %s is not enough until outcome verification passes", issueNumber, reason)
	return false
}

func outcomeHealthChecks(s *state.State) []outcome.HealthCheckResult {
	if s == nil || s.OutcomeHealth == nil {
		return nil
	}
	return []outcome.HealthCheckResult{*s.OutcomeHealth}
}

func (o *Orchestrator) mergeReadyPR(s *state.State, slotName string, sess *state.Session, pr github.PR) bool {
	// #697: `gh pr merge` on a draft fails with "still a draft", wedging the
	// pipeline until a human runs `gh pr ready`. A PR that reached this point
	// is green (CI pass + review gate resolved), so an unmarked draft is an
	// accident — ready it and merge in the same cycle. Deliberate WIP/Partial
	// drafts are filtered out by autoMergePRs before they get here; the guard
	// below is defense-in-depth for any other caller.
	if pr.IsDraft {
		if prHasDeliberateDraftMarker(pr) {
			log.Printf("[orch] PR #%d is a draft with an explicit WIP/Partial marker — skipping auto-ready/merge (#697)", pr.Number)
			return false
		}
		log.Printf("[orch] PR #%d is a green draft without a WIP/Partial marker — marking ready for review (#697)", pr.Number)
		if err := o.markPRReady(pr.Number); err != nil {
			log.Printf("[orch] mark PR #%d ready: %v", pr.Number, err)
			return false
		}
		// One-time mention: after a successful ready the PR stops being a
		// draft, so this path cannot fire again for the same PR.
		o.notifier.Sendf("📣 maestro: PR #%d (%s) was a green draft — marked ready for review, proceeding to merge", pr.Number, sess.Branch)
	}

	log.Printf("[orch] merging PR #%d (branch %s)", pr.Number, sess.Branch)
	if err := o.mergePR(pr.Number); err != nil {
		log.Printf("[orch] merge PR #%d: %v", pr.Number, err)

		// If the branch is behind main (not conflicting, just outdated),
		// rebase the worktree when present; otherwise (worker already cleaned
		// up, or the local rebase fails) fall back to server-side
		// update-branch (#551) so a behind PR still converges to merge
		// instead of dead-ending as an unresolvable conflict.
		if strings.Contains(err.Error(), "not up to date") && o.cfg.AutoRebase {
			log.Printf("[orch] PR #%d branch is behind main, updating %s", pr.Number, slotName)
			rebased := false
			if strings.TrimSpace(sess.Worktree) != "" {
				if rebaseErr := o.rebaseWorktree(sess.Worktree, sess.Branch); rebaseErr != nil {
					log.Printf("[orch] auto-rebase failed for %s: %v — falling back to server-side update-branch", slotName, rebaseErr)
				} else {
					rebased = true
				}
			}
			if !rebased {
				if upErr := o.updateBranch(pr.Number); upErr != nil {
					log.Printf("[orch] update-branch fallback for PR #%d failed: %v", pr.Number, upErr)
					o.markUnresolvableConflict(slotName, sess, pr.Number, upErr)
					return false
				}
				log.Printf("[orch] PR #%d updated via server-side update-branch — re-validating next cycle", pr.Number)
			}
			o.markRebaseQueued(slotName, sess, pr.Number)
			return false
		}

		// Real-conflict path (#602): a merge failure that is NOT "not up to
		// date" can still be a real merge conflict (GitHub returns variants
		// like "merge commit cannot be cleanly created"). Confirm with the
		// authoritative mergeable verdict and route to the conflict-resolution
		// path so retry_exhausted convergence candidates can't loop forever and
		// halt the queue.
		if o.routeConflictingMergeFailure(s, slotName, sess, pr, err) {
			return false
		}

		// Only notify merge failure once per PR
		if sess.LastNotifiedStatus != "merge_failed" {
			o.notifier.Sendf("❌ maestro: failed to merge PR #%d (%s): %v", pr.Number, sess.Branch, err)
			sess.LastNotifiedStatus = "merge_failed"
		}
		return false
	}

	log.Printf("[orch] merged PR #%d ✓", pr.Number)
	if s != nil {
		s.LastMergeAt = time.Now().UTC()
	}
	o.markCodeLanded(sess, pr.Number)

	if o.cfg.ShouldCleanupWorktrees() {
		log.Printf("[orch] cleaning up worktree for %s after merge", slotName)
		o.stopWorker(slotName, sess)
		sess.Worktree = "" // Mark as cleaned
	} else {
		log.Printf("[orch] skipping worktree cleanup for %s (cleanup_worktrees_on_merge=false)", slotName)
	}

	o.notifier.Sendf("✅ maestro: merged PR #%d for issue #%d (%s); issue remains open for runtime verification", pr.Number, sess.IssueNumber, sess.IssueTitle)

	// Auto version bump
	if o.cfg.Versioning.Enabled {
		if err := versioning.Run(o.cfg, o.gh, pr.Number); err != nil {
			log.Printf("[orch] version bump for PR #%d: %v", pr.Number, err)
			o.notifier.Sendf("⚠️ maestro: version bump failed for PR #%d: %v", pr.Number, err)
		} else {
			o.notifier.Sendf("🏷️ maestro: version bumped after PR #%d merge", pr.Number)
		}
	}

	// Self-deploy hook (#698)
	o.maybeSelfDeployAfterMerge(s, pr.Number)

	// Deploy hook
	deploySucceeded := false
	if o.cfg.DeployCmd != "" {
		if err := o.runDeployCmd(pr.Number); err != nil {
			log.Printf("[orch] deploy command failed for PR #%d: %v", pr.Number, err)
			o.notifier.Sendf("⚠️ maestro: deploy failed after PR #%d merge: %v", pr.Number, err)
		} else {
			deploySucceeded = true
			o.markDeploymentFinished(sess)
			o.notifier.Sendf("🚀 maestro: deploy succeeded after PR #%d merge", pr.Number)
		}
	} else if !o.cfg.Outcome.RequiresDeploy {
		deploySucceeded = true
	}

	if o.shouldVerifyOutcomeAfterMerge(deploySucceeded) {
		o.verifyOutcomeAfterMerge(s, sess, pr.Number)
	}

	return true
}

func (o *Orchestrator) shouldVerifyOutcomeAfterMerge(deploySucceeded bool) bool {
	if o == nil || o.cfg == nil {
		return false
	}
	if !o.cfg.Outcome.PassRequiredForDoneEnabled() || !o.cfg.Outcome.HasHealthSignal() {
		return false
	}
	if o.cfg.Outcome.RequiresDeploy && !deploySucceeded {
		log.Printf("[orch] skipping immediate outcome verification after merge: deploy is required and has not succeeded yet")
		return false
	}
	return true
}

func (o *Orchestrator) verifyOutcomeAfterMerge(s *state.State, sess *state.Session, prNumber int) {
	if s == nil || sess == nil {
		return
	}
	log.Printf("[orch] running outcome verifier after PR #%d merge", prNumber)
	result := o.checkOutcome(context.Background())
	s.OutcomeHealth = &result
	if result.State == outcome.HealthHealthy {
		log.Printf("[orch] outcome verifier passed after PR #%d; marking issue #%d done", prNumber, sess.IssueNumber)
		if o.markDoneAfterOutcomePass(sess, prNumber) {
			now := time.Now().UTC()
			stale := s.MarkCloseIssueApprovalsStaleForVerifiedIssue(sess.IssueNumber, now)
			if stale > 0 {
				log.Printf("[orch] expired %d stale close_issue approval(s) for auto-closed issue #%d", stale, sess.IssueNumber)
			}
			if rstale := s.MarkSpawnRepairWorkerApprovalsStaleForResolvedIssue(sess.IssueNumber, now); rstale > 0 {
				log.Printf("[orch] expired %d stale spawn_repair_worker approval(s) for auto-closed issue #%d", rstale, sess.IssueNumber)
			}
		}
		o.notifier.Sendf("✅ maestro: outcome verifier passed after PR #%d; issue #%d can be treated as done", prNumber, sess.IssueNumber)
		return
	}
	if sess.Status == state.StatusCodeLanded {
		o.syncProject(sess.IssueNumber, github.ProjectStatusLiveVerify)
	}
	log.Printf("[orch] outcome verifier after PR #%d is %s: %s", prNumber, result.State, result.Summary)
	o.notifier.Sendf("⚠️ maestro: outcome verifier after PR #%d is %s: %s", prNumber, result.State, result.Summary)
}

// runDeployCmd executes the configured deploy command with a configurable timeout.
func (o *Orchestrator) runDeployCmd(prNumber int) error {
	timeout := time.Duration(o.cfg.DeployTimeoutMinutes) * time.Minute
	log.Printf("[orch] running deploy command after PR #%d merge (timeout %dm): %s", prNumber, o.cfg.DeployTimeoutMinutes, o.cfg.DeployCmd)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", o.cfg.DeployCmd)
	cmd.Dir = o.cfg.LocalPath
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Printf("[orch] deploy output:\n%s", out)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("deploy command timed out after %d minutes", o.cfg.DeployTimeoutMinutes)
	}
	if err != nil {
		return fmt.Errorf("deploy command failed: %w\n%s", err, out)
	}
	return nil
}

// maybeSelfDeployAfterMerge launches the opt-in self-deploy (#698) for a
// merged PR. Default OFF: without self_deploy.enabled this is a no-op. The
// deploy script runs in a detached transient unit and restarts this very
// process, so we must not wait on it here — the outcome lands as a
// supervisor finding when the result file is consumed on a later cycle (see
// consumeSelfDeployResult).
func (o *Orchestrator) maybeSelfDeployAfterMerge(s *state.State, prNumber int) {
	if !o.cfg.SelfDeploy.Enabled {
		return
	}
	now := time.Now().UTC()
	// #722: debounce re-triggers. The deploy restarts the run-loop's own unit,
	// so a burst of merges — or a run-loop restarted by its own deploy — can
	// re-fire a fresh deploy while a previous one is still in flight (build +
	// drain + verify + rollback). That stacks deploys that bounce the fleet web
	// process mid-verify, so verify never converges (the cascade in #722). Skip
	// if we triggered a deploy within the min-interval window.
	if last, lastPR, ok := selfdeploy.LastTrigger(o.cfg.StateDir); ok {
		window := time.Duration(o.cfg.SelfDeploy.EffectiveMinIntervalMinutes()) * time.Minute
		if since := now.Sub(last); since >= 0 && since < window {
			log.Printf("[orch] self-deploy debounced for PR #%d: last trigger (PR #%d) was %s ago (< %s window)", prNumber, lastPR, since.Round(time.Second), window)
			return
		}
	}
	if err := o.triggerSelfDeploy(prNumber); err != nil {
		// #758: the central (daemon) launcher debounced this request — another
		// flow's deploy already covers this merge wave. Not a failure: don't
		// notify, don't record a finding, and don't write a per-flow marker
		// (the central marker owns the debounce). A later merge re-checks.
		if errors.Is(err, selfdeploy.ErrDebounced) {
			log.Printf("[orch] self-deploy for PR #%d debounced centrally — another flow's deploy covers this wave", prNumber)
			return
		}
		log.Printf("[orch] self-deploy trigger failed for PR #%d: %v", prNumber, err)
		o.notifier.Sendf("⚠️ maestro: self-deploy trigger failed after PR #%d merge: %v — fleet still on the previous binary", prNumber, err)
		// #742: surface the failure as a supervisor finding so a merge that
		// silently failed to deploy shows up as fleet attention rather than a
		// lone log line; the next merge retries (the trigger marker is recorded
		// only on success, below).
		if s != nil {
			s.RecordSupervisorDecision(selfdeploy.TriggerFailedFinding(prNumber, err, o.cfg.Repo, now), state.DefaultSupervisorDecisionLimit)
		}
		return
	}
	// Record the trigger now that the deploy is launched. The detached script
	// fetches+builds before it restarts any unit, so this marker lands well
	// before the run-loop's own restart — a run-loop restarted by this deploy
	// then sees it on its next cycle and does not re-fire for the same wave
	// (#722). Recording only on success keeps a pure trigger failure (no deploy
	// happened) from suppressing the next attempt.
	if err := selfdeploy.RecordTrigger(o.cfg.StateDir, prNumber, now); err != nil {
		log.Printf("[orch] self-deploy trigger marker write failed for PR #%d: %v", prNumber, err)
	}
	log.Printf("[orch] self-deploy started for PR #%d (detached transient unit)", prNumber)
	o.notifier.Sendf("🚀 maestro: self-deploy started after PR #%d merge — units will restart after drain", prNumber)
}

// maybeSelfDeployOnMainAdvance triggers the opt-in self-deploy (#698) when the
// orchestrator OBSERVES origin/main advancing past the running binary — i.e. a
// PR merged outside the orchestrator's own merge path: via the GitHub UI, a
// manual `gh pr merge`, or the approval-gate executor (#751). Without this, only
// orchestrator-merged PRs deploy and the fleet binary silently lags main.
//
// It reuses the same debounced trigger as maybeSelfDeployAfterMerge, gated on a
// real drift signal rather than a specific merge:
//   - storm guard (#751 AC3): deploy ONLY when origin/main's head SHA differs
//     from the SHA the running binary was built from, so a reconcile that sees
//     many already-merged historical PRs does not re-fire — once a deploy lands,
//     the binary matches main and the drift clears.
//   - debounce (#722 / #751 AC2): honor the existing RecordTrigger window so a
//     deploy the orchestrator just launched for its own merge (which advanced
//     main too) does not double-trigger here.
func (o *Orchestrator) maybeSelfDeployOnMainAdvance(s *state.State) {
	if o == nil || o.cfg == nil || !o.cfg.SelfDeploy.Enabled {
		return
	}
	// Without a resolvable build SHA (e.g. a bare "dev" binary) drift cannot be
	// told apart from a fresh build, so do nothing rather than redeploy every
	// cycle.
	if selfdeploy.BuildSHA(o.binaryVersion) == "" {
		return
	}
	now := time.Now().UTC()
	// Cheap debounce check first: if a deploy already fired within the window
	// (e.g. the orchestrator merged a PR this cycle and recorded it), skip
	// before spending a GitHub round-trip on the head-SHA lookup.
	if last, _, ok := selfdeploy.LastTrigger(o.cfg.StateDir); ok {
		window := time.Duration(o.cfg.SelfDeploy.EffectiveMinIntervalMinutes()) * time.Minute
		if since := now.Sub(last); since >= 0 && since < window {
			return
		}
	}
	head, err := o.mainHeadSHA()
	if err != nil {
		log.Printf("[orch] self-deploy main-advance check: could not resolve origin/main head: %v", err)
		return
	}
	if !selfdeploy.MainAdvanced(o.binaryVersion, head) {
		return // running binary already matches main — no externally-merged drift
	}
	log.Printf("[orch] self-deploy: origin/main (%s) advanced past running binary (%s) — deploying", shortHeadSHA(head), o.binaryVersion)
	// PR 0: this deploy was triggered by observing main advance, not by a merge
	// the orchestrator performed itself.
	if err := o.triggerSelfDeploy(0); err != nil {
		// #758: debounced by the central (daemon) launcher — another flow already
		// fired a deploy for the same main-advance wave. Benign skip.
		if errors.Is(err, selfdeploy.ErrDebounced) {
			log.Printf("[orch] self-deploy (main-advance) debounced centrally — another flow's deploy covers this wave")
			return
		}
		log.Printf("[orch] self-deploy (main-advance) trigger failed: %v", err)
		o.notifier.Sendf("⚠️ maestro: self-deploy trigger failed after observing origin/main advance: %v — fleet still on the previous binary", err)
		// Surface as a supervisor finding so a silently-undeployed external merge
		// shows as fleet attention; no marker is recorded, so the next cycle
		// retries (mirrors maybeSelfDeployAfterMerge).
		if s != nil {
			s.RecordSupervisorDecision(selfdeploy.TriggerFailedFinding(0, err, o.cfg.Repo, now), state.DefaultSupervisorDecisionLimit)
		}
		return
	}
	// Record the trigger so a run-loop restarted by this deploy — and the next
	// few cycles within the window — do not re-fire for the same wave (#722).
	if err := selfdeploy.RecordTrigger(o.cfg.StateDir, 0, now); err != nil {
		log.Printf("[orch] self-deploy (main-advance) trigger marker write failed: %v", err)
	}
	log.Printf("[orch] self-deploy started after observing origin/main advance (detached transient unit)")
	o.notifier.Sendf("🚀 maestro: self-deploy started — origin/main advanced past the running binary (externally-merged PR); units will restart after drain")
}

// triggerSelfDeploy starts the opt-in self-deploy (#698) for a merged PR.
func (o *Orchestrator) triggerSelfDeploy(prNumber int) error {
	if o.selfDeployStartFn != nil {
		return o.selfDeployStartFn(prNumber)
	}
	return selfdeploy.Trigger(o.cfg, prNumber)
}

// consumeSelfDeployResult surfaces a finished self-deploy (#698) as a
// supervisor finding. The deploy script runs detached from this process and
// drops a JSON result file in the state dir; the first cycle that sees it —
// typically the first cycle of the freshly deployed binary — records the
// deployed version (or rollback reason) as a supervisor decision and
// notifies the operator. Returns true when state changed.
func (o *Orchestrator) consumeSelfDeployResult(s *state.State) bool {
	res, err := selfdeploy.ReadResult(o.cfg.StateDir)
	if err != nil {
		log.Printf("[orch] self-deploy result unreadable: %v", err)
		// Clear the malformed file so the error is not re-logged forever.
		if rmErr := selfdeploy.ClearResult(o.cfg.StateDir); rmErr != nil {
			log.Printf("[orch] self-deploy result cleanup: %v", rmErr)
		}
		return false
	}
	if res == nil {
		return false
	}
	// Clear before recording so a save failure cannot double-record on the
	// next cycle; losing the finding is the lesser failure mode.
	if err := selfdeploy.ClearResult(o.cfg.StateDir); err != nil {
		log.Printf("[orch] self-deploy result cleanup: %v", err)
		return false
	}

	finding := selfdeploy.Finding(res, o.cfg.Repo, time.Now().UTC())
	s.RecordSupervisorDecision(finding, state.DefaultSupervisorDecisionLimit)
	log.Printf("[orch] %s", finding.Summary)

	switch res.Status {
	case selfdeploy.StatusDeployed:
		o.notifier.Sendf("✅ maestro: self-deploy finished — running v%s (PR #%d)", res.Version, res.PR)
	case selfdeploy.StatusRolledBack:
		o.notifier.Sendf("⏪ maestro: self-deploy ROLLED BACK after PR #%d: %s", res.PR, res.Reason)
	default:
		o.notifier.Sendf("❌ maestro: self-deploy FAILED after PR #%d: %s — manual intervention may be needed", res.PR, res.Reason)
	}
	return true
}

// routeConflictingMergeFailure reconciles a merge failure that GitHub reports
// as CONFLICTING — the real-merge-conflict sibling of the behind-base path
// (#602). Returns true when the failure was handled (session advanced to a
// terminal/self-healing state) and the caller should stop further fallthrough.
//
// For StatusPROpen sessions this mirrors the existing CONFLICTING loop in
// rebaseConflicts (rebase worktree → handleRebaseConflictRetry on failure /
// markUnresolvableConflict when rebase is impossible). For sessions that are
// already retry_exhausted (convergence-merge candidates after #565/#574) we
// must NOT respawn another worker via handleRebaseConflictRetry — the worker
// has already exhausted its budget — so on rebase failure we go straight to
// markUnresolvableConflict, which labels the issue blocked and frees the slot.
func (o *Orchestrator) routeConflictingMergeFailure(s *state.State, slotName string, sess *state.Session, pr github.PR, mergeErr error) bool {
	mergeable, _, mErr := o.prMergeStatus(pr.Number)
	if mErr != nil {
		log.Printf("[orch] PR #%d merge-status lookup after failed merge: %v", pr.Number, mErr)
		return false
	}
	if mergeable != "CONFLICTING" {
		return false
	}

	log.Printf("[orch] PR #%d merge failed and GitHub reports CONFLICTING — routing to conflict resolution (#602): %v", pr.Number, mergeErr)

	retryExhausted := sess.Status == state.StatusRetryExhausted

	if !o.cfg.AutoRebase {
		o.markUnresolvableConflict(slotName, sess, pr.Number, fmt.Errorf("auto_rebase disabled"))
		return true
	}

	if strings.TrimSpace(sess.Worktree) == "" {
		// No worktree to rebase. For an in-flight (pr_open) session let the
		// retry path handle worker respawn; for a settled retry_exhausted
		// session the worker is already gone — mark unresolvable so the slot
		// advances instead of looping on the same merge failure forever.
		if retryExhausted {
			o.markUnresolvableConflict(slotName, sess, pr.Number, mergeErr)
			return true
		}
		o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, mergeErr)
		return true
	}

	if rerr := o.rebaseWorktree(sess.Worktree, sess.Branch); rerr != nil {
		log.Printf("[orch] auto-rebase failed for %s: %v", slotName, rerr)
		if retryExhausted {
			o.markUnresolvableConflict(slotName, sess, pr.Number, rerr)
			return true
		}
		o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, rerr)
		return true
	}

	o.markRebaseQueued(slotName, sess, pr.Number)
	return true
}

// rebaseConflicts handles branch conflicts in two phases:
//  1. Auto-rebase (if enabled)
//  2. Label issue as blocked + keep session in conflict_failed permanently
func (o *Orchestrator) rebaseConflicts(s *state.State) {
	prs, err := o.listOpenPRs()
	if err != nil {
		log.Printf("[orch] list PRs (rebase): %v", err)
		return
	}

	branchToPR := make(map[string]github.PR)
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
	}

	for slotName, sess := range s.Sessions {
		pr, hasPR := branchToPR[sess.Branch]

		switch sess.Status {
		case state.StatusPROpen:
			if !hasPR {
				continue
			}
			mergeable, mergeState, err := o.prMergeStatus(pr.Number)
			if err != nil {
				log.Printf("[orch] mergeable PR #%d: %v", pr.Number, err)
				continue
			}
			if mergeState == "behind" && mergeable != "CONFLICTING" {
				if !o.cfg.AutoRebase {
					log.Printf("[orch] PR #%d is behind main for %s, auto_rebase disabled", pr.Number, slotName)
					continue
				}
				if sess.RebaseAttempted {
					continue
				}
				log.Printf("[orch] PR #%d is behind main, updating %s", pr.Number, slotName)
				if err := o.updateBehindPRBranch(slotName, sess, pr.Number); err != nil {
					log.Printf("[orch] update behind PR #%d for %s failed: %v", pr.Number, slotName, err)
					o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, err)
					continue
				}
				o.markRebaseQueued(slotName, sess, pr.Number)
				continue
			}
			if mergeable != "CONFLICTING" {
				continue
			}

			if !o.cfg.AutoRebase {
				log.Printf("[orch] PR #%d has conflicts for %s, auto_rebase disabled", pr.Number, slotName)
				o.markUnresolvableConflict(slotName, sess, pr.Number, fmt.Errorf("auto_rebase disabled"))
				continue
			}

			log.Printf("[orch] PR #%d has conflicts, auto-rebasing %s", pr.Number, slotName)
			if err := o.rebaseWorktree(sess.Worktree, sess.Branch); err != nil {
				log.Printf("[orch] rebase failed for %s: %v", slotName, err)
				o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, err)
				continue
			}
			o.markRebaseQueued(slotName, sess, pr.Number)

		case state.StatusConflictFailed:
			if !o.cfg.AutoRebase || sess.RebaseAttempted {
				continue
			}
			if !hasPR {
				log.Printf("[orch] conflict_failed session %s has no open PR, skipping auto-rebase", slotName)
				continue
			}

			log.Printf("[orch] retrying auto-rebase for conflict_failed session %s (PR #%d)", slotName, pr.Number)
			if err := o.rebaseWorktree(sess.Worktree, sess.Branch); err != nil {
				log.Printf("[orch] rebase retry failed for %s: %v", slotName, err)
				o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, err)
				continue
			}
			o.markRebaseQueued(slotName, sess, pr.Number)

		case state.StatusRetryExhausted:
			// #602: a retry_exhausted convergence candidate whose PR is
			// CONFLICTING must reach a terminal advanced state — the worker
			// has already exhausted retries, so we never respawn from this
			// branch. Acts as a safety net for mergeReadyPR's primary route.
			if !hasPR || sess.RebaseAttempted {
				continue
			}
			mergeable, mergeState, mErr := o.prMergeStatus(pr.Number)
			if mErr != nil {
				log.Printf("[orch] mergeable PR #%d: %v", pr.Number, mErr)
				continue
			}
			if mergeable != "CONFLICTING" {
				if mergeState == "behind" && o.cfg.AutoRebase && !sess.RebaseAttempted {
					log.Printf("[orch] retry_exhausted PR #%d is behind main, attempting maintenance update for %s", pr.Number, slotName)
					if err := o.updateBehindPRBranch(slotName, sess, pr.Number); err != nil {
						log.Printf("[orch] update behind retry_exhausted PR #%d for %s failed: %v", pr.Number, slotName, err)
						o.handleRebaseConflictRetry(s, slotName, sess, pr.Number, err)
						continue
					}
					o.markRebaseQueued(slotName, sess, pr.Number)
				}
				continue
			}
			if o.cfg.AutoRebase && strings.TrimSpace(sess.Worktree) != "" {
				log.Printf("[orch] retry_exhausted PR #%d is CONFLICTING, attempting one-shot rebase for %s (#602)", pr.Number, slotName)
				if err := o.rebaseWorktree(sess.Worktree, sess.Branch); err != nil {
					log.Printf("[orch] rebase for retry_exhausted %s failed: %v — marking unresolvable", slotName, err)
					o.markUnresolvableConflict(slotName, sess, pr.Number, err)
					continue
				}
				o.markRebaseQueued(slotName, sess, pr.Number)
				continue
			}
			log.Printf("[orch] retry_exhausted PR #%d is CONFLICTING and cannot self-heal (auto_rebase=%v, worktree=%q) — marking unresolvable for %s (#602)", pr.Number, o.cfg.AutoRebase, sess.Worktree, slotName)
			o.markUnresolvableConflict(slotName, sess, pr.Number, fmt.Errorf("retry_exhausted PR is CONFLICTING"))
		}
	}
}

func (o *Orchestrator) updateBehindPRBranch(slotName string, sess *state.Session, prNumber int) error {
	rebased := false
	if strings.TrimSpace(sess.Worktree) != "" {
		if err := o.rebaseWorktree(sess.Worktree, sess.Branch); err != nil {
			log.Printf("[orch] auto-rebase failed for %s: %v — falling back to server-side update-branch", slotName, err)
		} else {
			rebased = true
		}
	}
	if rebased {
		return nil
	}
	return o.updateBranch(prNumber)
}

func (o *Orchestrator) markRebaseQueued(slotName string, sess *state.Session, prNumber int) {
	log.Printf("[orch] rebase succeeded for %s", slotName)
	sess.Status = state.StatusQueued
	sess.RebaseAttempted = true
	sess.FinishedAt = nil
	sess.PRNumber = prNumber
	sess.NotifiedCIFail = false
	sess.LastNotifiedStatus = ""
	o.notifier.Sendf("🔄 maestro: rebased %s (PR #%d) successfully; session moved to queued", slotName, prNumber)
}

func (o *Orchestrator) handleRebaseConflictRetry(s *state.State, slotName string, sess *state.Session, prNumber int, cause error) {
	if !o.cfg.AutoRetryRebaseConflicts {
		o.markUnresolvableConflict(slotName, sess, prNumber, cause)
		return
	}

	maxRetries := o.maintenanceRetryBudget()
	if !o.canRetryPRMaintenance(sess) {
		log.Printf("[orch] rebase conflict on PR #%d — maintenance retry limit reached (%d/%d) for issue #%d",
			prNumber, sess.MaintenanceRetryCount, maxRetries, sess.IssueNumber)
		s.MarkIssueRetryExhausted(sess.IssueNumber)
		o.syncProject(sess.IssueNumber, github.ProjectStatusBlocked)
		sess.Status = state.StatusRetryExhausted
		sess.NextRetryAt = nil
		sess.LastNotifiedStatus = "rebase_conflict_retry_exhausted"
		now := time.Now().UTC()
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		o.notifier.Sendf("💀 maestro: rebase conflict on PR #%d (issue #%d: %s) — maintenance retry limit exhausted (%d attempts)",
			prNumber, sess.IssueNumber, sess.IssueTitle, sess.MaintenanceRetryCount)
		return
	}

	if sess.Worktree == "" {
		closeComment := fmt.Sprintf("Auto-rebase hit conflicts, but the PR worktree is unavailable — maestro is closing this PR and respawning a worker to resolve the conflict from a fresh branch (attempt %d).\n\nRebase failure:\n\n```\n%s\n```",
			sess.RetryCount+1, rebaseConflictFeedback(prNumber, cause))
		if err := o.closePR(prNumber, closeComment); err != nil {
			log.Printf("[orch] warn: could not close PR #%d after rebase conflict: %v — marking conflict_failed", prNumber, err)
			o.markUnresolvableConflict(slotName, sess, prNumber, cause)
			return
		}
		log.Printf("[orch] closed PR #%d due to rebase conflict (worktree unavailable)", prNumber)
		sess.PRNumber = 0
	} else {
		log.Printf("[orch] keeping PR #%d open and respawning %s in place to resolve rebase conflicts", prNumber, slotName)
		sess.PRNumber = prNumber
	}

	sess.CIFailureOutput = ""
	sess.PreviousAttemptFeedback = rebaseConflictFeedback(prNumber, cause)
	sess.PreviousAttemptFeedbackKind = "rebase_conflict"

	sess.MaintenanceRetryCount++
	backoffMs := retryBackoffMs(sess.MaintenanceRetryCount, o.cfg.MaxRetryBackoffMs)
	retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
	sess.NextRetryAt = &retryAt
	sess.Status = state.StatusDead
	sess.RebaseAttempted = true
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)

	log.Printf("[orch] rebase conflict on PR #%d — scheduling maintenance retry %d/%d in %dms for issue #%d",
		prNumber, sess.MaintenanceRetryCount, maxRetries, backoffMs, sess.IssueNumber)
	o.notifier.Sendf("🔄 maestro: rebase conflict on PR #%d (issue #%d: %s), in-place maintenance retry %d/%d scheduled in %ds",
		prNumber, sess.IssueNumber, sess.IssueTitle, sess.MaintenanceRetryCount, maxRetries, backoffMs/1000)
}

func rebaseConflictFeedback(prNumber int, cause error) string {
	msg := "(rebase failure unavailable)"
	if cause != nil {
		msg = strings.TrimSpace(cause.Error())
	}
	if len(msg) > 8000 {
		msg = msg[:8000] + "\n... (truncated)"
	}
	return fmt.Sprintf("PR #%d failed to rebase onto origin/main.\n\n%s", prNumber, msg)
}

func (o *Orchestrator) markUnresolvableConflict(slotName string, sess *state.Session, prNumber int, cause error) {
	if err := o.addIssueLabel(sess.IssueNumber, "blocked"); err != nil {
		log.Printf("[orch] warn: could not label issue #%d as blocked: %v", sess.IssueNumber, err)
	}
	if cause != nil {
		log.Printf("[orch] conflict for %s is unresolvable: %v", slotName, cause)
	}

	sess.Status = state.StatusConflictFailed
	sess.RebaseAttempted = true
	sess.PRNumber = prNumber
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
	o.notifier.Sendf("⚠️ Worker %s (issue #%d) has unresolvable conflicts — manual intervention needed", slotName, sess.IssueNumber)
}

// findOpenBlockers returns the subset of blocker issue numbers that are still open.
func (o *Orchestrator) findOpenBlockers(blockers []int) []int {
	return o.findOpenBlockersExceptEpics(blockers, nil)
}

func (o *Orchestrator) findOpenBlockersExceptEpics(blockers []int, issues []github.Issue) []int {
	epics := epicIssueNumbers(issues)
	var open []int
	for _, num := range blockers {
		if _, ok := epics[num]; ok {
			continue
		}
		closed, err := o.isIssueClosed(num)
		if err != nil {
			// If we can't determine the state, assume it's open (safe default)
			log.Printf("[orch] warn: could not check blocker #%d: %v (assuming open)", num, err)
			open = append(open, num)
			continue
		}
		if !closed {
			open = append(open, num)
		}
	}
	return open
}

func epicIssueNumbers(issues []github.Issue) map[int]struct{} {
	epics := make(map[int]struct{})
	for _, issue := range issues {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(issue.Title)), "epic:") || github.HasLabel(issue, []string{"epic"}) {
			epics[issue.Number] = struct{}{}
		}
	}
	return epics
}

// resolveBackend determines which backend to use for the given issue.
// Delegates to router.ResolveBackend which applies 3-tier priority:
//  1. model:<backend> label on the issue (highest priority)
//  2. Auto-routing via LLM (if routing.mode == "auto")
//  3. Default backend from config
func (o *Orchestrator) resolveBackend(issue github.Issue) string {
	decision := o.router.ResolveBackendDecision(issue)
	return decision.Backend
}

// resolveBackendWithReason is like resolveBackend but also returns the
// selection reason (one of router.Reason*) so the orchestrator can stamp it
// on the session for the dashboard. Introduced for #427 to make it visible
// whether a backend came from a model: label, auto-routing, or default.
func (o *Orchestrator) resolveBackendWithReason(issue github.Issue) (string, string) {
	decision := o.router.ResolveBackendDecision(issue)
	return decision.Backend, decision.Reason
}

func (o *Orchestrator) resolveBackendDecision(issue github.Issue) router.BackendDecision {
	return o.router.ResolveBackendDecision(issue)
}

// availableSlots calculates how many new workers can be started, considering
// both the global max_parallel limit and per-state limits from max_concurrent_by_state.
// New workers enter the "running" state, so the "running" per-state limit is applied.
func availableSlots(cfg *config.Config, s *state.State, active int) int {
	slots := cfg.MaxParallel - active
	if slots <= 0 {
		return 0
	}

	// Apply per-state limit for "running" — new workers enter running state
	if limit, ok := cfg.MaxConcurrentByState["running"]; ok && limit > 0 {
		statusCounts := s.CountByStatus()
		runningCount := statusCounts[state.StatusRunning]
		runningSlots := limit - runningCount
		if runningSlots < slots {
			log.Printf("[orch] per-state limit: running=%d max_running=%d (capped from %d to %d slots)",
				runningCount, limit, slots, runningSlots)
			slots = runningSlots
		}
	}

	if slots < 0 {
		return 0
	}
	return slots
}

// startNewWorkers picks eligible issues and starts workers for them
func (o *Orchestrator) listOpenIssues(labels []string) ([]github.Issue, error) {
	if o.listOpenIssuesFn != nil {
		return o.listOpenIssuesFn(labels)
	}
	return o.gh.ListOpenIssues(labels)
}

func (o *Orchestrator) startWorker(cfg *config.Config, s *state.State, issue github.Issue, promptBase, backend string) (string, error) {
	if cfg == nil {
		cfg = o.cfg
	}
	if o.workerStartFn != nil {
		return o.workerStartFn(cfg, s, o.repo, issue, promptBase, backend)
	}
	return worker.Start(cfg, s, o.repo, issue, promptBase, backend)
}

func issueHasLabel(issue github.Issue, label string) bool {
	for _, issueLabel := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(issueLabel.Name), label) {
			return true
		}
	}
	return false
}

func pipelineConfigForIssue(base *config.Config, issue github.Issue) (*config.Config, bool) {
	if base == nil || !issueHasLabel(issue, pipelineFullLabel) {
		return base, false
	}
	cfg := *base
	cfg.Pipeline.Enabled = true
	cfg.Pipeline.Planner.Enabled = true
	cfg.Pipeline.Validator.Enabled = true
	return &cfg, true
}

func (o *Orchestrator) orderedQueueIssueDone(s *state.State, issueNumber int) (bool, string, error) {
	queue := o.cfg.Supervisor.OrderedQueue
	if queue.IsDone(issueNumber) {
		return true, "policy done override", nil
	}

	closed, err := o.isIssueClosed(issueNumber)
	if err != nil {
		return false, "", fmt.Errorf("check issue closed: %w", err)
	}
	if closed {
		if !o.canTreatIssueDoneForQueue(s, issueNumber, "closed issue") {
			return false, "issue closed but outcome health is not verified", nil
		}
		return true, "issue closed", nil
	}

	merged, err := o.hasMergedPRForIssue(issueNumber)
	if err != nil {
		return false, "", fmt.Errorf("check merged PR for issue: %w", err)
	}
	if merged {
		if !o.canTreatIssueDoneForQueue(s, issueNumber, "linked PR merged") {
			return false, "linked PR merged but outcome health is not verified", nil
		}
		return true, "linked PR merged", nil
	}

	for _, slotName := range sortedStateSessionNames(s) {
		sess := s.Sessions[slotName]
		if sess == nil || sess.IssueNumber != issueNumber || (sess.Status != state.StatusDone && sess.Status != state.StatusCodeLanded) || sess.PRNumber <= 0 {
			continue
		}
		merged, err := o.isPRMerged(sess.PRNumber)
		if err != nil {
			return false, "", fmt.Errorf("check PR #%d merged: %w", sess.PRNumber, err)
		}
		if merged {
			reason := fmt.Sprintf("session %s is %s with merged PR #%d", slotName, sess.Status, sess.PRNumber)
			if !o.canTreatIssueDoneForQueue(s, issueNumber, reason) {
				return false, reason + " but outcome health is not verified", nil
			}
			return true, reason, nil
		}
	}

	return false, "", nil
}

func (o *Orchestrator) orderedQueueIssueNumberPauseReason(s *state.State, issueNumber int) string {
	if s.IssueInProgress(issueNumber) {
		return fmt.Sprintf("issue #%d already has an active session", issueNumber)
	}

	if hasOpenPR, err := o.hasOpenPRForIssue(issueNumber); err != nil {
		return fmt.Sprintf("could not check open PRs for issue #%d: %v", issueNumber, err)
	} else if hasOpenPR {
		return fmt.Sprintf("issue #%d already has an open PR", issueNumber)
	}

	if s.IssueRetryExhausted(issueNumber) {
		return fmt.Sprintf("issue #%d is retry-exhausted", issueNumber)
	}
	if o.cfg.MaxRetriesPerIssue > 0 {
		failed := s.FailedAttemptsForIssue(issueNumber)
		if failed >= o.cfg.MaxRetriesPerIssue {
			if !s.IssueRetryExhausted(issueNumber) {
				s.MarkIssueRetryExhausted(issueNumber)
				o.notifier.Sendf("⚠️ Issue #%d hit max retries (%d) — needs manual review",
					issueNumber, o.cfg.MaxRetriesPerIssue)
			}
			return fmt.Sprintf("issue #%d exhausted retries (%d/%d attempts)", issueNumber, failed, o.cfg.MaxRetriesPerIssue)
		}
	}

	return ""
}

func (o *Orchestrator) orderedQueueIssuePauseReason(s *state.State, issue github.Issue, issues []github.Issue) string {
	if s.IsMissionParent(issue.Number) {
		return fmt.Sprintf("issue #%d is a mission parent", issue.Number)
	}
	if o.cfg.Missions.Enabled && mission.IsMissionIssue(issue, o.cfg.Missions.Labels) && !s.IsMissionChild(issue.Number) {
		return fmt.Sprintf("issue #%d is a mission issue awaiting decomposition", issue.Number)
	}
	if github.HasLabel(issue, o.cfg.ExcludeLabels) {
		return fmt.Sprintf("issue #%d is excluded by configured label", issue.Number)
	}
	if len(o.cfg.BlockerPatterns) > 0 {
		blockers := github.FindBlockers(issue.Body, o.cfg.BlockerPatterns)
		if len(blockers) > 0 {
			openBlockers := o.findOpenBlockersExceptEpics(blockers, issues)
			if len(openBlockers) > 0 {
				return fmt.Sprintf("issue #%d is blocked by open issue(s) %v", issue.Number, openBlockers)
			}
		}
	}
	return ""
}

func (o *Orchestrator) applyOrderedQueueFilter(s *state.State, issues []github.Issue) ([]github.Issue, bool) {
	queue := o.cfg.Supervisor.OrderedQueue
	if !queue.Active() {
		return issues, false
	}

	openByNumber := make(map[int]github.Issue, len(issues))
	for _, issue := range issues {
		openByNumber[issue.Number] = issue
	}

	for _, issueNumber := range queue.Issues {
		done, reason, err := o.orderedQueueIssueDone(s, issueNumber)
		if err != nil {
			log.Printf("[orch] ordered queue paused at issue #%d: %v", issueNumber, err)
			return nil, true
		}
		if done {
			log.Printf("[orch] ordered queue skipping issue #%d: %s", issueNumber, reason)
			continue
		}

		if reason := o.orderedQueueIssueNumberPauseReason(s, issueNumber); reason != "" {
			log.Printf("[orch] ordered queue paused: %s", reason)
			return nil, true
		}

		issue, ok := openByNumber[issueNumber]
		if !ok {
			log.Printf("[orch] ordered queue paused at issue #%d: issue is not open or does not match issue_labels", issueNumber)
			return nil, true
		}

		if reason := o.orderedQueueIssuePauseReason(s, issue, issues); reason != "" {
			log.Printf("[orch] ordered queue paused: %s", reason)
			return nil, true
		}

		return []github.Issue{issue}, true
	}

	log.Printf("[orch] ordered queue complete: all configured issues are done")
	return nil, true
}

func (o *Orchestrator) supervisorOwnsDynamicReadyLabel() bool {
	return o.cfg != nil && o.cfg.Supervisor.DynamicWave.Active() && o.cfg.Supervisor.DynamicWave.OwnsReadyLabel
}

func (o *Orchestrator) supervisorSelectedRepairSpawn(s *state.State, issueNumber int) bool {
	if s == nil || issueNumber <= 0 {
		return false
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil {
		return false
	}
	switch decision.RecommendedAction {
	case supervisor.ActionSpawnRepairWorker, supervisor.ActionSpawnReviewRepair:
	default:
		return false
	}
	if decision.RequiresApproval || decision.Risk == supervisor.RiskApprovalGated {
		// #565: when cautious mode gates the spawn behind an approval, an
		// approved or awaiting_dispatch approval also unlocks dispatch.
		if !o.hasEffectiveApprovalForDecision(s, decision) {
			return false
		}
	}
	if decision.Target == nil || decision.Target.Issue != issueNumber {
		return false
	}
	return true
}

// tryClaimReviewRepairSlot bumps the (pr_number, head_sha) attempt counter
// and reports whether this dispatcher tick is allowed to spawn a worker.
// Returns false when the budget for the (pr,head_sha) pair has already
// been spent — the caller must NOT spawn, so the supervisor's next
// cycle observes the exhaust and emits the fall-through decision.
//
// This is the per-(pr,head_sha) idempotency guard called out in the
// #565 acceptance criteria: a new head pushed by the repair worker
// does not re-trigger on the already-handled SHA, and a settled repair
// does not loop.
func (o *Orchestrator) tryClaimReviewRepairSlot(s *state.State, target *state.SupervisorTarget, payload *state.SupervisorReviewRepairPayload) bool {
	if s == nil || target == nil || payload == nil {
		return false
	}
	now := time.Now().UTC()
	max := payload.MaxRetries
	if max <= 0 {
		max = o.cfg.Supervisor.ReviewRepair.EffectiveMaxRetries()
	}
	_, spawned := s.RecordReviewRepairAttempt(target.PR, payload.HeadSHA, target.Issue, "", max, now)
	if !spawned {
		log.Printf("[orch] auto review-repair: refusing duplicate spawn for PR #%d head %s — budget exhausted", target.PR, shortReviewRepairSHA(payload.HeadSHA))
		return false
	}
	return true
}

// shortReviewRepairSHA trims a SHA for log messages — keeps the path
// dependency-free from internal/supervisor's shortSHA helper.
func shortReviewRepairSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// supervisorSelectedReviewRepair returns the spawn_review_repair payload
// for the given issue when the supervisor has minted (and the cautious
// gate has cleared) such a decision. nil means "no review-repair spawn
// applies to this issue right now". The (pr_number, head_sha) idempotency
// check is the caller's responsibility — this only resolves the LATEST
// supervisor decision.
func (o *Orchestrator) supervisorSelectedReviewRepair(s *state.State, issueNumber int) (*state.SupervisorReviewRepairPayload, *state.SupervisorTarget) {
	if s == nil || issueNumber <= 0 {
		return nil, nil
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil || decision.RecommendedAction != supervisor.ActionSpawnReviewRepair {
		return nil, nil
	}
	if decision.Target == nil || decision.Target.Issue != issueNumber {
		return nil, nil
	}
	if decision.RequiresApproval || decision.Risk == supervisor.RiskApprovalGated {
		if !o.hasEffectiveApprovalForDecision(s, decision) {
			return nil, nil
		}
	}
	if decision.ReviewRepair == nil {
		return nil, decision.Target
	}
	return decision.ReviewRepair, decision.Target
}

// hasEffectiveApprovalForDecision reports whether the supervisor decision
// has a matching approval that is currently effective (approved or
// awaiting_dispatch). Used to gate cautious-mode dispatch on the
// operator's signoff. The match is by (Action, Target) — the same
// identity used by RecordPendingApprovalForDecision's at-mint dedup.
func (o *Orchestrator) hasEffectiveApprovalForDecision(s *state.State, decision *state.SupervisorDecision) bool {
	if s == nil || decision == nil {
		return false
	}
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action != decision.RecommendedAction {
			continue
		}
		if !approvalTargetSameAsDecision(a.Target, decision.Target) {
			continue
		}
		switch a.Status {
		case state.ApprovalStatusApproved, state.ApprovalStatusAwaitingDispatch, state.ApprovalStatusExecutionSkipped:
			return true
		}
	}
	return false
}

// approvalTargetSameAsDecision compares two SupervisorTargets by the
// identity fields the executor cares about (issue, pr, head_sha,
// session). Mirrors approvalTargetsEqual in the state package, kept
// here so the orchestrator does not need to reach into state internals.
func approvalTargetSameAsDecision(a, b *state.SupervisorTarget) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Issue != b.Issue || a.PR != b.PR {
		return false
	}
	if strings.TrimSpace(a.HeadSHA) != strings.TrimSpace(b.HeadSHA) {
		return false
	}
	if strings.TrimSpace(a.Session) != strings.TrimSpace(b.Session) {
		return false
	}
	return true
}

func (o *Orchestrator) issueHasLiveRunningSession(s *state.State, issueNumber int) bool {
	if s == nil || issueNumber <= 0 {
		return false
	}
	for _, sess := range s.Sessions {
		if sess == nil || sess.IssueNumber != issueNumber || sess.Status != state.StatusRunning {
			continue
		}
		if sess.PID <= 0 || o.pidAlive(sess.PID) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) supervisorOwnedReadySelectedIssue(s *state.State) (int, bool) {
	if !o.supervisorOwnsDynamicReadyLabel() || s == nil {
		return 0, false
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil || decision.PolicyRule != supervisor.PolicyRuleDynamicWave {
		return 0, false
	}
	if decision.QueueAnalysis != nil && decision.QueueAnalysis.SelectedCandidate != nil && decision.QueueAnalysis.SelectedCandidate.Number > 0 {
		return decision.QueueAnalysis.SelectedCandidate.Number, true
	}
	if decision.Target != nil && decision.Target.Issue > 0 {
		return decision.Target.Issue, true
	}
	return 0, false
}

func (o *Orchestrator) applySupervisorOwnedReadyFilter(s *state.State, issues []github.Issue) []github.Issue {
	if !o.supervisorOwnsDynamicReadyLabel() || len(issues) == 0 {
		return issues
	}

	selected, ok := o.supervisorOwnedReadySelectedIssue(s)
	if !ok {
		for _, issue := range issues {
			log.Printf("[orch] skipping issue #%d: supervisor-owned ready label has no selected dynamic-wave candidate yet", issue.Number)
		}
		return nil
	}

	filtered := make([]github.Issue, 0, 1)
	for _, issue := range issues {
		if issue.Number == selected {
			filtered = append(filtered, issue)
			continue
		}
		log.Printf("[orch] skipping issue #%d: not supervisor-selected candidate #%d for supervisor-owned ready label", issue.Number, selected)
	}
	if len(filtered) == 0 {
		log.Printf("[orch] supervisor-owned ready label selected issue #%d, but it is not currently returned by issue_labels", selected)
	}
	return filtered
}

func containsIssueNumber(issues []github.Issue, number int) bool {
	for _, issue := range issues {
		if issue.Number == number {
			return true
		}
	}
	return false
}

func (o *Orchestrator) augmentWithSupervisorSelectedIssue(s *state.State, issues []github.Issue) []github.Issue {
	if !o.supervisorOwnsDynamicReadyLabel() {
		return issues
	}

	selected, ok := o.supervisorOwnedReadySelectedIssue(s)
	if !ok || selected <= 0 || containsIssueNumber(issues, selected) {
		return issues
	}
	return o.appendFetchedSupervisorIssue(issues, selected)
}

func (o *Orchestrator) appendFetchedSupervisorIssue(issues []github.Issue, selected int) []github.Issue {
	if selected <= 0 || containsIssueNumber(issues, selected) {
		return issues
	}

	issue, err := o.getIssue(selected)
	if err != nil {
		log.Printf("[orch] supervisor-selected candidate #%d could not be fetched for immediate dispatch: %v", selected, err)
		return issues
	}

	log.Printf("[orch] fetched supervisor-selected candidate #%d directly for immediate dispatch", selected)
	return append(issues, issue)
}

func sortedStateSessionNames(s *state.State) []string {
	names := make([]string, 0, len(s.Sessions))
	for name := range s.Sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (o *Orchestrator) startNewWorkers(s *state.State, slots int) {
	// Graceful drain (#541): while a drain is requested, refuse to claim new
	// issues or spawn new workers. In-flight workers keep running; the
	// operator runs `maestro drain` before a restart so a `systemctl restart`
	// no longer kills mid-flight workers. The flag is cleared automatically on
	// the next orchestrator startup. Return before listing issues so drain is
	// a true "stop accepting new work" gate, not just a per-issue skip.
	if s.DrainActive() {
		log.Printf("[orch] drain active (since %s): not spawning new workers (running=%d)",
			s.SpawnDrainAt.Format(time.RFC3339), s.RunningSessionCount())
		return
	}
	// Operator pause (#683): while `maestro pause` is in effect, skip issue
	// selection entirely — no listing, no claiming, no spawns — while
	// in-flight workers keep running to completion. Unlike drain, the flag
	// is NOT cleared on startup; it persists until `maestro resume`, which
	// the next cycle picks up from disk without a unit restart.
	if s.PauseActive() {
		log.Printf("[orch] project paused (since %s): skipping issue selection — not spawning new workers (running=%d)",
			s.PausedAt.Format(time.RFC3339), s.RunningSessionCount())
		return
	}
	issues, err := o.listOpenIssues(o.cfg.IssueLabels)
	if err != nil {
		log.Printf("[orch] list issues: %v", err)
		return
	}
	if decision := s.LatestSupervisorDecision(); decision != nil && decision.RecommendedAction == supervisor.ActionSpawnRepairWorker && decision.Target != nil {
		if !decision.RequiresApproval && decision.Risk != supervisor.RiskApprovalGated {
			issues = o.appendFetchedSupervisorIssue(issues, decision.Target.Issue)
		}
	}
	issues = o.augmentWithSupervisorSelectedIssue(s, issues)
	if filtered, ordered := o.applyOrderedQueueFilter(s, issues); ordered {
		issues = filtered
	} else {
		issues = o.applySupervisorOwnedReadyFilter(s, issues)
	}

	started := 0
	// #695: when every backend is blocked or cooling down, fresh dispatch
	// pauses for the cycle. Log the reason once, not once per eligible issue.
	dispatchPauseLogged := false
	for _, issue := range issues {
		repairSpawn := o.supervisorSelectedRepairSpawn(s, issue.Number)
		if s.IssueInProgress(issue.Number) {
			if !repairSpawn {
				continue
			}
			if o.issueHasLiveRunningSession(s, issue.Number) {
				log.Printf("[orch] skipping issue #%d: supervisor selected repair, but a worker is already running for this issue", issue.Number)
				continue
			}
		}

		// Pre-spawn guard (issue #456): never start a new worker for an issue that is
		// already closed or whose linked PR already merged, even when a stale
		// maestro-ready label is still present and even for supervisor-selected repair
		// spawns. This is an independent safety check on top of the supervisor's ready-
		// label management: with the supervisor down, nothing strips the label, so the
		// run loop must refuse already-finished work on its own. Errors are logged and
		// treated as "not finished" so a transient GitHub failure cannot block dispatch.
		if closed, err := o.isIssueClosed(issue.Number); err != nil {
			log.Printf("[orch] warn: could not verify closed state for issue #%d before dispatch: %v", issue.Number, err)
		} else if closed {
			log.Printf("[orch] skipping issue #%d: already closed (stale ready label)", issue.Number)
			o.syncProject(issue.Number, github.ProjectStatusDone)
			continue
		}
		if merged, err := o.hasMergedPRForIssue(issue.Number); err != nil {
			log.Printf("[orch] warn: could not verify merged PR for issue #%d before dispatch: %v", issue.Number, err)
		} else if merged {
			log.Printf("[orch] skipping issue #%d: has merged PR (stale ready label)", issue.Number)
			o.syncProject(issue.Number, github.ProjectStatusDone)
			continue
		}

		// NOTE: the closed/merged checks above (issue #456 pre-spawn guard) already
		// cover the previous s.IssueDone() closed-issue case for every candidate, so a
		// separate done-session closed-check here would be a redundant GitHub call.

		// Skip mission parent issues — they are decomposed, not dispatched directly
		if s.IsMissionParent(issue.Number) {
			continue
		}

		// Skip issues that carry a mission/epic label (they should be decomposed first)
		if o.cfg.Missions.Enabled && mission.IsMissionIssue(issue, o.cfg.Missions.Labels) && !s.IsMissionChild(issue.Number) {
			continue
		}

		if github.HasLabel(issue, o.cfg.ExcludeLabels) {
			if repairSpawn {
				log.Printf("[orch] allowing repair worker for issue #%d despite excluded label because supervisor selected repair maintenance", issue.Number)
			} else {
				log.Printf("[orch] skipping issue #%d (excluded label)", issue.Number)
				continue
			}
		}

		// Check retry limit: skip issues that have exhausted their retry budget
		if o.cfg.MaxRetriesPerIssue > 0 {
			failed := s.FailedAttemptsForIssue(issue.Number)
			if failed >= o.cfg.MaxRetriesPerIssue {
				if repairSpawn {
					log.Printf("[orch] allowing repair worker for issue #%d despite retry limit because supervisor selected spawn_repair_worker", issue.Number)
				} else {
					if !s.IssueRetryExhausted(issue.Number) {
						// First time hitting the limit — mark, label, and notify
						s.MarkIssueRetryExhausted(issue.Number)
						// auto-label blocked disabled
						o.notifier.Sendf("⚠️ Issue #%d hit max retries (%d) — needs manual review",
							issue.Number, o.cfg.MaxRetriesPerIssue)
					}
					log.Printf("[orch] skipping issue #%d: retry limit exhausted (%d/%d attempts)",
						issue.Number, failed, o.cfg.MaxRetriesPerIssue)
					continue
				}
			}
		}

		// Check for open blockers: skip if any referenced blocking issues are still open
		if len(o.cfg.BlockerPatterns) > 0 {
			blockers := github.FindBlockers(issue.Body, o.cfg.BlockerPatterns)
			if len(blockers) > 0 {
				openBlockers := o.findOpenBlockersExceptEpics(blockers, issues)
				if len(openBlockers) > 0 {
					log.Printf("[orch] skipping issue #%d: blocked by open issues %v", issue.Number, openBlockers)
					continue
				}
			}
		}

		// No available slots — sync remaining eligible issues as backlog/todo.
		// This check is intentionally before hasOpenPRForIssue to avoid making
		// a GitHub API call per backlogged issue when all slots are full.
		//
		// Two independent caps (issue #457):
		//  1. `started >= slots` — never start more than the budget computed for this
		//     dispatch pass (after retry reservations were subtracted).
		//  2. `liveActive >= MaxParallel` — a hard backstop recomputed from live state
		//     each iteration so that retry respawns (which already turned dead sessions
		//     into running ones earlier in the cycle) plus fresh dispatch can never
		//     exceed max_parallel, even if the precomputed `slots` budget was stale.
		if started >= slots {
			o.syncProject(issue.Number, github.ProjectStatusTodo)
			continue
		}
		if liveActive := len(s.ActiveSessions()); o.cfg.MaxParallel > 0 && liveActive >= o.cfg.MaxParallel {
			log.Printf("[orch] dispatch cap: %d active >= max_parallel %d — queueing issue #%d", liveActive, o.cfg.MaxParallel, issue.Number)
			o.syncProject(issue.Number, github.ProjectStatusTodo)
			continue
		}

		// Safety net: check GitHub directly for any open PR referencing this issue.
		// This guards against the race where reconcileRunningSessions marked a session
		// dead before checkSessions could detect its PR and transition it to pr_open.
		if hasOpenPR, err := o.hasOpenPRForIssue(issue.Number); err != nil {
			log.Printf("[orch] warn: could not check open PRs for issue #%d: %v", issue.Number, err)
		} else if hasOpenPR {
			if !repairSpawn {
				log.Printf("[orch] skipping issue #%d: open PR already exists", issue.Number)
				continue
			}
			log.Printf("[orch] allowing repair worker for issue #%d despite open PR because supervisor selected spawn_repair_worker", issue.Number)
		}

		// Determine initial phase and backend. A pipeline:full issue label
		// enables the phase pipeline only for this worker's copied config.
		workerCfg, pipelineFull := pipelineConfigForIssue(o.cfg, issue)
		initialPhase := pipeline.InitialPhase(workerCfg)
		var backendName string
		var promptBase string
		// backendReason tracks why this backend was chosen so the dashboard /
		// session record can show provenance: label, role, auto, default,
		// router_error, phase, review_repair. (#427)
		var backendReason string
		var taskType string

		// #565: when the supervisor selected spawn_review_repair for this
		// issue, override backend + prompt with the strong backend and
		// the scoped Greptile-finding prompt. Skip the pipeline preamble
		// — the review-repair worker is a focused, single-phase fixer
		// (not a planner/implementer/validator pass).
		if reviewRepair, repairTarget := o.supervisorSelectedReviewRepair(s, issue.Number); reviewRepair != nil && repairTarget != nil {
			if !o.tryClaimReviewRepairSlot(s, repairTarget, reviewRepair) {
				continue
			}
			backendName = reviewRepair.Backend
			if backendName == "" {
				backendName = o.cfg.Supervisor.ReviewRepair.EffectiveBackend()
			}
			promptBase = supervisor.FormatReviewRepairPromptFromPayload(issue.Number, repairTarget.PR, reviewRepair)
			initialPhase = state.PhaseNone
			backendReason = "review_repair"
			log.Printf("[orch] starting auto review-repair worker for issue #%d (PR #%d, head %s, backend=%s, %d findings)",
				issue.Number, repairTarget.PR, shortReviewRepairSHA(reviewRepair.HeadSHA), backendName, len(reviewRepair.Findings))
		} else if initialPhase != state.PhaseNone && initialPhase != state.PhaseImplement {
			// Pipeline mode with planner — use planner backend and raw template
			// (worker.Start → assemblePrompt will substitute {{WORKTREE}} etc.)
			backendName = pipeline.BackendForPhase(workerCfg, initialPhase)
			promptBase = pipeline.PromptTemplateForPhase(workerCfg, initialPhase)
			backendReason = "phase"
		} else {
			// Normal mode or pipeline starting at implement — use standard
			// resolution, gated on BackendHealth so a fresh dispatch never
			// lands on a backend that is disabled or cooling down after an
			// auth failure / provider limit (#695).
			backendDecision, dispatchable, retryAt := o.resolveDispatchBackend(s, issue, time.Now().UTC())
			if !dispatchable {
				if !dispatchPauseLogged {
					expiry := "no cooldown expiry recorded"
					if retryAt != nil {
						expiry = "earliest cooldown expires " + retryAt.UTC().Format(time.RFC3339)
					}
					log.Printf("[orch] dispatch paused: all backends blocked or cooling down (%s) — not spawning fresh workers this cycle", expiry)
					dispatchPauseLogged = true
				}
				continue
			}
			backendName = backendDecision.Backend
			backendReason = backendDecision.Reason
			taskType = backendDecision.TaskType
			promptBase = o.selectPrompt(issue)
			if initialPhase == state.PhaseImplement {
				// Pipeline mode but no planner — add pipeline preamble
				preamble := pipeline.ImplementerPreamble(&state.Session{})
				promptBase = preamble + "\n" + promptBase
			}
		}

		// #427: surface router_error as a visible warning so operators don't
		// have to grep the daemon log to discover that auto-routing fell
		// through to default silently.
		if backendReason == router.ReasonRouterError {
			o.notifier.Sendf("⚠️ maestro: auto-routing failed for issue #%d — falling back to default backend %s. Check router model configuration.",
				issue.Number, backendName)
		}

		// Detect long-running label
		longRunning := false
		for _, label := range issue.Labels {
			if strings.EqualFold(label.Name, "long-running") {
				longRunning = true
				break
			}
		}

		log.Printf("[orch] starting worker for issue #%d: %s (backend=%s, reason=%s, phase=%s, long_running=%v)", issue.Number, issue.Title, backendName, backendReason, initialPhase, longRunning)
		slotName, err := o.startWorker(workerCfg, s, issue, promptBase, backendName)
		if err != nil {
			log.Printf("[orch] start worker for issue #%d: %v", issue.Number, err)
			o.notifier.Sendf("❌ maestro: failed to start worker for issue #%d (%s): %v",
				issue.Number, issue.Title, err)
			continue
		}

		if longRunning {
			s.Sessions[slotName].LongRunning = true
		}
		if initialPhase != state.PhaseNone {
			s.Sessions[slotName].Phase = initialPhase
		}
		if pipelineFull {
			s.Sessions[slotName].PipelineFull = true
		}
		// #427: stamp the backend selection reason on the session so the
		// dashboard / fleet API can show why this backend was chosen
		// (label / role / auto / default / router_error / phase / review_repair).
		if sess := s.Sessions[slotName]; sess != nil {
			sess.BackendSelection = &state.BackendSelection{
				SelectedBackend: backendName,
				SelectionReason: backendReason,
				TaskType:        taskType,
			}
			if taskType != "" && len(sess.Attribution) > 0 {
				sess.Attribution[len(sess.Attribution)-1].TaskType = taskType
			}
		}
		if o.syncProject(issue.Number, github.ProjectStatusInProgress) {
			s.MarkProjectStatusSynced(issue.Number, string(github.ProjectStatusInProgress), time.Now().UTC())
		}
		o.notifier.Sendf("🚀 maestro: started worker %s for issue #%d: %s", slotName, issue.Number, issue.Title)
		started++
	}

	if started == 0 {
		log.Printf("[orch] no new workers started (%d issues checked)", len(issues))
	}
}
