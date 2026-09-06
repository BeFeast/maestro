package orchestrator

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mergegate"
	"github.com/befeast/maestro/internal/mission"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/repopolicy"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/selfdeploy"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/versioning"
	"github.com/befeast/maestro/internal/worker"
)

const (
	minProjectGraphQLRemaining = 100
	projectBoardSweepInterval  = 30 * time.Minute
	projectBoardSweepRetry     = 10 * time.Minute
	// Leave one orchestrator tick of scheduling margin so a 60-second loop
	// observes the authoritative terminal state within the 10-minute SLA even
	// when the prior check landed immediately after a cycle boundary.
	terminalReconcileInterval = 9 * time.Minute
	// Give the normal merge-to-close flow a short window to settle before a
	// merged session whose issue is still open releases its terminal claim.
	terminalReconcileGrace = 15 * time.Minute
	// A fresh dispatch lease spans the slow pre-worker setup window. If its
	// owner dies, the next cycle renews the same exact slot/worktree/branch;
	// it never allocates a duplicate identity merely to retry startup.
	freshDispatchLeaseDuration = 10 * time.Minute
	// A vanished worker gets one automatic recovery in its exact canonical
	// session. If that replacement also vanishes, continuing to respawn is a
	// zombie loop even when max_retries_per_issue is configured as unlimited.
	maxAutomaticUnexpectedExitRetries = 1
	pipelineFullLabel                 = "pipeline:full"
	pipelineAdvisedLabel              = "pipeline:advised"
)

type cycleBoolResult struct {
	value bool
	err   error
}

// Orchestrator coordinates all agent sessions
type Orchestrator struct {
	cfg      *config.Config
	notifier *notify.Notifier
	gh       *github.Client
	// readSource is the optional mirror-first read source (#826). When set
	// (SetReadSource), the high-volume read wrappers consult it instead of the
	// direct GitHub client — it serves fresh mirror rows locally and falls back
	// to the API on a miss/stale. Writes and un-mirrored reads always stay on
	// gh, so GitHub remains authoritative. nil = today's API-direct behavior.
	readSource githubReadSource
	// tokenBudgetMillNotified remembers the streak length already alerted per
	// issue so a held budget-wall issue alerts on escalation, not every cycle.
	tokenBudgetMillNotified map[int]int
	// missingReviewNotified remembers PRs already reported as merged past an
	// absent review gate, so the alert fires once per PR.
	missingReviewNotified map[int]bool
	// reviewProduceInFlight guards one llm-review producer run per PR in this
	// process (#1162 S5); cross-process dedup is the posted statuses.
	reviewProduceMu       sync.Mutex
	reviewProduceInFlight map[int]bool
	// reviewProduceFn is the production seam (tests). nil = produceReviewStreams.
	reviewProduceFn func(prNumber int, headSHA string, streams []string, rp config.ReviewProducerConfig, fc config.ForgeConfig)
	router                *router.Router
	repo                  string
	binaryVersion         string
	promptBase            string
	bugPromptBase         string
	enhancementPromptBase string
	pidAliveFn            func(pid int) bool
	tmuxSessionExistsFn   func(name string) bool
	tmuxPaneIdentityFn    func(session string) (int, string, error)
	listOpenPRsFn         func() ([]github.PR, error)
	listClosedPRsFn       func() ([]github.PR, error)
	remoteBranchExistsFn  func(branch string) (bool, error)
	createPRFn            func(title, body, base, head string) (int, error)
	hasOpenPRForIssueFn   func(issueNumber int) (bool, error)
	hasMergedPRForIssueFn func(issueNumber int) (bool, error)
	mergedPRForIssueFn    func(issueNumber int) (int, error)
	isPRMergedFn          func(prNumber int) (bool, error)
	mergedPRForBranchFn   func(branch string) (int, error)

	// Testing hooks for checkSessions
	captureTmuxFn             func(session string) (string, error)
	tmuxCaptureFn             func(session string) (string, error)
	isIssueClosedFn           func(issueNumber int) (bool, error)
	addIssueLabelFn           func(number int, label string) error
	isRateLimitedFn           func(logFile string) bool
	rateLimitResetFromLogFn   func(logFile string) *time.Time
	authFailureFromLogFn      func(logFile string) (bool, string)
	modelUnavailableFromLogFn func(logFile string) (bool, string)
	usageLimitFromLogFn       func(logFile string, extraPatterns []string) (bool, string)
	beforeWorktreeCleanupFn   func(slotName string)
	removeWorktreeFn          func(localPath, worktreePath string) error
	workerStopProcessFn       func(slotName string, sess *state.Session) error
	// workerRespawnFn / respawnWorkerFn: used by respawnWorker() for dead-worker fallback (tests set one or the other)
	workerRespawnFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error
	respawnWorkerFn func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error
	getIssueFn      func(number int) (github.Issue, error)
	// saveCheckpointFn / restoreWorktreeFn / respawnInPlaceFn: used for soft
	// token threshold checkpoint+respawn. restoreWorktreeFn lets tests exercise
	// the production validation path without starting a real model process.
	saveCheckpointFn  func(sess *state.Session) (string, error)
	restoreWorktreeFn func(localPath, worktreeBase, slotName, worktree, branch string) error
	respawnInPlaceFn  func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error

	// Testing hooks for pipeline phase transitions
	workerStartPhaseFn func(cfg *config.Config, sess *state.Session, slotName, prompt, backendName string) error

	// Testing hooks for startNewWorkers
	listOpenIssuesFn     func(labels []string) ([]github.Issue, error)
	workerStartFn        func(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase, backend string) (string, error)
	workerStartClaimedFn func(cfg *config.Config, s *state.State, repo string, issue github.Issue, promptBase, backend, slot string) (string, error)

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

	// approvalsBinding, when SetApprovalStore is called, lets the standing
	// repair-approval reconciler (#866) mirror a moot-approval stale transition
	// into the same unified SQLite approval store the fleet approve/reject
	// endpoint uses, so JSON state and SQLite never diverge under
	// --approvals-store sqlite. Zero value (ModeJSON) makes the mirror a no-op.
	approvalsBinding approvalstore.Binding

	// Config hot-reload channel (nil = disabled, safe in select)
	configReloadCh <-chan *config.Config

	// emergencyHaltFn reports whether the fleet-wide EMERGENCY STOP switch (#840)
	// currently closes the spawn gate. nil = no gate (today's behavior). The
	// daemon wires it to read the switch from the unified DB each cycle, so an
	// active stop halts new worker spawns within one poll interval without a
	// restart. In-flight workers are unaffected — this gate only refuses NEW work.
	emergencyHaltFn func() bool

	// fleetSpawnCeilingFn is the daemon-wide fast gate checked before issue
	// listing. fleetSpawnReserveFn is the atomic per-worker backstop: concurrent
	// project flows and one flow's batch dispatch must each reserve one shared
	// slot before a worker process starts.
	fleetSpawnCeilingFn func() bool
	fleetSpawnReserveFn func() (commit func(slot string), release func(), ok bool)

	// spawnResourceHoldFn is the host-resource precondition (#1128): it reports
	// whether the host is too short on tmpfs space to accept another worker, and
	// why. nil = no precondition. It is a throughput pause only — see
	// SetSpawnResourceHold.
	spawnResourceHoldFn func() (hold bool, reason string)

	// Restart-required signal. Some config fields (currently routing.*) cannot be
	// hot-applied because their runtime components are built once at startup. When
	// such a field changes during a config reload we do not apply it; instead we raise this
	// persistent flag so a long-running daemon surfaces "restart required" in
	// `maestro status` and the Fleet API instead of silently ignoring the change.
	restartRequired       bool
	restartRequiredReason string

	// Testing hooks for autoMergePRs / mergeReadyPR
	ghPRCIStatusFn               func(prNumber int) (string, error)
	ghPRCheckRollupFn            func(prNumber int) (github.PRCheckRollup, error)
	ghPRMergeStatusFn            func(prNumber int) (mergeable string, mergeStateStatus string, err error)
	ghPRGreptileApprovedFn       func(prNumber int) (approved bool, pending bool, err error)
	ghPRReviewGateVerdictFn      func(prNumber int, streams []string) (github.ReviewGateVerdict, error)
	ghPRHasCriticalReviewFn      func(prNumber int) (bool, error)
	ghPRUnresolvedThreadsFn      func(prNumber int) (string, []github.ReviewThread, error)
	ghUpdateBranchFn             func(prNumber int) error
	ghMarkPRReadyFn              func(prNumber int) error
	ghMergePRFn                  func(prNumber int) error
	ghMergePRAtHeadFn            func(prNumber int, expectedHeadSHA string) error
	ghClosePRFn                  func(prNumber int, comment string) error
	ghPRChecksOutputFn           func(prNumber int) (string, error)
	ghPRFailingChecksFn          func(prNumber int) ([]github.FailingCheck, error)
	ghCollectPRReviewFeedbackFn  func(prNumber int, streams []string) (string, error)
	ghCloseIssueFn               func(number int, comment string) error
	ghPRHeadSHAFn                func(prNumber int) (string, error)
	ghPRDetailsFn                func(prNumber int) (github.PR, error)
	ghPRMergeInfoFn              func(prNumber int) (github.PRMergeInfo, error)
	ghCommentPRFn                func(prNumber int, body string) error
	ghPRChangedFilesFn           func(prNumber int) ([]string, error)
	ghPRLabelsFn                 func(prNumber int) ([]string, error)
	ghPRVisualEvidenceAttachedFn func(prNumber int) (bool, error)
	runVisualCaptureFn           func(v config.VerifyVisualConfig, worktreePath string) ([]string, error)
	workerStopFn                 func(cfg *config.Config, slotName string, sess *state.Session) error
	selfDeployStartFn            func(prNumber int) error
	mainHeadSHAFn                func() (string, error)
	deliveryRevisionContainsFn   func(ancestor, descendant string) (bool, error)
	rebaseWorktreeFn             func(worktreePath, branch string, autoResolveFiles, autoRestoreFiles []string) error
	outcomeCheckFn               func(context.Context, outcome.Brief) outcome.HealthCheckResult
	syncProjectFn                func(issueNumber int, status github.ProjectStatus) bool
	listNonDoneProjectItemsFn    func(pf *github.ProjectField) ([]github.ProjectItem, error)
	rateLimitFn                  func() (github.RateLimitStatus, error)
	primaryRESTPausedFn          func() (bool, time.Time)

	// Per-cycle open-PR cache (#794). ListOpenPRs is the dominant forge REST
	// read (~78% of the core-bucket calls at the 2026-06-28 secondary
	// rate-limit incident) and was issued 4× per RunOnce —
	// reconcileRunningSessions, checkSessions, autoMergePRs and rebaseConflicts
	// each fetched the same open-PR list independently. While a cycle is active
	// the first fetch is memoized and shared across those steps so a RunOnce
	// issues exactly one ListOpenPRs read. Outside RunOnce the cache is inactive
	// and every call fetches fresh, preserving the semantics of callers (and
	// tests) that invoke those steps in isolation. RunOnce is serial per flow,
	// so no locking is required.
	cycleActive         bool
	cyclePRsValid       bool
	cyclePRs            []github.PR
	cyclePRsErr         error
	cycleClosedPRsValid bool
	cycleClosedPRs      []github.PR
	cycleClosedPRsErr   error
	cycleIssueClosed    map[int]cycleBoolResult
	cyclePRMerged       map[int]cycleBoolResult
}

// New creates a new Orchestrator
func New(cfg *config.Config) *Orchestrator {
	n := notify.NewWithToken(cfg.Telegram.Token(), cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
	n.WithNtfy(cfg.Notify.Ntfy.BaseURL, cfg.Notify.Ntfy.Topic, cfg.Notify.Ntfy.Token())
	if cfg.Telegram.DigestMode {
		n.SetDigestMode(true)
		log.Printf("[orch] digest mode enabled — notifications will be batched per cycle")
	}
	gh := github.New(cfg.Repo, cfg.Forge)
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

// githubReadSource is the mirror-first read surface the orchestrator's poll-loop
// wrappers consult when one is wired (#826). *mirrorstore.Source satisfies it.
// It is a strict subset of *github.Client, so the orchestrator falls straight
// back to gh for every read it does not list here and for all writes.
type githubReadSource interface {
	ListOpenIssues(labels []string) ([]github.Issue, error)
	ListOpenPRs() ([]github.PR, error)
	GetIssue(number int) (github.Issue, error)
	IsIssueClosed(number int) (bool, error)
	IsPRMerged(prNumber int) (bool, error)
}

// SetReadSource wires a mirror-first read source (#826). The daemon builds one
// per flow when github_mirror.source is "mirror-first"; left unset, the
// orchestrator reads GitHub directly exactly as before.
func (o *Orchestrator) SetReadSource(src githubReadSource) {
	o.readSource = src
}

// SetEmergencyHalt wires the fleet-wide EMERGENCY STOP spawn gate (#840). fn is
// consulted at the top of startNewWorkers; when it returns true the orchestrator
// claims no new issues and spawns no new workers. Passing nil clears the gate.
func (o *Orchestrator) SetEmergencyHalt(fn func() bool) {
	o.emergencyHaltFn = fn
}

// SetFleetSpawnCeiling wires the daemon-wide pre-list spawn ceiling. Passing
// nil disables the gate for legacy single-project callers and tests.
func (o *Orchestrator) SetFleetSpawnCeiling(fn func() bool) {
	o.fleetSpawnCeilingFn = fn
}

// SetSpawnResourceHold wires the host-resource spawn precondition (#1128). fn
// is consulted at the top of startNewWorkers; when it reports a hold the
// orchestrator lists nothing and spawns nothing for this cycle.
//
// It is deliberately a throughput pause and not a gate: no state is written, no
// approval is requested, no ActionNone decision is produced, and no issue's
// retry budget is touched. The next poll re-evaluates fn, so dispatch resumes
// on its own as soon as the host recovers. Passing nil clears the precondition.
func (o *Orchestrator) SetSpawnResourceHold(fn func() (bool, string)) {
	o.spawnResourceHoldFn = fn
}

// SetFleetSpawnReserve wires the daemon's atomic one-worker reservation. The
// returned commit callback receives the started slot; release is used whenever
// dispatch aborts before a worker starts.
func (o *Orchestrator) SetFleetSpawnReserve(fn func() (commit func(slot string), release func(), ok bool)) {
	o.fleetSpawnReserveFn = fn
}

type fleetSpawnPermit struct {
	commitFn  func(string)
	releaseFn func()
	done      bool
}

func (o *Orchestrator) reserveFleetSpawn() (*fleetSpawnPermit, bool) {
	if o.fleetSpawnReserveFn == nil {
		return &fleetSpawnPermit{}, true
	}
	commit, release, ok := o.fleetSpawnReserveFn()
	if !ok {
		return nil, false
	}
	return &fleetSpawnPermit{commitFn: commit, releaseFn: release}, true
}

func (p *fleetSpawnPermit) Commit(slot string) {
	if p == nil || p.done {
		return
	}
	p.done = true
	if p.commitFn != nil {
		p.commitFn(slot)
	}
}

func (p *fleetSpawnPermit) Release() {
	if p == nil || p.done {
		return
	}
	p.done = true
	if p.releaseFn != nil {
		p.releaseFn()
	}
}

// SetApprovalStore configures which approval store the orchestrator's standing
// repair-approval reconciler (#866) mirrors a moot-approval stale transition
// into. mode/dbPath come from the daemon's --approvals-store/--approvals-db
// flags (the same pair the fleet approve/reject endpoint uses); StateDir and
// Repo are taken from the orchestrator config at reconcile time. An unparseable
// mode falls back to json (mirror is a no-op) and is logged rather than fatal —
// the JSON reconciliation, which the fleet dashboard reads, still happens.
func (o *Orchestrator) SetApprovalStore(mode, dbPath string) {
	if o == nil {
		return
	}
	m, err := approvalstore.ParseMode(mode)
	if err != nil {
		log.Printf("[orch] invalid --approvals-store %q; repair-approval reconcile mirrors to JSON only: %v", mode, err)
		m = approvalstore.ModeJSON
	}
	o.approvalsBinding = approvalstore.Binding{Mode: m, DBPath: dbPath}
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
	return tmuxsession.HasSession(name)
}

// tmuxPaneIdentity reads the live pane pid and worktree of a tmux session,
// used to adopt an already-running worker whose recorded pid is stale (#877
// restart-resume). Both values are required: a matching tmux name alone may
// refer to a stale or unrelated pane after a failed resume.
func (o *Orchestrator) tmuxPaneIdentity(session string) (int, string, error) {
	if o.tmuxPaneIdentityFn != nil {
		return o.tmuxPaneIdentityFn(session)
	}
	return worker.TmuxPaneIdentity(session)
}

func (o *Orchestrator) listOpenPRs() ([]github.PR, error) {
	if o.listOpenPRsFn != nil {
		return o.listOpenPRsFn()
	}
	if o.readSource != nil {
		return o.readSource.ListOpenPRs()
	}
	return o.gh.ListOpenPRs()
}

// beginCycle resets the per-cycle GitHub-read cache and marks a RunOnce as
// active so the cycle's steps share one ListOpenPRs fetch (#794).
func (o *Orchestrator) beginCycle() {
	o.cycleActive = true
	o.cyclePRsValid = false
	o.cyclePRs = nil
	o.cyclePRsErr = nil
	o.cycleClosedPRsValid = false
	o.cycleClosedPRs = nil
	o.cycleClosedPRsErr = nil
	o.cycleIssueClosed = make(map[int]cycleBoolResult)
	o.cyclePRMerged = make(map[int]cycleBoolResult)
}

// endCycle clears the per-cycle cache and deactivates memoization so any
// out-of-cycle caller fetches fresh.
func (o *Orchestrator) endCycle() {
	o.cycleActive = false
	o.cyclePRsValid = false
	o.cyclePRs = nil
	o.cyclePRsErr = nil
	o.cycleClosedPRsValid = false
	o.cycleClosedPRs = nil
	o.cycleClosedPRsErr = nil
	o.cycleIssueClosed = nil
	o.cyclePRMerged = nil
}

// invalidateCyclePRs drops the memoized per-cycle open-PR snapshot so the next
// listOpenPRsForCycle re-fetches fresh. The dedup cache (#794) is taken once at
// the start of a cycle and shared across its four open-PR consumers
// (reconcile, check, auto-merge, rebase), but a step that mutates the open-PR
// set within the same RunOnce — reconcileRunningSessions auto-creating a PR,
// autoMergePRs merging or closing one — would otherwise leave a later step
// looking at the pre-mutation snapshot. The concrete failure this guards
// against (#794 review): reconcile auto-creates a PR via
// reconcilePushedBranch and flips the session to pr_open, but autoMergePRs
// reuses the cache populated before that PR existed, fails to find it, takes
// the "no open PR — assuming merged/closed" branch and marks the session done —
// orphaning a live PR in the same cycle. Invalidating after every open-PR-set
// mutation keeps the dedup win for the common, no-mutation cycle (still one
// fetch) while guaranteeing correctness when the set does change (one extra
// fetch). A no-op outside an active cycle.
func (o *Orchestrator) invalidateCyclePRs() {
	o.cyclePRsValid = false
	o.cyclePRs = nil
	o.cyclePRsErr = nil
	o.cycleClosedPRsValid = false
	o.cycleClosedPRs = nil
	o.cycleClosedPRsErr = nil
	o.cyclePRMerged = make(map[int]cycleBoolResult)
}

// listClosedPRsForCycle shares one closed-PR snapshot across every historical
// issue/branch lookup in a RunOnce. The old path launched a fresh gh process for
// every retry_exhausted session, which made the single daemon sustain load 6-10
// while doing no worker computation (#940).
func (o *Orchestrator) listClosedPRsForCycle() ([]github.PR, error) {
	fetch := func() ([]github.PR, error) {
		if o.listClosedPRsFn != nil {
			return o.listClosedPRsFn()
		}
		if o.gh == nil {
			return nil, fmt.Errorf("no github client configured for closed-pr list")
		}
		return o.gh.ListClosedPRs()
	}
	if !o.cycleActive {
		return fetch()
	}
	if o.cycleClosedPRsValid {
		return o.cycleClosedPRs, o.cycleClosedPRsErr
	}
	prs, err := fetch()
	o.cycleClosedPRs = prs
	o.cycleClosedPRsErr = err
	o.cycleClosedPRsValid = true
	return prs, err
}

// listOpenPRsForCycle returns the open-PR list, fetching it at most once per
// orchestrator RunOnce. The four cycle steps that need open PRs share a single
// fetch instead of issuing four identical core-bucket reads (#794). Outside an
// active cycle it delegates straight to listOpenPRs (no memoization), so a
// direct caller — or a test invoking a single step — sees the fresh,
// per-call behavior. A fetch error is cached too: under a real 403 the cycle
// makes one attempt, not four, which is the rate-limit relief this exists for.
func (o *Orchestrator) listOpenPRsForCycle() ([]github.PR, error) {
	if !o.cycleActive {
		return o.listOpenPRs()
	}
	if o.cyclePRsValid {
		return o.cyclePRs, o.cyclePRsErr
	}
	// #812: when the shared core-REST bucket is exhausted (a primary rate-limit
	// pause armed by the gh wrapper), skip the fleet's dominant open-PR read
	// until the reset instead of issuing a doomed call. Generalizes the GraphQL
	// budget guard (projectGraphQLBudgetAvailable) to the core REST bucket: the
	// cached error fans the skip out to the cycle's four open-PR consumers
	// (reconcile / check / auto-merge / rebase), which each degrade to a no-op
	// this cycle exactly as they already do for any open-PR read failure.
	if paused, resetAt := o.primaryRESTPaused(); paused {
		o.cyclePRs = nil
		o.cyclePRsErr = &github.PrimaryRateLimitError{Endpoint: fmt.Sprintf("repos/%s/pulls", o.repo), ResetAt: resetAt}
		o.cyclePRsValid = true
		log.Printf("[orch] core REST bucket exhausted (primary rate limit, #812); skipping open-PR poll until %s", resetAt.UTC().Format(time.RFC3339))
		return o.cyclePRs, o.cyclePRsErr
	}
	prs, err := o.listOpenPRs()
	o.cyclePRs = prs
	o.cyclePRsErr = err
	o.cyclePRsValid = true
	return prs, err
}

// primaryRESTPaused reports whether the shared core-REST bucket is paused on a
// primary rate-limit exhaustion, and until when (#812). The gh wrapper arms the
// gate; the orchestrator consults it to skip doomed REST polling. Injectable for
// tests.
func (o *Orchestrator) primaryRESTPaused() (bool, time.Time) {
	if o.primaryRESTPausedFn != nil {
		return o.primaryRESTPausedFn()
	}
	return github.PrimaryRateLimitPaused()
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
	if o.cfg != nil {
		prohibited, err := repopolicy.ProhibitsPublicAIAttribution(o.cfg.LocalPath)
		if err != nil {
			return 0, err
		}
		if prohibited && repopolicy.ContainsForbiddenPublicAttribution(title+"\n"+body) {
			return 0, fmt.Errorf("repository policy prohibits AI attribution in public PR text")
		}
	}
	var (
		prNumber int
		err      error
	)
	if o.createPRFn != nil {
		prNumber, err = o.createPRFn(title, body, base, head)
	} else {
		prNumber, err = o.gh.CreatePR(title, body, base, head)
	}
	// A new PR mutates the open-PR set; drop any per-cycle cache so a later
	// cycle step re-fetches and sees it instead of the pre-creation snapshot
	// (#794 review).
	if err == nil {
		o.invalidateCyclePRs()
	}
	return prNumber, err
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
	if o.gh == nil {
		return false, fmt.Errorf("no github client configured for merged-pr check")
	}
	if o.cycleActive {
		prNumber, err := o.mergedPRForIssue(issueNumber)
		return prNumber > 0, err
	}
	return o.gh.HasMergedPRForIssue(issueNumber)
}

func (o *Orchestrator) mergedPRForIssue(issueNumber int) (int, error) {
	if o.mergedPRForIssueFn != nil {
		return o.mergedPRForIssueFn(issueNumber)
	}
	if o.gh == nil {
		return 0, fmt.Errorf("no github client configured for merged-pr number check")
	}
	if o.cycleActive {
		prs, err := o.listClosedPRsForCycle()
		if err != nil {
			return 0, err
		}
		for _, pr := range prs {
			if pr.MergedAt != "" && github.PRClosesIssue(pr, issueNumber) {
				return pr.Number, nil
			}
		}
		return 0, nil
	}
	return o.gh.MergedPRNumberForIssue(issueNumber)
}

func (o *Orchestrator) isPRMerged(prNumber int) (bool, error) {
	if o.isPRMergedFn != nil {
		return o.isPRMergedFn(prNumber)
	}
	if o.cycleActive {
		if result, ok := o.cyclePRMerged[prNumber]; ok {
			return result.value, result.err
		}
	}
	var merged bool
	var err error
	if o.readSource != nil {
		merged, err = o.readSource.IsPRMerged(prNumber)
	} else if o.gh == nil {
		err = fmt.Errorf("no github client configured for pr-merged check")
	} else {
		merged, err = o.gh.IsPRMerged(prNumber)
	}
	if o.cycleActive {
		o.cyclePRMerged[prNumber] = cycleBoolResult{value: merged, err: err}
	}
	return merged, err
}

func (o *Orchestrator) mergedPRForBranch(branch string) (int, error) {
	if o.mergedPRForBranchFn != nil {
		return o.mergedPRForBranchFn(branch)
	}
	if o.gh == nil {
		return 0, fmt.Errorf("no github client configured for merged-branch check")
	}
	if o.cycleActive {
		prs, err := o.listClosedPRsForCycle()
		if err != nil {
			return 0, err
		}
		for _, pr := range prs {
			if strings.EqualFold(pr.State, "closed") && pr.MergedAt != "" && pr.HeadRefName == branch {
				return pr.Number, nil
			}
		}
		return 0, nil
	}
	return o.gh.MergedPRNumberForBranch(branch)
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

// prMergeCommitSHA returns the exact immutable commit GitHub records for this
// merged PR. It must not be inferred from main, which may already contain a
// version bump or another concurrent merge.
func (o *Orchestrator) prMergeInfo(prNumber int) (github.PRMergeInfo, error) {
	if o.ghPRMergeInfoFn != nil {
		return o.ghPRMergeInfoFn(prNumber)
	}
	if o.gh == nil {
		return github.PRMergeInfo{}, fmt.Errorf("no github client configured for PR merge info")
	}
	return o.gh.PRMergeInfo(prNumber)
}

func (o *Orchestrator) prCIStatus(prNumber int) (string, error) {
	if o.ghPRCIStatusFn != nil {
		return o.ghPRCIStatusFn(prNumber)
	}
	return o.gh.PRCIStatus(prNumber)
}

func (o *Orchestrator) prCheckRollup(prNumber int) (github.PRCheckRollup, error) {
	if o.ghPRCheckRollupFn != nil {
		return o.ghPRCheckRollupFn(prNumber)
	}
	// Keep existing test/integration hooks source-compatible. A legacy hook has
	// no authoritative head or per-check identity, so it may drive merge logic
	// but deliberately cannot mint a durable PR-gate snapshot.
	if o.ghPRCIStatusFn != nil {
		verdict, err := o.ghPRCIStatusFn(prNumber)
		return github.PRCheckRollup{Verdict: verdict}, err
	}
	if o.gh == nil {
		return github.PRCheckRollup{Verdict: "unknown"}, fmt.Errorf("no github client configured for PR check rollup")
	}
	return o.gh.PRCheckRollup(prNumber)
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
	if o.ghPRReviewGateVerdictFn != nil {
		return o.ghPRReviewGateVerdictFn(prNumber, streams)
	}
	// Preserve the narrow legacy test hook, but let the production aggregate
	// reader return its structured actionable findings. The bool-only Greptile
	// verdict cannot distinguish a new late finding while the aggregate decision
	// remains blocked, so it is insufficient for the durable PR-gate snapshot.
	if len(streams) == 1 && streams[0] == "greptile" && o.ghPRGreptileApprovedFn != nil {
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
	if o.gh == nil {
		return github.ReviewGateVerdict{}, fmt.Errorf("no github client configured for PR review gate")
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

func (o *Orchestrator) prDetails(prNumber int) (github.PR, error) {
	if o.ghPRDetailsFn != nil {
		return o.ghPRDetailsFn(prNumber)
	}
	if o.gh == nil {
		return github.PR{}, fmt.Errorf("no github client configured for PR details")
	}
	return o.gh.PRDetails(prNumber)
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

func (o *Orchestrator) prLabels(prNumber int) ([]string, error) {
	if o.ghPRLabelsFn != nil {
		return o.ghPRLabelsFn(prNumber)
	}
	if o.gh == nil {
		return nil, fmt.Errorf("no github client configured for PR labels")
	}
	return o.gh.PRLabels(prNumber)
}

func (o *Orchestrator) prHasCriticalReview(prNumber int) (bool, error) {
	if o.ghPRHasCriticalReviewFn != nil {
		return o.ghPRHasCriticalReviewFn(prNumber)
	}
	if o.gh == nil {
		// Fail safe: cannot determine criticality -> caller must NOT auto-merge.
		return false, fmt.Errorf("no github client configured for critical-review check")
	}
	// The check must cover whatever streams actually gate this project: an
	// llm-review P0/P1 on head blocks the #565 convergence merge exactly like
	// a Greptile P0 does (#1148 review round 1, P1-1).
	return o.gh.PRHasCriticalReviewOnHead(prNumber, o.cfg.EffectiveReviewGateStreams())
}

func (o *Orchestrator) prUnresolvedReviewThreadsOnHead(prNumber int) (string, []github.ReviewThread, error) {
	if o.ghPRUnresolvedThreadsFn != nil {
		return o.ghPRUnresolvedThreadsFn(prNumber)
	}
	if o.gh == nil {
		return "", nil, fmt.Errorf("no github client configured for review-thread check")
	}
	return o.gh.PRUnresolvedReviewThreadsOnHead(prNumber)
}

// finalMerge* wrappers preserve the existing narrow merge test hooks without
// weakening production. A real orchestrator always has o.gh; only unit fakes
// that inject ghMergePRFn alone receive an empty safe snapshot.
func (o *Orchestrator) finalMergeIssue(number int) (github.Issue, error) {
	if o.getIssueFn == nil && o.ghMergePRFn != nil {
		return github.Issue{Number: number}, nil
	}
	return o.getIssue(number)
}

func (o *Orchestrator) finalMergePRLabels(prNumber int) ([]string, error) {
	if o.ghPRLabelsFn == nil && o.ghMergePRFn != nil {
		return nil, nil
	}
	return o.prLabels(prNumber)
}

func (o *Orchestrator) finalMergeReviewThreads(prNumber int) (string, []github.ReviewThread, error) {
	if o.ghPRUnresolvedThreadsFn == nil && o.ghMergePRFn != nil {
		if o.ghPRHeadSHAFn != nil {
			head, err := o.ghPRHeadSHAFn(prNumber)
			return head, nil, err
		}
		return strings.Repeat("a", 40), nil, nil
	}
	return o.prUnresolvedReviewThreadsOnHead(prNumber)
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

func (o *Orchestrator) mergePRAtHead(prNumber int, expectedHeadSHA string) error {
	var err error
	switch {
	case o.ghMergePRAtHeadFn != nil:
		err = o.ghMergePRAtHeadFn(prNumber, expectedHeadSHA)
	case o.ghMergePRFn != nil:
		// Legacy test hook compatibility. Production always takes the atomic
		// *github.Client.MergePRAtHead path below.
		err = o.ghMergePRFn(prNumber)
	case o.gh == nil:
		err = fmt.Errorf("no github client configured for head-bound merge")
	default:
		err = o.gh.MergePRAtHead(prNumber, expectedHeadSHA)
	}
	if err == nil {
		o.invalidateCyclePRs()
	}
	return err
}

func (o *Orchestrator) closePR(prNumber int, comment string) error {
	var err error
	switch {
	case o.ghClosePRFn != nil:
		err = o.ghClosePRFn(prNumber, comment)
	case o.gh == nil:
		err = fmt.Errorf("no github client configured for close-pr")
	default:
		err = o.gh.ClosePR(prNumber, comment)
	}
	// A close removes the PR from the open-PR set; drop the per-cycle cache so
	// a later step re-fetches a current view (#794 review).
	if err == nil {
		o.invalidateCyclePRs()
	}
	return err
}

func (o *Orchestrator) prChecksOutput(prNumber int) (string, error) {
	if o.ghPRChecksOutputFn != nil {
		return o.ghPRChecksOutputFn(prNumber)
	}
	return o.gh.PRChecksOutput(prNumber)
}

func (o *Orchestrator) collectPRReviewFeedback(prNumber int) (string, error) {
	// The configured review streams scope which bot logins count as review
	// feedback: on llm-review rows the bot's inline findings must reach the
	// retry pipeline (otherwise AutoRetryReviewFeedback never fires and the
	// PR hangs), while on greptile-only rows fleet-bot comments stay
	// invisible (#1148 round 2, P1).
	streams := o.cfg.EffectiveReviewGateStreams()
	if o.ghCollectPRReviewFeedbackFn != nil {
		return o.ghCollectPRReviewFeedbackFn(prNumber, streams)
	}
	return o.gh.CollectPRReviewFeedback(prNumber, streams)
}

func (o *Orchestrator) prFailingChecks(prNumber int) ([]github.FailingCheck, error) {
	if o.ghPRFailingChecksFn != nil {
		return o.ghPRFailingChecksFn(prNumber)
	}
	if o.gh == nil {
		return nil, fmt.Errorf("no github client configured for failing-checks")
	}
	return o.gh.PRFailingChecks(prNumber)
}

// collectFailingCheckContext fetches the check-runs still failing on a PR head
// and renders a bounded, redacted excerpt for the retry prompt (#857).
// Best-effort: a fetch error yields "" (no section) rather than blocking the
// retry, and an all-green head yields "" so a retry with no red check is
// unchanged.
func (o *Orchestrator) collectFailingCheckContext(prNumber int) string {
	checks, err := o.prFailingChecks(prNumber)
	if err != nil {
		log.Printf("[orch] warn: could not collect failing-check context for PR #%d: %v", prNumber, err)
		return ""
	}
	return formatFailingCheckContext(checks, failingCheckExcerptCapBytes)
}

func (o *Orchestrator) closeIssue(number int, comment string) error {
	var err error
	if o.ghCloseIssueFn != nil {
		err = o.ghCloseIssueFn(number, comment)
	} else if o.gh == nil {
		err = fmt.Errorf("no github client configured for close-issue")
	} else {
		err = o.gh.CloseIssue(number, comment)
	}
	if err == nil && o.cycleActive {
		delete(o.cycleIssueClosed, number)
	}
	return err
}

func (o *Orchestrator) stopWorker(slotName string, sess *state.Session) error {
	if o.workerStopFn != nil {
		return o.workerStopFn(o.cfg, slotName, sess)
	}
	return worker.Stop(o.cfg, slotName, sess)
}

func (o *Orchestrator) stopWorkerProcess(slotName string, sess *state.Session) error {
	if o.workerStopProcessFn != nil {
		return o.workerStopProcessFn(slotName, sess)
	}
	return worker.StopProcess(slotName, sess)
}

func (o *Orchestrator) getIssue(number int) (github.Issue, error) {
	if o.getIssueFn != nil {
		return o.getIssueFn(number)
	}
	if o.readSource != nil {
		return o.readSource.GetIssue(number)
	}
	if o.gh == nil {
		return github.Issue{}, fmt.Errorf("no github client configured for get issue")
	}
	return o.gh.GetIssue(number)
}

func (o *Orchestrator) respawnWorker(slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	return o.respawnWorkerWithConfig(o.cfg, slotName, sess, issue, promptBase, backendName)
}

// respawnWorkerWithConfig respawns a dead worker using the given config, which
// the escalation ladder uses to carry a tier's per-tier effort/model override
// (#783). It defaults to o.cfg via respawnWorker.
func (o *Orchestrator) respawnWorkerWithConfig(cfg *config.Config, slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	return state.WithSessionLease(cfg.StateDir, slotName, func() error {
		return o.respawnWorkerUnlocked(cfg, slotName, sess, issue, promptBase, backendName)
	})
}

func (o *Orchestrator) respawnWorkerUnlocked(cfg *config.Config, slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	// Support both hook names for test compatibility (respawnWorkerFn = branch, workerRespawnFn = HEAD)
	if o.respawnWorkerFn != nil {
		return o.respawnWorkerFn(cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	if o.workerRespawnFn != nil {
		return o.workerRespawnFn(cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	return worker.Respawn(cfg, slotName, sess, o.repo, issue, promptBase, backendName)
}

func (o *Orchestrator) saveCheckpoint(sess *state.Session) (string, error) {
	if o.saveCheckpointFn != nil {
		return o.saveCheckpointFn(sess)
	}
	return worker.SaveCheckpoint(sess)
}

func (o *Orchestrator) checkOutcome(ctx context.Context) outcome.HealthCheckResult {
	if o.emergencyHaltFn != nil && o.emergencyHaltFn() {
		return outcome.HealthCheckResult{
			CheckedAt: time.Now().UTC(),
			State:     outcome.HealthUnknown,
			Summary:   "skipped: emergency stop active",
		}
	}
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

// respawnInPlaceWithConfig respawns a worker in place using the given config so
// the escalation ladder (and the soft-token checkpoint path) can carry a tier's
// per-tier effort/model override (#783/#792). Callers that have no override pass
// o.cfg.
func (o *Orchestrator) respawnInPlaceWithConfig(cfg *config.Config, slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	return state.WithSessionLease(cfg.StateDir, slotName, func() error {
		return o.respawnInPlaceUnlocked(cfg, slotName, sess, issue, promptBase, backendName)
	})
}

func (o *Orchestrator) respawnInPlaceUnlocked(cfg *config.Config, slotName string, sess *state.Session, issue github.Issue, promptBase string, backendName string) error {
	if o.respawnInPlaceFn != nil {
		return o.respawnInPlaceFn(cfg, slotName, sess, o.repo, issue, promptBase, backendName)
	}
	return worker.RespawnInPlace(cfg, slotName, sess, o.repo, issue, promptBase, backendName)
}

// respawnPreservingWorktreeWithConfig is the only recovery path for a session
// that still owns a worktree. A provider transition or post-exit retry must not
// call worker.Respawn: that function intentionally deletes and recreates the
// worktree, which loses completed-but-uncommitted work. Dirty work is first
// checkpointed into the existing resumable checkpoint contract, then the same
// slot/branch/worktree is restarted in place on the selected backend.
func (o *Orchestrator) respawnPreservingWorktreeWithConfig(cfg *config.Config, slotName string, sess *state.Session, issue github.Issue, promptBase, backendName string) error {
	return state.WithSessionLease(cfg.StateDir, slotName, func() error {
		return o.respawnPreservingWorktreeUnlocked(cfg, slotName, sess, issue, promptBase, backendName)
	})
}

func (o *Orchestrator) respawnPreservingWorktreeUnlocked(cfg *config.Config, slotName string, sess *state.Session, issue github.Issue, promptBase, backendName string) error {
	if sess == nil || strings.TrimSpace(sess.Worktree) == "" {
		return o.respawnWorkerUnlocked(cfg, slotName, sess, issue, promptBase, backendName)
	}
	restoreWorktree := o.restoreWorktreeFn
	if restoreWorktree == nil && o.respawnInPlaceFn == nil {
		restoreWorktree = worker.RestoreMissingWorktree
	}
	if restoreWorktree != nil {
		// Directory existence is insufficient: a retained directory can contain
		// a stale .git pointer after administrative metadata was removed. The
		// restore helper validates Git usability, preserves an invalid directory,
		// and recreates only the exact canonical slot/path/branch (#957).
		if err := restoreWorktree(cfg.LocalPath, cfg.WorktreeBase, slotName, sess.Worktree, sess.Branch); err != nil {
			return err
		}
		log.Printf("[orch] worker %s: verified canonical worktree on existing branch %s", slotName, sess.Branch)
	}
	// Unit tests inject an in-place respawner with synthetic paths. Production
	// never has that hook and therefore still fails closed if a retained
	// worktree cannot be inspected.
	if _, statErr := os.Stat(sess.Worktree); errors.Is(statErr, os.ErrNotExist) && o.respawnInPlaceFn != nil {
		return o.respawnInPlaceUnlocked(cfg, slotName, sess, issue, promptBase, backendName)
	}
	dirty, err := worker.WorktreeDirty(sess.Worktree)
	if err != nil {
		return err
	}
	if dirty {
		checkpoint, err := o.saveCheckpoint(sess)
		if err != nil {
			return fmt.Errorf("checkpoint dirty worktree before recovery: %w", err)
		}
		sess.CheckpointFile = checkpoint
		log.Printf("[orch] worker %s: checkpointed dirty worktree before in-place recovery on %s", slotName, backendName)
	}
	return o.respawnInPlaceUnlocked(cfg, slotName, sess, issue, promptBase, backendName)
}

func (o *Orchestrator) respawnPreservingWorktree(slotName string, sess *state.Session, issue github.Issue, promptBase, backendName string) error {
	return o.respawnPreservingWorktreeWithConfig(o.cfg, slotName, sess, issue, promptBase, backendName)
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
	if o.cycleActive {
		if result, ok := o.cycleIssueClosed[number]; ok {
			return result.value, result.err
		}
	}
	var closed bool
	var err error
	if o.readSource != nil {
		closed, err = o.readSource.IsIssueClosed(number)
	} else if o.gh == nil {
		err = fmt.Errorf("no github client configured for issue-closed check")
	} else {
		closed, err = o.gh.IsIssueClosed(number)
	}
	if o.cycleActive {
		o.cycleIssueClosed[number] = cycleBoolResult{value: closed, err: err}
	}
	return closed, err
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

// rateLimitResetFromLog reads a dead worker's log tail and parses the
// provider-stated reset time ("try again at ...") if present. It returns nil
// when the log is unreadable or carries no parseable reset hint. Only the log
// tail is scanned, and the last parseable hint in it wins: a long-running
// worker can echo the live quota text as prompt or work content and later die
// for an unrelated reason, and a whole-log first-match scan read that echo as
// the terminal error (#805 review).
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
	// Resolve a time-only reset hint ("try again at 12:30 PM", #805) against
	// the log's last write — the moment the CLI printed the hint — not the
	// moment the daemon got around to reading it. A dead worker can sit
	// unreconciled past the stated reset (daemon restart, long poll gap);
	// resolved against time.Now() the already-elapsed hint rolls a full day
	// forward and gates the backend ~24h after its quota window actually
	// reset. Against the write time the stale hint resolves to the elapsed
	// instant instead, so the recorded cooldown is already expired.
	ref := time.Now().UTC()
	if fi, statErr := os.Stat(logFile); statErr == nil {
		ref = fi.ModTime().UTC()
	}
	if reset, ok := worker.ParseRateLimitResetFromLog(logFile, ref); ok {
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
//
// Both the classifier and the reset parse scan only the log TAIL (the CLI's
// terminal output), like every other post-mortem backend-failure detector: a
// long-running worker that echoed the live quota text mid-log — prompt
// context, work output touching this very signature — and later died
// normally must not be classified as a provider limit (#805 review).
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

// backendUsageLimitFromLog reports whether a dead worker died because its
// backend's account usage quota is exhausted (#805; live: codex "You've hit
// your usage limit ... try again at 12:30 PM" printed once and the CLI
// exited). Like the auth-failure detector it is trusted only within the
// early-death window: a quota-dead CLI dies on its first API call, while the
// same signature in a longer-lived worker's log is more likely incidental
// work content than a backend outage. A quota death whose reset time IS
// parseable never reaches this classifier — providerRateLimitFromLog handles
// it first (any worker age) with the provider-stated RetryAfter.
func (o *Orchestrator) backendUsageLimitFromLog(sess *state.Session, now time.Time) (bool, string) {
	if sess == nil {
		return false, ""
	}
	if sess.StartedAt.IsZero() || now.Sub(sess.StartedAt) > backendAuthFailureWindow {
		return false, ""
	}
	extra := o.backendUsageLimitPatterns(sess.Backend)
	if o.usageLimitFromLogFn != nil {
		return o.usageLimitFromLogFn(sess.LogFile, extra)
	}
	return worker.IsUsageLimit(sess.LogFile, extra)
}

// backendUsageLimitPatterns returns the operator-supplied extra quota-death
// regexes for one backend (config: model.backends.<name>.usage_limit_patterns,
// #805).
func (o *Orchestrator) backendUsageLimitPatterns(backend string) []string {
	if o == nil || o.cfg == nil || backend == "" {
		return nil
	}
	def, ok := o.cfg.Model.Backends[backend]
	if !ok {
		return nil
	}
	return def.UsageLimitPatterns
}

// classifyBackendFailure inspects a dead worker's terminal output for hard
// backend failures that must not burn the issue retry budget. A structured
// CLIProxyAPI model_cooldown is checked first because it is explicitly scoped
// to one provider/model route and carries the credential-rotation aggregate.
func (o *Orchestrator) classifyBackendFailure(sess *state.Session, now time.Time) (backendFailure, bool) {
	if sess != nil {
		result, ok := worker.IsCredentialRotationUnavailable(sess.LogFile, now)
		if ok && (result.Structured || (!sess.StartedAt.IsZero() && now.Sub(sess.StartedAt) <= backendAuthFailureWindow)) {
			pattern := result.AggregateReason
			if pattern == "" {
				pattern = state.BackendBlockModelCooldown
			}
			return backendFailure{
				reason:                    state.BackendBlockModelCooldown,
				pattern:                   pattern,
				provider:                  result.Provider,
				model:                     result.Model,
				credentialCandidates:      result.Candidates,
				credentialCandidatesKnown: result.CandidatesKnown,
				credentialUsable:          result.Usable,
				credentialUsableKnown:     result.UsableKnown,
				aggregateReason:           result.AggregateReason,
				retryAfter:                result.RetryAfter,
				modelScoped:               true,
			}, true
		}
	}
	if ok, label := o.backendAuthFailureFromLog(sess, now); ok {
		return backendFailure{reason: state.BackendBlockAuthFailure, pattern: label}, true
	}
	if ok, label := o.backendModelUnavailableFromLog(sess, now); ok {
		_, model := o.providerModelRouteForSession(sess, "", "")
		reason := state.BackendBlockModelUnavailable
		if label == "model_overloaded" {
			reason = state.BackendBlockModelOverloaded
		}
		return backendFailure{
			reason:      reason,
			pattern:     label,
			model:       model,
			modelScoped: model != "",
		}, true
	}
	if ok, label := o.backendUsageLimitFromLog(sess, now); ok {
		return backendFailure{reason: state.BackendBlockUsageLimit, pattern: label}, true
	}
	return backendFailure{}, false
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
	// Kimi's first-class worker command always defaults to stream-json, so it
	// uses the side channel without requiring the claude/codex usage_stream
	// opt-in. An operator-pinned text output produces no side channel and this
	// path simply leaves usage unchanged.
	if kind == config.BackendKindKimi {
		return o.updateKimiUsageFromJSONL(slotName, sess)
	}
	if kind == config.BackendKindOpencode && o.sessionUsageStream(sess) {
		return o.updateOpenCodeUsageFromJSONL(slotName, sess)
	}
	tokens := worker.ParseTokensFromOutput(output)
	if tokens <= sess.TokensUsedAttempt {
		return false
	}
	delta := tokens - sess.TokensUsedAttempt
	sess.TokensUsedAttempt = tokens
	sess.TokensUsedTotal += delta
	updateTokenBudgetUsage(sess, tokens, worker.TokenBudgetMeasureProviderTotal)
	log.Printf("[orch] %s tokens_used updated: attempt=%d total=%d", slotName, tokens, sess.TokensUsedTotal)
	return true
}

// updateTokenBudgetUsage records an absolute budget observation for the
// current attempt: the generic text parser's total, or the durable marker the
// per-generation live token monitor wrote. Both figures already cover exactly
// one attempt, which is why TokenBudgetTokensWatermark is cleared by
// beginSessionAttempt while the UsageStreamCursors read positions are not
// (#1120) — mixing a file-cumulative scale into this field is what let a
// respawn inherit the previous attempt's ceiling.
//
// The attempt counter is raised to the observation instead of accumulating on
// top of it, so an absolute marker cannot be added a second time to the
// per-attempt deltas a JSONL stream already contributed. With only absolute
// feeders the watermark and the attempt counter move together, which is the
// behaviour this function has always had.
func updateTokenBudgetUsage(sess *state.Session, observed int, measure string) bool {
	if sess == nil {
		return false
	}
	changed := false
	if measure = strings.TrimSpace(measure); measure != "" && sess.TokenBudgetMeasure != measure {
		sess.TokenBudgetMeasure = measure
		changed = true
	}
	if observed > sess.TokenBudgetTokensWatermark {
		sess.TokenBudgetTokensWatermark = observed
		changed = true
	}
	if sess.TokenBudgetTokensWatermark > sess.TokenBudgetTokensAttempt {
		sess.TokenBudgetTokensAttempt = sess.TokenBudgetTokensWatermark
		changed = true
	}
	return changed
}

// updateTokenBudgetUsageFromStream folds a file-cumulative budget measure into
// the attempt counter through that parser's own read position, so a respawn
// which keeps appending to the same file contributes only the new tail and a
// fallover partner's frames stay on their own cursor (#1120).
func updateTokenBudgetUsageFromStream(sess *state.Session, stream string, cumulative int, measure string) bool {
	if sess == nil {
		return false
	}
	changed := false
	if measure = strings.TrimSpace(measure); measure != "" && sess.TokenBudgetMeasure != measure {
		sess.TokenBudgetMeasure = measure
		changed = true
	}
	cursor := usageStreamCursor(sess, stream)
	if cumulative > cursor.BudgetTokens {
		delta := cumulative - cursor.BudgetTokens
		cursor.BudgetTokens = cumulative
		setUsageStreamCursor(sess, stream, cursor)
		sess.TokenBudgetTokensAttempt += delta
		changed = true
	}
	return changed
}

// Usage stream identifiers — one per parser, not one per backend. A fallover
// respawn hands the same <slot>.jsonl to a different backend and each parser
// sums only the frames it recognises, so after a codex->claude fallover
// ParseCodexUsage still returns the codex total while ParseClaudeUsage returns
// claude's, on two independent scales. A single shared watermark silently
// undercounts whichever of the two is the smaller number (#1120), so every
// parser keeps its own read position.
const (
	usageStreamPi       = "pi"
	usageStreamClaude   = "claude"
	usageStreamCodex    = "codex"
	usageStreamKimi     = "kimi"
	usageStreamOpencode = "opencode"

	// The two files usage parsers read. pi consumes the rendered <slot>.log;
	// the structured parsers consume the <slot>.jsonl side channel (#1140).
	usageStreamGroupLog   = "log"
	usageStreamGroupJSONL = "jsonl"
)

// usageStreamCursor returns this parser's read position into the worker's
// append-only usage side channel.
//
// A session that was already running when the cursor map was introduced
// carries only the flat watermarks. They were produced by whichever parser was
// polling at the time — in practice the one asking now, because the
// orchestrator polls the live attempt every tick — so they seed the first
// lookup instead of restarting from zero and re-counting the whole file. The
// first stored cursor closes that upgrade window.
func usageStreamCursor(sess *state.Session, stream string) state.UsageStreamCursor {
	if sess == nil {
		return state.UsageStreamCursor{}
	}
	if sess.UsageStreamCursors != nil {
		return sess.UsageStreamCursors[stream]
	}
	return state.UsageStreamCursor{
		TotalTokens:  sess.UsageTokensWatermark,
		BudgetTokens: sess.TokenBudgetTokensWatermark,
	}
}

// setUsageStreamCursor stores a parser's advanced read position. The flat
// UsageTokensWatermark is kept as a mirror of the stream that last reported so
// persisted state stays readable and a downgrade still finds a sane position.
func setUsageStreamCursor(sess *state.Session, stream string, cursor state.UsageStreamCursor) {
	if sess == nil {
		return
	}
	if sess.UsageStreamCursors == nil {
		sess.UsageStreamCursors = make(map[string]state.UsageStreamCursor, 2)
	}
	sess.UsageStreamCursors[stream] = cursor
	sess.UsageTokensWatermark = cursor.TotalTokens
}

// usageStreamGroup names the file a parser reads. The pi parser consumes the
// rendered <slot>.log; every structured parser consumes the <slot>.jsonl side
// channel. Two files, so a restart of one must not rewind readers of the other.
func usageStreamGroup(stream string) string {
	if stream == usageStreamPi {
		return usageStreamGroupLog
	}
	return usageStreamGroupJSONL
}

// usageStreamRead returns the read position for stream, first rewinding the
// cursors that share its file when cumulative proves that file restarted.
//
// Each parser is cumulative over an append-only file, so its own total can
// only shrink when that file was replaced: an in-place respawn rotates
// <slot>.log (worker.rotateWorkerAttemptLog), a phase transition points the
// session at a different log path, and state-dir cleanup can remove either
// file. Nothing rotates the <slot>.jsonl side channel today —
// rotateWorkerAttemptLog moves <slot>.log and <slot>.log.jsonl, which is not
// the path JSONLPathForLog derives — so for the structured parsers this is a
// guard against an out-of-band replacement rather than a routine event.
//
// The rewind is scoped to the restarted file. Clearing every cursor instead
// meant an in-place respawn, which rotates only <slot>.log, also wiped the
// structured cursors; the next frame from a .jsonl parser then re-read an
// un-rotated file from zero and counted it a second time (#1140).
//
// A cumulative of zero is never a restart: a terminal frame with missing usage
// reports zero, and rewinding on it would let the next real frame be counted
// twice.
func usageStreamRead(sess *state.Session, stream string, cumulative int) state.UsageStreamCursor {
	cursor := usageStreamCursor(sess, stream)
	if sess == nil || cumulative <= 0 || cumulative >= cursor.TotalTokens {
		return cursor
	}
	// Keep the cursors reading the OTHER file; only this file restarted.
	group := usageStreamGroup(stream)
	kept := make(map[string]state.UsageStreamCursor, len(sess.UsageStreamCursors))
	highest := 0
	for name, c := range sess.UsageStreamCursors {
		if usageStreamGroup(name) == group {
			continue
		}
		kept[name] = c
		if c.TotalTokens > highest {
			highest = c.TotalTokens
		}
	}
	sess.UsageStreamCursors = kept
	// UsageTokensWatermark mirrors the most advanced surviving cursor. Zeroing
	// it unconditionally would hand a stale legacy seed to whichever parser
	// asks next, which is the same double-count by another route.
	sess.UsageTokensWatermark = highest
	return state.UsageStreamCursor{}
}

// cloneUsageStreamCursors copies the read-position map so a runtime projection
// overlay cannot alias the source session's map.
func cloneUsageStreamCursors(src map[string]state.UsageStreamCursor) map[string]state.UsageStreamCursor {
	if src == nil {
		return nil
	}
	out := make(map[string]state.UsageStreamCursor, len(src))
	for stream, cursor := range src {
		out[stream] = cursor
	}
	return out
}

func applyTokenBudgetObservation(sess *state.Session, marker worker.TokenBudgetMarker) {
	if sess == nil {
		return
	}
	measure := strings.TrimSpace(marker.Measure)
	if measure == "" {
		measure = worker.TokenBudgetMeasureProviderTotalLegacy
	}
	updateTokenBudgetUsage(sess, marker.TokensObserved, measure)
	// Markers written before measure-aware accounting used the inclusive
	// provider total. Preserve that legacy projection without mixing a new
	// uncached observation into cost telemetry.
	if measure == worker.TokenBudgetMeasureProviderTotalLegacy && marker.TokensObserved > sess.TokensUsedAttempt {
		delta := marker.TokensObserved - sess.TokensUsedAttempt
		sess.TokensUsedAttempt = marker.TokensObserved
		sess.TokensUsedTotal += delta
	}
}

// tokenBudgetKillStreakLimit is how many consecutive PR-less token-budget
// stops on one issue are treated as a misconfigured budget rather than three
// unlucky workers. Two is deliberate: one stop is a plausible runaway worker,
// two in a row on the same issue means the budget itself is the wall.
const tokenBudgetKillStreakLimit = 2

// tokenBudgetMillHold reports whether fresh dispatch must hold for this issue's
// budget-kill streak, and owns the alert bookkeeping. A streak below the limit
// clears the alert memory: once a worker ends any other way the wall is gone,
// and a later wall on the same issue must alert again rather than hold it
// silently — a guard that parks work without telling anyone is the failure mode
// this whole change exists to remove.
func (o *Orchestrator) tokenBudgetMillHold(issueNumber, kills int) bool {
	if o == nil {
		return false
	}
	if kills < tokenBudgetKillStreakLimit {
		delete(o.tokenBudgetMillNotified, issueNumber)
		return false
	}
	o.notifyTokenBudgetMill(issueNumber, kills)
	return true
}

// notifyTokenBudgetMill surfaces a budget-kill streak once per streak length so
// a held issue cannot become a silent stall. The alert class is futile_recovery:
// automated re-dispatch that cannot succeed until an operator changes config.
func (o *Orchestrator) notifyTokenBudgetMill(issueNumber, kills int) {
	if o == nil || o.notifier == nil {
		return
	}
	if o.tokenBudgetMillNotified == nil {
		o.tokenBudgetMillNotified = make(map[int]int)
	}
	if o.tokenBudgetMillNotified[issueNumber] >= kills {
		return
	}
	o.tokenBudgetMillNotified[issueNumber] = kills
	project := strings.TrimSpace(o.repo)
	if project == "" && o.cfg != nil {
		project = strings.TrimSpace(o.cfg.Repo)
	}
	title := "maestro token budget wall"
	if project != "" {
		title += ": " + project
	}
	body := tokenBudgetMillAlertBody(issueNumber, kills, o.cfg.WorkerMaxTokens)
	if err := o.notifier.Alert(notify.AlertFutileRecovery, fmt.Sprintf("%s:token_budget_wall:%d", project, issueNumber), title, body); err != nil {
		log.Printf("[orch] token-budget-wall notification failed for issue #%d: %v", issueNumber, err)
	}
}

// tokenBudgetMillAlertBody names the one action that actually releases the hold.
// The pre-#1124 text asked the operator to raise the budget while the streak
// ignored the budget entirely, so the stated remedy was silently ineffective;
// the escape route has to be both true and specific.
func tokenBudgetMillAlertBody(issueNumber, kills, maxTokens int) string {
	return fmt.Sprintf(
		"issue #%d stopped at the token budget %d times in a row with no PR; worker_max_tokens=%d is likely below the floor this issue needs. Dispatch stays held until worker_max_tokens is raised above %d — the hold then clears itself on the next dispatch cycle, because stops recorded under a lower ceiling stop counting (#1124). Re-adding a ready label does not release it.",
		issueNumber, kills, maxTokens, maxTokens,
	)
}

func tokenBudgetObservation(sess *state.Session) (int, string) {
	if sess == nil {
		return 0, worker.TokenBudgetMeasureProviderTotalLegacy
	}
	if strings.TrimSpace(sess.TokenBudgetMeasure) != "" || sess.TokenBudgetTokensWatermark > 0 || sess.TokenBudgetTokensAttempt > 0 {
		return sess.TokenBudgetTokensAttempt, sess.TokenBudgetMeasure
	}
	return sess.TokensUsedAttempt, worker.TokenBudgetMeasureProviderTotalLegacy
}

func (o *Orchestrator) markTokenBudgetExceeded(slotName string, sess *state.Session, marker worker.TokenBudgetMarker, now time.Time) {
	if sess == nil {
		return
	}
	firstTerminalization := sess.WorkerOutcome != worker.TokenBudgetExceededOutcome
	if now.IsZero() {
		now = time.Now().UTC()
	}
	o.updateTokensUsedFromWorkerLog(slotName, sess)
	applyTokenBudgetObservation(sess, marker)
	// Stamp the ceiling this stop actually hit so a later budget raise can
	// retire the kill streak instead of holding the issue forever (#1124).
	if marker.MaxTokens > 0 {
		sess.TokenBudgetMaxTokens = marker.MaxTokens
	}
	budgetObserved, budgetMeasure := tokenBudgetObservation(sess)
	sess.WorkerOutcome = worker.TokenBudgetExceededOutcome
	sess.LastNotifiedStatus = worker.TokenBudgetExceededOutcome
	sess.Status = state.StatusFailed
	sess.PID = 0
	sess.TmuxSession = ""
	sess.NextRetryAt = nil
	sess.RetryHoldReason = ""
	if sess.FinishedAt == nil {
		sess.FinishedAt = &now
	}
	state.MarkWorkerEnded(sess, *sess.FinishedAt)
	if !firstTerminalization {
		return
	}
	log.Printf("[orch] worker %s stopped by token budget: observed=%d max=%d measure=%s", slotName, budgetObserved, marker.MaxTokens, budgetMeasure)
	if o.notifier != nil {
		o.notifier.Sendf("maestro: worker %s (issue #%d) stopped at its token budget: %s %s observed / %s configured",
			slotName, sess.IssueNumber, worker.FormatTokens(budgetObserved), budgetMeasure, worker.FormatTokens(marker.MaxTokens))
	}
}

// terminalizeTokenBudgetIfExceeded is the retry/reconcile backstop for a hard
// budget stop. The live stream marker is authoritative when present; durable
// per-attempt accounting covers the race where the process disappears before
// the marker is consumed. An already-stamped outcome is normalized through the
// same idempotent sink so stale Dead+NextRetryAt state cannot respawn.
func (o *Orchestrator) terminalizeTokenBudgetIfExceeded(slotName string, sess *state.Session, now time.Time) bool {
	if sess == nil {
		return false
	}
	marker, markerOK := worker.ReadTokenBudgetMarkerForAttempt(sess.LogFile, sess.WorkerGeneration, sess.StartedAt)
	budgetObserved, budgetMeasure := tokenBudgetObservation(sess)
	maxTokens := 0
	if o != nil && o.cfg != nil {
		maxTokens = o.cfg.WorkerMaxTokens
	}
	if !markerOK {
		alreadyTerminal := sess.WorkerOutcome == worker.TokenBudgetExceededOutcome
		if !alreadyTerminal && (maxTokens <= 0 || budgetObserved < maxTokens) {
			return false
		}
		marker = worker.TokenBudgetMarker{
			Outcome:          worker.TokenBudgetExceededOutcome,
			Backend:          sess.Backend,
			TokensObserved:   budgetObserved,
			MaxTokens:        maxTokens,
			Measure:          budgetMeasure,
			WorkerGeneration: sess.WorkerGeneration,
			MeasuredAt:       now,
		}
	}
	measuredAt := marker.MeasuredAt
	if measuredAt.IsZero() {
		measuredAt = now
	}
	o.markTokenBudgetExceeded(slotName, sess, marker, measuredAt)
	return true
}

func (o *Orchestrator) markRepeatedUnexpectedExit(slotName string, sess *state.Session, now time.Time) {
	if sess == nil {
		return
	}
	firstTerminalization := sess.WorkerOutcome != state.WorkerOutcomeRepeatedUnexpectedExit
	if now.IsZero() {
		now = time.Now().UTC()
	}
	sess.WorkerOutcome = state.WorkerOutcomeRepeatedUnexpectedExit
	sess.LastNotifiedStatus = state.WorkerOutcomeRepeatedUnexpectedExit
	sess.Status = state.StatusFailed
	sess.PID = 0
	sess.TmuxSession = ""
	sess.NextRetryAt = nil
	sess.RetryHoldReason = ""
	if sess.FinishedAt == nil {
		sess.FinishedAt = &now
	}
	state.MarkWorkerEnded(sess, *sess.FinishedAt)
	if !firstTerminalization {
		return
	}
	log.Printf("[orch] worker %s terminalized after repeated unexpected exits; automatic respawn disabled", slotName)
	if o.notifier != nil {
		o.notifier.Sendf("maestro: worker %s (issue #%d: %s) disappeared again after automatic recovery — terminalized with no further auto-respawn",
			slotName, sess.IssueNumber, sess.IssueTitle)
	}
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
	cursor := usageStreamRead(sess, usageStreamPi, usage.TotalTokens)
	changed := false
	// #730: Pi re-parses the full appended slot log on each call, so
	// usage.TotalTokens is the cumulative run total across every attempt.
	// On a respawn, the runner keeps appending to the same slot log while
	// TokensUsedAttempt resets to 0 — comparing against TokensUsedAttempt
	// would re-add the prior attempts' tokens. #1120: the Pi read position in
	// UsageStreamCursors survives the new attempt instead (beginSessionAttempt
	// no longer clears it) so only the unseen tail is counted, and an in-place
	// respawn that really does rotate <slot>.log rewinds it in usageStreamRead.
	if usage.TotalTokens > cursor.TotalTokens {
		delta := usage.TotalTokens - cursor.TotalTokens
		cursor.TotalTokens = usage.TotalTokens
		setUsageStreamCursor(sess, usageStreamPi, cursor)
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
	if updateTokenBudgetUsageFromStream(sess, usageStreamPi, usage.BudgetTokens, worker.TokenBudgetMeasureUncached) {
		changed = true
	}
	if strings.TrimSpace(usage.Model) != "" && strings.TrimSpace(sess.Model) == "" {
		sess.Model = usage.Model
		changed = true
	}
	// #730: cost uses the same monotonic read position as tokens — the full-log
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
// ParseClaudeUsage returns the cumulative total of every claude frame in the
// file. #1120: no respawn rotates that file — the same <slot>.log path is
// recomputed and the splitter reopens the jsonl with O_APPEND — so claude's
// read position in UsageStreamCursors survives the new attempt and only the
// unseen tail is added. The position is claude's alone: after a codex->claude
// fallover the same file also holds codex frames that ParseClaudeUsage never
// sees, and a shared watermark would undercount claude's first result frame by
// whatever codex had already reached. Cost prefers the backend-reported
// total_cost_usd. A successful terminal frame with missing/zero usage marks
// the active attribution usage-unreliable and logs once; zero never advances
// token counters or budget progress.
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
	cursor := state.UsageStreamCursor{}
	if ok {
		cursor = usageStreamRead(sess, usageStreamClaude, usage.TotalTokens)
	}
	changed := false
	if usage.UsageUnreliable && state.MarkActiveAttributionUsageUnreliable(sess, usage.UsageUnreliableReason, usage.UsageUnreliableScope) {
		log.Printf("[orch] %s claude usage-unreliable: scope=%s reason=%s; preserving observed counters and treating zero tokens as unavailable, not progress",
			slotName, usage.UsageUnreliableScope, usage.UsageUnreliableReason)
		changed = true
	}
	telemetryChanged := false
	if ok && usage.TotalTokens > cursor.TotalTokens {
		delta := usage.TotalTokens - cursor.TotalTokens
		cursor.TotalTokens = usage.TotalTokens
		setUsageStreamCursor(sess, usageStreamClaude, cursor)
		sess.TokensUsedAttempt += delta
		sess.TokensUsedTotal += delta
		// #739: stamp the cache-aware split (input/output/cache_read/cache_write)
		// so the cost panel can price the cache-read discount. The full-jsonl
		// parse is cumulative, so assigning the run totals tracks this parser's
		// running total alongside the read-position guard.
		sess.TokensInput = usage.Input
		sess.TokensOutput = usage.Output
		sess.TokensCacheRead = usage.CacheRead
		sess.TokensCacheWrite = usage.CacheWrite
		telemetryChanged = true
	}
	if ok && updateTokenBudgetUsageFromStream(sess, usageStreamClaude, usage.BudgetTokens, worker.TokenBudgetMeasureUncached) {
		telemetryChanged = true
	}
	if strings.TrimSpace(usage.Model) != "" && strings.TrimSpace(sess.Model) == "" {
		sess.Model = usage.Model
		telemetryChanged = true
	}
	// total_cost_usd is the run-cumulative the full-jsonl parse sums across
	// every result frame, so guard with `>` (mirrors the Pi cost fix in #732):
	// a first-non-zero freeze would never update past the first attempt.
	if usage.CostUSD > sess.CostUSDBackend {
		sess.CostUSDBackend = usage.CostUSD
		telemetryChanged = true
	}
	if telemetryChanged {
		log.Printf("[orch] %s claude usage: model=%s tokens=%d cost=$%.4f (total=%d)",
			slotName, usage.Model, usage.TotalTokens, usage.CostUSD, sess.TokensUsedTotal)
	}
	return changed || telemetryChanged
}

// updateCodexUsageFromJSONL parses the codex `exec --json` side-channel
// (slot.jsonl) and stamps tokens onto the session (#738). codex emits one
// terminal turn.completed usage event per `codex exec` invocation; the
// splitter appends each attempt's frames to the same slot.jsonl, so
// ParseCodexUsage sums them to the cumulative codex total. #1120: nothing
// rotates that file, so codex's read position in UsageStreamCursors survives a
// respawn and a forced retry adds only the unseen tail. The position is
// codex's alone, because after a fallover the same file also holds the other
// backend's frames that ParseCodexUsage never sees (mirrors the claude/Pi
// paths). codex reports no USD, so cost stays virtual:
// CostUSDBackend is left at 0 and sessionCostEstimate supplies the dollar
// figure from the configured pricing block. codex --json carries no
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
	cursor := usageStreamRead(sess, usageStreamCodex, usage.TotalTokens)
	changed := false
	if usage.TotalTokens > cursor.TotalTokens {
		delta := usage.TotalTokens - cursor.TotalTokens
		cursor.TotalTokens = usage.TotalTokens
		setUsageStreamCursor(sess, usageStreamCodex, cursor)
		sess.TokensUsedAttempt += delta
		sess.TokensUsedTotal += delta
		changed = true
	}
	if updateTokenBudgetUsageFromStream(sess, usageStreamCodex, usage.TotalTokens, worker.TokenBudgetMeasureCodexRollout) {
		changed = true
	}
	if changed {
		log.Printf("[orch] %s codex usage: input=%d output=%d cache_read=%d tokens=%d (total=%d)",
			slotName, usage.Input, usage.Output, usage.CacheRead, usage.TotalTokens, sess.TokensUsedTotal)
	}
	return changed
}

// updateKimiUsageFromJSONL parses Kimi's first-class stream-json side channel
// and stamps split tokens onto the session. ParseKimiUsage returns cumulative
// Kimi usage across the append-only file, so Kimi's own read position in
// UsageStreamCursors survives a respawn and keeps retries from double-counting
// (#1120) without hiding a fallover partner's frames behind a shared
// watermark. Native Kimi usage normally has no USD field; when absent, Fleet cost
// observability applies the backend's configured split pricing to these
// counters.
func (o *Orchestrator) updateKimiUsageFromJSONL(slotName string, sess *state.Session) bool {
	jsonlPath := o.workerJSONLFile(slotName, sess)
	if jsonlPath == "" {
		return false
	}
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		return false
	}
	usage, ok := worker.ParseKimiUsage(string(data))
	if !ok {
		return false
	}
	cursor := usageStreamRead(sess, usageStreamKimi, usage.TotalTokens)
	changed := false
	if usage.TotalTokens > cursor.TotalTokens {
		delta := usage.TotalTokens - cursor.TotalTokens
		cursor.TotalTokens = usage.TotalTokens
		setUsageStreamCursor(sess, usageStreamKimi, cursor)
		sess.TokensUsedAttempt += delta
		sess.TokensUsedTotal += delta
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
	if usage.CostUSD > sess.CostUSDBackend {
		sess.CostUSDBackend = usage.CostUSD
		changed = true
	}
	if changed {
		log.Printf("[orch] %s kimi usage: input=%d output=%d cache_read=%d cache_write=%d tokens=%d cost=$%.4f (total=%d)",
			slotName, usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite,
			usage.TotalTokens, usage.CostUSD, sess.TokensUsedTotal)
	}
	return changed
}

// updateOpenCodeUsageFromJSONL parses the opencode --format json side-channel
// (slot.jsonl) and stamps tokens/cost onto the session. opencode emits one
// terminal step_finish event per `opencode run` invocation; the stream-splitter
// appends each attempt's frames to the same slot.jsonl, so
// ParseOpenCodeUsage sums them to the cumulative opencode total. #1120:
// nothing rotates that file, so opencode's own read position in
// UsageStreamCursors survives a respawn and a forced retry adds only the
// unseen tail, while a fallover partner's frames stay on their own cursor
// (mirrors the claude/codex/Pi paths). opencode carries no model name in the
// event stream, so sess.Model is left as configured. Returns true when
// anything changed; false (tokens stay 0) when the jsonl is absent — the
// documented degradation when the stream-splitter was unavailable.
func (o *Orchestrator) updateOpenCodeUsageFromJSONL(slotName string, sess *state.Session) bool {
	jsonlPath := o.workerJSONLFile(slotName, sess)
	if jsonlPath == "" {
		return false
	}
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		return false
	}
	usage, ok := worker.ParseOpenCodeUsage(string(data))
	if !ok {
		return false
	}
	cursor := usageStreamRead(sess, usageStreamOpencode, usage.TotalTokens)
	changed := false
	if usage.TotalTokens > cursor.TotalTokens {
		delta := usage.TotalTokens - cursor.TotalTokens
		cursor.TotalTokens = usage.TotalTokens
		setUsageStreamCursor(sess, usageStreamOpencode, cursor)
		sess.TokensUsedAttempt += delta
		sess.TokensUsedTotal += delta
		// Reasoning tokens are a separate thinking-tokens bucket in opencode
		// (not a subset of output). Assign output+reasoning to TokensOutput
		// since there is no dedicated reasoning field on the session.
		sess.TokensInput = usage.Input
		sess.TokensOutput = usage.Output + usage.Reasoning
		sess.TokensCacheRead = usage.CacheRead
		sess.TokensCacheWrite = usage.CacheWrite
		changed = true
	}
	if updateTokenBudgetUsageFromStream(sess, usageStreamOpencode, usage.TotalTokens, worker.TokenBudgetMeasureInputOutputReasoning) {
		changed = true
	}
	if usage.CostUSD > sess.CostUSDBackend {
		sess.CostUSDBackend = usage.CostUSD
		changed = true
	}
	if changed {
		log.Printf("[orch] %s opencode usage: input=%d output=%d reasoning=%d cache_read=%d cache_write=%d tokens=%d cost=$%.4f (total=%d)",
			slotName, usage.Input, usage.Output, usage.Reasoning, usage.CacheRead, usage.CacheWrite, usage.TotalTokens, usage.CostUSD, sess.TokensUsedTotal)
	}
	return changed
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
	fallbacks := o.cfg.Model.FallbackCandidates(sess.Backend)
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
//     whether the outcome contract requires deploy), unless
//     ReleasedForRedispatch (#1020) which maps to Todo so an issue whose merge
//     did not fix it (docs-only / ineffective) stays runnable
//   - done                  => Done
//   - retry_exhausted /
//     conflict_failed       => Blocked
//   - failed                => Blocked, unless ReleasedForRedispatch (#818),
//     which maps to Todo so a released-for-fresh-dispatch issue stays runnable
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
		// #1020: a code_landed session released for redispatch (docs-only /
		// record-only delivery, or an ineffective fix whose blocking outcome
		// check never recovered) did not settle its issue. Mirror it as runnable
		// Todo, not Deploying/Live Verification, so the dynamic wave re-dispatches
		// instead of leaving the issue stranded behind an ineffective merge.
		if sess.ReleasedForRedispatch {
			return github.ProjectStatusTodo, true
		}
		return codeLandedProjectStatusForSession(sess, requiresDeploy), true
	case state.StatusDone:
		// #1103: a merged session released because its issue stayed open did
		// not settle the issue. Keep a standing Todo mapping so a transient
		// failure in the edge-triggered release sync cannot strand dynamic wave
		// behind an Awaiting Close / Done board status. Forge-closed sessions
		// retain Done even though closed-issue reconciliation releases their
		// state claim for audit/reopen semantics.
		if sess.ReleasedForRedispatch && sess.IssueClosedAt == nil {
			return github.ProjectStatusTodo, true
		}
		return github.ProjectStatusDone, true
	case state.StatusRetryExhausted, state.StatusConflictFailed:
		return github.ProjectStatusBlocked, true
	case state.StatusFailed:
		// #818: a closed-unmerged retry_exhausted session is settled failed to
		// release its issue for fresh dispatch. Mirror that released session as
		// runnable Todo, not Blocked — otherwise reconcileSessionsToProjectBoard
		// would re-push Blocked over the Todo the reconcile set, and the dynamic
		// wave (which treats Blocked as non-runnable) would re-strand the issue
		// instead of re-dispatching it. Genuinely-failed sessions still map to
		// Blocked.
		if sess.ReleasedForRedispatch {
			return github.ProjectStatusTodo, true
		}
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
	out, err := tmuxsession.CommandForSession(session, "capture-pane", "-t", "="+session+":", "-p").Output()
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

The current canonical PR for this issue failed CI and remains open.
You are an in-place retry worker on that same branch and worktree. Fix the CI
failures described below, push to the existing PR, and do NOT open another PR.

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

func appendRecoveryHandoffContext(promptBase, handoff string) string {
	return fmt.Sprintf(`%s

### Preserved Work From a Superseded Session

Maestro recovered the canonical issue/PR identity after a duplicate or stale
session. The following committed work is preserved locally:

%s

Inspect these exact commits, carry every useful change onto your current
canonical branch, and push only to the existing PR. Do NOT delete the preserved
branch/worktree and do NOT open another PR.
`, promptBase, handoff)
}

// failingCheckExcerptCapBytes hard-caps the failing-check excerpt placed in a
// retry prompt, mirroring the post-mortem cap discipline from #835 so a verbose
// lint log cannot crowd out the rest of the prompt.
const failingCheckExcerptCapBytes = 2048

const failingCheckTruncationMarker = "\n… (failing-check excerpt truncated for the prompt)"

// formatFailingCheckContext assembles a bounded, secret-redacted excerpt naming
// each check-run still failing on the PR head. Each check contributes its
// distilled error lines; a check with no fetchable log degrades to its name and
// conclusion only. The whole excerpt is redacted and then hard-capped. It
// returns "" when nothing is failing, so the caller adds no section (#857).
func formatFailingCheckContext(checks []github.FailingCheck, capBytes int) string {
	var b strings.Builder
	for _, ck := range checks {
		name := strings.TrimSpace(ck.Name)
		if name == "" {
			name = "check"
		}
		conclusion := strings.TrimSpace(ck.Conclusion)
		if conclusion == "" {
			conclusion = "failure"
		}
		excerpt := strings.TrimSpace(ck.Excerpt)
		if excerpt == "" {
			fmt.Fprintf(&b, "- %s failed (conclusion: %s); no log excerpt available.\n", name, conclusion)
			continue
		}
		fmt.Fprintf(&b, "- %s failed (conclusion: %s):\n", name, conclusion)
		for _, line := range strings.Split(excerpt, "\n") {
			line = strings.TrimRight(line, " \t\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	assembled := strings.TrimRight(b.String(), "\n")
	if assembled == "" {
		return ""
	}
	assembled = supervisor.RedactSensitive(assembled)
	return capFailingCheckExcerpt(assembled, capBytes)
}

// capFailingCheckExcerpt bounds s to capBytes with a check-specific truncation
// marker, delegating to the shared worker cap so it honors the same UTF-8 /
// line-boundary discipline as the #835 post-mortem cap. capBytes <= 0 disables
// the cap.
func capFailingCheckExcerpt(s string, capBytes int) string {
	return worker.CapWithMarker(s, capBytes, failingCheckTruncationMarker)
}

// appendFailingCheckContext appends a section naming the check-run(s) still
// failing on the PR head, so a retry treats a red lint check as a hard
// constraint on the new diff instead of only the review comments / bare checks
// overview that triggered the retry (#857). excerpt is already bounded/redacted
// by formatFailingCheckContext.
func appendFailingCheckContext(promptBase, excerpt string) string {
	return fmt.Sprintf(`%s

---

## IMPORTANT: Failing Check on the Current PR Head

Besides any review or CI-failure context above, the following CI check(s) are
FAILING on your PR's current head commit — your previous push did not make them
pass. Treat this as a hard constraint on the new diff and do NOT reintroduce the
same failure:

%s

Make the failing check(s) pass AND address any other feedback before you push again.
`, promptBase, excerpt)
}

// appendPriorAttemptPostmortem appends a bounded, automatically-extracted
// summary of the previous failed attempt's own worker-log trajectory (#835).
// It sits alongside — not in place of — the CI/review/conflict context, so the
// retry worker sees what the last attempt actually tried locally and can avoid
// repeating the same dead ends until retry_exhausted. The postmortem argument
// is already secret-redacted and hard-capped by the caller.
func appendPriorAttemptPostmortem(promptBase, postmortem string, attempt int) string {
	return fmt.Sprintf(`%s

---

## Prior Attempt Post-Mortem (Attempt %d)

A previous attempt on this issue failed. The following is an automatically
extracted summary of what that attempt tried, distilled from its own worker log
(not from GitHub CI or review):

`+"```"+`
%s
`+"```"+`

Do NOT repeat the same approach without addressing why it failed. If the summary
shows a command, file, or hypothesis that led nowhere, change strategy rather
than retrying it verbatim.
`, promptBase, attempt, postmortem)
}

// maybeAppendPriorAttemptPostmortem distills the previous attempt's worker log
// into a post-mortem, persists the full version to the state dir for operator
// inspection, and appends a hard-capped excerpt to the retry prompt. It is
// best-effort: a missing/empty/unreadable log yields no section and no error,
// so respawn proceeds exactly as it does today.
func (o *Orchestrator) maybeAppendPriorAttemptPostmortem(slotName string, sess *state.Session, promptBase string) string {
	logFile := o.workerLogFile(slotName, sess)
	postmortem := worker.ExtractPostmortem(logFile, worker.PostmortemTailLines)
	if strings.TrimSpace(postmortem) == "" {
		return promptBase
	}
	// Persist the full post-mortem for operators; only a capped excerpt goes
	// into the prompt (token budget). All attempts accumulate on disk under a
	// per-attempt filename.
	o.persistPostmortem(slotName, sess.RetryCount, postmortem)
	capped := worker.CapPostmortem(postmortem, worker.PostmortemPromptCapBytes)
	return appendPriorAttemptPostmortem(promptBase, capped, sess.RetryCount)
}

// persistPostmortem writes the full post-mortem to
// <state_dir>/<slot>-attempt<N>-postmortem.md. Best-effort: a write failure is
// logged, never fatal, and never blocks the respawn.
func (o *Orchestrator) persistPostmortem(slotName string, attempt int, body string) {
	if o == nil || o.cfg == nil || strings.TrimSpace(o.cfg.StateDir) == "" {
		return
	}
	name := fmt.Sprintf("%s-attempt%d-postmortem.md", slotName, attempt)
	path := filepath.Join(o.cfg.StateDir, name)
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		log.Printf("[orch] warn: could not persist post-mortem for %s: %v", slotName, err)
	}
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
	slotNames := make([]string, 0, len(s.Sessions))
	for slotName := range s.Sessions {
		slotNames = append(slotNames, slotName)
	}
	sort.Strings(slotNames)

	// Terminal safety outcomes do not need a free worker slot to settle. Repair
	// stale/racy Dead+NextRetryAt projections first so an over-budget worker can
	// never remain queued merely because capacity is full, and a previously
	// terminalized zombie cannot re-enter the spawn path after a state merge.
	now := time.Now().UTC()
	for _, slotName := range slotNames {
		sess := s.Sessions[slotName]
		if sess.Status != state.StatusDead || sess.NextRetryAt == nil {
			continue
		}
		if sess.RetryReason == state.RetryReasonOperatorRestart {
			continue
		}
		o.updateTokensUsedFromWorkerLog(slotName, sess)
		if o.terminalizeTokenBudgetIfExceeded(slotName, sess, now) {
			continue
		}
		if sess.WorkerOutcome == state.WorkerOutcomeRepeatedUnexpectedExit || sess.UnexpectedExitRetries > maxAutomaticUnexpectedExitRetries {
			o.markRepeatedUnexpectedExit(slotName, sess, now)
		}
	}

	if slots <= 0 {
		if pending := pendingRetryReservations(s); pending > 0 {
			log.Printf("[orch] retry queue has %d pending session(s), but no worker slots are available", pending)
		}
		return
	}

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

		// #800: the saved retry state can outlive the work it was scheduled
		// for — the PR may have merged or the issue closed while the backoff
		// ran. Re-check GitHub before consuming a slot; a stale session
		// settles instead of respawning a zombie worker.
		if o.retireStaleRetry(s, slotName, sess) {
			continue
		}
		if hold, prNumber, ok := o.operatorGateHoldForRetry(sess); ok {
			o.applyOperatorGateHold(sess, github.PR{Number: prNumber}, hold)
			log.Printf("[orch] worker %s retry held by operator gate %q - not respawning", slotName, hold.Name)
			continue
		}
		clearOperatorGateHold(sess)

		// Backoff elapsed — respawn the worker
		log.Printf("[orch] worker %s backoff elapsed, respawning (retry %d)", slotName, sess.RetryCount)
		sess.NextRetryAt = nil

		issue, err := o.getIssue(sess.IssueNumber)
		if err != nil {
			log.Printf("[orch] fetch issue #%d for retry: %v — marking as failed", sess.IssueNumber, err)
			sess.RetryHoldReason = ""
			sess.Status = state.StatusFailed
			now := time.Now().UTC()
			sess.FinishedAt = &now
			state.MarkWorkerEnded(sess, now)
			o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) retry failed (could not fetch issue)",
				slotName, sess.IssueNumber, sess.IssueTitle)
			continue
		}
		// A saved retry is not authority to bypass the issue's current dispatch
		// guards. The issue may have gained `blocked` (or another configured
		// exclude label) while the worker ran or while the retry waited. Preserve
		// the canonical session/worktree and re-check next cycle; never consume
		// the retry into a forbidden or duplicate worker.
		if github.HasLabel(issue, o.cfg.ExcludeLabels) {
			retryAt := time.Now().UTC().Add(time.Minute)
			sess.NextRetryAt = &retryAt
			sess.RetryHoldReason = fmt.Sprintf("issue #%d has a current excluded label", sess.IssueNumber)
			log.Printf("[orch] worker %s retry deferred until %s: issue #%d has a current excluded label",
				slotName, retryAt.Format(time.RFC3339), sess.IssueNumber)
			continue
		}
		// The current issue read is authoritative: a previously persisted hold
		// has cleared, so this exact canonical retry may proceed in place.
		sess.RetryHoldReason = ""
		permit, permitOK := o.reserveFleetSpawn()
		if !permitOK {
			retryAt := time.Now().UTC().Add(time.Minute)
			sess.NextRetryAt = &retryAt
			log.Printf("[orch] worker %s retry deferred until %s: fleet live-worker ceiling reached",
				slotName, retryAt.Format(time.RFC3339))
			return
		}

		promptBase := o.selectPrompt(issue)

		// #783: under routing.mode: policy, re-resolve the tier for this retry
		// and climb the escalation ladder when the retry's trigger is enabled,
		// instead of blindly reusing sess.Backend. Resolved BEFORE the CI /
		// review context is consumed below, since the trigger is derived from
		// those fields. The per-issue retry budget bounds the total retries, so
		// the climb cannot loop.
		respawnBackend := sess.Backend
		respawnCfg := o.cfg
		if dec, ok := o.escalateRetryBackend(s, sess, issue); ok {
			if dec.Backend != sess.Backend || dec.Tier != "" {
				log.Printf("[orch] issue #%d retry %d: policy re-resolved backend %s → %s (%s)",
					sess.IssueNumber, sess.RetryCount, sess.Backend, dec.Backend, dec.Reason)
			}
			respawnBackend = dec.Backend
			respawnCfg = applyTierOverride(o.cfg, dec.Backend, dec)
			sel := o.policyBackendSelection(dec)
			sel.PreviousBackend = sess.Backend
			sess.BackendSelection = sel
			// Keep sess.Backend consistent with the dispatched backend for the
			// in-place retry path (RespawnInPlace does not restamp it).
			sess.Backend = dec.Backend
		}

		// #805: a scheduled retry must not respawn onto a backend that is
		// blocked in BackendHealth (quota exhausted, auth-failed, provider
		// limit) or disabled. During the live incident every backoff retry
		// re-resolved to the quota-dead default backend and burned the whole
		// per-issue budget against it. Substitute the first healthy fallback
		// (recorded on BackendSelection), or push the retry to the cooldown
		// expiry when no backend is healthy. Sessions without a stamped
		// backend (legacy states, minimal configs) keep the old behavior.
		if respawnBackend != "" && len(o.cfg.Model.Backends) > 0 {
			now := time.Now().UTC()
			model := ""
			if sess.BackendSelection != nil {
				model = sess.BackendSelection.Model
			}
			if blockedBy, blockedRetry := o.dispatchBackendBlock(s, respawnBackend, model, now); blockedBy != "" {
				selection := o.selectBackendFallback(s, sess, now, selectionReasonRetryBlockedFallback)
				if selection.SelectedBackend == "" {
					// Defer to the EARLIEST cooldown expiry across the blocked
					// candidate set — the session backend and every fallback —
					// not the session backend's alone: with the default gated
					// until 18:00 but a fallback freeing at 13:00, the next
					// pass can resume on the fallback hours earlier. 5 minutes
					// is the re-probe default when no blocked backend states
					// an expiry.
					retryAt := now.Add(5 * time.Minute)
					earliest := blockedRetry
					if candidateRetry := earliestCandidateRetry(selection); candidateRetry != nil && (earliest == nil || candidateRetry.Before(*earliest)) {
						earliest = candidateRetry
					}
					if earliest != nil && earliest.After(now) {
						retryAt = earliest.UTC()
					}
					sess.NextRetryAt = &retryAt
					permit.Release()
					log.Printf("[orch] worker %s retry deferred until %s: backend %s blocked (%s) and no healthy fallback is available",
						slotName, retryAt.Format(time.RFC3339), respawnBackend, blockedBy)
					continue
				}
				log.Printf("[orch] worker %s retry: backend %s blocked (%s%s) — substituting %s",
					slotName, respawnBackend, blockedBy, retryAfterHint(blockedRetry), selection.SelectedBackend)
				selection.PreviousBackend = respawnBackend
				sess.BackendSelection = &selection
				respawnBackend = selection.SelectedBackend
				sess.Backend = respawnBackend
				// Any tier override targeted the blocked backend; respawn the
				// substitute on the plain config.
				respawnCfg = o.cfg
			}
		}

		// If this is a CI failure retry, include CI output and review feedback
		// in the prompt so the new worker knows what went wrong.
		if sess.CIFailureOutput != "" {
			promptBase = appendCIFailureContext(promptBase, sess.CIFailureOutput, sess.RetryCount)
			sess.CIFailureOutput = "" // consumed — don't persist stale output
		}
		if sess.PreviousAttemptFeedback != "" {
			if sess.PreviousAttemptFeedbackKind == "rebase_conflict" {
				promptBase = appendRebaseConflictContext(promptBase, sess.PreviousAttemptFeedback)
			} else if sess.PreviousAttemptFeedbackKind == "recovery_handoff" {
				promptBase = appendRecoveryHandoffContext(promptBase, sess.PreviousAttemptFeedback)
			} else {
				if sess.PreviousAttemptFeedbackKind == state.RetryReasonReviewFeedback {
					sess.RetryReason = state.RetryReasonReviewFeedback
				}
				promptBase = appendReviewFeedbackContext(promptBase, sess.PreviousAttemptFeedback)
			}
			sess.PreviousAttemptFeedback = "" // consumed — don't persist stale feedback
			sess.PreviousAttemptFeedbackKind = ""
		}
		// #857: a retry whose PR head also has a red check (e.g. agent-lint)
		// carries a bounded excerpt of that check alongside — not in place of —
		// the CI/review context above, so the worker fixes the lint failure its
		// previous push introduced instead of only the review P1s / bare
		// checks-overview. Populated by both the CI-failure and review-feedback
		// retry handlers; consumed here regardless of which path scheduled it.
		if sess.FailingCheckContext != "" {
			promptBase = appendFailingCheckContext(promptBase, sess.FailingCheckContext)
			sess.FailingCheckContext = "" // consumed — don't persist stale excerpt
		}

		// #835: append a bounded post-mortem distilled from the previous
		// attempt's own worker log, alongside (not replacing) the CI/review/
		// conflict context above. Read before respawn — respawnWorker /
		// RespawnInPlace repoint sess.LogFile to the new attempt's log.
		promptBase = o.maybeAppendPriorAttemptPostmortem(slotName, sess, promptBase)

		respawnErr := o.respawnPreservingWorktreeWithConfig(respawnCfg, slotName, sess, issue, promptBase, respawnBackend)
		if respawnErr != nil {
			permit.Release()
			log.Printf("[orch] respawn worker %s: %v — marking as failed", slotName, respawnErr)
			sess.Status = state.StatusFailed
			now := time.Now().UTC()
			sess.FinishedAt = &now
			state.MarkWorkerEnded(sess, now)
			o.notifier.Sendf("💀 maestro: worker %s (issue #%d: %s) respawn failed: %v",
				slotName, sess.IssueNumber, sess.IssueTitle, respawnErr)
			continue
		}
		permit.Commit(slotName)

		o.notifier.Sendf("🔄 maestro: retrying worker %s for issue #%d: %s (attempt %d)",
			slotName, sess.IssueNumber, sess.IssueTitle, sess.RetryCount)
		respawned++
	}
}

// retireStaleRetry re-checks issue and PR state on GitHub for a dead session
// whose retry backoff has elapsed (#800). Merge of the session's PR — the one
// still recorded on the session or the one the CI-retry path closed and an
// operator later reopened and merged — or closing of its issue invalidates
// the pending retry: the work is settled, and respawning would burn a worker
// run on a closed issue and let reconcile auto-create a junk PR from the
// already-squash-merged branch. Stale sessions transition to code_landed
// (merged PR, so the normal post-merge reconcile converges them to done) or
// done (issue closed) instead of respawning. Best-effort: a GitHub read
// error fails open — the retry proceeds — so a transient API hiccup cannot
// strand a legitimate retry.
func (o *Orchestrator) retireStaleRetry(s *state.State, slotName string, sess *state.Session) bool {
	for _, prNumber := range []int{sess.PRNumber, sess.LastClosedPRNumber} {
		if prNumber <= 0 {
			continue
		}
		merged, err := o.isPRMerged(prNumber)
		if err != nil {
			log.Printf("[orch] retry staleness check for %s could not read PR #%d: %v — proceeding with retry", slotName, prNumber, err)
			continue
		}
		if !merged {
			continue
		}
		log.Printf("[orch] worker %s retry invalidated: PR #%d for issue #%d is merged — marking code_landed instead of respawning", slotName, prNumber, sess.IssueNumber)
		// markCodeLanded clears NextRetryAt and the issue-guard hold at the
		// shared code_landed sink (#1013), so the retry cannot outlive the merge.
		o.markCodeLanded(sess, prNumber)
		if o.notifier != nil {
			o.notifier.Sendf("🧟 maestro: cancelled scheduled retry for issue #%d (%s) — PR #%d already merged", sess.IssueNumber, sess.IssueTitle, prNumber)
		}
		return true
	}

	closed, err := o.isIssueClosed(sess.IssueNumber)
	if err != nil {
		log.Printf("[orch] retry staleness check for %s could not read issue #%d: %v — proceeding with retry", slotName, sess.IssueNumber, err)
		return false
	}
	if !closed {
		return false
	}
	if !o.reconcileClosedIssueSession(s, slotName, sess, time.Now().UTC()) {
		return true
	}
	if o.notifier != nil {
		o.notifier.Sendf("🧟 maestro: cancelled scheduled retry for issue #%d (%s) — issue already closed", sess.IssueNumber, sess.IssueTitle)
	}
	return true
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
	// Activate the per-cycle GitHub-read cache so the cycle's steps share one
	// ListOpenPRs fetch (#794), and clear it on the way out so nothing leaks
	// across cycles.
	o.beginCycle()
	defer o.endCycle()

	// EMERGENCY STOP: skip the whole orchestrator cycle. Spawn halt alone left
	// outcome verifiers (java/gradle/android), gh api children, and go builds
	// running under the daemon cgroup; engaging the switch must not re-create them.
	if o.emergencyHaltFn != nil && o.emergencyHaltFn() {
		log.Printf("[orch] EMERGENCY STOP active: skipping cycle (no GitHub/outcome/spawn)")
		return nil
	}

	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	log.Printf("[orch] === cycle start — %d sessions in state ===", len(s.Sessions))

	// Storage/process ownership is reconciled before any session-status or
	// scheduling decision. An orphaned exact lease is stopped and cleaned here;
	// ambiguity is persisted as attention and never converted into a broad
	// process/path sweep.
	leaseReconcile := worker.ReconcileWorkerLeases(o.cfg, s, time.Now().UTC())
	if len(leaseReconcile.Cleaned) > 0 {
		log.Printf("[orch] worker lease reconcile cleaned %d exact lease(s)", len(leaseReconcile.Cleaned))
	}
	if leaseReconcile.Attention > 0 {
		log.Printf("[orch] worker lease reconcile surfaced %d ownership attention item(s)", leaseReconcile.Attention)
	}

	// Step 0: Surface a finished self-deploy (#698) as a supervisor finding.
	// Persist immediately: the result file is already consumed, so the
	// finding must not depend on the rest of the cycle succeeding.
	// The stale-trigger watchdog (#807) runs in the same step, right after — it
	// makes the OPPOSITE outcome loud: a trigger that produced no result because
	// the detached deploy unit died silently. Both persist together.
	changed := leaseReconcile.Changed || o.consumeSelfDeployResult(s)
	if o.maybeSurfaceStaleSelfDeploy(s) {
		changed = true
	}
	if changed {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			return fmt.Errorf("save state after pre-cycle reconciliation: %w", err)
		}
	}

	// Step 1: Reconcile stale running sessions before scheduling/slot calculation.
	reconciled := o.reconcileRunningSessions(s)

	// Persist immediately when reconciliation changes state, so slot calculation
	// always sees healed state on disk.
	if reconciled {
		if err := o.saveStatePreservingLiveRuntime(s); err != nil {
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

	// Step 3b2: Standing reconciliation of moot spawn_repair_worker approvals
	// (#866). Independent of the exact done-transition cycle, this stales any
	// repair approval whose target issue already reached a terminal state (a
	// done session, or the issue closed on GitHub), so a transition lost to a
	// conflicting concurrent state save self-heals here instead of aging past
	// SLA as a false operator gate. Runs after checkSessions/reconcileCodeLanded
	// so freshly-done sessions are already reflected in state.
	o.reconcileGuardedRepairApprovals(s)
	o.reconcileResolvedRepairApprovals(s)

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
	if err := o.saveStatePreservingLiveRuntime(s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// Step 5: Start new workers for available slots
	capNow := s.Capacity(capacityInput(o.cfg))
	slots := capNow.AvailableSlots
	if reserved := pendingRetryReservations(s); reserved > 0 && slots > 0 {
		if reserved > slots {
			reserved = slots
		}
		slots -= reserved
		log.Printf("[orch] reserving %d worker slot(s) for scheduled retries", reserved)
	}
	log.Printf("[orch] live_workers=%d pr_gates=%d limit=%d available_slots=%d (separated=%t blocked_by_gates=%d)",
		capNow.LiveWorkers, capNow.PRGates, capNow.Limit, slots, capNow.Separated, capNow.BlockedByGates)

	if slots > 0 {
		o.startNewWorkers(s, slots)
	}

	// Step 5b: persist the machine-readable top-level dispatch hold and the
	// two-cycle idle-stall debounce after fresh dispatch had its chance to
	// create a live worker. This also records gate-bound and paused cycles where
	// slots==0 and startNewWorkers was intentionally skipped.
	o.reconcileDispatchVisibility(s, time.Now().UTC())
	if err := o.saveStatePreservingLiveRuntime(s); err != nil {
		return fmt.Errorf("save state after dispatch visibility: %w", err)
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

// startupJitterCap bounds the random first-cycle phase offset so a freshly
// (re)started daemon still begins work promptly on long poll intervals (#794).
const startupJitterCap = 60 * time.Second

// startupJitterFrac yields a random fraction in [0,1); a package var so tests
// can make the offset deterministic.
var startupJitterFrac = rand.Float64

// startupJitter returns a random phase offset in [0, min(interval, cap)) used
// to de-synchronize the fleet's first GitHub read — and the poll ticker
// anchored to it — so the flows do not burst on the shared PAT in one window
// (#794). Returns 0 for a non-positive interval.
func startupJitter(interval time.Duration) time.Duration {
	return computeStartupJitter(interval, startupJitterFrac())
}

// computeStartupJitter is the pure phase-offset calculation: frac (in [0,1))
// scaled across min(interval, startupJitterCap). Split out so it is unit
// testable without touching the rng.
func computeStartupJitter(interval time.Duration, frac float64) time.Duration {
	if interval <= 0 {
		return 0
	}
	span := interval
	if span > startupJitterCap {
		span = startupJitterCap
	}
	return time.Duration(frac * float64(span))
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

	// Reap orphan container-client wrappers left by a previous run (#977): a
	// `docker run` verification wrapper whose owning worker died can outlive it
	// as a parentless (PPID=1) zombie once its named container exits. A unit
	// restart cannot reparent it back, so clear terminal-container orphans on
	// startup alongside the visual-QA sweep. The watchdog interval catches ones
	// that appear while the daemon keeps running.
	worker.ReapOrphanContainerWrappers(nil)

	// One-time startup sequence before the first poll cycle. The provider-
	// credential scrub (#888) runs in BOTH the long-running daemon and a
	// `run --once` reconcile; the daemon-restart reconciliation steps are
	// daemon-only. Split into runStartupTasks so the once/daemon gating is
	// unit-testable without driving a full poll cycle.
	o.runStartupTasks(once)

	if !once {
		// #794: de-phase this flow from the rest of the fleet before the first
		// cycle. The daemon starts all flows in a tight loop and each anchors
		// its poll ticker to its first RunOnce, so without a phase offset the 8
		// flows fire their GitHub reads in one 1–2s window every interval and
		// trip GitHub's secondary (burst) rate limit on the shared PAT. A random
		// offset before the first cycle spreads both the first burst AND the
		// ticker (anchored right after) across the poll window. `run --once`
		// skips this (a one-shot reconcile must run now); the wait honors ctx so
		// shutdown is never delayed.
		if d := startupJitter(interval); d > 0 {
			log.Printf("[orch] startup jitter — first cycle in %s (%s)", d.Round(time.Second), o.repo)
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-refreshCh:
				t.Stop()
				log.Printf("[orch] startup jitter interrupted by refresh (%s)", o.repo)
			case <-t.C:
			}
		}
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

// runStartupTasks performs the one-time startup sequence before the first poll
// cycle, gated on run mode. It is split out of Run so the once/daemon gating is
// unit-testable without driving a full cycle (which would touch GitHub).
//
// The provider-credential scrub (#888) runs in BOTH the long-running daemon and
// a `run --once` reconcile: a one-shot deployment must not retain stale, raw
// `*-run.env` credential copies until some future daemon happens to start
// (greptile P1 on #890). It is a pure file-content/mode pass that never signals
// a worker process, so it is safe on a live fleet and cannot repeat the #877
// control-group kill.
//
// The restart-reconciliation steps below only make sense for the long-running
// daemon (they lift flags a previous process persisted, or defer expensive
// sweeps) and are deliberately skipped for a `run --once` reconcile tick.
func (o *Orchestrator) runStartupTasks(once bool) {
	// #888: inventory and scrub provider-credential material a pre-fix daemon
	// left in the state dir — remove stale per-worker `*-run.env` copies, strip
	// inlined credential exports from legacy `*-run.sh`, redact historical text
	// state/prompts/logs in place, and repair permissions. Runs in every mode; see
	// this function's doc comment.
	o.scrubLegacyCredentialArtifactsOnStartup()

	if once {
		return
	}

	// Reconcile isolated scratch before startup jitter delays the first normal
	// cycle. This is best-effort and idempotent; RunOnce repeats the same exact
	// pass periodically.
	o.reconcileWorkerLeasesOnStartup()

	// The long-running daemon just (re)started — that is the "restart" the
	// restart-required banner asks for. Reconcile any stale restart_required flag
	// persisted by a previous process into this process's reality (the in-memory
	// signal is false on a clean start) so the banner does not survive the very
	// restart it requested. A genuine post-start config change still re-raises
	// the signal via reloadConfig.
	o.clearStaleRestartRequired()

	// Full ProjectV2 item sweeps are expensive and only repair board drift. Do
	// not run one immediately after every daemon restart; session-state mirroring
	// still runs, and the broad sweep can wait for the normal throttle window.
	o.deferProjectBoardSweep(time.Now().UTC())

	// Graceful drain (#541) is a one-shot "drain then restart" request. A fresh
	// daemon must start in normal (non-drained) mode, so clear any leftover drain
	// flag before the first cycle. Only the long-running daemon clears it — a
	// `run --once` reconcile tick must not lift a drain that an operator is
	// mid-way through.
	o.clearSpawnDrainOnStartup()

	// #497: bound state.Sessions growth — sweep terminal sessions older than the
	// retention window at daemon startup, before the first poll cycle. Idempotent:
	// a no-op when nothing is past the floors. Failures are logged inside the
	// helper.
	o.compactTerminalSessionsOnStartup()
}

func (o *Orchestrator) reconcileWorkerLeasesOnStartup() {
	if o == nil || o.cfg == nil {
		return
	}
	s, err := state.Load(o.cfg.StateDir)
	if err != nil {
		log.Printf("[orch] worker lease startup reconcile: load state: %v", err)
		return
	}
	result := worker.ReconcileWorkerLeases(o.cfg, s, time.Now().UTC())
	if !result.Changed {
		return
	}
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		log.Printf("[orch] worker lease startup reconcile: save state: %v", err)
		return
	}
	log.Printf("[orch] worker lease startup reconcile complete (cleaned=%d attention=%d)", len(result.Cleaned), result.Attention)
}

// scrubLegacyCredentialArtifactsOnStartup neutralizes provider-credential
// material a pre-#888 (or attempt-0) daemon left in the state dir. See
// worker.ScrubLegacyRunArtifacts for the exact file/mode operations; it is a
// pure file-content/mode pass that preserves active workers.
func (o *Orchestrator) scrubLegacyCredentialArtifactsOnStartup() {
	if o == nil || o.cfg == nil {
		return
	}
	worker.ScrubLegacyRunArtifacts(o.cfg.StateDir)
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
func (o *Orchestrator) reloadConfig(newCfg *config.Config, ticker **time.Ticker) {
	old := o.cfg
	var changed []string

	// Restart-required fields — warn only, do not apply
	if newCfg.Repo != old.Repo {
		log.Printf("[orch] config reload: repo changed (%s → %s) — requires restart", old.Repo, newCfg.Repo)
	}

	// The router keeps a pointer to o.cfg, so the complete model selector can be
	// swapped live without reconstructing the orchestrator. Apply backend
	// definitions with the route so a lane added in the same edit is dispatchable.
	if !reflect.DeepEqual(newCfg.Model, old.Model) {
		changed = append(changed, fmt.Sprintf("model policy: %s→%s",
			old.Model.ResolvedRoute().SelectionReason, newCfg.Model.ResolvedRoute().SelectionReason))
		o.cfg.Model = cloneModelConfig(newCfg.Model)
	}
	if !reflect.DeepEqual(newCfg.Routing, old.Routing) {
		o.markRestartRequired(fmt.Sprintf("routing.* changed (router_model %s → %s, mode %s → %s)",
			old.Routing.RouterModel, newCfg.Routing.RouterModel, old.Routing.Mode, newCfg.Routing.Mode))
		log.Printf("[orch] config reload: routing.* changed (router_model %s → %s, mode %s → %s) — requires restart (not applied)",
			old.Routing.RouterModel, newCfg.Routing.RouterModel, old.Routing.Mode, newCfg.Routing.Mode)
	}

	// Capacity is one dispatch decision, so compare and apply every input as one
	// unit. Keeping max_parallel, max_live_workers, and max_concurrent_by_state
	// behind one equality boundary prevents Fleet from publishing a freshly
	// loaded capacity model while the running orchestrator retains one omitted
	// field (#884).
	if !dispatchCapacityConfigEqual(newCfg, old) {
		if newCfg.MaxParallel != old.MaxParallel {
			changed = append(changed, fmt.Sprintf("max_parallel: %d→%d", old.MaxParallel, newCfg.MaxParallel))
		}
		if newCfg.MaxLiveWorkers != old.MaxLiveWorkers {
			changed = append(changed, fmt.Sprintf("max_live_workers: %d→%d", old.MaxLiveWorkers, newCfg.MaxLiveWorkers))
		}
		if !maps.Equal(newCfg.MaxConcurrentByState, old.MaxConcurrentByState) {
			changed = append(changed, "max_concurrent_by_state")
		}
		applyDispatchCapacityConfig(o.cfg, newCfg)
	}

	// Other hot-reloadable fields
	if newCfg.MaxRuntimeMinutes != old.MaxRuntimeMinutes {
		changed = append(changed, fmt.Sprintf("max_runtime_minutes: %d→%d", old.MaxRuntimeMinutes, newCfg.MaxRuntimeMinutes))
		o.cfg.MaxRuntimeMinutes = newCfg.MaxRuntimeMinutes
	}
	if newCfg.MaxRetriesPerIssue != old.MaxRetriesPerIssue {
		changed = append(changed, fmt.Sprintf("max_retries_per_issue: %d→%d", old.MaxRetriesPerIssue, newCfg.MaxRetriesPerIssue))
		o.cfg.MaxRetriesPerIssue = newCfg.MaxRetriesPerIssue
	}
	if newCfg.StalledProgressWatchdog != old.StalledProgressWatchdog {
		changed = append(changed, fmt.Sprintf("stalled_progress_watchdog: active=%t→%t cadence=%s→%s",
			old.StalledProgressWatchdog.IsActive(), newCfg.StalledProgressWatchdog.IsActive(),
			old.StalledProgressWatchdog.EffectiveEvalInterval(), newCfg.StalledProgressWatchdog.EffectiveEvalInterval()))
		o.cfg.StalledProgressWatchdog = newCfg.StalledProgressWatchdog
	}
	if newCfg.WorkerSilentTimeoutMinutes != old.WorkerSilentTimeoutMinutes {
		changed = append(changed, fmt.Sprintf("worker_silent_timeout_minutes: %d→%d", old.WorkerSilentTimeoutMinutes, newCfg.WorkerSilentTimeoutMinutes))
		o.cfg.WorkerSilentTimeoutMinutes = newCfg.WorkerSilentTimeoutMinutes
	}
	if newCfg.WorkerMaxTokens != old.WorkerMaxTokens {
		changed = append(changed, fmt.Sprintf("worker_max_tokens: %d→%d", old.WorkerMaxTokens, newCfg.WorkerMaxTokens))
		o.cfg.WorkerMaxTokens = newCfg.WorkerMaxTokens
	}
	if newCfg.WorkerRuntime != old.WorkerRuntime {
		changed = append(changed, fmt.Sprintf("worker_runtime.mode: %s→%s", old.WorkerRuntime.EffectiveMode(), newCfg.WorkerRuntime.EffectiveMode()))
		o.cfg.WorkerRuntime = newCfg.WorkerRuntime
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
	if newCfg.GitHubMirror != old.GitHubMirror {
		// #826: the mirror-first read source's escape hatch reads o.cfg.GitHubMirror
		// each cycle, so applying it here flips this flow between mirror-first and
		// API-direct on a live config-store edit — no redeploy (AC 3/8).
		changed = append(changed, fmt.Sprintf("github_mirror.source: %s→%s", old.GitHubMirror.Source, newCfg.GitHubMirror.Source))
		o.cfg.GitHubMirror = newCfg.GitHubMirror
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
	if !reflect.DeepEqual(newCfg.ReviewProducer, old.ReviewProducer) {
		changed = append(changed, "review_producer")
		o.cfg.ReviewProducer = newCfg.ReviewProducer
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
		changed = append(changed, "deploy_cmd changed")
		o.cfg.DeployCmd = newCfg.DeployCmd
	}
	if newCfg.DeployTimeoutMinutes != old.DeployTimeoutMinutes {
		changed = append(changed, fmt.Sprintf("deploy_timeout_minutes: %d→%d", old.DeployTimeoutMinutes, newCfg.DeployTimeoutMinutes))
		o.cfg.DeployTimeoutMinutes = newCfg.DeployTimeoutMinutes
	}
	if !reflect.DeepEqual(newCfg.Delivery, old.Delivery) {
		changed = append(changed, "delivery")
		o.cfg.Delivery = newCfg.Delivery
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

func cloneModelConfig(in config.ModelConfig) config.ModelConfig {
	out := in
	out.FallbackBackends = append([]string(nil), in.FallbackBackends...)
	out.ProviderLanes = make([]config.ProviderLane, len(in.ProviderLanes))
	for i, lane := range in.ProviderLanes {
		out.ProviderLanes[i] = lane
		out.ProviderLanes[i].FallbackBackends = append([]string(nil), lane.FallbackBackends...)
	}
	out.Backends = make(map[string]config.BackendDef, len(in.Backends))
	for name, def := range in.Backends {
		if def.Enabled != nil {
			enabled := *def.Enabled
			def.Enabled = &enabled
		}
		def.ExtraArgs = append([]string(nil), def.ExtraArgs...)
		def.UsageLimitPatterns = append([]string(nil), def.UsageLimitPatterns...)
		def.MCP.Configs = append([]string(nil), def.MCP.Configs...)
		if def.MCP.Servers != nil {
			servers := make(map[string]config.MCPServerDef, len(def.MCP.Servers))
			for serverName, server := range def.MCP.Servers {
				server.Args = append([]string(nil), server.Args...)
				server.AllowedTools = append([]string(nil), server.AllowedTools...)
				server.Env = cloneStringMap(server.Env)
				server.Headers = cloneStringMap(server.Headers)
				servers[serverName] = server
			}
			def.MCP.Servers = servers
		}
		out.Backends[name] = def
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// dispatchCapacityConfigEqual compares every config field consumed by
// capacityInput. Keep this as the single reload-detection boundary for dispatch
// capacity so adding a capacity input cannot silently update only Fleet's view.
func dispatchCapacityConfigEqual(a, b *config.Config) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.MaxParallel == b.MaxParallel &&
		a.MaxLiveWorkers == b.MaxLiveWorkers &&
		maps.Equal(a.MaxConcurrentByState, b.MaxConcurrentByState)
}

// applyDispatchCapacityConfig replaces the running orchestrator's complete
// capacity model. Clone the map so the orchestrator keeps its private config
// snapshot instead of aliasing the watcher/Fleet snapshot.
func applyDispatchCapacityConfig(dst, src *config.Config) {
	if dst == nil || src == nil {
		return
	}
	dst.MaxParallel = src.MaxParallel
	dst.MaxLiveWorkers = src.MaxLiveWorkers
	dst.MaxConcurrentByState = maps.Clone(src.MaxConcurrentByState)
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

// checkpointDrainDeathForRestart marks one explicit unexpected process-loss
// transition as resumable while the session is still running. The caller must
// invoke it before changing Status to Dead, and only the daemon's persisted
// in-process shutdown drain qualifies: a generic operator drain can overlap an
// ordinary crash and must not override its terminal or retry policy (#967).
func checkpointDrainDeathForRestart(s *state.State, sess *state.Session, at time.Time) bool {
	if s == nil || sess == nil || sess.Status != state.StatusRunning || !s.ShutdownDrainActive() {
		return false
	}
	worktree := strings.TrimSpace(sess.Worktree)
	if worktree == "" {
		return false
	}
	if _, err := os.Stat(worktree); err != nil {
		return false
	}
	stamp := at.UTC()
	sess.RestartCheckpointAt = &stamp
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
	// Shared across the cycle's four open-PR consumers (#794).
	prs, prErr := o.listOpenPRsForCycle()
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
		// A worker launch is an external side effect: tmux may have accepted it
		// even when the following state.Save lost a concurrent supervisor/watchdog
		// merge race. Older code only inspected StatusRunning, so a live in-place
		// repair hidden behind the previous pr_open/dead projection became invisible
		// to Fleet and the progress watchdog indefinitely. Recover exact ownership
		// before the status filter. Done/code_landed are outcome terminals and must
		// never be reopened merely because an obsolete pane survived.
		if sess.Status != state.StatusRunning && sess.Status != state.StatusDone && sess.Status != state.StatusCodeLanded {
			if o.adoptLiveWorkerRuntime(slotName, sess, time.Now().UTC()) {
				reconciled = true
			}
		}
		isRestartCheckpointedDead := sess.Status == state.StatusDead && sess.RestartCheckpointAt != nil
		if sess.Status != state.StatusRunning && !isRestartCheckpointedDead {
			// A terminal/pr_open projection can outlive its tmux pane while a
			// double-forked tool remains in the worker cgroup. Durable ownership
			// metadata is the restart-safe cleanup receipt: retain it on failure and
			// retry next cycle; clear it only after exact-scope teardown succeeds.
			if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
				if err := o.stopWorkerProcess(slotName, sess); err != nil {
					log.Printf("[orch] reconcile: %s process lease cleanup deferred: %v", slotName, err)
					continue
				}
				reconciled = true
			}
			continue
		}
		// #967: a worker may die after drain starts but before the daemon's
		// shutdown checkpoint pass. Its terminal transition carries the restart
		// marker, but the old draining daemon must not immediately replay it.
		// The replacement daemon clears the one-shot drain flag on startup and
		// then consumes the marker below.
		if isRestartCheckpointedDead && s.DrainActive() {
			continue
		}
		if _, markerOK := worker.ReadTokenBudgetMarkerForAttempt(sess.LogFile, sess.WorkerGeneration, sess.StartedAt); markerOK {
			if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
				if err := o.stopWorkerProcess(slotName, sess); err != nil {
					log.Printf("[orch] reconcile: %s token-budget process lease cleanup deferred: %v", slotName, err)
					continue
				}
			}
			o.runAfterRunHook(sess)
			o.terminalizeTokenBudgetIfExceeded(slotName, sess, time.Now().UTC())
			reconciled = true
			continue
		}

		tmuxName := sess.TmuxSession
		if tmuxName == "" {
			tmuxName = worker.TmuxSessionName(slotName)
		}

		var reasons []string
		tmuxAlive := o.tmuxSessionExists(tmuxName)
		if sess.PID <= 0 {
			reasons = append(reasons, "pid missing")
		} else if !o.pidAlive(sess.PID) {
			reasons = append(reasons, fmt.Sprintf("pid %d dead", sess.PID))
		}
		if !tmuxAlive {
			reasons = append(reasons, fmt.Sprintf("tmux session %q missing", tmuxName))
		}

		if len(reasons) == 0 {
			// Isolated worker scopes survive maestro.service restarts (#891). A
			// shutdown marker can therefore arrive at the replacement daemon while
			// the original PID/tmux is still alive. Validate the exact pane/worktree
			// identity and consume the one-shot marker without respawning or changing
			// the attempt: the same process keeps running exactly once (#966).
			if sess.RestartCheckpointAt != nil {
				pid, paneWorktree, identityErr := o.tmuxPaneIdentity(tmuxName)
				if identityErr != nil || pid <= 0 {
					log.Printf("[orch] reconcile: %s survived restart but pane identity is unreadable (%v); preserving restart marker for a later non-destructive retry", slotName, identityErr)
				} else if strings.TrimSpace(sess.Worktree) == "" || filepath.Clean(paneWorktree) != filepath.Clean(sess.Worktree) {
					log.Printf("[orch] reconcile: %s survived restart but pane worktree %q does not match retained worktree %q; preserving marker and leaving the live worker untouched", slotName, paneWorktree, sess.Worktree)
				} else {
					sess.PID = pid
					sess.TmuxSession = tmuxName
					sess.RestartCheckpointAt = nil
					reconciled = true
					log.Printf("[orch] reconcile: %s adopted the same surviving worker across daemon restart (issue #%d, pid=%d, tmux=%q) — no respawn, no duplicate", slotName, sess.IssueNumber, pid, tmuxName)
				}
			}
			continue
		}
		// Worker process/session is gone. Before marking dead, check whether it
		// already opened a PR. If so, transition to pr_open — the worker succeeded.
		// Without this check, reconcile would mark the session dead, causing
		// IssueInProgress to return false and startNewWorkers to spawn a duplicate.
		if pr, found := branchToPR[sess.Branch]; found {
			o.updateTokensUsedFromWorkerLog(slotName, sess)
			log.Printf("[orch] reconcile: %s running->pr_open (PR #%d already open for branch %q; %s)",
				slotName, pr.Number, sess.Branch, strings.Join(reasons, ", "))
			sess.Status = state.StatusPROpen
			sess.PRNumber = pr.Number
			sess.PID = 0
			sess.TmuxSession = ""
			sess.NextRetryAt = nil
			now := time.Now().UTC()
			sess.FinishedAt = &now
			state.MarkWorkerEnded(sess, now)
			state.MarkPROpened(sess, now)
			reconciled = true
			continue
		}
		if prErr == nil {
			if o.reconcilePushedBranch(s, slotName, sess, strings.Join(reasons, ", ")) {
				reconciled = true
				continue
			}
		}

		// #877: this session was deliberately checkpointed on a previous daemon's
		// shutdown (self-deploy/operator restart) — its recorded pid is gone
		// (reasons above) but its dirty worktree survived. Resume the SAME logical
		// session in place exactly once instead of a false running->dead. This path
		// is deliberately non-destructive and duplicate-proof:
		//
		//   - If a worker tmux session is ALREADY alive under this slot, a
		//     replacement started by a PRIOR restart-resume whose new runtime
		//     identity did not fully persist (e.g. the post-resume state save
		//     failed, so the next daemon reloaded the old marker + dead pid and
		//     re-entered here) is still running. ADOPT it — refresh the recorded
		//     pid from the live pane and consume the marker — rather than calling
		//     RespawnInPlace, which would KILL that replacement (it kills the
		//     slot's deterministic tmux session first) and lose the work it did
		//     since it started (#877 review comment 3). Adoption starts no worker,
		//     so it cannot duplicate-dispatch. On the FIRST resume after a restart
		//     the tmux was reaped by the cgroup SIGKILL, so this branch is skipped.
		//   - Otherwise resume in place. The marker is consumed UP FRONT so a
		//     respawn that itself errors falls through to terminal handling and can
		//     never loop or duplicate-dispatch.
		if sess.RestartCheckpointAt != nil {
			if wt := strings.TrimSpace(sess.Worktree); wt == "" {
				sess.RestartCheckpointAt = nil
				log.Printf("[orch] reconcile: %s restart-resume skipped — session has no worktree; falling through (%s)", slotName, strings.Join(reasons, ", "))
			} else if _, statErr := os.Stat(wt); statErr != nil {
				sess.RestartCheckpointAt = nil
				log.Printf("[orch] reconcile: %s restart-resume skipped — worktree %q gone (%v); falling through", slotName, wt, statErr)
			} else if tmuxAlive {
				// A replacement is already running — adopt it only when both its
				// live PID and worktree match this retained session. Never respawn
				// over a live pane, and never adopt a same-name foreign pane.
				pid, paneWorktree, perr := o.tmuxPaneIdentity(tmuxName)
				if perr != nil || pid <= 0 {
					// Leave the marker SET so the next cycle retries the (harmless,
					// non-destructive) adoption, rather than consuming it and falling
					// to a false running->dead over the live replacement. Unlike a
					// respawn, an adoption retry cannot loop or duplicate.
					log.Printf("[orch] reconcile: %s restart-resume — replacement tmux %q alive but pane identity unreadable (%v); will retry adoption next cycle", slotName, tmuxName, perr)
				} else if filepath.Clean(paneWorktree) != filepath.Clean(wt) {
					// A same-name tmux pane in another directory is not this worker.
					// Fail closed without killing it and consume the automatic resume
					// marker so the next cycle cannot repeatedly adopt or overwrite an
					// unrelated process. The retained worktree remains untouched for
					// explicit operator repair.
					now := time.Now().UTC()
					sess.Status = state.StatusDead
					sess.PID = 0
					sess.TmuxSession = ""
					sess.RestartCheckpointAt = nil
					sess.FinishedAt = &now
					sess.LastNotifiedStatus = "restart_resume_identity_mismatch"
					reconciled = true
					log.Printf("[orch] reconcile: %s restart-resume refused foreign tmux %q: pane worktree %q != retained worktree %q; session left dead, retained worktree preserved, no process killed", slotName, tmuxName, paneWorktree, wt)
				} else {
					sess.PID = pid
					sess.TmuxSession = tmuxName
					sess.RestartCheckpointAt = nil
					reconciled = true
					log.Printf("[orch] reconcile: %s adopted an already-running replacement across restart (issue #%d, pid=%d, tmux=%q) — no respawn, no work lost, exactly once",
						slotName, sess.IssueNumber, pid, tmuxName)
				}
				continue
			} else if issue, fetchErr := o.getIssue(sess.IssueNumber); fetchErr != nil {
				// A transient GitHub read is not proof that the durable recovery is
				// invalid. Preserve the marker and retry next cycle; consuming it here
				// would strand the retained worktree as dead with no retry.
				log.Printf("[orch] reconcile: %s restart-resume deferred — could not fetch issue #%d: %v; marker preserved for next cycle", slotName, sess.IssueNumber, fetchErr)
				continue
			} else {
				o.updateTokensUsedFromWorkerLog(slotName, sess)
				promptBase := o.selectPrompt(issue)
				if respawnErr := o.respawnInPlaceWithConfig(o.cfg, slotName, sess, issue, promptBase, sess.Backend); respawnErr != nil {
					// RespawnInPlace reconciles against the deterministic tmux identity,
					// so retrying cannot create a parallel worker. Keep the marker: a
					// transient backend/launcher failure must not erase recovery intent.
					sess.Status = state.StatusDead
					log.Printf("[orch] reconcile: %s restart-resume in-place failed: %v; marker preserved for next cycle", slotName, respawnErr)
					continue
				} else {
					reconciled = true
					log.Printf("[orch] reconcile: %s resumed in place across restart (issue #%d) — same session, dirty worktree preserved, exactly once (%s)",
						slotName, sess.IssueNumber, strings.Join(reasons, ", "))
					o.notifier.Sendf("🔄 maestro: worker %s (issue #%d: %s) resumed in place after a restart — dirty worktree preserved, no work lost",
						slotName, sess.IssueNumber, sess.IssueTitle)
					continue
				}
			}
		}

		if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
			if err := o.stopWorkerProcess(slotName, sess); err != nil {
				log.Printf("[orch] reconcile: %s stale runtime process lease cleanup deferred: %v", slotName, err)
				continue
			}
			reconciled = true
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
			if respawnErr := o.respawnPreservingWorktree(slotName, sess, issue, promptBase, nextBackend); respawnErr != nil {
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
		if failure, hit := o.classifyBackendFailure(sess, time.Now().UTC()); hit {
			now := time.Now().UTC()
			cp := backendFailureCopyFor(failure.reason)
			o.updateTokensUsedFromWorkerLog(slotName, sess)
			o.recordBackendFailure(s, slotName, sess, failure, now)
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
					slotName, cp.noun, previousBackend, failure.pattern, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s (%s); no fallback backend available — %s",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, failure.pattern, cp.remedy)
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
					slotName, cp.noun, previousBackend, failure.pattern, nextBackend, sess.IssueNumber, fetchErr, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s; fallback to %s failed (could not fetch issue): %v",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, nextBackend, fetchErr)
				continue
			}

			if previousBackend != "" {
				sess.TriedBackends = append(sess.TriedBackends, previousBackend)
			}
			promptBase := o.selectPrompt(issue)
			if respawnErr := o.respawnPreservingWorktree(slotName, sess, issue, promptBase, nextBackend); respawnErr != nil {
				sess.PID = 0
				sess.TmuxSession = ""
				sess.FinishedAt = &now
				state.MarkWorkerEnded(sess, now)
				sess.LastNotifiedStatus = cp.displayToken
				sess.Status = state.StatusDead
				reconciled = true
				log.Printf("[orch] reconcile: %s running->dead via %s on backend=%s signature=%s (fallback respawn on %s failed: %v; %s); pid=%d tmux=%q",
					slotName, cp.noun, previousBackend, failure.pattern, nextBackend, respawnErr, strings.Join(reasons, ", "), oldPID, oldTmux)
				o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s; fallback to %s failed: %v",
					slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, nextBackend, respawnErr)
				continue
			}

			reconciled = true
			log.Printf("[orch] reconcile: %s %s on backend=%s signature=%s — respawned with backend=%s, retry budget preserved (%s); old pid=%d tmux=%q",
				slotName, cp.noun, previousBackend, failure.pattern, nextBackend, strings.Join(reasons, ", "), oldPID, oldTmux)
			o.notifier.Sendf("🔄 maestro: worker %s (issue #%d: %s) backend %s %s (%s) — respawned on %s, retry budget preserved",
				slotName, sess.IssueNumber, sess.IssueTitle, previousBackend, cp.desc, failure.pattern, nextBackend)
			continue
		}

		oldPID := sess.PID
		oldTmux := tmuxName
		// Evaluate retry capacity while this attempt is still Running. Once the
		// status becomes Dead, FailedAttemptsForIssue includes this session; adding
		// sess.RetryCount after that would count the same exit twice and suppress
		// the first recovery when max_retries_per_issue is 1.
		unexpectedRetryAllowed := strings.TrimSpace(sess.Worktree) != "" && o.canRetryIssue(s, sess)
		o.updateTokensUsedFromWorkerLog(slotName, sess)
		now := time.Now().UTC()
		if o.terminalizeTokenBudgetIfExceeded(slotName, sess, now) {
			reconciled = true
			continue
		}
		if !s.DrainActive() && sess.UnexpectedExitRetries >= maxAutomaticUnexpectedExitRetries {
			o.markRepeatedUnexpectedExit(slotName, sess, now)
			reconciled = true
			continue
		}
		// #967: preserve recovery intent while this is still known to be the
		// explicit process/tmux-gone path. Inferring the cause later from a dead
		// session's FinishedAt would also revive ordinary provider failures and
		// scheduled retries that happened during drain.
		if checkpointDrainDeathForRestart(s, sess, now) {
			log.Printf("[orch] reconcile: %s died during drain — retained worktree marked for exact in-place restart resume", slotName)
		}
		sess.Status = state.StatusDead
		sess.PID = 0
		sess.TmuxSession = ""
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		if !s.DrainActive() && unexpectedRetryAllowed {
			if _, statErr := os.Stat(sess.Worktree); statErr == nil {
				// Unexpected process loss used to become an unscheduled dead
				// session. Dead sessions are outside the material-progress watchdog,
				// so recovery waited for a later supervisor cycle and could exceed
				// the 10-minute SLA. Reserve an immediate bounded retry here;
				// respawnDueRetries runs in this same orchestrator cycle and reuses
				// the exact slot/branch/worktree after re-checking GitHub guards.
				retryAt := now
				sess.RetryCount++
				sess.UnexpectedExitRetries++
				sess.RetryReason = state.RetryReasonStalledProgress
				sess.NextRetryAt = &retryAt
				log.Printf("[orch] reconcile: %s scheduled immediate canonical in-place retry %d after unexpected worker exit", slotName, sess.RetryCount)
			}
		}
		reconciled = true

		log.Printf("[orch] reconcile: %s running->dead (%s); pid=%d tmux=%q",
			slotName, strings.Join(reasons, ", "), oldPID, oldTmux)
	}
	return reconciled
}

// adoptLiveWorkerRuntime adopts only the deterministic tmux identity belonging
// to slotName and only when its pane cwd matches the retained worktree exactly.
// A same-name foreign/unreadable pane is left untouched and is not evidence of
// ownership. No worker is started or stopped here, so retrying every cycle is
// idempotent and cannot create a duplicate.
func (o *Orchestrator) adoptLiveWorkerRuntime(slotName string, sess *state.Session, observedAt time.Time) bool {
	if sess == nil || strings.TrimSpace(sess.Worktree) == "" {
		return false
	}
	tmuxName := strings.TrimSpace(sess.TmuxSession)
	if tmuxName == "" {
		tmuxName = worker.TmuxSessionName(slotName)
	}
	if !o.tmuxSessionExists(tmuxName) {
		return false
	}
	pid, paneWorktree, err := o.tmuxPaneIdentity(tmuxName)
	if err != nil || pid <= 0 {
		log.Printf("[orch] reconcile: %s has live tmux %q but pane identity is unreadable (%v); refusing unsafe adoption", slotName, tmuxName, err)
		return false
	}
	if filepath.Clean(paneWorktree) != filepath.Clean(sess.Worktree) {
		log.Printf("[orch] reconcile: %s refusing foreign tmux %q: pane worktree %q != retained worktree %q", slotName, tmuxName, paneWorktree, sess.Worktree)
		return false
	}
	if !o.pidAlive(pid) {
		log.Printf("[orch] reconcile: %s tmux %q reported dead pane pid %d; refusing adoption", slotName, tmuxName, pid)
		return false
	}
	previous := sess.Status
	worker.AdoptLiveRuntime(o.cfg, sess, pid, tmuxName, observedAt)
	log.Printf("[orch] reconcile: %s adopted live worker runtime hidden behind status=%s (issue #%d, pid=%d, tmux=%q, worktree=%q) — no respawn, no duplicate",
		slotName, previous, sess.IssueNumber, pid, tmuxName, sess.Worktree)
	return true
}

// saveStatePreservingLiveRuntime closes the side-effect/state gap around worker
// launches. A normal three-way Save remains the fast path. If it conflicts on
// the same session after tmux already accepted a launch, recover only runtime
// snapshots whose exact deterministic tmux + pane cwd are still live, and
// overlay those snapshots onto the latest state under state.Update's lock.
//
// Other concurrent supervisor/watchdog fields stay authoritative. This does
// not retry a spawn and cannot create a process; it merely prevents a proven
// live worker from becoming invisible because its post-launch Save lost a CAS
// race. All other conflicting mutations remain fail-closed for the next cycle.
func (o *Orchestrator) saveStatePreservingLiveRuntime(s *state.State) error {
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		if !errors.Is(err, state.ErrStateConflict) {
			return err
		}
		originalErr := err
		live := make(map[string]state.Session)
		for slotName, sess := range s.Sessions {
			if sess == nil || sess.Status != state.StatusRunning || strings.TrimSpace(sess.Worktree) == "" {
				continue
			}
			tmuxName := strings.TrimSpace(sess.TmuxSession)
			if tmuxName == "" {
				tmuxName = worker.TmuxSessionName(slotName)
			}
			if !o.tmuxSessionExists(tmuxName) {
				continue
			}
			pid, paneWorktree, identityErr := o.tmuxPaneIdentity(tmuxName)
			if identityErr != nil || pid <= 0 || !o.pidAlive(pid) || filepath.Clean(paneWorktree) != filepath.Clean(sess.Worktree) {
				continue
			}
			copy := *sess
			copy.PID = pid
			copy.TmuxSession = tmuxName
			live[slotName] = copy
		}
		if len(live) == 0 {
			return originalErr
		}

		adopted := 0
		updateErr := state.Update(o.cfg.StateDir, func(latest *state.State) error {
			for slotName, runtime := range live {
				current, exists := latest.Sessions[slotName]
				if exists {
					if current == nil || current.IssueNumber != runtime.IssueNumber || strings.TrimSpace(current.Branch) != strings.TrimSpace(runtime.Branch) || filepath.Clean(current.Worktree) != filepath.Clean(runtime.Worktree) {
						continue
					}
					// A stop/reconcile that completed after this runtime began is
					// authoritative. The pre-lock liveness observation may have
					// raced that stop, so never resurrect a newer terminal state.
					if liveRuntimeSuperseded(current, &runtime) {
						continue
					}
					applyLiveRuntimeProjection(current, &runtime)
				} else {
					// A fresh spawn can be absent from the concurrently-written
					// snapshot. Only accept its deterministic slot worktree; never
					// attach an arbitrary tmux pane/path as a new session.
					expected := filepath.Join(o.cfg.WorktreeBase, slotName)
					if filepath.Clean(runtime.Worktree) != filepath.Clean(expected) {
						continue
					}
					copy := runtime
					latest.Sessions[slotName] = &copy
				}
				adopted++
			}
			if adopted == 0 {
				return state.ErrNoStateChange
			}
			return nil
		})
		if updateErr != nil {
			return fmt.Errorf("%w (live-runtime recovery: %v)", originalErr, updateErr)
		}
		if adopted == 0 {
			return originalErr
		}
		latest, loadErr := state.Load(o.cfg.StateDir)
		if loadErr != nil {
			return fmt.Errorf("reload after preserving %d live runtime(s): %w", adopted, loadErr)
		}
		*s = *latest
		log.Printf("[orch] state conflict recovered by preserving %d exact live worker runtime(s); concurrent supervisor/watchdog fields retained", adopted)
		return nil
	}
	return nil
}

func liveRuntimeSuperseded(current, runtime *state.Session) bool {
	if current == nil || runtime == nil {
		return true
	}
	if current.Status == state.StatusDone || current.Status == state.StatusCodeLanded {
		return true
	}
	if current.WorkerGeneration > runtime.WorkerGeneration {
		return true
	}
	if current.Status == state.StatusRunning && current.PID > 0 && current.PID != runtime.PID && !current.StartedAt.Before(runtime.StartedAt) {
		return true
	}
	if current.FinishedAt != nil && !current.FinishedAt.Before(runtime.StartedAt) {
		return true
	}
	if current.WorkerEndedAt != nil && !current.WorkerEndedAt.Before(runtime.StartedAt) {
		return true
	}
	return false
}

// applyLiveRuntimeProjection copies only fields owned by the external worker
// launch/attempt. Concurrent supervisor fields on the same session (review
// observations, retry accounting, PR identity, etc.) remain on current. This
// narrow overlay is the critical distinction between recovering process truth
// and replaying an entire stale orchestrator snapshot after a CAS conflict.
func applyLiveRuntimeProjection(current, runtime *state.Session) {
	if current == nil || runtime == nil {
		return
	}
	current.PID = runtime.PID
	current.TmuxSession = runtime.TmuxSession
	current.ProcessLeaseUnit = runtime.ProcessLeaseUnit
	current.ProcessLeaseManager = runtime.ProcessLeaseManager
	current.WorkerGeneration = runtime.WorkerGeneration
	current.LogFile = runtime.LogFile
	current.StartedAt = runtime.StartedAt
	current.FinishedAt = runtime.FinishedAt
	current.WorkerEndedAt = runtime.WorkerEndedAt
	current.Status = state.StatusRunning
	current.Backend = runtime.Backend
	current.Model = runtime.Model
	current.CostUSDBackend = runtime.CostUSDBackend
	current.UsageTokensWatermark = runtime.UsageTokensWatermark
	current.UsageStreamCursors = cloneUsageStreamCursors(runtime.UsageStreamCursors)
	current.TokensUsedAttempt = runtime.TokensUsedAttempt
	current.TokenBudgetTokensWatermark = runtime.TokenBudgetTokensWatermark
	current.TokenBudgetTokensAttempt = runtime.TokenBudgetTokensAttempt
	current.TokenBudgetMeasure = runtime.TokenBudgetMeasure
	current.WorkerOutcome = runtime.WorkerOutcome
	current.WorkerLeaseID = runtime.WorkerLeaseID
	current.WorkerLeaseUnit = runtime.WorkerLeaseUnit
	current.WorkerLeaseScope = runtime.WorkerLeaseScope
	current.WorkerScratchDir = runtime.WorkerScratchDir
	current.WorkerLeaseManifest = runtime.WorkerLeaseManifest
	current.WorkerLeaseAttention = runtime.WorkerLeaseAttention
	current.Attribution = append([]state.BackendAttribution(nil), runtime.Attribution...)
	current.NextRetryAt = runtime.NextRetryAt
	current.RestartCheckpointAt = runtime.RestartCheckpointAt
	current.LastOutputHash = runtime.LastOutputHash
	current.LastOutputChangedAt = runtime.LastOutputChangedAt
	current.LastNotifiedStatus = runtime.LastNotifiedStatus
	current.NotifiedCIFail = runtime.NotifiedCIFail
	if runtime.BackendSelection != nil {
		selection := *runtime.BackendSelection
		current.BackendSelection = &selection
	}
}

// reconcilePushedBranch handles a stale running session whose worker died
// after pushing its branch. Outcomes, in order (#800):
//
//   - branch already merged via an earlier PR → settle the session as
//     code_landed. Falling through to dead instead would strand it outside
//     IssueInProgress: maestro PRs carry no auto-closing keywords, so the
//     issue is still open and the HasMergedPRForIssue dispatch guard cannot
//     see a branch-only merge — the next cycle would spawn a fresh worker
//     for already-landed work.
//   - issue closed → settle as done (same transition retireStaleRetry uses).
//   - otherwise → auto-create a PR and flip the session to pr_open, rescuing
//     the worker's pushed result.
//
// Returns true when the session was transitioned; false leaves the caller's
// dead-marking fall-through in charge. GitHub read errors on the staleness
// checks fail open — the auto-create rescue proceeds — so a transient API
// hiccup cannot disable the rescue path.
func (o *Orchestrator) reconcilePushedBranch(s *state.State, slotName string, sess *state.Session, reasons string) bool {
	branch := strings.TrimSpace(sess.Branch)
	if branch == "" {
		return false
	}
	exists, err := o.remoteBranchExists(branch)
	if err != nil {
		log.Printf("[orch] reconcile: could not check remote branch %q for %s: %v", branch, slotName, err)
		return false
	}
	if !exists {
		return false
	}

	// #800: a branch can outlive its squash-merged PR (an operator merge
	// without branch deletion leaves the tip un-ancestored but the content
	// landed). Auto-creating a PR from it produces a junk PR that only exists
	// to be closed.
	if mergedPR, mergeErr := o.mergedPRForBranch(branch); mergeErr != nil {
		log.Printf("[orch] reconcile: could not check merged PRs for branch %q (%s): %v — proceeding with auto-create", branch, slotName, mergeErr)
	} else if mergedPR > 0 {
		o.updateTokensUsedFromWorkerLog(slotName, sess)
		sess.PID = 0
		sess.TmuxSession = ""
		o.markCodeLanded(sess, mergedPR)
		log.Printf("[orch] reconcile: %s running->code_landed (branch %q already merged via PR #%d; %s)",
			slotName, branch, mergedPR, reasons)
		if o.notifier != nil {
			o.notifier.Sendf("🧟 maestro: worker %s (issue #%d: %s) exited, but branch %s already merged via PR #%d — settled as code_landed",
				slotName, sess.IssueNumber, sess.IssueTitle, branch, mergedPR)
		}
		return true
	}
	if closed, closedErr := o.isIssueClosed(sess.IssueNumber); closedErr != nil {
		log.Printf("[orch] reconcile: could not check issue #%d state for %s: %v — proceeding with auto-create", sess.IssueNumber, slotName, closedErr)
	} else if closed {
		if !o.reconcileClosedIssueSession(s, slotName, sess, time.Now().UTC()) {
			log.Printf("[orch] reconcile: %s issue-closed teardown deferred; not auto-creating a PR", slotName)
			return true
		}
		log.Printf("[orch] reconcile: %s running->done (issue #%d closed; not auto-creating a PR; %s)",
			slotName, sess.IssueNumber, reasons)
		if o.notifier != nil {
			o.notifier.Sendf("🧟 maestro: worker %s (issue #%d: %s) exited with the issue already closed — settled as done",
				slotName, sess.IssueNumber, sess.IssueTitle)
		}
		return true
	}

	title := autoCreatedPRTitle(sess)
	body := autoCreatedPRBody(sess, branch)
	prNumber, err := o.createPR(title, body, "main", branch)
	if err != nil {
		log.Printf("[orch] reconcile: could not auto-create PR for %s branch %q: %v", slotName, branch, err)
		return false
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
	log.Printf("[orch] reconcile: %s running->pr_open (auto-created PR #%d for pushed branch %q; %s)",
		slotName, prNumber, branch, reasons)
	if o.notifier != nil {
		o.notifier.Sendf("🔀 maestro: worker %s pushed branch %s and exited before opening a PR; auto-created PR #%d for issue #%d (%s)",
			slotName, branch, prNumber, sess.IssueNumber, sess.IssueTitle)
	}
	return true
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

// autoCreatedPRBody renders the public PR body for the reconcile auto-create
// path: issue ref, branch, and a neutral one-liner only. Backend attribution,
// pids, tmux session names, and host paths must never appear here — the PR
// body lands on the target repo, which may be public (#799). Observed-worker-
// state diagnostics stay in the orchestrator log at the call site.
func autoCreatedPRBody(sess *state.Session, branch string) string {
	return fmt.Sprintf(`Refs #%d

Maestro auto-created this pull request for pushed branch %s because no open pull request was found for it.
`, sess.IssueNumber, branch)
}

// terminalReconcileDue bounds authoritative forge polling for historical
// sessions. A successful check remains valid for at most the hands-off stall
// SLA; failures are not stamped and therefore retry on the next cycle. The
// timestamp is durable so restarting the single daemon does not fan every old
// issue/PR back out into hundreds of gh subprocesses at once (#940).
func terminalReconcileDue(sess *state.Session, now time.Time) bool {
	if sess == nil || sess.LastTerminalReconcileAt == nil {
		return true
	}
	if sess.StartedAt.After(*sess.LastTerminalReconcileAt) ||
		(sess.FinishedAt != nil && sess.FinishedAt.After(*sess.LastTerminalReconcileAt)) {
		return true
	}
	return now.Sub(*sess.LastTerminalReconcileAt) >= terminalReconcileInterval
}

// reconcileClosedIssueSession applies GitHub's authoritative terminal issue
// lifecycle without consulting delivery or runtime-outcome gates. Those gates
// remain project-level facts (and their history/approvals remain durable), but
// they must never keep a closed issue actionable or revive worker controls.
func (o *Orchestrator) reconcileClosedIssueSession(s *state.State, slotName string, sess *state.Session, now time.Time) bool {
	if s == nil || sess == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	previous := sess.Status
	if sess.PRNumber <= 0 {
		if prNumber, err := o.mergedPRForDoneLikeSession(sess); err != nil {
			log.Printf("[orch] issue #%d is closed; merged PR audit identity could not be refreshed: %v", sess.IssueNumber, err)
		} else if prNumber > 0 {
			sess.PRNumber = prNumber
		}
	}
	if sess.PRNumber > 0 && !sess.PRMerged {
		switch previous {
		case state.StatusCodeLanded:
			// code_landed is reached only from authoritative merge evidence.
			// Preserve that fact before replacing the lifecycle status with done.
			sess.PRMerged = true
		default:
			// A manually closed issue may still have an open PR. Query the exact
			// PR when the read path is available and retain the result so Fleet can
			// distinguish a merged historical PR from an orphaned open PR.
			if o.isPRMergedFn != nil || o.readSource != nil || o.gh != nil {
				if merged, err := o.isPRMerged(sess.PRNumber); err != nil {
					log.Printf("[orch] issue #%d is closed; PR #%d merge state could not be refreshed: %v", sess.IssueNumber, sess.PRNumber, err)
				} else {
					sess.PRMerged = merged
				}
			}
		}
	}
	if sess.Status == state.StatusRunning || sess.Status == state.StatusPROpen || sess.Status == state.StatusQueued || sess.PID > 0 || strings.TrimSpace(sess.TmuxSession) != "" || strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
		o.updateTokensUsedFromWorkerLog(slotName, sess)
		if err := o.stopWorker(slotName, sess); err != nil {
			log.Printf("[orch] issue-closed teardown deferred for %s: %v", slotName, err)
			// Any teardown error is ambiguous. Legacy PID/tmux workers have no
			// process-lease receipt to prove survival, and an injected/controller
			// stop can mutate receipts before returning an error. Preserve the
			// nonterminal lifecycle and retry instead of releasing a possibly-live
			// worker merely because its receipt fields are now empty.
			return false
		}
	}
	sess.Status = state.StatusDone
	sess.PID = 0
	sess.TmuxSession = ""
	sess.NextRetryAt = nil
	sess.RetryHoldReason = ""
	sess.RestartCheckpointAt = nil
	sess.ReleasedForRedispatch = true
	sess.IssueClosedAt = &now
	sess.LastTerminalReconcileAt = &now
	if sess.FinishedAt == nil {
		sess.FinishedAt = &now
	}
	state.MarkWorkerEnded(sess, now)
	if o.cfg != nil {
		o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
	}

	reason := fmt.Sprintf("issue #%d is closed on GitHub — issue-scoped approval is moot", sess.IssueNumber)
	for _, approval := range s.StaleIssueApprovalsForClosedIssue(sess.IssueNumber, now, reason) {
		log.Printf("[orch] reconciled moot approval %s after issue #%d closed", approval.ID, sess.IssueNumber)
	}
	if o.cfg != nil && o.approvalsBinding.UseSQLite() {
		binding := o.approvalsBinding
		binding.StateDir = o.cfg.StateDir
		binding.Repo = o.repo
		binding.Project = o.repo
		for _, approval := range s.MootIssueApprovalMirrorCandidates(sess.IssueNumber) {
			if err := approvalstore.ReconcileMoot(binding, approval.ID, now, reason); err != nil {
				log.Printf("[orch] approval %s closed-issue mirror to SQLite failed (JSON will retry on later reconciliation): %v", approval.ID, err)
			}
		}
	}
	log.Printf("[orch] issue #%d closed, transitioning session %s from %s to done; merged PR/deployment history retained", sess.IssueNumber, slotName, previous)
	return true
}

// checkSessions inspects all sessions and updates their status
func (o *Orchestrator) checkSessions(s *state.State) {
	// Fetch open PRs once for the whole check cycle (shared per-cycle, #794)
	prs, prErr := o.listOpenPRsForCycle()
	branchToPR := make(map[string]github.PR)
	if prErr != nil {
		log.Printf("[orch] list PRs (check): %v", prErr)
	} else {
		for _, pr := range prs {
			branchToPR[pr.HeadRefName] = pr
		}
	}

	for slotName, sess := range s.Sessions {
		// GitHub issue closure is terminal external truth for every nonterminal
		// session status, including code_landed. Check it before PR, delivery,
		// outcome, retry, or worker reconciliation so none of those independent
		// facts can keep the issue lifecycle artificially open.
		if !state.IsTerminal(sess.Status) {
			closed, err := o.isIssueClosed(sess.IssueNumber)
			if err != nil {
				log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
			} else if closed {
				o.reconcileClosedIssueSession(s, slotName, sess, time.Now().UTC())
				continue
			}
		}
		// NextRetryAt is meaningful only while a dead session is queued for an
		// in-place respawn. A pr_open session already owns its canonical PR and
		// has no pending process start; retaining an expired retry marker there
		// makes Fleet report a contradictory "open PR + overdue retry" state and
		// can survive daemon restarts indefinitely. Normalize legacy/racy state
		// before any remote reconciliation so the durable store self-heals.
		if sess.Status == state.StatusPROpen && sess.NextRetryAt != nil {
			log.Printf("[orch] clearing stale retry marker for pr_open session %s / PR #%d", slotName, sess.PRNumber)
			sess.NextRetryAt = nil
		}
		switch sess.Status {
		case state.StatusDone, state.StatusCodeLanded, state.StatusDead, state.StatusConflictFailed, state.StatusFailed, state.StatusRetryExhausted:
			now := time.Now().UTC()
			// #1106: aged token_budget_exceeded must not hold a permanent issue
			// claim that freezes redispatch / supervisor attention forever.
			// After grace, release for redispatch while keeping history.
			o.releaseAgedTokenBudgetClaim(s, sess, now)
			remoteReconcileDue := terminalReconcileDue(sess, now)
			remoteReconcileSucceeded := false
			remoteReconcileFailed := false
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
			// A completed PR keeps a terminal-reconciliation issue lease while the
			// linked issue is still open, closing the merge->close redispatch gap.
			// Once the forge proves the issue closed, release that lease so Fleet
			// diagnostics do not retain a permanent active claim. A later explicit
			// issue reopen is then eligible by design.
			if remoteReconcileDue && sess.Status == state.StatusDone && sess.PRNumber > 0 && sess.FinishedAt != nil && (!sess.ReleasedForRedispatch || sess.IssueClosedAt == nil) {
				closed, err := o.isIssueClosed(sess.IssueNumber)
				if err != nil {
					remoteReconcileFailed = true
					log.Printf("[orch] terminal claim check for issue #%d / PR #%d: %v", sess.IssueNumber, sess.PRNumber, err)
				} else {
					remoteReconcileSucceeded = true
					if closed {
						if !o.reconcileClosedIssueSession(s, slotName, sess, now) {
							remoteReconcileFailed = true
						}
					} else if !sess.ReleasedForRedispatch {
						// #1103: PR finished but the GitHub issue is still open.
						// Holding terminal_reconcile forever makes the issue look
						// "in progress" with zero live workers and starves the
						// ready queue. After a short grace for close-issue to land,
						// release so hands-off fill can redispatch or move on.
						if now.Sub(sess.FinishedAt.UTC()) >= terminalReconcileGrace {
							merged, mErr := o.isPRMerged(sess.PRNumber)
							if mErr != nil {
								remoteReconcileFailed = true
								log.Printf("[orch] terminal claim merge check for PR #%d: %v", sess.PRNumber, mErr)
							} else if merged {
								o.releaseDoneMergedOpenIssueForRedispatch(s, sess,
									fmt.Sprintf("issue #%d still open after merged PR #%d past %s grace — released for redispatch",
										sess.IssueNumber, sess.PRNumber, terminalReconcileGrace))
							}
						}
					}
				}
			}
			// Zombie cleanup: if the underlying issue is closed, transition to done.
			// This prevents conflict_failed/failed/dead/retry_exhausted sessions from lingering
			// indefinitely when their issues are closed externally (#187).
			if remoteReconcileDue && sess.Status != state.StatusDone && sess.Status != state.StatusCodeLanded {
				done := false
				closed, err := o.isIssueClosed(sess.IssueNumber)
				if err != nil {
					remoteReconcileFailed = true
					log.Printf("[orch] check issue #%d: %v", sess.IssueNumber, err)
				} else {
					remoteReconcileSucceeded = true
					if closed {
						done = true
					}
				}
				if done {
					if !o.reconcileClosedIssueSession(s, slotName, sess, time.Now().UTC()) {
						remoteReconcileFailed = true
					}
				} else if sess.Status != state.StatusCodeLanded && sess.PRNumber > 0 {
					merged, err := o.isPRMerged(sess.PRNumber)
					if err != nil {
						remoteReconcileFailed = true
						log.Printf("[orch] check PR #%d merged: %v", sess.PRNumber, err)
					} else {
						remoteReconcileSucceeded = true
						if merged {
							log.Printf("[orch] PR #%d merged, transitioning zombie session %s from %s to code_landed", sess.PRNumber, slotName, sess.Status)
							o.markCodeLanded(sess, sess.PRNumber)
						}
					}
				}
			}
			if remoteReconcileSucceeded && !remoteReconcileFailed {
				checkedAt := time.Now().UTC()
				sess.LastTerminalReconcileAt = &checkedAt
			}
			// Terminal states — cleanup old worktrees after 1h
			// Use StartedAt as fallback when FinishedAt is nil (orphaned sessions)
			// to preserve the grace period for recently-killed workers.
			nilAndOld := sess.FinishedAt == nil && !sess.StartedAt.IsZero() && time.Since(sess.StartedAt) > 1*time.Hour
			finishedAndOld := sess.FinishedAt != nil && time.Since(*sess.FinishedAt) > 1*time.Hour
			if sess.Worktree != "" && (nilAndOld || finishedAndOld) {
				if _, err := os.Stat(sess.Worktree); err == nil {
					lease := worker.CaptureCleanupLease(slotName, sess)
					if o.beforeWorktreeCleanupFn != nil {
						o.beforeWorktreeCleanupFn(slotName)
					}
					if sess.FinishedAt != nil {
						log.Printf("[orch] cleaning up stale worktree for %s (finished %s ago)", slotName, time.Since(*sess.FinishedAt).Round(time.Minute))
					} else {
						log.Printf("[orch] cleaning up orphaned worktree for %s (started %s ago, no finishedAt)", slotName, time.Since(sess.StartedAt).Round(time.Minute))
					}
					if cleanupErr := o.cleanupLeasedWorktree(s, lease); cleanupErr != nil {
						if errors.Is(cleanupErr, worker.ErrCleanupConsistencyViolation) {
							log.Printf("[orch] P0 consistency violation during stale-worktree cleanup for %s: %v", slotName, cleanupErr)
						} else {
							log.Printf("[orch] aborting stale-worktree cleanup for %s before filesystem mutation: %v", slotName, cleanupErr)
						}
					}
				}
			}
			continue
		}

		// Running-session lifecycle and process reconciliation. Closed issues
		// were handled by the authoritative check at the top of this loop.
		if sess.Status == state.StatusRunning {
			if _, markerOK := worker.ReadTokenBudgetMarkerForAttempt(sess.LogFile, sess.WorkerGeneration, sess.StartedAt); markerOK {
				if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
					if err := o.stopWorkerProcess(slotName, sess); err != nil {
						log.Printf("[orch] token-budget process lease cleanup deferred for %s: %v", slotName, err)
						continue
					}
				}
				o.runAfterRunHook(sess)
				o.terminalizeTokenBudgetIfExceeded(slotName, sess, time.Now().UTC())
				if sess.Phase == state.PhaseAdvisor {
					o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "token_budget_exceeded", fmt.Sprintf("Advisor reached its configured token budget before producing %s.", pipeline.AdvisorReviewFile))
				}
				continue
			}
			// Check if process is still alive
			if sess.PID > 0 && !o.pidAlive(sess.PID) {
				if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
					if err := o.stopWorkerProcess(slotName, sess); err != nil {
						log.Printf("[orch] dead worker process lease cleanup deferred for %s: %v", slotName, err)
						continue
					}
				}
				if sess.Phase == state.PhaseAdvisor {
					if pr, found := branchToPR[sess.Branch]; found {
						o.runAfterRunHook(sess)
						o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "advisor_pr_created", fmt.Sprintf("Advisor created or exposed PR #%d before implementation.", pr.Number))
						continue
					}
					if o.handleAdvisorBackendDeath(s, slotName, sess) {
						continue
					}
				}
				// Pipeline phase transition: if this is a pipeline session,
				// try to advance to the next phase before falling through
				// to normal dead-worker handling.
				if sess.Phase != state.PhaseNone && o.advancePipeline(s, slotName, sess) {
					continue
				}

				// Worker process died — run after_run hook
				o.runAfterRunHook(sess)
				o.updateTokensUsedFromWorkerLog(slotName, sess)
				if o.terminalizeTokenBudgetIfExceeded(slotName, sess, time.Now().UTC()) {
					if sess.Phase == state.PhaseAdvisor {
						o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "token_budget_exceeded", fmt.Sprintf("Advisor reached its configured token budget before producing %s.", pipeline.AdvisorReviewFile))
					}
					continue
				}

				// Check if there's an open PR for this branch BEFORE marking dead
				if pr, found := branchToPR[sess.Branch]; found {
					o.updateTokensUsedFromWorkerLog(slotName, sess)
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
					if err := o.respawnPreservingWorktree(slotName, sess, issue, promptBase, nextBackend); err != nil {
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
				} else if failure, hit := o.classifyBackendFailure(sess, time.Now().UTC()); hit {
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
					cp := backendFailureCopyFor(failure.reason)
					o.updateTokensUsedFromWorkerLog(slotName, sess)
					o.recordBackendFailure(s, slotName, sess, failure, now)
					selection := o.selectBackendFallback(s, sess, now, cp.selectionReason)
					sess.BackendSelection = &selection
					nextBackend := selection.SelectedBackend
					if nextBackend == "" {
						log.Printf("[orch] worker %s (backend=%s) %s (%s); no fallback backend available", slotName, sess.Backend, cp.noun, failure.pattern)
						sess.Status = state.StatusDead
						sess.LastNotifiedStatus = cp.displayToken
						sess.FinishedAt = &now
						state.MarkWorkerEnded(sess, now)
						o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) backend %s %s (%s); no fallback backend available — %s",
							slotName, sess.IssueNumber, sess.IssueTitle, sess.Backend, cp.desc, failure.pattern, cp.remedy)
						continue
					}
					log.Printf("[orch] worker %s (backend=%s) %s (%s), falling back to %s — retry budget preserved",
						slotName, sess.Backend, cp.desc, failure.pattern, nextBackend)

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
					if err := o.respawnPreservingWorktree(slotName, sess, issue, promptBase, nextBackend); err != nil {
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
						slotName, sess.IssueNumber, previousBackend, cp.desc, failure.pattern, nextBackend)
				} else if sess.UnexpectedExitRetries >= maxAutomaticUnexpectedExitRetries {
					o.markRepeatedUnexpectedExit(slotName, sess, time.Now().UTC())
				} else if o.canRetryIssue(s, sess) {
					// Schedule retry with exponential backoff (respects max_retries_per_issue)
					o.updateTokensUsedFromWorkerLog(slotName, sess)
					sess.RetryCount++
					sess.UnexpectedExitRetries++
					sess.RetryReason = state.RetryReasonStalledProgress
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
				if sess.Phase == state.PhaseAdvisor {
					o.runAfterRunHook(sess)
					if err := o.stopWorker(slotName, sess); err != nil {
						log.Printf("[pipeline] stop Advisor %s after premature PR #%d: %v", slotName, pr.Number, err)
					}
					o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "advisor_pr_created", fmt.Sprintf("Advisor created or exposed PR #%d before implementation.", pr.Number))
					continue
				}
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
						classifyHit, pattern, confidence, resetTime := worker.ClassifyRateLimitAt(output, time.Now().UTC())
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
								if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
									continue
								}
							}
							now := time.Now().UTC()
							resetAt := &resetTime
							o.recordProviderLimit(s, slotName, sess, pattern, resetAt, now)
							if sess.Phase == state.PhaseAdvisor {
								o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "backend_unavailable", fmt.Sprintf("Required Advisor backend %s hit a provider limit (%s).", sess.Backend, pattern))
								continue
							}
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
								if respawnErr := o.respawnPreservingWorktree(slotName, sess, issue, promptBase, fallback); respawnErr != nil {
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
					budgetTokens, budgetMeasure := tokenBudgetObservation(sess)
					if sess.Phase != state.PhaseAdvisor && o.cfg.WorkerMaxTokens > 0 && o.cfg.SoftTokenThreshold() > 0 && sess.CheckpointFile == "" {
						softLimit := int(float64(o.cfg.WorkerMaxTokens) * o.cfg.SoftTokenThreshold())
						if budgetTokens >= softLimit {
							log.Printf("[orch] worker %s hit soft token threshold (%d >= %d, measure=%s), checkpointing",
								slotName, budgetTokens, softLimit, budgetMeasure)

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
									// #792: re-apply this policy session's tier effort/model
									// override so the checkpoint respawn resumes on the
									// selected tier instead of the base backend def (no-op
									// for non-policy/shadow sessions — base o.cfg unchanged).
									respawnCfg := o.tierOverrideConfigForSession(sess)
									if respawnErr := o.respawnInPlaceWithConfig(respawnCfg, slotName, sess, issue, promptBase, sess.Backend); respawnErr != nil {
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
					if o.cfg.WorkerMaxTokens > 0 && budgetTokens >= o.cfg.WorkerMaxTokens && sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
						log.Printf("[orch] worker %s reached token limit (%d >= %d, measure=%s), killing",
							slotName, budgetTokens, o.cfg.WorkerMaxTokens, budgetMeasure)
						o.runAfterRunHook(sess)
						if err := o.stopWorker(slotName, sess); err != nil {
							log.Printf("[orch] warn: could not stop token-limit worker %s: %v", slotName, err)
							if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
								continue
							}
						}
						now := time.Now().UTC()
						o.markTokenBudgetExceeded(slotName, sess, worker.TokenBudgetMarker{
							Outcome:        worker.TokenBudgetExceededOutcome,
							Backend:        sess.Backend,
							TokensObserved: budgetTokens,
							MaxTokens:      o.cfg.WorkerMaxTokens,
							Measure:        budgetMeasure,
							MeasuredAt:     now,
						}, now)
						if sess.Phase == state.PhaseAdvisor {
							o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "token_budget_exceeded", fmt.Sprintf("Advisor reached its configured token budget before producing %s.", pipeline.AdvisorReviewFile))
						}
						continue
					}

					// --- Silent worker detection ---
					if timeout := o.cfg.EffectiveWorkerSilentTimeout(); timeout > 0 {
						hash := hashOutput(output)
						now := time.Now().UTC()

						if sess.LastOutputHash == "" || sess.LastOutputChangedAt.IsZero() || hash != sess.LastOutputHash {
							sess.LastOutputHash = hash
							sess.LastOutputChangedAt = now
						} else {
							if time.Since(sess.LastOutputChangedAt) > timeout {
								timeoutMinutes := int(timeout / time.Minute)
								log.Printf("[orch] worker %s silent for >%dm, killing", slotName, timeoutMinutes)
								o.runAfterRunHook(sess)
								if err := o.stopWorker(slotName, sess); err != nil {
									log.Printf("[orch] warn: could not stop silent worker %s: %v", slotName, err)
									if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
										continue
									}
								}

								// Count previous silent-timeout kills before updating this session,
								// so the current kill is not included in the count.
								prevSilentKills := countSilentTimeoutKillsForIssue(s, sess.IssueNumber)

								if sess.Phase == state.PhaseAdvisor {
									o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "silent_timeout", sess.AdvisorUnresolvedFindings)
									continue
								} else {
									sess.Status = state.StatusDead
									sess.LastNotifiedStatus = "silent_timeout"
									sess.FinishedAt = &now
									state.MarkWorkerEnded(sess, now)
								}

								if prevSilentKills > 0 {
									// auto-label blocked disabled
								}

								o.notifier.Sendf("⏱️ maestro: worker %s (issue #%d) killed — no output for %d minutes",
									slotName, sess.IssueNumber, timeoutMinutes)
								continue
							}
						}
					}
				}
			}

			// Check if worker exceeded max runtime — hard fail (no retry) with diagnostics
			maxMinutes := pipeline.MaxRuntimeForPhase(o.pipelineConfigForSession(sess), sess.Phase)
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
					if strings.TrimSpace(sess.ProcessLeaseUnit) != "" {
						continue
					}
				}
				if sess.Phase == state.PhaseAdvisor {
					o.finishAdvisorGate(o.pipelineConfigForSession(sess), slotName, sess, pipeline.AdvisorVerdictInvalid, "timeout", sess.AdvisorUnresolvedFindings)
				} else {
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
}

func (o *Orchestrator) cleanupLeasedWorktree(s *state.State, lease worker.WorktreeCleanupLease) error {
	remove := o.removeWorktreeFn
	if remove == nil {
		remove = worker.RemoveWorktree
	}
	return worker.CleanupLeasedWorktree(
		o.cfg,
		s,
		lease,
		worker.CleanupProbes{PIDAlive: o.pidAlive, TmuxAlive: o.tmuxSessionExists},
		worker.CleanupPolicy{RequireTerminal: true, RequireClean: true},
		worker.CleanupHooks{
			BeforeRemove: func() error {
				return worker.RunHook(o.cfg, "before_remove", o.cfg.Hooks.BeforeRemove, worker.HookEnv{
					IssueID:       fmt.Sprintf("%s#%d", o.cfg.Repo, lease.IssueNumber),
					IssueNumber:   lease.IssueNumber,
					WorkspacePath: lease.Worktree,
				})
			},
			Remove: remove,
		},
	)
}

// autoMergePRs checks open PRs and merges ones with green CI
func (o *Orchestrator) autoMergePRs(s *state.State) {
	prs, err := o.listOpenPRsForCycle()
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

	// A session previously marked done from a disappearing branch can
	// contradict the authoritative open-PR snapshot (live #949: a closed
	// duplicate PR hid an older canonical draft). Rebind only when exactly one
	// open PR references the issue. Multiple candidates are ambiguous and must
	// remain untouched; choosing one would manufacture a new canonical identity.
	o.reconcileTerminalSessionsWithOpenPRs(s, prs)
	// One open PR is one maintenance/merge lifecycle even when a continuation
	// issue and its historical source issue both retain exact session records.
	// Gate side effects (CI retry accounting, review repair, rebase, merge) must
	// run through one deterministic owner. Without this, a red head can be
	// observed twice: the current continuation safely defers while its branch is
	// moving, then an older session consumes retry budget and becomes
	// retry_exhausted for the same PR (OK Player #388 / issues #345 and #406).
	mergeOwners := canonicalMergeFlowOwners(s, branchToPR, numberToPR)

	type mergeCandidate struct {
		slotName string
		sess     *state.Session
		pr       github.PR
		headSHA  string
		// missingReviewFor is non-zero when this candidate bypassed a silent
		// review gate. The operator alert fires only after the merge actually
		// succeeds — a candidate can still be deferred by the merge interval,
		// dropped by conflict filtering, or fail to merge.
		missingReviewFor time.Duration
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
					o.reconcileNoPRRetryExhausted(s, slotName, sess, prs)
					continue
				}
				// #818: retry_exhausted session records a PR, but no open PR
				// was found. The PR was either merged (→ close issue) or closed
				// without merge (→ label blocked so the queue advances).
				// Previously this logged "waiting for reconciliation" forever,
				// holding the issue slot and stalling the dynamic-wave queue.
				o.reconcileClosedPRRetryExhausted(s, slotName, sess)
				continue
			}
			merged := false
			var mergedErr error
			if sess.PRNumber > 0 {
				merged, mergedErr = o.isPRMerged(sess.PRNumber)
			} else {
				merged, mergedErr = o.hasMergedPRForIssue(sess.IssueNumber)
			}
			if mergedErr != nil {
				log.Printf("[orch] no open PR found for branch %s (slot %s), but merge state is unavailable: %v — preserving session for retry", sess.Branch, slotName, mergedErr)
				continue
			}
			closed := false
			if !merged {
				var closedErr error
				closed, closedErr = o.isIssueClosed(sess.IssueNumber)
				if closedErr != nil {
					log.Printf("[orch] no open PR found for branch %s (slot %s), but issue state is unavailable: %v — preserving session for retry", sess.Branch, slotName, closedErr)
					continue
				}
				if !closed {
					o.releaseClosedUnmergedSession(sess, slotName)
					continue
				}
			}
			if closed {
				o.reconcileClosedIssueSession(s, slotName, sess, time.Now().UTC())
				continue
			}
			log.Printf("[orch] no open PR found for branch %s (slot %s) — authoritative merge outcome confirmed", sess.Branch, slotName)
			if !o.canMarkDoneForOutcome(s, sess, fmt.Sprintf("PR for branch %s is no longer open", sess.Branch)) {
				if sess.Status != state.StatusCodeLanded && sess.PRNumber > 0 {
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
		if owner := mergeOwners[pr.Number]; owner != "" && owner != slotName {
			log.Printf("[orch] PR #%d lifecycle is owned by canonical session %s; skipping historical session %s", pr.Number, owner, slotName)
			continue
		}

		if sess.PRNumber == 0 {
			sess.PRNumber = pr.Number
		}
		// #705: opt-in visual evidence for UI-affecting PRs. One-shot,
		// advisory — posts a warning comment when evidence is missing but
		// never blocks or delays the merge flow below.
		o.ensureVisualEvidence(slotName, sess, pr)

		// Check the actual current head plus its aggregate and per-check rollup.
		// The normalized rollup remains the merge gate; the durable snapshot below
		// additionally hashes individual check transitions (#887).
		ciRollup, ciStatus, gateTransition, gateObservable, observedAt, err := o.observePRGateCI(sess, pr)
		if err != nil {
			log.Printf("[orch] CI status for PR #%d: %v", pr.Number, err)
			continue
		}
		persistGate := func() {
			if gateObservable {
				o.persistPRGateTransition(s, gateTransition, observedAt)
			}
		}
		if hold, ok := o.operatorGateHoldFromLabels(sess.IssueNumber, pr.Number); ok {
			persistGate()
			o.applyOperatorGateHold(sess, pr, hold)
			continue
		}

		switch ciStatus {
		case "success":
			// Reset CI-failure notification state when CI goes green. Keep
			// review retry-exhausted markers so actionable feedback does not
			// re-notify on every orchestration cycle.
			if sess.LastNotifiedStatus == "ci_failure" || sess.LastNotifiedStatus == "ci_retry_exhausted" || sess.LastNotifiedStatus == operatorGateStatus {
				sess.LastNotifiedStatus = ""
			}
			clearOperatorGateHold(sess)
			sess.NotifiedCIFail = false // backward compat

			// The configured review gate is authoritative. In particular, a
			// successful Greptile check means "ok to merge" (4/5 or 5/5) even
			// when the review left advisory inline findings. The old ordering
			// collected P1 comments first and scheduled a repair before reading
			// the successful gate, producing the contradictory
			// greptile_not_approved/retry_exhausted state seen on OK Player #361.
			// Read the verdict first and only feed comments to a repair worker
			// when the configured gate itself has not passed.
			var currentReviewVerdict *github.ReviewGateVerdict
			if o.reviewGate() != "none" {
				verdict, reviewErr := o.prReviewGateVerdict(pr.Number)
				if reviewErr == nil {
					currentReviewVerdict = &verdict
				}
			}

			if o.cfg.AutoRetryReviewFeedback &&
				(currentReviewVerdict == nil || currentReviewVerdict.Pending || !currentReviewVerdict.Passed) {
				reviewFeedback, err := o.collectPRReviewFeedback(pr.Number)
				if err != nil {
					log.Printf("[orch] warn: could not collect review feedback for PR #%d: %v", pr.Number, err)
				} else if strings.TrimSpace(reviewFeedback) != "" {
					if gateObservable && !o.prGateHeadMatches(pr.Number, gateTransition.HeadSHA) {
						persistGate()
						continue
					}
					// Preserve the provider's real score/verdict alongside the
					// actionable feedback fingerprint. Otherwise the early repair
					// branch records only a generic blocked decision and Fleet loses
					// the concrete Greptile 3/5 fact that caused the retry.
					if currentReviewVerdict != nil {
						addPRGateReviewVerdict(&gateTransition, *currentReviewVerdict)
					}
					addPRGateLateFeedback(&gateTransition, reviewFeedback)
					persistGate()
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
								ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr, headSHA: gateTransition.HeadSHA})
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
				addPRGateReviewDisabled(&gateTransition)
				persistGate()
				ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr, headSHA: gateTransition.HeadSHA})
				continue
			}

			var reviewVerdict github.ReviewGateVerdict
			if currentReviewVerdict != nil {
				reviewVerdict = *currentReviewVerdict
			} else {
				var err error
				reviewVerdict, err = o.prReviewGateVerdict(pr.Number)
				if err != nil {
					log.Printf("[orch] review gate check PR #%d: %v", pr.Number, err)
					persistGate()
					continue // skip this cycle, try next
				}
			}
			if gateObservable && !o.prGateHeadMatches(pr.Number, gateTransition.HeadSHA) {
				persistGate()
				continue
			}
			addPRGateReviewVerdict(&gateTransition, reviewVerdict)
			persistGate()
			// Record what the gate said for this head BEFORE branching: a
			// settled rejection is evidence the reviewer is alive just as much
			// as a pending one, and only a head change may erase it.
			now := time.Now().UTC()
			o.trackReviewGateHead(sess, o.reviewClockHead(pr, gateTransition.HeadSHA), reviewVerdict, now)
			if reviewVerdict.Pending {
				o.maybeRetriggerStalePendingReview(sess, pr, reviewVerdict)
				// #1162 S5: an expected llm-review-* stream with no signal on
				// this head is ours to produce, not to wait for.
				o.maybeProduceMissingReview(pr.Number, gateTransition.HeadSHA, reviewVerdict)
				// A gate that never produced any signal is not "reviewing" —
				// it is absent. Past the configured grace the PR proceeds on
				// its own merits (CI is already green here) instead of waiting
				// forever on a reviewer that is not coming.
				if silentFor, missing := o.missingReviewGateElapsed(sess, reviewVerdict, now); missing {
					log.Printf("[orch] PR #%d: review gate produced no signal for %s — treating the missing review as non-blocking (review_retrigger.missing_after_minutes)",
						pr.Number, silentFor.Round(time.Minute))
					// Deliberately keep the per-head tracking: sequential mode
					// may defer this candidate, or the merge may fail, and
					// clearing here would restart the grace from zero on the
					// next cycle. Repeated deferrals would then reset the
					// window forever and the PR would never merge. The state
					// is irrelevant once the merge succeeds, and a new head
					// resets it through trackReviewGateHead.
					ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr, headSHA: gateTransition.HeadSHA, missingReviewFor: silentFor})
					continue
				}
				log.Printf("[orch] PR #%d waiting for review gate (%s)", pr.Number, reviewVerdict.Summary())
				continue // not ready yet
			}
			clearReviewPendingTracking(sess)
			if !reviewVerdict.Passed {
				log.Printf("[orch] PR #%d blocked by review gate (%s)", pr.Number, reviewVerdict.Summary())
				// auto-label blocked disabled
				continue
			}

			ready = append(ready, mergeCandidate{slotName: slotName, sess: sess, pr: pr, headSHA: gateTransition.HeadSHA})
		case "failure":
			persistGate()
			if sess.Status == state.StatusQueued {
				sess.Status = state.StatusPROpen
			}
			if hold, ok := o.operatorGateHoldForPR(sess, pr, ciRollup); ok {
				o.applyOperatorGateHold(sess, pr, hold)
				continue
			}
			clearOperatorGateHold(sess)
			// A failed rollup is actionable only for the exact head that was
			// observed. Attribution stamping, an operator push, or another repair
			// can advance the branch between listOpenPRsForCycle and this decision.
			// Success/review paths already re-check this identity before mutating;
			// failure must do the same or an old red head can schedule a repair
			// after the new head is already pending (OK Player PR #388).
			if gateObservable && !o.prGateHeadMatches(pr.Number, gateTransition.HeadSHA) {
				continue
			}
			// Auto-retry on CI failure: close the PR, capture CI output, and schedule retry
			if sess.LastNotifiedStatus != "ci_failure" && sess.LastNotifiedStatus != "ci_retry_exhausted" {
				o.handleCIFailureRetry(s, slotName, sess, pr, gateTransition.HeadSHA)
			}
		case "pending":
			persistGate()
			if sess.Status == state.StatusQueued {
				sess.Status = state.StatusPROpen
			}
			if hold, ok := o.operatorGateHoldForPR(sess, pr, ciRollup); ok {
				o.applyOperatorGateHold(sess, pr, hold)
				continue
			}
			clearOperatorGateHold(sess)
		default:
			persistGate()
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
			if o.mergeReadyPRAtExpectedHead(s, candidate.slotName, candidate.sess, candidate.pr, candidate.headSHA) && candidate.missingReviewFor > 0 {
				o.notifyMissingReviewGate(candidate.pr.Number, candidate.missingReviewFor)
			}
		}
		return
	}

	// Sequential means one successful merge at a time, not "the oldest
	// unmergeable PR blocks the fleet". Freshly preflight candidates and remove
	// real conflicts before selecting the single merge slot. Conflict repair is
	// handled against the existing session/worktree by the rebase/repair paths;
	// skipping it here prevents a failed merge attempt from consuming this
	// cycle's head-of-line position and lets a younger clean PR advance.
	mergeableReady := ready[:0]
	for _, candidate := range ready {
		mergeable, mergeState, err := o.prMergeStatus(candidate.pr.Number)
		if err == nil && mergeable == "CONFLICTING" {
			log.Printf("[orch] sequential merge mode: PR #%d is %s/CONFLICTING — skipping merge slot and leaving canonical session %s for in-place conflict repair",
				candidate.pr.Number, mergeState, candidate.slotName)
			continue
		}
		mergeableReady = append(mergeableReady, candidate)
	}
	ready = mergeableReady
	if len(ready) == 0 {
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
	if o.mergeReadyPRAtExpectedHead(s, candidate.slotName, candidate.sess, candidate.pr, candidate.headSHA) && candidate.missingReviewFor > 0 {
		o.notifyMissingReviewGate(candidate.pr.Number, candidate.missingReviewFor)
	}
	if len(ready) > 1 {
		log.Printf("[orch] sequential merge mode: deferring %d additional ready PR(s) to next cycle", len(ready)-1)
	}
}

func (o *Orchestrator) reconcileTerminalSessionsWithOpenPRs(s *state.State, prs []github.PR) {
	if s == nil || len(prs) == 0 {
		return
	}
	prsByIssue := make(map[int][]github.PR)
	for _, pr := range prs {
		for _, sess := range s.Sessions {
			if sess == nil || sess.IssueNumber <= 0 || !github.PRReferencesIssue(pr, sess.IssueNumber) {
				continue
			}
			already := false
			for _, known := range prsByIssue[sess.IssueNumber] {
				if known.Number == pr.Number {
					already = true
					break
				}
			}
			if !already {
				prsByIssue[sess.IssueNumber] = append(prsByIssue[sess.IssueNumber], pr)
			}
		}
	}

	for issue, matches := range prsByIssue {
		if len(matches) != 1 {
			log.Printf("[orch] terminal/open-PR reconcile held for issue #%d: %d open PRs reference the issue; canonical identity is ambiguous", issue, len(matches))
			continue
		}
		canonical := matches[0]
		var allSlots, exactSlots, activeSlots []string
		for _, slotName := range sortedStateSessionNames(s) {
			sess := s.Sessions[slotName]
			if sess == nil || sess.IssueNumber != issue {
				continue
			}
			allSlots = append(allSlots, slotName)
			if repairIssueSessionActive(sess.Status) || (sess.PRNumber == canonical.Number && sess.Status == state.StatusRetryExhausted) {
				activeSlots = append(activeSlots, slotName)
			}
			if terminalOpenPRAdoptionCandidate(sess) &&
				(sess.PRNumber == canonical.Number || sess.LastClosedPRNumber == canonical.Number || sess.Branch == canonical.HeadRefName) {
				exactSlots = append(exactSlots, slotName)
			}
		}
		if len(activeSlots) > 1 {
			log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / PR #%d: %d active sessions claim the issue", issue, canonical.Number, len(activeSlots))
			continue
		}
		if len(activeSlots) == 1 {
			// Never activate a second session while another issue-level lease is
			// live, even if the open PR points at an older canonical branch. The
			// active canonical session still needs a durable handoff from terminal
			// sibling branches, however: otherwise work preserved by a duplicate
			// worker remains invisible once the canonical PR is reattached.
			activeSlot := activeSlots[0]
			active := s.Sessions[activeSlot]
			if active != nil && (active.PRNumber == canonical.Number || active.Branch == canonical.HeadRefName) {
				o.attachPreservedSiblingHandoffs(s, issue, activeSlot, active, canonical, allSlots)
				continue
			}
			// A disappearing duplicate can still be recorded as pr_open/queued
			// during this first reconciliation pass. Do not defer canonical
			// adoption to the next cycle: an awaiting repair approval for the sole
			// open canonical PR runs later in this same cycle and would otherwise
			// see the stale duplicate PR identity and be rejected. Verify the exact
			// recorded PR did not merge and the issue remains open, release that
			// stale gate truthfully, then fall through to the normal deterministic
			// canonical-session selection below.
			if !staleOpenPRGateAdoptionCandidate(active, canonical) {
				continue
			}
			merged, err := o.isPRMerged(active.PRNumber)
			if err != nil {
				log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / session %s: stale PR #%d merge state unavailable: %v", issue, activeSlot, active.PRNumber, err)
				continue
			}
			if merged {
				continue
			}
			closed, err := o.isIssueClosed(issue)
			if err != nil {
				log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / session %s: authoritative issue state unavailable: %v", issue, activeSlot, err)
				continue
			}
			if closed {
				continue
			}
			o.releaseClosedUnmergedSession(active, activeSlot)
		}

		candidateSlot := ""
		switch {
		case len(exactSlots) == 1:
			candidateSlot = exactSlots[0]
		case len(exactSlots) > 1:
			log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / PR #%d: %d terminal sessions claim the canonical identity", issue, canonical.Number, len(exactSlots))
			continue
		case len(allSlots) == 1:
			candidateSlot = allSlots[0]
		default:
			log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / PR #%d: no exact session among %d historical sessions", issue, canonical.Number, len(allSlots))
			continue
		}
		sess := s.Sessions[candidateSlot]
		if !terminalOpenPRAdoptionCandidate(sess) {
			continue
		}
		merged := false
		var err error
		if sess.PRNumber > 0 && sess.PRNumber != canonical.Number {
			merged, err = o.isPRMerged(sess.PRNumber)
		} else if sess.PRNumber == 0 && sess.LastClosedPRNumber > 0 && sess.LastClosedPRNumber != canonical.Number {
			merged, err = o.isPRMerged(sess.LastClosedPRNumber)
		} else if sess.PRNumber == 0 && sess.LastClosedPRNumber == 0 {
			merged, err = o.hasMergedPRForIssue(sess.IssueNumber)
		}
		if err != nil {
			log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / session %s: authoritative merge state unavailable: %v", sess.IssueNumber, candidateSlot, err)
			continue
		}
		if merged {
			continue
		}
		closed, err := o.isIssueClosed(sess.IssueNumber)
		if err != nil {
			log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / session %s: authoritative issue state unavailable: %v", sess.IssueNumber, candidateSlot, err)
			continue
		}
		if closed {
			continue
		}
		// A retained terminal worktree may still be checked out on the closed
		// duplicate branch. Metadata alone cannot adopt the canonical PR because
		// RespawnInPlace deliberately preserves the existing checkout. Reattach a
		// clean worktree first and fail closed on dirty/mismatched state so no work
		// is lost or accidentally pushed to the wrong PR.
		if strings.TrimSpace(sess.Worktree) != "" {
			if _, statErr := os.Stat(sess.Worktree); statErr == nil {
				if err := worker.EnsureWorktreeBranch(sess.Worktree, canonical.HeadRefName); err != nil {
					log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / session %s: canonical worktree reattach failed: %v", sess.IssueNumber, candidateSlot, err)
					continue
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				log.Printf("[orch] terminal/open-PR reconcile held for issue #%d / session %s: stat retained worktree: %v", sess.IssueNumber, candidateSlot, statErr)
				continue
			}
		}
		oldPR := sess.PRNumber
		if oldPR > 0 && oldPR != canonical.Number {
			sess.LastClosedPRNumber = oldPR
		}
		sess.PRNumber = canonical.Number
		sess.Branch = canonical.HeadRefName
		sess.Status = state.StatusPROpen
		sess.FinishedAt = nil
		sess.ReleasedForRedispatch = false
		o.attachPreservedSiblingHandoffs(s, issue, candidateSlot, sess, canonical, allSlots)
		log.Printf("[orch] reconciled terminal session %s for issue #%d: closed/unavailable PR #%d replaced by sole open canonical PR #%d on branch %s", candidateSlot, sess.IssueNumber, oldPR, canonical.Number, canonical.HeadRefName)
	}
}

// terminalOpenPRAdoptionCandidate reports whether authoritative GitHub state
// may rebind this non-running session to the sole open PR that references its
// issue. An unscheduled dead session is eligible: the worker may have pushed to
// an existing PR branch that differs from the synthetic session branch before
// exiting. A dead session with NextRetryAt is deliberately excluded because it
// still owns a pending in-place retry; flipping it to pr_open would consume the
// retry without running the repair (#758).
func terminalOpenPRAdoptionCandidate(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	switch sess.Status {
	case state.StatusDone, state.StatusFailed, state.StatusRetryExhausted:
		return true
	case state.StatusDead:
		return sess.NextRetryAt == nil
	default:
		return false
	}
}

// staleOpenPRGateAdoptionCandidate reports whether a non-running PR gate may be
// settled and rebound to the sole open canonical PR in the same reconciliation
// pass. Running/code_landed sessions are deliberately excluded: only GitHub's
// authoritative terminal reconciliation may move those active lifecycles.
func staleOpenPRGateAdoptionCandidate(sess *state.Session, canonical github.PR) bool {
	if sess == nil || sess.PRNumber <= 0 {
		return false
	}
	if sess.Status != state.StatusPROpen && sess.Status != state.StatusQueued {
		return false
	}
	return sess.PRNumber != canonical.Number && sess.Branch != canonical.HeadRefName
}

func (o *Orchestrator) attachPreservedSiblingHandoffs(s *state.State, issue int, canonicalSlot string, canonicalSession *state.Session, canonical github.PR, allSlots []string) {
	if o.cfg == nil || strings.TrimSpace(o.cfg.LocalPath) == "" || canonicalSession == nil {
		return
	}
	var handoffs []string
	var released []string
	for _, siblingSlot := range allSlots {
		if siblingSlot == canonicalSlot {
			continue
		}
		sibling := s.Sessions[siblingSlot]
		if sibling == nil || repairIssueSessionActive(sibling.Status) || strings.TrimSpace(sibling.Branch) == "" || sibling.Branch == canonical.HeadRefName {
			continue
		}
		commits, err := worker.UniqueBranchCommits(o.cfg.LocalPath, canonical.HeadRefName, sibling.Branch)
		if err != nil {
			log.Printf("[orch] terminal/open-PR reconcile: could not inspect preserved sibling branch %s for issue #%d: %v", sibling.Branch, issue, err)
			continue
		}
		if len(commits) > 0 {
			handoff := fmt.Sprintf("- session %s branch `%s`: unique commits `%s`", siblingSlot, sibling.Branch, strings.Join(commits, "`, `"))
			if !strings.Contains(canonicalSession.PreviousAttemptFeedback, handoff) {
				handoffs = append(handoffs, handoff)
			}
		}
		// A successful patch-equivalence comparison proves that every durable
		// sibling change is either already canonical or represented by the exact
		// commit handoff above. Release only the sibling's dispatcher claim; keep
		// its branch/worktree intact for forensics and recovery. Without this,
		// the preserved worktree itself blocks the canonical repair forever.
		sibling.ReleasedForRedispatch = true
		sibling.WorkerOutcome = state.WorkerOutcomeDuplicateDispatchReconciled
		released = append(released, siblingSlot)
	}
	if len(handoffs) > 0 {
		handoff := strings.Join(handoffs, "\n")
		if strings.TrimSpace(canonicalSession.PreviousAttemptFeedback) == "" {
			canonicalSession.PreviousAttemptFeedback = handoff
			canonicalSession.PreviousAttemptFeedbackKind = "recovery_handoff"
		} else {
			canonicalSession.PreviousAttemptFeedback += "\n\nPreserved sibling work:\n" + handoff
		}
		log.Printf("[orch] attached preserved sibling commit handoff to canonical session %s for issue #%d / PR #%d", canonicalSlot, issue, canonical.Number)
	}
	if len(released) > 0 {
		log.Printf("[orch] released reconciled sibling claim(s) %s behind canonical session %s for issue #%d / PR #%d", strings.Join(released, ","), canonicalSlot, issue, canonical.Number)
	}
}

func (o *Orchestrator) releaseClosedUnmergedSession(sess *state.Session, slotName string) {
	if sess == nil {
		return
	}
	now := time.Now().UTC()
	oldPR := sess.PRNumber
	if oldPR > 0 {
		sess.LastClosedPRNumber = oldPR
	}
	sess.PRNumber = 0
	sess.Status = state.StatusFailed
	sess.ReleasedForRedispatch = true
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
	log.Printf("[orch] PR #%d for issue #%d / session %s closed without merge while issue remains open — released for truthful redispatch", oldPR, sess.IssueNumber, slotName)
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
	head := sess.ReviewPendingHeadSHA
	now := time.Now().UTC()
	if sess.ReviewPendingSince == nil {
		return // clock not started yet; trackReviewGateHead owns that
	}
	pendingFor := now.Sub(*sess.ReviewPendingSince)
	if pendingFor < o.cfg.ReviewRetrigger.EffectivePendingFor() {
		return
	}
	if sess.ReviewRetriggerAt != nil && now.Sub(*sess.ReviewRetriggerAt) < o.cfg.ReviewRetrigger.EffectiveCooldown() {
		return
	}
	// Stop nagging a reviewer that is not answering, when the operator opted
	// into a cap. Past it the PR still waits (or clears through the
	// missing-review policy). Unlimited by default: the nudges are the only
	// automatic recovery for a reviewer that comes back later, so silencing
	// them without the missing-review escape hatch would wedge the PR for good.
	maxAttempts := o.cfg.ReviewRetrigger.EffectiveMaxAttempts()
	if maxAttempts > 0 && sess.ReviewRetriggerCount >= maxAttempts {
		return
	}
	if err := o.commentPR(pr.Number, greptileRetriggerComment); err != nil {
		log.Printf("[orch] review re-trigger: comment on PR #%d: %v", pr.Number, err)
		return
	}
	sess.ReviewRetriggerAt = &now
	sess.ReviewRetriggerCount++
	cap := "unlimited"
	if maxAttempts > 0 {
		cap = strconv.Itoa(maxAttempts)
	}
	log.Printf("[orch] review re-trigger: PR #%d greptile=pending for %s on head %s with no review — posted %q (attempt %d/%s, #691)",
		pr.Number, pendingFor.Round(time.Second), shortHeadSHA(head), greptileRetriggerComment,
		sess.ReviewRetriggerCount, cap)
}

// reviewClockHead picks the head SHA the review clock applies to: the one the
// gate transition already read, falling back to a direct lookup when the gate
// is not observable. An empty result skips the clock rather than anchoring it
// to the wrong head.
func (o *Orchestrator) reviewClockHead(pr github.PR, gateHead string) string {
	if strings.TrimSpace(gateHead) != "" {
		return gateHead
	}
	head, err := o.prHeadSHA(pr.Number)
	if err != nil {
		log.Printf("[orch] review clock: head SHA for PR #%d: %v", pr.Number, err)
		return ""
	}
	return head
}

// trackReviewGateHead maintains the per-head review-gate memory. It runs for
// EVERY verdict, not just pending ones: a settled rejection is also proof the
// reviewer is alive, and skipping it would let a later run of failed
// check-runs reads look like "the reviewer never showed up" and merge a PR the
// reviewer had already rejected.
//
// ReviewGateObserved is sticky within a head and reset ONLY when the head
// actually changes — a push or server-side update-branch genuinely restarts
// the review, everything else must not erase what we already saw. The pending
// clock starts at the first pending observation on the head; the
// Greptile-specific comment re-trigger does not own any of this.
func (o *Orchestrator) trackReviewGateHead(sess *state.Session, head string, verdict github.ReviewGateVerdict, now time.Time) {
	if sess == nil || head == "" {
		return
	}
	if sess.ReviewPendingHeadSHA != head {
		sess.ReviewPendingHeadSHA = head
		sess.ReviewPendingSince = nil
		sess.ReviewRetriggerCount = 0
		sess.ReviewGateObserved = false
		sess.ReviewGateStreamObserved = nil
	}
	if verdict.Observed {
		sess.ReviewGateObserved = true
	}
	// Per-stream sticky memory (#1148 review round 1, P1-2): a multi-stream
	// gate can be half-observed — one stream settled, the other never spoke.
	// The absent-stream escape needs to know WHICH streams were ever seen on
	// this head, so an observed stream keeps hard-blocking while a silent one
	// can expire.
	for _, stream := range verdict.Streams {
		if !stream.Observed || stream.Name == "" {
			continue
		}
		if sess.ReviewGateStreamObserved == nil {
			sess.ReviewGateStreamObserved = make(map[string]bool)
		}
		sess.ReviewGateStreamObserved[stream.Name] = true
	}
	if verdict.Pending && sess.ReviewPendingSince == nil {
		started := now
		sess.ReviewPendingSince = &started
	}
}

// notifyMissingReviewGate tells the operator that a merge proceeded without a
// review, once per PR. Merging past an absent gate is a deliberate, bounded
// weakening of the safety net — it must never be silent.
func (o *Orchestrator) notifyMissingReviewGate(prNumber int, silentFor time.Duration) {
	if o == nil || o.notifier == nil {
		return
	}
	if o.missingReviewNotified == nil {
		o.missingReviewNotified = make(map[int]bool)
	}
	if o.missingReviewNotified[prNumber] {
		return
	}
	o.missingReviewNotified[prNumber] = true
	project := strings.TrimSpace(o.repo)
	if project == "" && o.cfg != nil {
		project = strings.TrimSpace(o.cfg.Repo)
	}
	title := "maestro review gate absent"
	if project != "" {
		title += ": " + project
	}
	if err := o.notifier.Alert(
		notify.AlertGateFailStreak,
		fmt.Sprintf("%s:missing_review:%d", project, prNumber),
		title,
		fmt.Sprintf("PR #%d merged with NO review signal at all (gate silent for %s). CI was green. Check whether the review service is down or out of credits.",
			prNumber, silentFor.Round(time.Minute)),
	); err != nil {
		log.Printf("[orch] missing-review notification failed for PR #%d: %v", prNumber, err)
	}
}

// missingReviewGateElapsed reports whether every stream still holding the gate
// at pending has been completely silent — no check run, no status, no comment
// — on this head for longer than review_retrigger.missing_after_minutes.
//
// The evaluation is per-stream (#1148 review round 1, P1-2): a multi-stream
// gate can be half-observed — one stream posted its status while the other
// never reported at all. The aggregate used to be Pending+Observed forever in
// that state, so the escape never fired and no re-trigger existed: the PR was
// wedged for good. Now a pending stream that was never seen on this head (this
// read or the sticky per-stream memory) can expire, while a stream that DID
// produce any signal keeps blocking at any duration, and a settled rejection
// from any stream disables the escape outright.
//
// Opt-in: 0 keeps today's block-forever behavior.
func (o *Orchestrator) missingReviewGateElapsed(sess *state.Session, verdict github.ReviewGateVerdict, now time.Time) (time.Duration, bool) {
	if o == nil || o.cfg == nil || sess == nil {
		return 0, false
	}
	grace := o.cfg.ReviewRetrigger.MissingReviewGraceOrZero()
	if grace <= 0 || !verdict.Pending {
		return 0, false
	}
	// A degraded read cannot prove silence. Without this, a GitHub check-runs
	// outage lasting longer than the grace would look exactly like a reviewer
	// that never showed up, and PRs would merge unreviewed because of an API
	// failure.
	if verdict.LookupFailed {
		return 0, false
	}
	if sess.ReviewPendingSince == nil {
		return 0, false
	}
	if len(verdict.Streams) == 0 {
		// Legacy aggregate-only verdict (narrow test hooks): keep the
		// original whole-gate semantics, including the sticky memory.
		if verdict.Observed || sess.ReviewGateObserved {
			return 0, false
		}
	} else {
		// State written by a pre-#1148 binary has the aggregate sticky bit
		// without per-stream attribution — treat it conservatively as "some
		// stream was seen" and keep blocking.
		if len(sess.ReviewGateStreamObserved) == 0 && sess.ReviewGateObserved {
			return 0, false
		}
		for _, stream := range verdict.Streams {
			// A stream that settled as failed is a live rejection, not an
			// absent reviewer; the escape must never merge past it.
			if !stream.Pending && !stream.Passed {
				return 0, false
			}
			if !stream.Pending {
				continue
			}
			// Sticky per-stream memory: a reviewer seen on this head keeps
			// blocking even if this read came back unobserved (e.g. a failed
			// check-runs lookup fell through to the comment path).
			if stream.Observed || sess.ReviewGateStreamObserved[stream.Name] {
				return 0, false
			}
		}
	}
	silentFor := now.Sub(*sess.ReviewPendingSince)
	if silentFor < grace {
		return 0, false
	}
	return silentFor, true
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
// clearReviewPendingTracking stops the pending clock once the gate settles.
// The head anchor is deliberately KEPT: a check that settles and then goes
// pending again on the same commit (a re-run, or a check that disappears) must
// not look like a new head, or the per-head re-trigger cap would reset on every
// such flap and the PR could collect unlimited "@greptile review" comments.
// Only trackReviewGateHead clears the anchor, and only for a real head change.
func clearReviewPendingTracking(sess *state.Session) {
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

// canonicalMergeFlowOwners elects exactly one session for each open PR before
// autoMergePRs performs any gate mutation. The ordering deliberately matches
// supervisor/watchdog PR ownership: a live worker wins; otherwise the newest
// unresolved session wins, with stable tie-breaking. Keeping this election
// local to the current open-PR snapshot also means a closed/merged PR is left
// to terminal reconciliation rather than retaining a synthetic owner.
func canonicalMergeFlowOwners(s *state.State, byBranch map[string]github.PR, byNumber map[int]github.PR) map[int]string {
	owners := make(map[int]string)
	if s == nil {
		return owners
	}
	for _, slot := range sortedStateSessionNames(s) {
		sess := s.Sessions[slot]
		if !mergeFlowOwnerEligibleStatus(sess) {
			continue
		}
		pr, found := mergeFlowPRForSession(sess, byBranch, byNumber)
		if !found {
			continue
		}
		currentSlot, exists := owners[pr.Number]
		if !exists || mergeFlowOwnerPrecedes(slot, sess, currentSlot, s.Sessions[currentSlot]) {
			owners[pr.Number] = slot
		}
	}
	return owners
}

func mergeFlowOwnerEligibleStatus(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	switch sess.Status {
	case state.StatusRunning, state.StatusPROpen, state.StatusQueued, state.StatusDead,
		state.StatusFailed, state.StatusConflictFailed, state.StatusRetryExhausted:
		return sess.PRNumber > 0 || strings.TrimSpace(sess.Branch) != ""
	default:
		return false
	}
}

func mergeFlowOwnerPrecedes(candidateSlot string, candidate *state.Session, currentSlot string, current *state.Session) bool {
	if candidate == nil {
		return false
	}
	candidateLive := candidate.Status == state.StatusRunning && candidate.PID > 0
	currentLive := current != nil && current.Status == state.StatusRunning && current.PID > 0
	if candidateLive != currentLive {
		return candidateLive
	}
	if current == nil || candidate.StartedAt.After(current.StartedAt) {
		return true
	}
	if current.StartedAt.After(candidate.StartedAt) {
		return false
	}
	if candidate.Status != current.Status {
		return candidate.Status == state.StatusQueued
	}
	return candidateSlot < currentSlot
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

// prBelongsToIssue reports whether an already-fetched PR is part of the
// issue's aggregate session state. The durable session identity is important:
// worker PR bodies use non-closing references, and historical/manual PRs may
// omit an issue reference entirely even though their owning session records the
// exact issue, PR number, and branch.
func prBelongsToIssue(s *state.State, issueNumber int, pr github.PR) bool {
	if issueNumber <= 0 {
		return false
	}
	if github.PRReferencesIssue(pr, issueNumber) {
		return true
	}
	if s == nil {
		return false
	}
	for _, sess := range s.Sessions {
		if sess == nil || sess.IssueNumber != issueNumber {
			continue
		}
		if pr.Number > 0 && (sess.PRNumber == pr.Number || sess.LastClosedPRNumber == pr.Number) {
			return true
		}
		if branch := strings.TrimSpace(sess.Branch); branch != "" && branch == pr.HeadRefName {
			return true
		}
	}
	return false
}

func openPRForIssueState(s *state.State, issueNumber int, prs []github.PR) (github.PR, bool) {
	for _, pr := range prs {
		if prBelongsToIssue(s, issueNumber, pr) {
			return pr, true
		}
	}
	return github.PR{}, false
}

// mergedPRForIssueState checks the issue's aggregate durable identity before a
// no-PR retry_exhausted slot is allowed to block it. It recognizes a merged PR
// owned by any sibling session even when the PR used only `Refs #N` (or no
// textual reference), then falls back to the legacy closing-keyword lookup for
// historical work without a retained session.
func (o *Orchestrator) mergedPRForIssueState(s *state.State, issueNumber int) (int, bool, error) {
	if issueNumber <= 0 {
		return 0, false, nil
	}

	seen := make(map[int]struct{})
	var firstErr error
	checkPR := func(prNumber int) (int, bool) {
		if prNumber <= 0 {
			return 0, false
		}
		if _, ok := seen[prNumber]; ok {
			return 0, false
		}
		seen[prNumber] = struct{}{}
		merged, err := o.isPRMerged(prNumber)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("check PR #%d: %w", prNumber, err)
			}
			return 0, false
		}
		if merged {
			return prNumber, true
		}
		return 0, false
	}

	if s != nil {
		for _, slotName := range sortedStateSessionNames(s) {
			sess := s.Sessions[slotName]
			if sess == nil || sess.IssueNumber != issueNumber {
				continue
			}
			if prNumber, merged := checkPR(sess.PRNumber); merged {
				return prNumber, true, nil
			}
			if prNumber, merged := checkPR(sess.LastClosedPRNumber); merged {
				return prNumber, true, nil
			}
		}
	}

	// During a real cycle the shared closed-PR snapshot lets this path detect
	// bare non-closing references without one subprocess per stale slot. Direct
	// unit callers opt in by injecting listClosedPRsFn; otherwise preserve the
	// legacy hasMergedPRForIssue test surface below.
	if o.cycleActive || o.listClosedPRsFn != nil {
		prs, err := o.listClosedPRsForCycle()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list closed PRs: %w", err)
			}
		} else {
			for _, pr := range prs {
				if pr.MergedAt != "" && prBelongsToIssue(s, issueNumber, pr) {
					return pr.Number, true, nil
				}
			}
		}
	}

	merged, err := o.hasMergedPRForIssue(issueNumber)
	if err != nil {
		if firstErr != nil {
			return 0, false, fmt.Errorf("aggregate merged-PR state unavailable (%v; legacy lookup: %w)", firstErr, err)
		}
		return 0, false, err
	}
	if merged {
		prNumber := 0
		if o.mergedPRForIssueFn != nil || o.gh != nil {
			if resolved, resolveErr := o.mergedPRForIssue(issueNumber); resolveErr == nil {
				prNumber = resolved
			}
		}
		return prNumber, true, nil
	}
	if firstErr != nil {
		return 0, false, firstErr
	}
	return 0, false, nil
}

// settleNoPRRetryExhaustedSiblings retires every stale no-PR terminal slot for
// an issue after authoritative issue-level state proves that blocking is moot.
// It intentionally does not sync the project board: a stale slot must never
// overwrite the live/closed issue's aggregate status. skipSlot remains the
// canonical code_landed owner when delivery approval is required.
func settleNoPRRetryExhaustedSiblings(s *state.State, issueNumber int, skipSlot string, issueClosed bool, now time.Time) int {
	if s == nil || issueNumber <= 0 {
		return 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	settled := 0
	for _, slotName := range sortedStateSessionNames(s) {
		if slotName == skipSlot {
			continue
		}
		sess := s.Sessions[slotName]
		if sess == nil || sess.IssueNumber != issueNumber || sess.Status != state.StatusRetryExhausted || sess.PRNumber != 0 {
			continue
		}
		sess.Status = state.StatusDone
		sess.LastNotifiedStatus = noPRReconciledStatus
		sess.NextRetryAt = nil
		sess.RetryHoldReason = ""
		sess.RestartCheckpointAt = nil
		sess.ReleasedForRedispatch = true
		if sess.FinishedAt == nil {
			sess.FinishedAt = &now
		}
		state.MarkWorkerEnded(sess, now)
		if issueClosed {
			sess.IssueClosedAt = &now
			sess.LastTerminalReconcileAt = &now
		}
		settled++
	}
	return settled
}

func markNoPRRetryExhaustedReconciled(s *state.State, issueNumber int) int {
	if s == nil || issueNumber <= 0 {
		return 0
	}
	marked := 0
	for _, sess := range s.Sessions {
		if sess == nil || sess.IssueNumber != issueNumber || sess.Status != state.StatusRetryExhausted || sess.PRNumber != 0 {
			continue
		}
		if sess.LastNotifiedStatus != noPRReconciledStatus {
			sess.LastNotifiedStatus = noPRReconciledStatus
			marked++
		}
	}
	return marked
}

// noPRReconciledStatus marks no-PR retry_exhausted sessions for one issue after
// their aggregate issue outcome has been reconciled by autoMergePRs. Marking
// every stale sibling keeps later map iterations and subsequent cycles from
// re-labelling, re-closing, re-syncing, or re-notifying per slot.
const noPRReconciledStatus = "no_pr_reconciled"

// closedPRCloseRetryStatus marks a merged-PR retry_exhausted session whose
// auto-close hit a transient GitHub failure. The session stays retry_exhausted
// so the next cycle retries the close instead of settling done with the issue
// still open (#818 follow-up); the marker suppresses re-posting the close
// comment and re-notifying on each retry.
const closedPRCloseRetryStatus = "closed_pr_close_retry"

// reconcileNoPRRetryExhausted handles the #577 deadlock: a worker exhausted
// retries without opening a PR (often because the issue was already
// implemented by a prior merge via `Refs #N`), so the merge flow has nothing
// to advance, the session is terminal, and at max_parallel=1 the dynamic-wave
// queue halts. This helper first evaluates the current issue-level state. An
// open sibling PR preserves the stale slots, while a closed issue or merged
// sibling PR retires them. Only a genuinely stuck issue receives the configured
// blocked label, once for the aggregate issue rather than once per slot.
func (o *Orchestrator) reconcileNoPRRetryExhausted(s *state.State, slotName string, sess *state.Session, openPRs []github.PR) {
	if sess == nil || sess.IssueNumber <= 0 {
		return
	}
	if sess.LastNotifiedStatus == noPRReconciledStatus {
		// Already reconciled — suppress duplicate side-effects and the
		// "waiting for reconciliation" log spam.
		return
	}

	closed, err := o.isIssueClosed(sess.IssueNumber)
	if err != nil {
		log.Printf("[orch] no-PR retry_exhausted: could not check issue #%d state (slot %s): %v", sess.IssueNumber, slotName, err)
		return
	}
	if closed {
		settled := settleNoPRRetryExhaustedSiblings(s, sess.IssueNumber, "", true, time.Now().UTC())
		log.Printf("[orch] no-PR retry_exhausted: issue #%d is already closed; reconciled %d stale no-PR slot(s) to done without applying blocked or syncing stale slot status", sess.IssueNumber, settled)
		return
	}

	if openPR, ok := openPRForIssueState(s, sess.IssueNumber, openPRs); ok {
		log.Printf("[orch] no-PR retry_exhausted: issue #%d has open PR #%d in aggregate session state; slot %s is superseded in flight, preserving it without applying blocked", sess.IssueNumber, openPR.Number, slotName)
		return
	}

	mergedPR, merged, err := o.mergedPRForIssueState(s, sess.IssueNumber)
	if err != nil {
		// Don't mark reconciled on transient GitHub failure; try again next
		// cycle. Log so operators can correlate with API errors.
		log.Printf("[orch] no-PR retry_exhausted: could not check aggregate merged PR state for issue #%d (slot %s): %v", sess.IssueNumber, slotName, err)
		return
	}

	if merged {
		// A closing-keyword PR is a real landed generation. In
		// approval_required mode it must enter the standing delivery gate before
		// this retry-reconciliation path may close the issue or report Done.
		if o.approvalRequiredDeliveryEnabled() {
			prNumber := mergedPR
			var prErr error
			if prNumber <= 0 {
				prNumber, prErr = o.mergedPRForIssue(sess.IssueNumber)
			}
			if prErr != nil || prNumber <= 0 {
				log.Printf("[orch] no-PR retry_exhausted: merged work for issue #%d could not be pinned to a PR for delivery: %v — holding", sess.IssueNumber, prErr)
				return
			}
			o.markCodeLanded(sess, prNumber)
			settled := settleNoPRRetryExhaustedSiblings(s, sess.IssueNumber, slotName, false, time.Now().UTC())
			if settled > 0 {
				log.Printf("[orch] no-PR retry_exhausted: merged PR #%d owns issue #%d; reconciled %d stale sibling slot(s) to done while slot %s holds delivery", prNumber, sess.IssueNumber, settled, slotName)
			}
			if !o.reconcileCodeLandedDelivery(s, sess) {
				log.Printf("[orch] no-PR retry_exhausted: issue #%d is held in code_landed for delivery approval", sess.IssueNumber)
			}
			return
		}
		// The work was already landed by another PR (closing-keyword link).
		// Auto-close when the operator has granted close_issue as a safe
		// action without an approval gate; otherwise surface it as a
		// close-candidate via notification.
		comment := fmt.Sprintf("Maestro: closing this issue because worker session %s exhausted retries without producing a PR (the work appears to be implemented by an already-merged PR).", slotName)
		issueNowClosed := false
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
			issueNowClosed = true
		} else {
			log.Printf("[orch] no-PR retry_exhausted: issue #%d (slot %s) has a merged PR — surfaced as operator close-candidate", sess.IssueNumber, slotName)
			if o.notifier != nil {
				o.notifier.Sendf("⚠️ maestro: issue #%d retry_exhausted with no PR; a merged PR already implements it — operator close-candidate", sess.IssueNumber)
			}
		}
		settled := settleNoPRRetryExhaustedSiblings(s, sess.IssueNumber, "", issueNowClosed, time.Now().UTC())
		if mergedPR > 0 {
			log.Printf("[orch] no-PR retry_exhausted: merged PR #%d owns issue #%d; reconciled %d stale no-PR slot(s) to done without applying blocked", mergedPR, sess.IssueNumber, settled)
		} else {
			log.Printf("[orch] no-PR retry_exhausted: merged work owns issue #%d; reconciled %d stale no-PR slot(s) to done without applying blocked", sess.IssueNumber, settled)
		}
		return
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

	// A genuinely stuck issue keeps every historical retry_exhausted status,
	// but the issue-level marker prevents sibling slots from repeating the same
	// label, status sync, and notification later in this or a future cycle.
	markNoPRRetryExhaustedReconciled(s, sess.IssueNumber)
}

// reconcileClosedPRRetryExhausted handles retry_exhausted sessions whose PR
// is no longer open (#818). The PR was either merged by another path or
// closed without merge. Without this reconciliation the session sat in
// retry_exhausted forever: reported live (SessionAttentionForAt marks
// retry_exhausted+PRNumber>0 as needs_attention/actionable), holding the
// issue slot (IssueRetryExhausted stays true), logging "waiting for
// reconciliation" every cycle, and surviving daemon restarts.
//
// The fix transitions the *session status* out of retry_exhausted so it stops
// being reported live and releases the issue slot without any supervisor:
//
//   - merged PR → StatusDone. The work landed, so the session settles done
//     (SessionAttentionForAt no longer flags it, SessionLive → false) and the
//     issue is auto-closed when close_issue is a safe action, otherwise
//     surfaced as an operator close-candidate. A *transient* close failure is
//     the exception: the session is left retry_exhausted so the next cycle
//     retries the close, rather than settling done with the issue still open.
//   - closed-unmerged PR → StatusFailed with the recorded PR cleared and
//     ReleasedForRedispatch set. Clearing the PR drops IssueRetryExhausted for
//     the issue and makes the attempt count via FailedAttemptsForIssue, so the
//     dispatch loop re-picks the issue for a fresh worker while it stays under
//     max_retries_per_issue. The marker keeps the released session mirrored as
//     runnable Todo on the board (projectStatusForSession) instead of Blocked,
//     so the reconcile does not re-strand it. No blocked label is applied:
//     only the supervisor removes that label, so labelling blocked would
//     re-create the exact supervisor-dependence #818 removes.
func (o *Orchestrator) reconcileClosedPRRetryExhausted(s *state.State, slotName string, sess *state.Session) {
	if sess == nil || sess.IssueNumber <= 0 || sess.PRNumber <= 0 {
		return
	}

	merged, err := o.isPRMerged(sess.PRNumber)
	if err != nil {
		// Outcome unknown on a transient GitHub failure — leave the session in
		// retry_exhausted and try again next cycle rather than settling it
		// on a guess.
		log.Printf("[orch] closed-PR retry_exhausted: could not check if PR #%d was merged for issue #%d (slot %s): %v", sess.PRNumber, sess.IssueNumber, slotName, err)
		return
	}

	now := time.Now().UTC()

	if merged {
		// This path knows the PR was merged, so approval-gated projects must not
		// jump straight from retry_exhausted to Done. Move into the same standing
		// code_landed gate used by the normal merge path and mint/persist the
		// exact-SHA approval immediately.
		if o.approvalRequiredDeliveryEnabled() {
			o.markCodeLanded(sess, sess.PRNumber)
			if !o.reconcileCodeLandedDelivery(s, sess) {
				log.Printf("[orch] closed-PR retry_exhausted: issue #%d PR #%d is held in code_landed for delivery approval", sess.IssueNumber, sess.PRNumber)
			}
			return
		}
		if o.supervisorActionAllowed(config.SupervisorActionCloseIssue) && !o.supervisorActionRequiresApproval(config.SupervisorActionCloseIssue) {
			// Post the close comment only on the first attempt; a retry after a
			// transient close failure passes an empty comment so it does not
			// spam the issue with a duplicate close note (CloseIssue skips the
			// comment when it is empty).
			comment := ""
			if sess.LastNotifiedStatus != closedPRCloseRetryStatus {
				comment = fmt.Sprintf("Maestro: closing this issue because worker session %s exhausted retries, but its PR #%d was merged.", slotName, sess.PRNumber)
			}
			if cerr := o.closeIssue(sess.IssueNumber, comment); cerr != nil {
				// Transient close failure: do NOT settle the session done, or
				// the issue can stay open permanently — the retry_exhausted
				// session would leave the merge flow and the close would never
				// be retried (#818 follow-up). Leave it retry_exhausted so the
				// next cycle retries the close; mark it so the retry does not
				// re-post the comment or re-notify.
				log.Printf("[orch] closed-PR retry_exhausted: auto-close failed for issue #%d (slot %s, PR #%d): %v — leaving retry_exhausted to retry close next cycle", sess.IssueNumber, slotName, sess.PRNumber, cerr)
				if o.notifier != nil && sess.LastNotifiedStatus != closedPRCloseRetryStatus {
					o.notifier.Sendf("⚠️ maestro: issue #%d retry_exhausted (PR #%d merged); auto-close failed: %v — will retry", sess.IssueNumber, sess.PRNumber, cerr)
				}
				sess.LastNotifiedStatus = closedPRCloseRetryStatus
				return
			}
			log.Printf("[orch] closed-PR retry_exhausted: auto-closed issue #%d (slot %s, PR #%d merged)", sess.IssueNumber, slotName, sess.PRNumber)
			if o.notifier != nil {
				o.notifier.Sendf("✅ maestro: closed issue #%d after retry_exhausted (PR #%d merged)", sess.IssueNumber, sess.PRNumber)
			}
		} else {
			log.Printf("[orch] closed-PR retry_exhausted: issue #%d (slot %s) PR #%d was merged — surfaced as operator close-candidate", sess.IssueNumber, slotName, sess.PRNumber)
			if o.notifier != nil {
				o.notifier.Sendf("⚠️ maestro: issue #%d retry_exhausted; PR #%d was merged — operator close-candidate", sess.IssueNumber, sess.PRNumber)
			}
		}
		o.syncProject(sess.IssueNumber, github.ProjectStatusDone)
		sess.Status = state.StatusDone
		sess.FinishedAt = &now
		state.MarkWorkerEnded(sess, now)
		return
	}

	// PR closed without merge: release the issue for fresh dispatch. Marking
	// the session failed and clearing the recorded PR drops
	// IssueRetryExhausted for the issue and makes FailedAttemptsForIssue count
	// this attempt, so the dispatch loop re-picks the issue (subject to
	// max_retries_per_issue) without any supervisor un-block step.
	log.Printf("[orch] closed-PR retry_exhausted: PR #%d closed without merge for issue #%d (slot %s) — marking session failed and releasing issue for fresh dispatch (subject to max_retries_per_issue)", sess.PRNumber, sess.IssueNumber, slotName)
	if o.notifier != nil {
		o.notifier.Sendf("🔁 maestro: issue #%d retry_exhausted (PR #%d closed without merge) — released for fresh dispatch (subject to max_retries)", sess.IssueNumber, sess.PRNumber)
	}
	o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
	sess.LastClosedPRNumber = sess.PRNumber
	sess.PRNumber = 0
	sess.Status = state.StatusFailed
	// #818: mark the session so projectStatusForSession mirrors it as runnable
	// Todo rather than Blocked. Otherwise reconcileSessionsToProjectBoard maps
	// this fresh StatusFailed session to Blocked and overwrites the Todo synced
	// above — and because the fresh worker may not start in the same cycle (no
	// slots, pause/drain/emergency halt, backend dispatch pause), the dynamic
	// wave would then see Blocked (non-runnable) and re-strand the released
	// issue instead of dispatching it.
	sess.ReleasedForRedispatch = true
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
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
		sess.LastClosedPRNumber = pr.Number
		sess.PRNumber = 0
	} else {
		log.Printf("[orch] keeping PR #%d open and respawning %s in place to address review feedback", pr.Number, slotName)
		sess.PRNumber = pr.Number
	}

	sess.CIFailureOutput = ""
	sess.PreviousAttemptFeedback = reviewFeedback
	sess.PreviousAttemptFeedbackKind = state.RetryReasonReviewFeedback
	sess.RetryReason = state.RetryReasonReviewFeedback
	// #857: a review-feedback retry is scheduled off a green aggregate CI, but
	// an individual non-required check (e.g. agent-lint) can still be red on the
	// PR head — the #424 mergeable_state=unstable override treats that as
	// "success" and routes here. Capture a bounded excerpt so the respawned
	// worker also sees the failing check its previous push introduced, not just
	// the review comments.
	sess.FailingCheckContext = o.collectFailingCheckContext(pr.Number)

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

// handleCIFailureRetry captures the failed checks and schedules an in-place
// repair on the same slot/worktree/branch/PR. A red check is not a reason to
// destroy canonical identity: closing the PR and clearing its session lease
// let the ready issue dispatch a second worker before the retry completed
// (live #949 / OK Player #346).
func (o *Orchestrator) handleCIFailureRetry(s *state.State, slotName string, sess *state.Session, pr github.PR, expectedHead string) {
	maxRetries := o.cfg.MaxRetriesPerIssue
	totalAttempts := s.FailedAttemptsForIssue(sess.IssueNumber) + sess.RetryCount

	if maxRetries > 0 && totalAttempts >= maxRetries {
		// Exhaustion is also a mutation of the exact PR lifecycle. Never mark a
		// newer head retry-exhausted from an obsolete failed rollup.
		if strings.TrimSpace(expectedHead) != "" && !o.prGateHeadMatches(pr.Number, expectedHead) {
			return
		}
		sess.NotifiedCIFail = true // backward compat
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

	// Capture CI failure output while the failed head is still authoritative.
	ciOutput, err := o.prChecksOutput(pr.Number)
	if err != nil {
		log.Printf("[orch] warn: could not capture CI output for PR #%d: %v", pr.Number, err)
		ciOutput = "(CI output unavailable)"
	}

	// Collect review feedback for the same in-place repair prompt.
	reviewFeedback, err := o.collectPRReviewFeedback(pr.Number)
	if err != nil {
		log.Printf("[orch] warn: could not collect review feedback for PR #%d: %v", pr.Number, err)
	}

	// #857: this is the path a red required check (e.g. agent-lint) actually
	// takes — its failure conclusion makes the aggregate CI verdict "failure".
	// CIFailureOutput above is only the bare checks overview (name + state); the
	// concrete `##[error]` annotation lines that say WHY the check failed live
	// here. Capture them while the failed head is authoritative so the retry worker sees the
	// exact lint constraint its previous push broke, not just "agent-lint
	// failed" — this was the observed PR #850 blindness.
	failingCheckContext := o.collectFailingCheckContext(pr.Number)

	// The reads above can take seconds. A worker/operator push in that window
	// invalidates every captured failure/review excerpt just as surely as a push
	// before the caller's first head check. Re-check immediately before the first
	// durable session mutation; the next cycle will observe the new head and
	// decide from its own rollup instead of repairing stale evidence.
	if strings.TrimSpace(expectedHead) != "" && !o.prGateHeadMatches(pr.Number, expectedHead) {
		return
	}
	sess.NotifiedCIFail = true // backward compat

	// Preserve the exact PR lease. Cleanup may already have cleared the
	// worktree field on a terminal transition; record only the deterministic
	// same-slot path so respawnPreservingWorktreeWithConfig can restore the
	// retained local branch without allocating a new slot or branch.
	sess.PRNumber = pr.Number
	if strings.TrimSpace(sess.Branch) == "" {
		sess.Branch = pr.HeadRefName
	}
	if strings.TrimSpace(sess.Worktree) == "" && strings.TrimSpace(o.cfg.WorktreeBase) != "" {
		sess.Worktree = filepath.Join(o.cfg.WorktreeBase, slotName)
	}

	// Store CI failure output and review feedback for the next worker
	sess.CIFailureOutput = ciOutput
	sess.FailingCheckContext = failingCheckContext // #857: concrete failing-check error lines, alongside the overview
	sess.PreviousAttemptFeedback = reviewFeedback
	if strings.TrimSpace(reviewFeedback) != "" {
		sess.PreviousAttemptFeedbackKind = "review_feedback"
	} else {
		sess.PreviousAttemptFeedbackKind = ""
	}

	// Schedule the same-session repair with exponential backoff. The open PR
	// remains the issue-level lease throughout the wait, so ordinary dispatch
	// cannot race the repair with a fresh worker.
	sess.RetryCount++
	backoffMs := retryBackoffMs(sess.RetryCount, o.cfg.MaxRetryBackoffMs)
	retryAt := time.Now().UTC().Add(time.Duration(backoffMs) * time.Millisecond)
	sess.NextRetryAt = &retryAt
	sess.Status = state.StatusDead
	now := time.Now().UTC()
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)

	log.Printf("[orch] CI failure on PR #%d — scheduling in-place retry %d in %dms for issue #%d on session %s",
		pr.Number, sess.RetryCount, backoffMs, sess.IssueNumber, slotName)
	o.notifier.Sendf("🔄 maestro: CI failed on PR #%d (issue #%d: %s), in-place retry %d scheduled in %ds",
		pr.Number, sess.IssueNumber, sess.IssueTitle, sess.RetryCount, backoffMs/1000)
}

func (o *Orchestrator) reviewGate() string {
	switch strings.ToLower(strings.TrimSpace(o.cfg.ReviewGate)) {
	case "none", "off", "disabled":
		return "none"
	case "llm-review":
		return "llm-review"
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
	sess.PRMerged = sess.PRNumber > 0
	// #1013: a code_landed transition is terminal reconciliation — the PR
	// merged, so any scheduled retry (including one deliberately held behind a
	// current issue-guard label) is settled and must not survive. Clear it at
	// this shared sink so the invariant holds for every merge-detection path,
	// not only the retireStaleRetry pre-check that also clears it explicitly.
	sess.NextRetryAt = nil
	sess.RetryHoldReason = ""
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
	sess.PRMerged = sess.PRNumber > 0
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
	sess.PRMerged = sess.PRNumber > 0
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

// reconcileResolvedRepairApprovals is the standing, idempotent safety net for
// #866. The edge-triggered stale calls in verifyOutcomeAfterMerge /
// reconcileCodeLandedSessions fire only in the exact cycle a session transitions
// to done; if that save is rolled back by a conflicting concurrent write (the
// supervisor re-recording the same approval, a dashboard action, a merge landing
// on disk between load and save), the session is already done next cycle and
// neither edge re-fires — the spawn_repair_worker approval then ages past SLA as
// a false operator gate, exactly the #858 dogfood incident. This pass runs every
// cycle and re-derives the transition for any active repair approval whose target
// issue has reached a terminal state (a done session, or the issue closed on
// GitHub), so reconciliation converges regardless of which cycle first missed it
// and survives a daemon restart. markApprovalStale is a no-op once stale, so it
// is safe to run every cycle; unrelated approvals are never touched.
func (o *Orchestrator) reconcileResolvedRepairApprovals(s *state.State) {
	if o == nil || s == nil {
		return
	}
	issues := s.ActiveSpawnRepairWorkerApprovalIssues()
	if len(issues) == 0 {
		return
	}
	now := time.Now().UTC()
	for _, issue := range issues {
		resolved, reason := o.repairApprovalIssueResolved(s, issue)
		if !resolved {
			continue
		}
		o.reconcileMootRepairApprovals(s, issue, now, reason)
	}
}

// reconcileGuardedRepairApprovals retires delayed repair authority when the
// issue's current labels no longer permit dispatch. This is deliberately a
// standing pass rather than only a startNewWorkers guard: removing the ready
// label also removes the issue from the normal candidate list, but must not
// leave an already-approved repair intent active forever or let it execute
// after the issue becomes blocked.
func (o *Orchestrator) reconcileGuardedRepairApprovals(s *state.State) {
	if o == nil || o.cfg == nil || s == nil {
		return
	}
	numbers := s.ActiveRepairDispatchApprovalIssues()
	if len(numbers) == 0 {
		return
	}
	now := time.Now().UTC()
	for _, issueNumber := range numbers {
		issue, err := o.getIssue(issueNumber)
		if err != nil {
			log.Printf("[orch] repair-approval guard: issue #%d label check failed; leaving approval active: %v", issueNumber, err)
			continue
		}
		if !github.HasLabel(issue, o.cfg.ExcludeLabels) {
			continue
		}
		reason := fmt.Sprintf("issue #%d has a current excluded label — delayed repair approval/intent is stale", issueNumber)
		for _, approval := range s.StaleActiveRepairDispatchApprovals(issueNumber, now, reason) {
			log.Printf("[orch] reconciled guarded repair approval %s for issue #%d: %s", approval.ID, issueNumber, reason)
			o.mirrorRepairApprovalTerminal(approval.ID, now, reason)
		}
	}
}

// repairApprovalIssueResolved reports whether a spawn_repair_worker approval for
// issue is moot: its target work reached a terminal state. An active session
// always takes precedence over an older done session for the same issue, because
// the active session may be the very repair the approval controls. When no
// session is active, a done session is authoritative and needs no GitHub read
// only when no terminal-failure session also exists. A failed/dead/conflicted/
// retry-exhausted session means the issue may have reopened or a newer repair
// may still be needed, so it takes precedence over the done shortcut and falls
// through to the GitHub issue check. Otherwise the issue is treated as resolved
// when closed (the externally-closed path). A GitHub read error is reported
// unresolved so a legitimately-pending repair approval is never staled out from
// under an operator.
func (o *Orchestrator) repairApprovalIssueResolved(s *state.State, issue int) (bool, string) {
	activeSession := false
	for _, sess := range s.Sessions {
		if sess == nil || sess.IssueNumber != issue {
			continue
		}
		if repairIssueSessionActive(sess.Status) {
			activeSession = true
		}
	}
	if activeSession {
		return false, ""
	}
	closed, err := o.isIssueClosed(issue)
	if err != nil {
		log.Printf("[orch] repair-approval reconcile: issue #%d closed-state check failed; leaving approval pending: %v", issue, err)
		return false, ""
	}
	if closed {
		return true, fmt.Sprintf("issue #%d closed — repair worker moot", issue)
	}
	merged, err := o.hasMergedPRForIssue(issue)
	if err != nil {
		log.Printf("[orch] repair-approval reconcile: issue #%d merged-PR check failed; leaving approval pending: %v", issue, err)
		return false, ""
	}
	if merged {
		return true, fmt.Sprintf("issue #%d has an authoritative merged PR — repair worker moot", issue)
	}
	return false, ""
}

// repairIssueSessionActive reports whether a session status means the issue is
// still being worked, so a repair approval is not yet moot. The terminal-failure
// statuses (failed/dead/conflict_failed/retry_exhausted) are deliberately NOT
// active: with those the repair approval may be legitimate, so resolution falls
// through to the GitHub issue-closed check rather than assuming the issue open.
func repairIssueSessionActive(status state.SessionStatus) bool {
	switch status {
	case state.StatusQueued, state.StatusRunning, state.StatusPROpen, state.StatusCodeLanded:
		return true
	default:
		return false
	}
}

// reconcileMootRepairApprovals stales every active spawn_repair_worker approval
// targeting issue, emits a per-approval operator journal record (approval id +
// issue/PR target + terminal-outcome reason), and mirrors the terminal
// transition into the SQLite approval store when one is configured. It is the
// single reconcile path shared by the edge-triggered post-merge/outcome callers
// and the standing reconciler, so every path journals and persists identically
// (#866). Returns the number staled.
// mirrorRepairApprovalTerminal mirrors a repair approval's terminal transition
// (changed-head supersede / post-dispatch consume) into the SQLite approval
// store when one is configured. JSON state remains the dispatch source of truth;
// this keeps the queryable mirror from showing a lingering active approval.
func (o *Orchestrator) mirrorRepairApprovalTerminal(id string, now time.Time, reason string) {
	if o == nil || !o.approvalsBinding.UseSQLite() || strings.TrimSpace(id) == "" {
		return
	}
	b := o.approvalsBinding
	b.StateDir = o.cfg.StateDir
	b.Repo = o.repo
	b.Project = o.repo
	if err := approvalstore.ReconcileMoot(b, id, now, reason); err != nil {
		log.Printf("[orch] repair approval %s terminal mirror to SQLite failed (JSON already updated, retried next cycle): %v", id, err)
	}
}

func (o *Orchestrator) reconcileMootRepairApprovals(s *state.State, issue int, now time.Time, reason string) int {
	staled := s.StaleSpawnRepairWorkerApprovalsForResolvedIssue(issue, now, reason)
	for i := range staled {
		ap := staled[i]
		pr := 0
		if ap.Target != nil {
			pr = ap.Target.PR
		}
		log.Printf("[orch] reconciled moot spawn_repair_worker approval %s (issue #%d, PR #%d): %s", ap.ID, issue, pr, reason)
		if o.approvalsBinding.UseSQLite() {
			b := o.approvalsBinding
			b.StateDir = o.cfg.StateDir
			b.Repo = o.repo
			b.Project = o.repo
			if err := approvalstore.ReconcileMoot(b, ap.ID, now, reason); err != nil {
				log.Printf("[orch] approval %s stale mirror to SQLite failed (JSON already reconciled, retried next cycle): %v", ap.ID, err)
			}
		}
	}
	return len(staled)
}

// reconcileMootRepairApprovalsAfterResolution is the edge-triggered counterpart
// to the standing reconciler. A session may have just reached done while another
// same-issue session is still active; in that case the repair approval is not
// moot and must survive. Reuse the same full-session resolution check before
// staling by issue number so edge and standing paths cannot disagree (#866).
func (o *Orchestrator) reconcileMootRepairApprovalsAfterResolution(s *state.State, issue int, now time.Time, reason string) int {
	resolved, _ := o.repairApprovalIssueResolved(s, issue)
	if !resolved {
		return 0
	}
	return o.reconcileMootRepairApprovals(s, issue, now, reason)
}

func (o *Orchestrator) reconcileCodeLandedSessions(s *state.State) {
	if o == nil || o.cfg == nil || s == nil {
		return
	}

	codeLanded := make([]*state.Session, 0)
	for _, slotName := range sortedStateSessionNames(s) {
		sess := s.Sessions[slotName]
		// #1020: a code_landed session already released for redispatch (docs-only
		// record delivery or an ineffective fix) is terminal for reconciliation —
		// it no longer owns its issue and must never be settled toward done, or a
		// later cycle would re-close the issue the release just re-opened.
		if sess != nil && sess.Status == state.StatusCodeLanded && !sess.ReleasedForRedispatch {
			codeLanded = append(codeLanded, sess)
		}
	}
	if len(codeLanded) == 0 {
		return
	}

	// Delivery is a standing gate, not an edge-triggered post-merge callback.
	// This recovers a crash immediately after GitHub merged, covers UI/manual
	// merges, imports the authoritative SQLite result, and prevents code_landed
	// from closing while the matching delivery is pending or unverified.
	ready := make([]*state.Session, 0, len(codeLanded))
	for _, sess := range codeLanded {
		now := time.Now().UTC()
		if !terminalReconcileDue(sess, now) {
			// Delivery execution is mirrored into project state by the shared
			// executor. Consume an exact verified mirror immediately without
			// polling GitHub again; unchanged pending/terminal approvals wait for
			// the bounded authoritative refresh below (#940).
			if o.reconcileVerifiedDeliveryFromState(s, sess, o.cfg.EffectiveDelivery()) {
				ready = append(ready, sess)
			}
			continue
		}
		if !o.codeLandedPRMerged(sess) {
			continue
		}
		checkedAt := time.Now().UTC()
		sess.LastTerminalReconcileAt = &checkedAt
		o.ensureMergedPRGateSnapshot(s, sess, checkedAt)
		// #1020: before settling, inspect the merged diff. A bug issue whose PR
		// changed only non-functional (docs/record) paths is a record delivery,
		// not a fix: release it for fresh dispatch instead of silencing the issue.
		switch o.classifyMergedCodeLandedDelivery(s, sess) {
		case codeLandedRecordOnly:
			continue // released for redispatch; must not settle the issue
		case codeLandedHold:
			continue // transient classification read error; retry next cycle
		}
		if !o.reconcileCodeLandedDelivery(s, sess) {
			o.syncProject(sess.IssueNumber, github.ProjectStatusLiveVerify)
			continue
		}
		ready = append(ready, sess)
	}
	if len(ready) == 0 {
		return
	}

	if o.cfg.Outcome.PassRequiredForDoneEnabled() {
		if !o.cfg.Outcome.HasHealthSignal() {
			log.Printf("[orch] %d code_landed session(s) need outcome verification, but no health signal is configured", len(ready))
			return
		}
		result := o.checkOutcome(context.Background())
		s.OutcomeHealth = &result
		if result.State != outcome.HealthHealthy {
			log.Printf("[orch] code_landed reconcile held: outcome verifier is %s: %s", result.State, result.Summary)
			now := time.Now().UTC()
			for _, sess := range ready {
				// #1020 independent guard: a real code fix that merged but left the
				// blocking outcome check failing with the SAME fingerprint past the
				// verification deadline is ineffective — release it for redispatch
				// instead of holding the issue in live-verification indefinitely.
				if o.reconcileIneffectiveCodeLanded(s, sess, result, now) {
					continue
				}
				o.syncProject(sess.IssueNumber, github.ProjectStatusLiveVerify)
			}
			return
		}
	}

	for _, sess := range ready {
		log.Printf("[orch] code_landed session for issue #%d passed outcome reconciliation; marking done", sess.IssueNumber)
		if o.markDoneAfterOutcomePass(sess, sess.PRNumber) {
			now := time.Now().UTC()
			stale := s.MarkCloseIssueApprovalsStaleForVerifiedIssue(sess.IssueNumber, now)
			if stale > 0 {
				log.Printf("[orch] expired %d stale close_issue approval(s) for auto-closed issue #%d", stale, sess.IssueNumber)
			}
			o.reconcileMootRepairApprovalsAfterResolution(s, sess.IssueNumber, now,
				fmt.Sprintf("issue #%d resolved (verified merge) — repair worker moot", sess.IssueNumber))
		}
	}
}

// codeLandedVerifyGraceDefault is how long a code_landed session may keep
// failing its blocking outcome check before the merge is judged ineffective
// (#1020). It must exceed one terminalReconcileInterval so the same failure
// fingerprint is observed on at least two independent cycles before conviction.
const codeLandedVerifyGraceDefault = 30 * time.Minute

func (o *Orchestrator) codeLandedVerifyGrace() time.Duration {
	return codeLandedVerifyGraceDefault
}

// codeLandedClassification is the verdict of inspecting a merged code_landed PR
// before it settles its issue (#1020).
type codeLandedClassification int

const (
	codeLandedSettle     codeLandedClassification = iota // proceed to the normal settle path
	codeLandedRecordOnly                                 // released as a docs/record delivery
	codeLandedHold                                       // transient read error; retry next cycle
)

// classifyMergedCodeLandedDelivery inspects a merged code_landed session before
// it settles its issue. For a bug-labelled issue whose merged PR changed only
// non-functional (docs/record) paths, it releases the session for fresh
// dispatch and returns codeLandedRecordOnly — the merge delivered evidence, not
// a fix, so the issue must stay dispatchable rather than being silenced. A
// transient GitHub read failure returns codeLandedHold so the caller retries
// next cycle instead of settling on incomplete evidence. Every other case
// (non-bug issue, or any functional path in the diff) returns codeLandedSettle.
func (o *Orchestrator) classifyMergedCodeLandedDelivery(s *state.State, sess *state.Session) codeLandedClassification {
	if sess == nil || sess.IssueNumber <= 0 || sess.PRNumber <= 0 || sess.ReleasedForRedispatch {
		return codeLandedSettle
	}
	// Cheapest discriminator first: the changed-file set. A merged PR that
	// touches any functional path is a fix and settles exactly as today. Only a
	// fully non-functional diff is worth the extra issue read below, so a normal
	// code PR (and any changed-files read failure) falls straight through to
	// settle without ever calling getIssue — preserving the legacy close path.
	files, err := o.prChangedFiles(sess.PRNumber)
	if err != nil {
		log.Printf("[orch] code_landed classify: changed-files lookup for PR #%d failed; settling on the legacy path: %v", sess.PRNumber, err)
		return codeLandedSettle
	}
	if !pipeline.AllPathsNonFunctional(o.cfg.Supervisor.EffectiveNonFunctionalPaths(), files) {
		return codeLandedSettle
	}
	// The diff is entirely non-functional. Only bug-labelled issues are guarded:
	// a docs/enhancement issue may legitimately be closed by a docs-only PR.
	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		log.Printf("[orch] code_landed classify: issue #%d lookup failed for a non-functional PR #%d; holding (will retry next cycle): %v", sess.IssueNumber, sess.PRNumber, err)
		return codeLandedHold
	}
	if !github.HasLabel(issue, []string{"bug"}) {
		return codeLandedSettle
	}
	o.releaseCodeLandedForRedispatch(s, sess, state.WorkerOutcomeRecordOnlyDelivery,
		fmt.Sprintf("bug issue #%d merged PR #%d changed only non-functional paths (%s) — record delivery, not a fix",
			sess.IssueNumber, sess.PRNumber, summarizeChangedPaths(files)))
	if o.notifier != nil {
		o.notifier.Sendf("♻️ maestro: PR #%d for bug issue #%d changed only documentation/record paths — released for fresh dispatch instead of settling the issue", sess.PRNumber, sess.IssueNumber)
	}
	return codeLandedRecordOnly
}

// reconcileIneffectiveCodeLanded is the independent #1020 guard: a code_landed
// session whose real code change merged but whose blocking outcome check stayed
// failing with the SAME fingerprint past the verification deadline is released
// for fresh dispatch. Deterministic and logged. The deadline is armed the first
// time a given failure fingerprint is observed; a changed fingerprint re-arms
// (a new, different failure is not evidence the merged fix was ineffective).
// Returns true only when it released the session.
func (o *Orchestrator) reconcileIneffectiveCodeLanded(s *state.State, sess *state.Session, result outcome.HealthCheckResult, now time.Time) bool {
	if sess == nil || sess.ReleasedForRedispatch {
		return false
	}
	fp := state.OutcomeFailureFingerprint(result)
	if fp == "" {
		return false // pending/unknown/healthy: not a definite failure, keep holding
	}
	if sess.CodeLandedVerifyDeadline == nil || sess.OutcomeFailureFingerprint != fp {
		deadline := now.Add(o.codeLandedVerifyGrace())
		sess.CodeLandedVerifyDeadline = &deadline
		sess.OutcomeFailureFingerprint = fp
		log.Printf("[orch] issue #%d code_landed but blocking outcome check is failing (%s); verification deadline armed until %s",
			sess.IssueNumber, fp, deadline.Format(time.RFC3339))
		return false
	}
	if !state.CodeLandedIneffective(sess, result, now) {
		return false
	}
	o.releaseCodeLandedForRedispatch(s, sess, state.WorkerOutcomeCodeLandedIneffective,
		fmt.Sprintf("issue #%d code_landed via PR #%d but the blocking outcome check stayed failing with the same fingerprint (%s) past the verification deadline",
			sess.IssueNumber, sess.PRNumber, fp))
	if o.notifier != nil {
		o.notifier.Sendf("♻️ maestro: PR #%d for issue #%d merged but the blocking outcome check never recovered — released for fresh dispatch (code_landed_ineffective)", sess.PRNumber, sess.IssueNumber)
	}
	return true
}

// releaseCodeLandedForRedispatch marks a code_landed session terminal-but-
// released: it keeps its status and history (the PR really did merge) while
// ReleasedForRedispatch drops its issue claim (state.sessionIssueClaim) and
// maps the board to runnable Todo (projectStatusForSession), so the dynamic
// wave dispatches a fresh worker within one cycle. State is persisted eagerly
// so a crash before end-of-tick cannot resurrect the settled-but-ineffective
// claim.
func (o *Orchestrator) releaseCodeLandedForRedispatch(s *state.State, sess *state.Session, workerOutcome, reason string) {
	sess.ReleasedForRedispatch = true
	sess.WorkerOutcome = workerOutcome
	o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
	log.Printf("[orch] %s: %s — released for redispatch", workerOutcome, reason)
	if strings.TrimSpace(o.cfg.StateDir) != "" {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			log.Printf("[orch] release for redispatch: state save failed for issue #%d: %v", sess.IssueNumber, err)
		}
	}
}

// releaseDoneMergedOpenIssueForRedispatch drops the terminal_reconcile claim on
// a StatusDone session whose PR merged but whose GitHub issue stayed open.
// Keeps history; marks ReleasedForRedispatch so dynamic wave can fill again (#1103).
func (o *Orchestrator) releaseDoneMergedOpenIssueForRedispatch(s *state.State, sess *state.Session, reason string) {
	if sess == nil || sess.ReleasedForRedispatch {
		return
	}
	sess.ReleasedForRedispatch = true
	if strings.TrimSpace(sess.WorkerOutcome) == "" {
		sess.WorkerOutcome = state.WorkerOutcomeMergedPRIssueStillOpen
	}
	o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
	log.Printf("[orch] %s: %s", state.WorkerOutcomeMergedPRIssueStillOpen, reason)
	if o.notifier != nil {
		o.notifier.Sendf("♻️ maestro: PR #%d merged but issue #%d is still open — released terminal claim for redispatch", sess.PRNumber, sess.IssueNumber)
	}
	if strings.TrimSpace(o.cfg.StateDir) != "" {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			log.Printf("[orch] release done+merged open issue: state save failed for issue #%d: %v", sess.IssueNumber, err)
		}
	}
}

// tokenBudgetClaimGrace is how long a deterministic token_budget_exceeded stop
// may keep an exclusive issue claim before hands-off fill releases it (#1106).
const tokenBudgetClaimGrace = 30 * time.Minute

func (o *Orchestrator) releaseAgedTokenBudgetClaim(s *state.State, sess *state.Session, now time.Time) {
	if sess == nil || sess.ReleasedForRedispatch {
		return
	}
	if sess.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		return
	}
	finished := sess.FinishedAt
	if finished == nil || finished.IsZero() {
		if sess.StartedAt.IsZero() {
			return
		}
		t := sess.StartedAt.UTC()
		finished = &t
	}
	if now.Sub(finished.UTC()) < tokenBudgetClaimGrace {
		return
	}
	// A legacy cycle may already have promoted this deterministic budget stop to
	// retry_exhausted. Normalize it before release so IssueRetryExhausted cannot
	// outlive the claim and keep selectors blocked forever.
	if sess.Status == state.StatusRetryExhausted {
		sess.Status = state.StatusFailed
	}
	sess.ReleasedForRedispatch = true
	o.syncProject(sess.IssueNumber, github.ProjectStatusTodo)
	log.Printf("[orch] token_budget_exceeded: issue #%d session aged past %s — released for redispatch",
		sess.IssueNumber, tokenBudgetClaimGrace)
	if strings.TrimSpace(o.cfg.StateDir) != "" {
		if err := state.Save(o.cfg.StateDir, s); err != nil {
			log.Printf("[orch] release aged token budget: state save failed for issue #%d: %v", sess.IssueNumber, err)
		}
	}
}

// summarizeChangedPaths renders a bounded, operator-facing list of changed
// paths for a log line.
func summarizeChangedPaths(files []string) string {
	const max = 3
	if len(files) <= max {
		return strings.Join(files, ", ")
	}
	return fmt.Sprintf("%s, … (+%d more)", strings.Join(files[:max], ", "), len(files)-max)
}

// reconcileCodeLandedDelivery returns true only when no approval-gated
// delivery is configured or the approval for this exact PR merge SHA is
// durably executed and verified. Every other status intentionally holds the
// session in code_landed for operator action/reconciliation.
func (o *Orchestrator) reconcileCodeLandedDelivery(s *state.State, sess *state.Session) bool {
	eff := o.cfg.EffectiveDelivery()
	if eff.Mode != config.DeliveryModeApprovalRequired || strings.TrimSpace(eff.Command) == "" {
		return true
	}
	if o.reconcileVerifiedDeliveryFromState(s, sess, eff) {
		return true
	}
	if sess == nil || sess.PRNumber <= 0 {
		log.Printf("[orch] delivery reconcile held: code_landed session has no PR number to pin")
		return false
	}
	approval, err := o.enqueueDeliveryApproval(s, sess, github.PR{Number: sess.PRNumber}, eff)
	if err != nil || approval == nil {
		log.Printf("[orch] delivery reconcile held for PR #%d: %v", sess.PRNumber, err)
		return false
	}
	completed := approval
	if approval.Status != state.ApprovalStatusExecuted || approval.Delivery == nil || !approval.Delivery.Verified {
		completed = o.verifiedDeliveryCovering(s, approval.Delivery)
	}
	if completed == nil || completed.Delivery == nil {
		log.Printf("[orch] delivery reconcile held for PR #%d: approval %s is %s (verified=%v)",
			sess.PRNumber, approval.ID, approval.Status, approval.Delivery != nil && approval.Delivery.Verified)
		return false
	}
	return o.recordVerifiedDelivery(s, sess, completed)
}

// reconcileVerifiedDeliveryFromState consumes the exact verified delivery
// mirror for a code_landed session without any GitHub or repository read. This
// keeps approval execution responsive while unchanged historical delivery gates
// use the bounded terminal reconciliation interval instead of polling the forge
// every orchestrator cycle (#940).
func (o *Orchestrator) reconcileVerifiedDeliveryFromState(s *state.State, sess *state.Session, eff config.DeliveryConfig) bool {
	if eff.Mode != config.DeliveryModeApprovalRequired || strings.TrimSpace(eff.Command) == "" {
		return true
	}
	if s == nil || sess == nil || sess.PRNumber <= 0 {
		return false
	}
	digest := eff.ApprovalDigest()
	var completed *state.Approval
	for i := range s.Approvals {
		candidate := &s.Approvals[i]
		if candidate.Action != state.ApprovalActionDeployProject || candidate.Status != state.ApprovalStatusExecuted ||
			candidate.Delivery == nil || !candidate.Delivery.Verified || candidate.Delivery.PR != sess.PRNumber ||
			!strings.EqualFold(strings.TrimSpace(candidate.Delivery.Repo), strings.TrimSpace(o.cfg.Repo)) ||
			strings.TrimSpace(candidate.Delivery.Project) != strings.TrimSpace(o.cfg.Repo) ||
			strings.TrimSpace(candidate.Delivery.ConfigDigest) != strings.TrimSpace(digest) {
			continue
		}
		if completed == nil || state.CompareDeliveryGeneration(completed.Delivery, completed.CreatedAt, candidate.Delivery, candidate.CreatedAt) < 0 {
			completed = candidate
		}
	}
	return o.recordVerifiedDelivery(s, sess, completed)
}

func (o *Orchestrator) recordVerifiedDelivery(s *state.State, sess *state.Session, completed *state.Approval) bool {
	if sess == nil || completed == nil || completed.Delivery == nil || !completed.Delivery.Verified {
		return false
	}
	if sess.DeploymentFinishedAt == nil {
		finished := completed.Delivery.FinishedAt
		if finished.IsZero() {
			finished = time.Now().UTC()
		}
		finished = finished.UTC()
		sess.DeploymentFinishedAt = &finished
		if strings.TrimSpace(o.cfg.StateDir) != "" {
			if err := state.Save(o.cfg.StateDir, s); err != nil {
				log.Printf("[orch] delivery result matched but deployment timestamp save failed for PR #%d: %v", sess.PRNumber, err)
				return false
			}
		}
	}
	return true
}

// verifiedDeliveryCovering allows a newer verified main revision to satisfy an
// older code_landed session whose own delivery approval was superseded. It is
// fail-closed on ancestry: merge timestamps alone do not prove the newer
// commit contains the older one.
func (o *Orchestrator) verifiedDeliveryCovering(s *state.State, required *state.DeliveryPayload) *state.Approval {
	if s == nil || required == nil || strings.TrimSpace(required.MergedSHA) == "" {
		return nil
	}
	var best *state.Approval
	for i := range s.Approvals {
		candidate := &s.Approvals[i]
		if candidate.Action != state.ApprovalActionDeployProject || candidate.Status != state.ApprovalStatusExecuted ||
			candidate.Delivery == nil || !candidate.Delivery.Verified {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.Delivery.Repo), strings.TrimSpace(required.Repo)) ||
			strings.TrimSpace(candidate.Delivery.Project) != strings.TrimSpace(required.Project) ||
			strings.TrimSpace(candidate.Delivery.ConfigDigest) != strings.TrimSpace(required.ConfigDigest) {
			continue
		}
		contains, err := o.deliveryRevisionContains(required.MergedSHA, candidate.Delivery.MergedSHA)
		if err != nil || !contains {
			continue
		}
		if best == nil || state.CompareDeliveryGeneration(best.Delivery, best.CreatedAt, candidate.Delivery, candidate.CreatedAt) < 0 {
			best = candidate
		}
	}
	return best
}

func (o *Orchestrator) deliveryRevisionContains(ancestor, descendant string) (bool, error) {
	if o.deliveryRevisionContainsFn != nil {
		return o.deliveryRevisionContainsFn(ancestor, descendant)
	}
	ancestor = strings.TrimSpace(ancestor)
	descendant = strings.TrimSpace(descendant)
	if ancestor == descendant && ancestor != "" {
		return true, nil
	}
	if len(ancestor) != 40 || len(descendant) != 40 {
		return false, errors.New("delivery ancestry requires full 40-character commit SHAs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	contains, err := approver.RevisionContains(ctx, o.cfg.Repo, o.cfg.LocalPath, ancestor, descendant)
	if err != nil {
		return false, fmt.Errorf("check delivery revision ancestry in isolated canonical repository: %w", err)
	}
	return contains, nil
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

func (o *Orchestrator) approvalRequiredDeliveryEnabled() bool {
	if o == nil || o.cfg == nil {
		return false
	}
	eff := o.cfg.EffectiveDelivery()
	return eff.Mode == config.DeliveryModeApprovalRequired && strings.TrimSpace(eff.Command) != ""
}

// mergedPRForDoneLikeSession resolves the concrete merged PR, if any, behind a
// session that reached a Done-like path without passing through the normal
// merge callback. It checks every durable identity the session may retain.
// Read errors fail closed: an external/UI merge must never be mistaken for an
// unmerged close merely because GitHub was temporarily unavailable.
func (o *Orchestrator) mergedPRForDoneLikeSession(sess *state.Session) (int, error) {
	if sess == nil {
		return 0, nil
	}
	if sess.PRNumber > 0 {
		merged, err := o.isPRMerged(sess.PRNumber)
		if err != nil {
			return 0, fmt.Errorf("check recorded PR #%d: %w", sess.PRNumber, err)
		}
		if merged {
			return sess.PRNumber, nil
		}
	}
	if sess.LastClosedPRNumber > 0 && sess.LastClosedPRNumber != sess.PRNumber {
		merged, err := o.isPRMerged(sess.LastClosedPRNumber)
		if err != nil {
			return 0, fmt.Errorf("check recorded closed PR #%d: %w", sess.LastClosedPRNumber, err)
		}
		if merged {
			return sess.LastClosedPRNumber, nil
		}
	}
	if branch := strings.TrimSpace(sess.Branch); branch != "" {
		prNumber, err := o.mergedPRForBranch(branch)
		if err != nil {
			return 0, fmt.Errorf("check merged PR for branch %q: %w", branch, err)
		}
		if prNumber > 0 {
			return prNumber, nil
		}
	}
	return 0, nil
}

// canMarkDoneForDelivery is the central backstop for Done-like transitions
// that did not originate in mergeReadyPR. If a merge is discoverable, the
// session first enters code_landed and the exact-SHA approval is persisted;
// only an executed+verified matching delivery may pass this gate.
func (o *Orchestrator) canMarkDoneForDelivery(s *state.State, sess *state.Session, trigger string) bool {
	if !o.approvalRequiredDeliveryEnabled() || sess == nil {
		return true
	}
	prNumber, err := o.mergedPRForDoneLikeSession(sess)
	if err != nil {
		log.Printf("[orch] %s, but required delivery merge identity could not be verified: %v — holding out of done", trigger, err)
		o.syncProject(sess.IssueNumber, github.ProjectStatusLiveVerify)
		return false
	}
	if prNumber <= 0 {
		// A closed/cancelled issue with no merged PR has no product revision to
		// deliver, so existing non-merge completion behavior remains valid.
		return true
	}
	if sess.Status != state.StatusCodeLanded || sess.PRNumber != prNumber {
		o.markCodeLanded(sess, prNumber)
	}
	if !o.reconcileCodeLandedDelivery(s, sess) {
		log.Printf("[orch] %s, but merged PR #%d still requires an executed and verified delivery — holding code_landed", trigger, prNumber)
		return false
	}
	return true
}

func (o *Orchestrator) canMarkDoneForOutcome(s *state.State, sess *state.Session, trigger string) bool {
	if !o.canMarkDoneForDelivery(s, sess, trigger) {
		return false
	}
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
	// #970: project-wide outcome drift only gates a session that owns a merged
	// product revision. A closed/cancelled issue with no discoverable merged PR
	// has nothing left to deploy or live-verify; retaining its prior dead/failed
	// status forever makes a reconciled issue dominate Fleet operator_state and
	// hold a resumable-worktree claim indefinitely. The project outcome remains
	// independently visible and actionable, but this no-delivery session may
	// become terminal. Conversely, a merged revision discovered only through the
	// recorded PR/branch link must still enter code_landed and remain held here.
	if sess != nil {
		prNumber, err := o.mergedPRForDoneLikeSession(sess)
		if err != nil {
			log.Printf("[orch] %s, but merged delivery identity could not be verified while outcome health is %s: %v — holding out of done", trigger, status.HealthState, err)
			return false
		}
		if prNumber <= 0 {
			log.Printf("[orch] %s with no merged delivery identity; project outcome health is %s but does not keep this session non-terminal", trigger, status.HealthState)
			return true
		}
		if sess.Status != state.StatusCodeLanded || sess.PRNumber != prNumber {
			o.markCodeLanded(sess, prNumber)
		}
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
	return o.mergeReadyPRAtExpectedHead(s, slotName, sess, pr, "")
}

func (o *Orchestrator) mergeReadyPRAtExpectedHead(s *state.State, slotName string, sess *state.Session, pr github.PR, expectedHead string) bool {
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
	mergeResult := mergegate.Execute(mergegate.Request{
		StateDir:     o.cfg.StateDir,
		Repo:         o.cfg.Repo,
		IssueNumber:  sess.IssueNumber,
		PRNumber:     pr.Number,
		ExpectedHead: expectedHead,
		Owner:        "orchestrator",
		HoldLabels:   o.operatorGateLabels(),
	}, mergegate.ReadFuncs{
		Issue:         o.finalMergeIssue,
		PRLabels:      o.finalMergePRLabels,
		ReviewThreads: o.finalMergeReviewThreads,
	}, func(headSHA string) error {
		return o.mergePRAtHead(pr.Number, headSHA)
	})
	if mergeResult.Refused {
		action := "Remove the merge hold or resolve every current-head review thread, then let Maestro re-evaluate the PR."
		o.applyOperatorGateHold(sess, pr, operatorGateHold{Name: mergeResult.RefusalName, RequiredAction: action})
		log.Printf("[orch] refusing final merge of PR #%d at compare/claim boundary: %s", pr.Number, mergeResult.RefusalReason)
		return false
	}
	if err := mergeResult.Err; err != nil {
		log.Printf("[orch] merge PR #%d: %v", pr.Number, err)

		// If the branch is behind main (not conflicting, just outdated),
		// rebase the worktree when present; otherwise (worker already cleaned
		// up, or the local rebase fails) fall back to server-side
		// update-branch (#551) so a behind PR still converges to merge
		// instead of dead-ending as an unresolvable conflict.
		// Belt and braces (#1172 M3): the sentinel is the typed contract both
		// transports wrap (gh needle match / Forgejo out-of-date refusal); the
		// legacy text match stays for error paths that predate it.
		if (errors.Is(err, github.ErrMergeNotUpToDate) || strings.Contains(err.Error(), "not up to date")) && o.cfg.AutoRebase {
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
	mergedObservedAt := time.Now().UTC()
	if s != nil {
		s.LastMergeAt = mergedObservedAt
		if mergeInfo, err := o.prMergeInfo(pr.Number); err != nil {
			log.Printf("[orch] PR-gate merge identity for PR #%d unavailable: %v", pr.Number, err)
		} else {
			o.persistMergedPRGateTransition(s, sess, pr.Number, mergeInfo, mergedObservedAt)
		}
	}
	o.markCodeLanded(sess, pr.Number)

	if o.cfg.ShouldCleanupWorktrees() {
		log.Printf("[orch] cleaning up worktree for %s after merge", slotName)
		if err := o.stopWorker(slotName, sess); err != nil {
			log.Printf("[orch] post-merge worker teardown deferred for %s: %v", slotName, err)
		} else {
			sess.Worktree = "" // Mark as cleaned
		}
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

	// Delivery hook (#872): route the post-merge deploy/install through the
	// configured delivery mode. approval_required mints a pending, revision-
	// pinned deploy_project approval and runs nothing until an operator approves.
	deploySucceeded := o.deliverAfterMerge(s, sess, pr)

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
	if o.approvalRequiredDeliveryEnabled() && !deploySucceeded {
		log.Printf("[orch] skipping immediate outcome verification after merge: approval-gated delivery has not completed")
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
		if !o.canMarkDoneForDelivery(s, sess, fmt.Sprintf("outcome verifier passed after PR #%d", prNumber)) {
			log.Printf("[orch] outcome verifier passed after PR #%d, but required delivery is not executed+verified; holding code_landed", prNumber)
			return
		}
		log.Printf("[orch] outcome verifier passed after PR #%d; marking issue #%d done", prNumber, sess.IssueNumber)
		if o.markDoneAfterOutcomePass(sess, prNumber) {
			now := time.Now().UTC()
			stale := s.MarkCloseIssueApprovalsStaleForVerifiedIssue(sess.IssueNumber, now)
			if stale > 0 {
				log.Printf("[orch] expired %d stale close_issue approval(s) for auto-closed issue #%d", stale, sess.IssueNumber)
			}
			o.reconcileMootRepairApprovalsAfterResolution(s, sess.IssueNumber, now,
				fmt.Sprintf("issue #%d resolved (verified merge) — repair worker moot", sess.IssueNumber))
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

// deliverAfterMerge dispatches the post-merge delivery (#872) by the resolved
// delivery mode:
//
//   - automatic (the legacy deploy_cmd behavior and the explicit opt-out from
//     the approval gate): run the delivery command immediately.
//   - approval_required: mint a pending deploy_project approval pinned to the
//     exact merge commit and run NOTHING — an operator approve later executes
//     the delivery through the DeliveryExecutor behind a durable claim. The
//     merge itself does not count as a completed deploy.
//   - disabled: no delivery.
//
// It returns whether a deploy has *succeeded* for the purpose of gating
// immediate outcome verification. When no delivery command completed this cycle
// (approval_required, disabled, or an inert automatic block), that mirrors the
// legacy empty-deploy_cmd path: true only when the project does not require a
// deploy.
func (o *Orchestrator) deliverAfterMerge(s *state.State, sess *state.Session, pr github.PR) bool {
	eff := o.cfg.EffectiveDelivery()
	switch eff.Mode {
	case config.DeliveryModeAutomatic:
		if strings.TrimSpace(eff.Command) == "" {
			break // inert automatic block — nothing to run
		}
		if err := o.runDeliveryCommand(pr.Number, eff); err != nil {
			log.Printf("[orch] deploy command failed for PR #%d: %v", pr.Number, err)
			o.notifier.Sendf("⚠️ maestro: deploy failed after PR #%d merge: %v", pr.Number, err)
			return false
		}
		o.markDeploymentFinished(sess)
		o.notifier.Sendf("🚀 maestro: deploy succeeded after PR #%d merge", pr.Number)
		return true
	case config.DeliveryModeApprovalRequired:
		if strings.TrimSpace(eff.Command) != "" {
			if _, err := o.enqueueDeliveryApproval(s, sess, pr, eff); err != nil {
				log.Printf("[orch] delivery approval mint failed for PR #%d: %v", pr.Number, err)
				o.notifier.Sendf("⚠️ maestro: delivery approval for PR #%d could not be persisted — no deploy ran", pr.Number)
			}
		}
		// Pending approval is never equivalent to a successful deployment, even
		// when outcome.requires_deploy is false. The delivery gate is independent
		// from health/outcome semantics and must keep the session code_landed.
		return false
	}
	// approval_required (delivery pending), disabled, or an inert automatic
	// block: no deploy has completed this cycle.
	return !o.cfg.Outcome.RequiresDeploy
}

// enqueueDeliveryApproval mints (or idempotently re-mints / supersedes) a
// pending deploy_project approval for a merged PR in delivery.mode=
// approval_required (#872). It pins the exact merge commit — main's HEAD after
// the merge — so approval later deploys THAT revision and a superseding merge
// invalidates a stale pending. It executes ZERO delivery command here: the
// approval carries only explicit operator-declared-safe labels, never command,
// target/rollback execution text, local paths, output, or error strings.
func (o *Orchestrator) enqueueDeliveryApproval(s *state.State, sess *state.Session, pr github.PR, eff config.DeliveryConfig) (*state.Approval, error) {
	if s == nil {
		return nil, errors.New("delivery approval requires state")
	}
	mergeInfo, err := o.prMergeInfo(pr.Number)
	mergedSHA := strings.TrimSpace(mergeInfo.SHA)
	if err != nil || mergedSHA == "" {
		// Without the pinned merge commit we cannot mint a revision-safe
		// delivery approval — never fall back to deploying whatever revision is
		// at local_path. Surface it and skip; a later merge re-mints once the
		// revision is resolvable.
		log.Printf("[orch] delivery: could not resolve merge commit SHA for PR #%d: %v — skipping delivery approval mint", pr.Number, err)
		o.notifier.Sendf("⚠️ maestro: could not pin merge commit for PR #%d delivery — no deploy approval created", pr.Number)
		return nil, fmt.Errorf("resolve PR #%d merge commit: %w", pr.Number, err)
	}
	mergedSHA = strings.TrimSpace(mergedSHA)
	if len(mergedSHA) != 40 {
		return nil, fmt.Errorf("PR #%d merge commit is not a full SHA: %q", pr.Number, mergedSHA)
	}
	issue := 0
	if sess != nil {
		issue = sess.IssueNumber
	}
	now := time.Now().UTC()
	payload := state.DeliveryPayload{
		Project:           o.cfg.Repo,
		Repo:              o.cfg.Repo,
		PR:                pr.Number,
		Issue:             issue,
		MergedSHA:         mergedSHA,
		MergedAt:          mergeInfo.MergedAt,
		TargetLabel:       eff.TargetLabel,
		VerificationLabel: eff.VerificationLabel,
		RollbackLabel:     eff.RollbackLabel,
		TimeoutMinutes:    eff.TimeoutMinutes,
		ConfigDigest:      eff.ApprovalDigest(),
		ExpiresAt:         now.Add(eff.EffectiveApprovalTimeout()),
	}
	before := len(s.Approvals)
	approval := s.RecordDeliveryApproval(payload, now)
	if approval == nil {
		return nil, errors.New("state refused delivery approval")
	}
	// Only a pending row minted by THIS call may create a new authoritative
	// ledger record. An approval returned from the JSON mirror is not an
	// authorization source: after a DB-path mistake, DB loss, or mirror tamper it
	// may already say approved/executing/terminal. Re-seeding that status into an
	// empty ledger would bypass the operator claim (and could replay a delivery).
	// Existing rows remain recoverable because persistDeliveryApproval first reads
	// the configured ledger and mirrors its authoritative copy.
	freshlyMintedPending := len(s.Approvals) == before+1 && approval.Status == state.ApprovalStatusPending
	approval, err = o.persistDeliveryApproval(s, approval, now, freshlyMintedPending)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(eff.TargetLabel)
	if len(s.Approvals) > before && approval.Status == state.ApprovalStatusPending {
		log.Printf("[orch] delivery: pending deploy_project approval %s for PR #%d @ %s → %s (awaiting operator approval)", approval.ID, pr.Number, shortHeadSHA(mergedSHA), target)
		o.notifier.Sendf("🔔 maestro: PR #%d merged — delivery to %s awaits approval (%s, revision %s)", pr.Number, target, approval.ID, shortHeadSHA(mergedSHA))
	}
	return approval, nil
}

// persistDeliveryApproval makes the SQLite delivery ledger authoritative even
// when the generic approval UI is configured for JSON mode. PutDelivery seeds
// the new generation and supersedes older actionable generations atomically
// against execution claims. The authoritative row is then mirrored into JSON
// and saved immediately, closing the merge→end-of-cycle crash window.
func (o *Orchestrator) persistDeliveryApproval(s *state.State, approval *state.Approval, now time.Time, freshlyMintedPending bool) (*state.Approval, error) {
	if strings.TrimSpace(o.cfg.StateDir) == "" {
		// Lightweight unit callers have no durable project state. Production
		// configs always carry StateDir and take the authoritative path below.
		return approval, nil
	}
	dbPath := strings.TrimSpace(o.approvalsBinding.DBPath)
	if dbPath == "" {
		dbPath = approvalstore.DefaultDBPath()
	}
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open delivery approval store: %w", err)
	}
	defer store.Close()
	binding := approvalstore.RowBinding{
		Project:  o.repo,
		Repo:     o.repo,
		StateDir: o.cfg.StateDir,
	}
	if binding.Project == "" {
		binding.Project = o.cfg.Repo
		binding.Repo = o.cfg.Repo
	}
	// SQLite is the authorization source after first mint. Never reconstruct a
	// missing row from a pre-existing JSON mirror: an approved mirror in a fresh
	// or wrong DB would otherwise become immediately executable. A restart after
	// the normal DB-first/state-second commit point takes the existing-row branch
	// and remains recoverable.
	if _, getErr := store.Get(context.Background(), o.cfg.StateDir, approval.ID); getErr != nil {
		if !errors.Is(getErr, state.ErrApprovalNotFound) {
			return nil, fmt.Errorf("read delivery approval %s from authoritative ledger: %w", approval.ID, getErr)
		}
		if !freshlyMintedPending || approval.Status != state.ApprovalStatusPending {
			return nil, fmt.Errorf("delivery approval %s is absent from the configured authoritative ledger; refusing to seed it from the state mirror", approval.ID)
		}
		if _, err := store.PutDelivery(context.Background(), approval, binding, now); err != nil {
			return nil, fmt.Errorf("seed delivery approval %s: %w", approval.ID, err)
		}
	}
	rows, err := store.List(context.Background(), o.cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("read back delivery approvals: %w", err)
	}
	var authoritative *state.Approval
	for _, row := range rows {
		if row == nil || row.Action != state.ApprovalActionDeployProject || row.Delivery == nil {
			continue
		}
		if row.Delivery.DeliveryExpired(now) &&
			(row.Status == state.ApprovalStatusPending || row.Status == state.ApprovalStatusApproved) {
			row, err = store.MarkStale(context.Background(), o.cfg.StateDir, row.ID, now, "delivery approval expired")
			if err != nil {
				return nil, fmt.Errorf("expire delivery approval %s: %w", row.ID, err)
			}
		}
		mirrored := replaceStateApproval(s, row)
		if row.ID == approval.ID {
			authoritative = mirrored
		}
	}
	if authoritative == nil {
		return nil, fmt.Errorf("delivery approval %s missing after seed", approval.ID)
	}
	approval = authoritative
	if err := state.Save(o.cfg.StateDir, s); err != nil {
		return nil, fmt.Errorf("save state after delivery approval %s: %w", approval.ID, err)
	}
	return approval, nil
}

func replaceStateApproval(s *state.State, authoritative *state.Approval) *state.Approval {
	if s == nil || authoritative == nil {
		return authoritative
	}
	for i := range s.Approvals {
		if s.Approvals[i].ID == authoritative.ID {
			s.Approvals[i] = *authoritative
			return &s.Approvals[i]
		}
	}
	s.Approvals = append(s.Approvals, *authoritative)
	return &s.Approvals[len(s.Approvals)-1]
}

// runDeployCmd executes the configured delivery command with its timeout. It
// resolves the effective delivery config so the legacy deploy_cmd and an
// explicit delivery.mode=automatic block share one execution path.
func (o *Orchestrator) runDeployCmd(prNumber int) error {
	return o.runDeliveryCommand(prNumber, o.cfg.EffectiveDelivery())
}

// runDeliveryCommand runs eff.Command in the project checkout with eff's
// timeout, bounding it with a context deadline and folding a timeout into a
// distinct error.
func (o *Orchestrator) runDeliveryCommand(prNumber int, eff config.DeliveryConfig) error {
	timeout := eff.EffectiveTimeout()
	minutes := int(timeout / time.Minute)
	log.Printf("[orch] running automatic delivery after PR #%d merge (timeout %dm)", prNumber, minutes)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := approver.RunBoundedShell(ctx, o.cfg.LocalPath, eff.Command, state.DefaultDeliveryOutputLimit)
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("deploy command timed out after %d minutes", minutes)
	}
	if err != nil {
		return errors.New("deploy command failed")
	}
	if verify := strings.TrimSpace(eff.VerifyCommand); verify != "" {
		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), timeout)
		_, verifyErr := approver.RunBoundedShell(verifyCtx, o.cfg.LocalPath, verify, state.DefaultDeliveryOutputLimit)
		verifyCancel()
		if verifyErr != nil {
			return errors.New("delivery verifier failed")
		}
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

	now := time.Now().UTC()
	// Advance the resolved watermark so the stale-trigger watchdog (#807) does not
	// later flag this now-consumed deploy: its trigger marker legitimately outlives
	// the result file we just cleared, and without this watermark that surviving
	// marker would eventually look like a trigger that never produced a result.
	if err := selfdeploy.RecordResolved(o.cfg.StateDir, now); err != nil {
		log.Printf("[orch] self-deploy resolved-watermark write failed: %v", err)
	}

	finding := selfdeploy.Finding(res, o.cfg.Repo, now)
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

// selfDeployStaleTimeout is how long after a trigger a missing result file is
// treated as a dead deploy (#807). The transient unit's RuntimeMaxSec hard
// backstop is 2× the deploy budget (see selfdeploy.TriggerCommand), past which
// systemd has definitively killed the unit — so a trigger older than that with
// no result cannot be an in-flight deploy. Match that backstop so the watchdog
// never false-flags a slow-but-live deploy.
func (o *Orchestrator) selfDeployStaleTimeout() time.Duration {
	return time.Duration(o.cfg.SelfDeploy.EffectiveTimeoutMinutes()) * 2 * time.Minute
}

// maybeSurfaceStaleSelfDeploy is the watchdog for the silent-no-op failure that
// motivated #807: a merge triggered a self-deploy, but the detached transient
// unit died without ever writing a result file (SIGKILLed at RuntimeMaxSec, or
// crashed before its own EXIT trap could run), so the fleet keeps running the
// previous binary with nothing but a lone journald line to show for it. The
// normal consume path only surfaces deploys that DID write a result; this makes
// the absence itself loud.
//
// It runs right after consumeSelfDeployResult (which clears any present result
// and advances the resolved watermark), so a trigger seen here with no result
// and older than the RuntimeMaxSec backstop is a genuine silent loss. On
// detection it records a supervisor finding and advances the resolved watermark
// past the dead trigger, so the finding surfaces once (not every cycle) and a
// later merge/main-advance re-triggers a fresh deploy. Returns true when it
// recorded a finding (state changed). No-op unless self_deploy is enabled.
func (o *Orchestrator) maybeSurfaceStaleSelfDeploy(s *state.State) bool {
	if o == nil || o.cfg == nil || !o.cfg.SelfDeploy.Enabled || s == nil {
		return false
	}
	now := time.Now().UTC()
	pr, age, stale := selfdeploy.StaleTrigger(o.cfg.StateDir, now, o.selfDeployStaleTimeout())
	if !stale {
		return false
	}
	// Advance the resolved watermark past this dead trigger BEFORE recording, so
	// even if the save below fails the watchdog does not re-flag the same trigger
	// every cycle. Losing a single finding is the lesser failure mode; a later
	// merge re-triggers regardless.
	if err := selfdeploy.RecordResolved(o.cfg.StateDir, now); err != nil {
		log.Printf("[orch] self-deploy stale-trigger watermark write failed: %v", err)
	}
	finding := selfdeploy.StaleTriggerFinding(pr, age, o.cfg.Repo, now)
	s.RecordSupervisorDecision(finding, state.DefaultSupervisorDecisionLimit)
	log.Printf("[orch] %s", finding.Summary)
	o.notifier.Sendf("❗ maestro: self-deploy produced no result %s after triggering (PR #%d) — the deploy unit died; fleet still on the previous binary", age.Round(time.Minute), pr)
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
	prs, err := o.listOpenPRsForCycle()
	if err != nil {
		log.Printf("[orch] list PRs (rebase): %v", err)
		return
	}

	branchToPR := make(map[string]github.PR)
	numberToPR := make(map[int]github.PR)
	for _, pr := range prs {
		branchToPR[pr.HeadRefName] = pr
		numberToPR[pr.Number] = pr
	}
	owners := canonicalMergeFlowOwners(s, branchToPR, numberToPR)

	for slotName, sess := range s.Sessions {
		pr, hasPR := mergeFlowPRForSession(sess, branchToPR, numberToPR)
		if hasPR {
			if owner := owners[pr.Number]; owner != "" && owner != slotName {
				log.Printf("[orch] PR #%d rebase lifecycle is owned by canonical session %s; skipping historical session %s", pr.Number, owner, slotName)
				continue
			}
		}

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
	sess.FailingCheckContext = "" // #857: rebase-conflict retry carries no failing-check excerpt
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

// capacityInput builds the state.CapacityInput from a project config so the
// spawn budget is computed by the single shared state.Capacity source of truth.
func capacityInput(cfg *config.Config) state.CapacityInput {
	return state.CapacityInput{
		MaxParallel:          cfg.MaxParallel,
		MaxLiveWorkers:       cfg.MaxLiveWorkers,
		MaxConcurrentByState: cfg.MaxConcurrentByState,
	}
}

// availableSlots calculates how many new workers can be started. It delegates to
// state.Capacity, which separates live implementation workers from pr_open
// PR-gate sessions: with max_live_workers>0, gate-bound sessions no longer
// consume spawn capacity, so a long queue keeps dispatching implementation work
// instead of stalling behind a handful of open PRs (#814). The active parameter
// is retained for call-site clarity but Capacity recomputes from live state.
func availableSlots(cfg *config.Config, s *state.State, active int) int {
	_ = active
	return s.Capacity(capacityInput(cfg)).AvailableSlots
}

// startNewWorkers picks eligible issues and starts workers for them
func (o *Orchestrator) listOpenIssues(labels []string) ([]github.Issue, error) {
	if o.listOpenIssuesFn != nil {
		return o.listOpenIssuesFn(labels)
	}
	if o.readSource != nil {
		return o.readSource.ListOpenIssues(labels)
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

func (o *Orchestrator) startClaimedWorker(cfg *config.Config, s *state.State, issue github.Issue, promptBase, backend, slot string) (string, error) {
	if cfg == nil {
		cfg = o.cfg
	}
	if o.workerStartClaimedFn != nil {
		return o.workerStartClaimedFn(cfg, s, o.repo, issue, promptBase, backend, slot)
	}
	return worker.StartReserved(cfg, s, o.repo, issue, promptBase, backend, slot)
}

func issueHasLabel(issue github.Issue, label string) bool {
	for _, issueLabel := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(issueLabel.Name), label) {
			return true
		}
	}
	return false
}

func isOutcomeRepairIssue(issue github.Issue) bool {
	return issueHasLabel(issue, outcome.OutcomeRepairLabel) ||
		strings.Contains(issue.Body, outcome.OutcomeRepairMarkerPrefix)
}

func pipelineConfigForIssue(base *config.Config, issue github.Issue) (*config.Config, bool, bool) {
	if base == nil {
		return base, false, false
	}
	pipelineFull := issueHasLabel(issue, pipelineFullLabel)
	pipelineAdvised := issueHasLabel(issue, pipelineAdvisedLabel)
	if !pipelineFull && !pipelineAdvised {
		return base, false, false
	}
	cfg := *base
	cfg.Pipeline.Enabled = true
	cfg.Pipeline.Planner.Enabled = true
	cfg.Pipeline.Validator.Enabled = true
	if pipelineAdvised {
		cfg.Pipeline.Advisor.Enabled = true
	}
	return &cfg, pipelineFull, pipelineAdvised
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
	if s.IssueInProgress(issueNumber) && s.IssueHasNonFreshClaim(issueNumber) {
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
	if label, ok := matchingIssueLabel(issue, o.operatorGateLabels()); ok {
		return fmt.Sprintf("issue #%d is held by operator gate label %q", issue.Number, label)
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
	if _, ok := s.ActiveRepairDispatchApproval(issueNumber, supervisor.ActionSpawnRepairWorker); ok {
		return true
	}
	if _, ok := s.ActiveRepairDispatchApproval(issueNumber, supervisor.ActionSpawnReviewRepair); ok {
		return true
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil || decision.RecommendationDropped() {
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

type spawnRepairDispatch struct {
	target     *state.SupervisorTarget
	approvalID string
}

type spawnRepairGateDecision struct {
	actionable bool
	stale      bool
	reason     string
}

func (o *Orchestrator) resolveSpawnRepairDispatch(s *state.State, issueNumber int) *spawnRepairDispatch {
	if s == nil || issueNumber <= 0 {
		return nil
	}
	if approval, ok := s.ActiveRepairDispatchApproval(issueNumber, supervisor.ActionSpawnRepairWorker); ok {
		return &spawnRepairDispatch{target: approval.Target, approvalID: approval.ID}
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil || decision.RecommendationDropped() || decision.RecommendedAction != supervisor.ActionSpawnRepairWorker || decision.Target == nil || decision.Target.Issue != issueNumber {
		return nil
	}
	if decision.RequiresApproval || decision.Risk == supervisor.RiskApprovalGated {
		return nil
	}
	return &spawnRepairDispatch{target: decision.Target}
}

// dispatchSpawnRepairWorker revalidates the durable issue/session reservation
// and repairs that exact session. It never allocates a second slot or removes a
// competing worktree.
func (o *Orchestrator) dispatchSpawnRepairWorker(s *state.State, issue github.Issue, dispatch *spawnRepairDispatch) bool {
	if s == nil || dispatch == nil || dispatch.target == nil {
		return false
	}
	target := dispatch.target
	slot := strings.TrimSpace(target.Session)
	if slot == "" {
		reason := fmt.Sprintf("issue #%d repair reservation has no target session", issue.Number)
		log.Printf("[orch] refusing repair dispatch: %s", reason)
		o.staleInvalidSpawnRepairApproval(s, dispatch, reason)
		return false
	}
	sess, ok := s.SessionAt(slot)
	if !ok || sess.IssueNumber != issue.Number {
		reason := fmt.Sprintf("issue #%d reserved session %s is missing or belongs to another issue", issue.Number, slot)
		log.Printf("[orch] refusing repair dispatch: %s", reason)
		o.staleInvalidSpawnRepairApproval(s, dispatch, reason)
		return false
	}
	if target.PR > 0 && sess.PRNumber != target.PR {
		reason := fmt.Sprintf("issue #%d reserved PR #%d no longer matches session %s PR #%d", issue.Number, target.PR, slot, sess.PRNumber)
		log.Printf("[orch] refusing repair dispatch: %s", reason)
		o.staleInvalidSpawnRepairApproval(s, dispatch, reason)
		return false
	}
	for _, claim := range s.ActiveIssueClaims() {
		if claim.IssueNumber != issue.Number || claim.Session == "" || claim.Session == slot {
			continue
		}
		// Approval-derived claims are durable intent, not proof that their
		// reserved session still exists or remains canonical. Retire an invalid
		// sibling reservation before comparing it with this valid exact repair;
		// otherwise lexical claim order can stale both approvals and lose the
		// one recovery that was still safe to dispatch.
		seenInvalidApprovals := make(map[string]struct{})
		for claim.Kind == state.IssueClaimRepairDispatch {
			claimed, exists := s.SessionAt(claim.Session)
			if exists && claimed.IssueNumber == issue.Number && (claim.PRNumber <= 0 || claimed.PRNumber == claim.PRNumber) {
				break
			}
			// More than one obsolete approval can reserve the same missing or
			// mismatched sibling session. Peel every invalid approval-derived
			// claim until either no claim remains or a real session claim is
			// revealed; validating only the first revealed approval loses the
			// selected canonical repair on the next stale sibling.
			if claim.ApprovalID == "" {
				break
			}
			if _, repeated := seenInvalidApprovals[claim.ApprovalID]; repeated {
				break
			}
			seenInvalidApprovals[claim.ApprovalID] = struct{}{}
			reason := fmt.Sprintf("issue #%d competing repair reservation %s is invalid: session missing, belongs to another issue, or no longer matches PR #%d", issue.Number, claim.Session, claim.PRNumber)
			o.staleInvalidRepairApproval(s, claim.ApprovalID, reason)
			// Approval reservations suppress the underlying session claim.
			// Rebuild claims after each stale transition so a real
			// running/open-PR session cannot remain hidden behind a chain of
			// obsolete approvals.
			revealed, stillClaimed := activeIssueClaimForSession(s, issue.Number, claim.Session)
			if !stillClaimed {
				claim = state.IssueClaim{}
				break
			}
			claim = revealed
		}
		if claim.Session == "" {
			continue
		}
		// A completed older PR can retain the issue's terminal-reconciliation
		// claim until GitHub closes the issue. That claim must still prevent a
		// fresh implementation dispatch, but it must not veto an explicitly
		// approved repair of a newer, already-existing canonical PR/session.
		// The repair reservation is exact (issue + session + PR), so ignoring
		// only a chronologically earlier done session cannot create another
		// worker identity. Other active/open/retry claims continue to fail
		// closed below.
		if claim.Kind == state.IssueClaimTerminalReconcile &&
			claim.Status == string(state.StatusDone) &&
			claim.PRNumber > 0 && target.PR > claim.PRNumber {
			prior, exists := s.SessionAt(claim.Session)
			if exists && prior.Status == state.StatusDone {
				log.Printf("[orch] ignoring older terminal claim on session %s / PR #%d while repairing reserved newer session %s / PR #%d for issue #%d",
					claim.Session, claim.PRNumber, slot, target.PR, issue.Number)
				continue
			}
		}
		reason := fmt.Sprintf("issue #%d reserved session %s is superseded by competing canonical claim on session %s (%s)", issue.Number, slot, claim.Session, claim.Reason)
		log.Printf("[orch] refusing repair dispatch: %s", reason)
		o.staleInvalidSpawnRepairApproval(s, dispatch, reason)
		return false
	}

	// A prior dispatch may have reached the worker and persisted the running
	// session before its approval terminal transition. Reconcile the approval
	// without restarting an already-running target.
	if sess.Status == state.StatusRunning {
		o.resolveSpawnRepairApproval(s, dispatch.approvalID, slot, target.PR, "reserved session already running")
		return false
	}
	if target.PR > 0 {
		gate := o.revalidateSpawnRepairPR(s, target)
		if !gate.actionable {
			log.Printf("[orch] refusing repair dispatch for issue #%d / PR #%d on %s: %s", issue.Number, target.PR, slot, gate.reason)
			if gate.stale {
				o.staleInvalidSpawnRepairApproval(s, dispatch, gate.reason)
			}
			return false
		}
		log.Printf("[orch] current PR gate authorizes in-place repair for issue #%d / PR #%d on %s: %s", issue.Number, target.PR, slot, gate.reason)
	}

	backend := strings.TrimSpace(sess.Backend)
	if backend == "" {
		backend = o.cfg.Model.EffectiveDefault()
	}
	previousBackend := backend
	// A current model:<backend> label is an explicit operator routing decision
	// and outranks the backend recorded on the failed attempt. This matters for
	// retained-worktree recovery: changing model:claude to model:sol must resume
	// the same slot/worktree on Sol, not silently replay the stale Fable route.
	// Without an explicit label, keep the already-selected session backend so an
	// in-place recovery does not forget a successful fallover route (#911).
	var explicitSelection *router.BackendDecision
	if labelBackend := router.BackendFromLabels(issue); labelBackend != "" {
		decision := o.resolveBackendDecision(issue)
		if decision.Backend == labelBackend {
			if _, exists := o.cfg.Model.Backends[labelBackend]; !exists {
				log.Printf("[orch] refusing repair dispatch for issue #%d on %s: explicit backend %s is not configured", issue.Number, slot, labelBackend)
				return false
			}
			if blockedBy, retryAt := o.dispatchBackendBlock(s, labelBackend, decision.Model, time.Now().UTC()); blockedBy != "" {
				log.Printf("[orch] refusing repair dispatch for issue #%d on %s: explicit backend %s is blocked (%s%s)", issue.Number, slot, labelBackend, blockedBy, retryAfterHint(retryAt))
				return false
			}
			if labelBackend != backend {
				log.Printf("[orch] issue #%d repair recovery honors current model label: %s → %s on retained session %s", issue.Number, backend, labelBackend, slot)
			}
			backend = labelBackend
			explicitSelection = &decision
		}
	}
	promptBase := o.selectPrompt(issue)
	var err error
	if sess.PRNumber > 0 {
		if strings.TrimSpace(sess.Worktree) == "" {
			// Cleanup can clear the persisted Worktree field after a terminal
			// attempt even when an operator has restored the exact slot worktree
			// from the canonical PR branch. Reattach only the deterministic
			// <worktree_base>/<slot> path and require git worktree metadata; never
			// discover or allocate a different slot here.
			restored := filepath.Join(o.cfg.WorktreeBase, slot)
			if _, statErr := os.Stat(filepath.Join(restored, ".git")); statErr == nil {
				sess.Worktree = restored
				log.Printf("[orch] reattached restored worktree %s to reserved repair session %s / PR #%d", restored, slot, sess.PRNumber)
			} else {
				// Cleanup may have removed the worktree directory while retaining
				// the canonical local branch. Record the deterministic path and let
				// the preserving recovery helper recreate that exact worktree; it
				// fails closed if the branch is unavailable or another worktree owns it.
				sess.Worktree = restored
			}
		}
		if o.respawnInPlaceFn != nil {
			// Synthetic tests provide their own in-place executor and paths.
			// Production has no hook and always takes the checkpointing,
			// missing-worktree-restoring path below.
			err = o.respawnInPlaceWithConfig(o.cfg, slot, sess, issue, promptBase, backend)
		} else {
			err = o.respawnPreservingWorktreeWithConfig(o.cfg, slot, sess, issue, promptBase, backend)
		}
	} else {
		err = o.respawnPreservingWorktreeWithConfig(o.cfg, slot, sess, issue, promptBase, backend)
	}
	if err != nil {
		log.Printf("[orch] repair dispatch for issue #%d on %s failed: %v", issue.Number, slot, err)
		return false
	}
	if explicitSelection != nil {
		selection := o.policyBackendSelection(*explicitSelection)
		selection.PreviousBackend = previousBackend
		sess.BackendSelection = selection
		sess.Backend = backend
	}
	sess.NextRetryAt = nil
	o.resolveSpawnRepairApproval(s, dispatch.approvalID, slot, target.PR, "repair worker dispatched")
	if o.syncProject(issue.Number, github.ProjectStatusInProgress) {
		s.MarkProjectStatusSynced(issue.Number, string(github.ProjectStatusInProgress), time.Now().UTC())
	}
	o.notifier.Sendf("🔄 maestro: repairing issue #%d in place on worker %s: %s", issue.Number, slot, issue.Title)
	return true
}

// revalidateSpawnRepairPR prevents a delayed approval or supervisor decision
// from replaying a repair against a PR state that no longer exists. The live
// GitHub head and gates, not the state captured when the decision was minted,
// authorize worker spend. A current CI failure, merge conflict, or blocking
// review finding is actionable; pending/green gates make an old repair intent
// stale. Read errors and unknown states fail closed without consuming the
// approval so a later healthy control-loop read can retry safely.
func (o *Orchestrator) revalidateSpawnRepairPR(s *state.State, target *state.SupervisorTarget) spawnRepairGateDecision {
	if target == nil || target.PR <= 0 {
		return spawnRepairGateDecision{actionable: true, reason: "repair has no PR gate to revalidate"}
	}
	pr := target.PR
	currentHead, err := o.prHeadSHA(pr)
	if err != nil || strings.TrimSpace(currentHead) == "" {
		return spawnRepairGateDecision{reason: fmt.Sprintf("current PR head is unavailable: %v", err)}
	}
	currentHead = strings.TrimSpace(currentHead)
	boundHead := strings.TrimSpace(target.HeadSHA)
	if boundHead != "" && !strings.EqualFold(boundHead, currentHead) {
		return spawnRepairGateDecision{
			stale:  true,
			reason: fmt.Sprintf("repair intent was bound to head %s, but current head is %s", shortHeadSHA(boundHead), shortHeadSHA(currentHead)),
		}
	}

	rollup, err := o.prCheckRollup(pr)
	if err != nil {
		return spawnRepairGateDecision{reason: fmt.Sprintf("current PR checks are unavailable: %v", err)}
	}
	if observedHead := strings.TrimSpace(rollup.HeadSHA); observedHead != "" && !strings.EqualFold(observedHead, currentHead) {
		return spawnRepairGateDecision{reason: "PR head changed while current checks were being read"}
	}
	if !rollup.Complete {
		return spawnRepairGateDecision{reason: "current PR check rollup is incomplete; repair authority remains retryable"}
	}
	ci := strings.ToLower(strings.TrimSpace(rollup.Verdict))
	actionableReason := ""
	if ci == "failure" {
		actionableReason = "current-head CI is failing"
	}

	if actionableReason == "" {
		mergeable, mergeState, mergeErr := o.prMergeStatus(pr)
		if mergeErr != nil {
			return spawnRepairGateDecision{reason: fmt.Sprintf("current PR merge state is unavailable: %v", mergeErr)}
		}
		if strings.EqualFold(strings.TrimSpace(mergeable), "CONFLICTING") || strings.EqualFold(strings.TrimSpace(mergeState), "dirty") {
			actionableReason = "current PR head has a merge conflict"
		}
	}
	// A current-head CI run in progress is not repair authority. In particular,
	// do not let a stale/legacy review verdict override that fresh pending state
	// and spend another worker before the exact-head checks and review settle.
	// A real merge conflict above remains immediately actionable.
	if actionableReason == "" && ci == "pending" {
		return spawnRepairGateDecision{stale: true, reason: "current PR head checks are pending; no repair is presently actionable"}
	}

	reviewPending := false
	if actionableReason == "" {
		review, reviewErr := o.prReviewGateVerdict(pr)
		if reviewErr != nil {
			return spawnRepairGateDecision{reason: fmt.Sprintf("current PR review gate is unavailable: %v", reviewErr)}
		}
		if !review.Pending && !review.Passed {
			actionableReason = "current PR head has blocking review findings"
		}
		reviewPending = review.Pending
	}

	latestHead, err := o.prHeadSHA(pr)
	if err != nil || !strings.EqualFold(strings.TrimSpace(latestHead), currentHead) {
		return spawnRepairGateDecision{reason: "PR head changed while repair gates were being revalidated"}
	}
	if actionableReason != "" {
		return spawnRepairGateDecision{actionable: true, reason: actionableReason}
	}
	// A green open draft is still explicitly incomplete. Supervisor already
	// treats draft PRs as repairable, so the executor must not reinterpret green
	// CI as terminal and strand a deliberate WIP forever. Re-read this mutable
	// metadata from the exact PR at actuation time (not the cycle's cached list),
	// and wait rather than spend while a review is still in progress. A green
	// non-draft PR remains stale repair intent and proceeds through merge gates.
	if ci == "success" && !reviewPending {
		details, detailsErr := o.prDetails(pr)
		if detailsErr != nil {
			return spawnRepairGateDecision{reason: fmt.Sprintf("current PR draft state is unavailable: %v", detailsErr)}
		}
		if !strings.EqualFold(strings.TrimSpace(details.State), "OPEN") {
			return spawnRepairGateDecision{stale: true, reason: "current PR is no longer open"}
		}
		if details.IsDraft {
			if o.automaticOutcomeRecoveryOwnsFailure(s) {
				return spawnRepairGateDecision{stale: true, reason: "automatic outcome recovery owns the failing project outcome; green draft repair is not independently actionable"}
			}
			return spawnRepairGateDecision{actionable: true, reason: "current PR remains an explicit draft/WIP continuation"}
		}
	}

	switch ci {
	case "pending":
		return spawnRepairGateDecision{stale: true, reason: "current PR head checks are pending; no repair is presently actionable"}
	case "success":
		return spawnRepairGateDecision{stale: true, reason: "current PR head is green and has no actionable conflict or review blocker"}
	default:
		return spawnRepairGateDecision{reason: fmt.Sprintf("current PR check verdict %q is not authoritative", ci)}
	}
}

func (o *Orchestrator) automaticOutcomeRecoveryOwnsFailure(s *state.State) bool {
	return o != nil && o.cfg != nil && o.cfg.Outcome.AutomaticRecoveryEnabled() &&
		s != nil && s.OutcomeHealth != nil && s.OutcomeHealth.State == outcome.HealthFailing &&
		s.OutcomeRecovery.OwnsActiveFailure(time.Now().UTC())
}

func activeIssueClaimForSession(s *state.State, issueNumber int, slot string) (state.IssueClaim, bool) {
	if s == nil || issueNumber <= 0 || strings.TrimSpace(slot) == "" {
		return state.IssueClaim{}, false
	}
	for _, claim := range s.ActiveIssueClaims() {
		if claim.IssueNumber == issueNumber && claim.Session == slot {
			return claim, true
		}
	}
	return state.IssueClaim{}, false
}

// staleInvalidSpawnRepairApproval makes an irrecoverably invalid exact
// reservation terminal in both the project state and the SQLite approval
// mirror. Without this convergence an awaiting_dispatch record is selected and
// refused every orchestrator cycle forever, presenting a false operator gate
// and burning control-loop work even though no safe dispatch is possible.
func (o *Orchestrator) staleInvalidSpawnRepairApproval(s *state.State, dispatch *spawnRepairDispatch, reason string) {
	if s == nil || dispatch == nil || strings.TrimSpace(dispatch.approvalID) == "" {
		return
	}
	o.staleInvalidRepairApproval(s, dispatch.approvalID, reason)
}

func (o *Orchestrator) staleInvalidRepairApproval(s *state.State, approvalID, reason string) {
	if s == nil || strings.TrimSpace(approvalID) == "" {
		return
	}
	now := time.Now().UTC()
	if !s.StaleActiveRepairDispatchApproval(approvalID, now, reason) {
		return
	}
	log.Printf("[orch] reconciled invalid repair approval %s as stale: %s", approvalID, reason)
	o.mirrorRepairApprovalTerminal(approvalID, now, reason)
}

func (o *Orchestrator) resolveSpawnRepairApproval(s *state.State, approvalID, slot string, pr int, outcome string) {
	if strings.TrimSpace(approvalID) == "" {
		return
	}
	now := time.Now().UTC()
	reason := fmt.Sprintf("repair worker %s for PR #%d: %s — approval consumed", slot, pr, outcome)
	if pr <= 0 {
		reason = fmt.Sprintf("repair worker %s: %s — approval consumed", slot, outcome)
	}
	if s.ResolveDispatchedSpawnRepairApproval(approvalID, now, reason) {
		log.Printf("[orch] %s (approval %s)", reason, approvalID)
		o.mirrorRepairApprovalTerminal(approvalID, now, reason)
	}
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

// releaseReviewRepairSlot rolls back a (pr,head_sha) attempt claimed by
// tryClaimReviewRepairSlot when the worker start that would have consumed it
// failed (#874). Without this, a failed start spends an attempt from the
// bounded budget while the approval stays active, so a run of start failures
// exhausts the budget and the approved repair can never reach a worker.
func (o *Orchestrator) releaseReviewRepairSlot(s *state.State, target *state.SupervisorTarget, payload *state.SupervisorReviewRepairPayload) {
	if s == nil || target == nil || payload == nil {
		return
	}
	s.ReleaseReviewRepairAttempt(target.PR, payload.HeadSHA, time.Now().UTC())
	log.Printf("[orch] review-repair: released claimed attempt for PR #%d head %s after failed start", target.PR, shortReviewRepairSHA(payload.HeadSHA))
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
	if decision == nil || decision.RecommendationDropped() || decision.RecommendedAction != supervisor.ActionSpawnReviewRepair {
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

// resolveReviewRepairDispatch resolves the review-repair payload the
// dispatcher should act on for issueNumber. It prefers the latest supervisor
// decision (the auto pipeline), then falls back to a still-effective
// spawn_review_repair approval's DURABLE payload (#874). The fallback is what
// makes a manually enqueued or decision-ring-evicted approval dispatchable: it
// no longer requires the coincident LatestSupervisorDecision to still be a
// spawn_review_repair. Returns the approval id when the payload came from the
// durable approval path (so the caller can resolve it after dispatch); "" for
// the decision path.
func (o *Orchestrator) resolveReviewRepairDispatch(s *state.State, issueNumber int) (*state.SupervisorReviewRepairPayload, *state.SupervisorTarget, string) {
	if payload, target := o.supervisorSelectedReviewRepair(s, issueNumber); payload != nil && target != nil {
		return payload, target, ""
	}
	return o.approvalReviewRepairDispatch(s, issueNumber)
}

// approvalSelectedReviewRepair returns the durable payload + target + id of a
// still-effective (approved / awaiting_dispatch) spawn_review_repair approval
// for issueNumber, or nils when none applies. Pending (unapproved) approvals
// are intentionally excluded — the cautious gate must clear first.
func (o *Orchestrator) approvalSelectedReviewRepair(s *state.State, issueNumber int) (*state.SupervisorReviewRepairPayload, *state.SupervisorTarget, string) {
	if s == nil || issueNumber <= 0 {
		return nil, nil, ""
	}
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action != supervisor.ActionSpawnReviewRepair || a.ReviewRepair == nil {
			continue
		}
		switch a.Status {
		case state.ApprovalStatusApproved, state.ApprovalStatusAwaitingDispatch:
		default:
			continue
		}
		if a.Target == nil || a.Target.Issue != issueNumber {
			continue
		}
		return a.ReviewRepair, a.Target, a.ID
	}
	return nil, nil, ""
}

// approvalReviewRepairDispatch resolves a durable-approval review-repair
// payload, enforcing the #874 changed-head guard: before spawning a worker it
// reads the PR's current head and refuses to repair a stale revision. When the
// head has moved past the approved payload, the approval is superseded (the
// next supervisor cycle re-proves the current head) and (nil, nil, "") is
// returned. A head-read error is treated conservatively — leave the approval
// pending rather than repair a possibly-wrong revision.
func (o *Orchestrator) approvalReviewRepairDispatch(s *state.State, issueNumber int) (*state.SupervisorReviewRepairPayload, *state.SupervisorTarget, string) {
	payload, target, id := o.approvalSelectedReviewRepair(s, issueNumber)
	if payload == nil || target == nil {
		return nil, nil, ""
	}
	currentHead, err := o.prHeadSHA(target.PR)
	if err != nil {
		log.Printf("[orch] review-repair approval %s: PR #%d head read failed; leaving approval pending: %v", id, target.PR, err)
		return nil, nil, ""
	}
	if strings.TrimSpace(currentHead) != strings.TrimSpace(payload.HeadSHA) {
		now := time.Now().UTC()
		reason := fmt.Sprintf("PR #%d head moved to %s (review-repair approved for %s) — superseded to avoid repairing a stale revision",
			target.PR, shortReviewRepairSHA(currentHead), shortReviewRepairSHA(payload.HeadSHA))
		for _, staled := range s.SupersedeReviewRepairApprovalsForStaleHead(target.PR, currentHead, now, reason) {
			log.Printf("[orch] %s (approval %s)", reason, staled)
			o.mirrorRepairApprovalTerminal(staled, now, reason)
		}
		return nil, nil, ""
	}
	return payload, target, id
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
	if decision == nil || decision.RecommendationDropped() {
		return 0, false
	}

	switch decision.PolicyRule {
	case supervisor.PolicyRuleDynamicWave:
		if decision.QueueAnalysis != nil && decision.QueueAnalysis.SelectedCandidate != nil && decision.QueueAnalysis.SelectedCandidate.Number > 0 {
			return decision.QueueAnalysis.SelectedCandidate.Number, true
		}
		if decision.Target != nil && decision.Target.Issue > 0 {
			return decision.Target.Issue, true
		}
	case supervisor.PolicyRuleRuntimeState, supervisor.PolicyRuleReviewRepair:
		// When the supervisor selects a repair/review-repair action, the targeted
		// issue is the effective "owned ready" candidate even though the policy
		// rule is not dynamic-wave. Without this branch the orchestrator filters
		// the issue out and the repair spawn never dispatches (#816 regression).
		if decision.Target != nil && decision.Target.Issue > 0 &&
			(decision.RecommendedAction == supervisor.ActionSpawnReviewRepair || decision.RecommendedAction == supervisor.ActionSpawnRepairWorker) {
			return decision.Target.Issue, true
		}
	}
	return 0, false
}

func (o *Orchestrator) applySupervisorOwnedReadyFilter(s *state.State, issues []github.Issue) []github.Issue {
	if !o.supervisorOwnsDynamicReadyLabel() || len(issues) == 0 {
		return issues
	}

	selected, ok := o.supervisorOwnedReadySelectedIssue(s)
	// Dynamic-wave ownership constrains fresh issue selection, but it must not
	// hide already-authorized exact-session repairs. Those approvals reserve an
	// existing issue/session/PR identity and cannot allocate a competing slot;
	// filtering them out behind the latest fresh candidate leaves dead workers
	// in awaiting_dispatch forever (#940).
	filtered := make([]github.Issue, 0, 1)
	for _, issue := range issues {
		if _, claimed := s.FreshDispatchClaimFor(issue.Number); claimed {
			filtered = append(filtered, issue)
			continue
		}
		if o.supervisorSelectedRepairSpawn(s, issue.Number) {
			filtered = append(filtered, issue)
			continue
		}
		// A bounded-futility intake issue is the one fresh issue allowed through
		// the red-outcome hold. This bypasses only supervisor-owned ready
		// selection; the normal closed/merged, exclusion, operator-gate, retry,
		// claim, and backend checks below still apply.
		if isOutcomeRepairIssue(issue) {
			filtered = append(filtered, issue)
			continue
		}
		if ok && issue.Number == selected {
			filtered = append(filtered, issue)
			continue
		}
		if ok {
			log.Printf("[orch] skipping issue #%d: not supervisor-selected candidate #%d for supervisor-owned ready label", issue.Number, selected)
		} else {
			log.Printf("[orch] skipping issue #%d: supervisor-owned ready label has no selected dynamic-wave candidate yet", issue.Number)
		}
	}
	if ok && len(filtered) == 0 {
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

func (o *Orchestrator) augmentWithFreshDispatchClaims(s *state.State, issues []github.Issue) []github.Issue {
	if s == nil || len(s.FreshDispatchClaims) == 0 {
		return issues
	}
	numbers := make([]int, 0, len(s.FreshDispatchClaims))
	for issue := range s.FreshDispatchClaims {
		if _, active := s.FreshDispatchClaimFor(issue); active {
			numbers = append(numbers, issue)
		}
	}
	sort.Ints(numbers)
	for _, issue := range numbers {
		if !containsIssueNumber(issues, issue) {
			issues = o.appendFetchedSupervisorIssue(issues, issue)
		}
	}
	return issues
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

func (o *Orchestrator) durableFreshDispatchClaimsEnabled() bool {
	if o == nil || o.cfg == nil || strings.TrimSpace(o.cfg.StateDir) == "" {
		return false
	}
	// Legacy unit hooks synthesize arbitrary slot names and intentionally bypass
	// production worker identity. A reservation-aware hook opts tests into the
	// durable path explicitly.
	return o.workerStartFn == nil || o.workerStartClaimedFn != nil
}

func (o *Orchestrator) claimFreshDispatch(s *state.State, issue github.Issue, now time.Time) (*state.FreshDispatchClaim, bool, error) {
	if !o.durableFreshDispatchClaimsEnabled() {
		return nil, true, nil
	}
	leaseID, err := newFreshDispatchLeaseID()
	if err != nil {
		return nil, false, err
	}
	var (
		claim    state.FreshDispatchClaim
		acquired bool
	)
	err = state.Update(o.cfg.StateDir, func(latest *state.State) error {
		reserved, ok, claimErr := latest.ClaimFreshDispatch(issue.Number, o.cfg.SessionPrefix, leaseID, freshDispatchLeaseDuration, now)
		if claimErr != nil {
			return claimErr
		}
		if reserved == nil {
			return state.ErrNoStateChange
		}
		if ok && strings.TrimSpace(reserved.Branch) == "" {
			reserved.Branch = worker.BranchName(reserved.Slot, issue)
			reserved.Worktree = filepath.Join(o.cfg.WorktreeBase, reserved.Slot)
			reserved.UpdatedAt = now.UTC()
		}
		claim = *reserved
		acquired = ok
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if claim.IssueNumber > 0 {
		if s.FreshDispatchClaims == nil {
			s.FreshDispatchClaims = make(map[int]*state.FreshDispatchClaim)
		}
		copy := claim
		s.FreshDispatchClaims[issue.Number] = &copy
		if next := freshDispatchSlotSequence(claim.Slot, o.cfg.SessionPrefix) + 1; next > s.NextSlot {
			s.NextSlot = next
		}
	}
	return &claim, acquired, nil
}

func (o *Orchestrator) completeFreshDispatch(s *state.State, claim *state.FreshDispatchClaim, sess *state.Session, now time.Time) error {
	if claim == nil {
		return nil
	}
	if err := state.Update(o.cfg.StateDir, func(latest *state.State) error {
		return latest.CompleteFreshDispatch(claim.IssueNumber, claim.LeaseID, sess, now)
	}); err != nil {
		return err
	}
	copy := *claim
	copy.Status = state.FreshDispatchClaimStatusCompleted
	copy.UpdatedAt = now.UTC()
	copy.CompletedAt = now.UTC()
	copy.SessionStartedAt = sess.StartedAt.UTC()
	copy.LeaseExpiresAt = time.Time{}
	copy.TerminalReason = "session_committed"
	s.FreshDispatchClaims[claim.IssueNumber] = &copy
	return nil
}

func (o *Orchestrator) supersedeFreshDispatch(s *state.State, claim *state.FreshDispatchClaim, terminalReason string, now time.Time) error {
	if claim == nil {
		return nil
	}
	var superseded state.FreshDispatchClaim
	if err := state.Update(o.cfg.StateDir, func(latest *state.State) error {
		if err := latest.SupersedeFreshDispatch(claim.IssueNumber, claim.LeaseID, terminalReason, now); err != nil {
			return err
		}
		superseded = *latest.FreshDispatchClaims[claim.IssueNumber]
		return nil
	}); err != nil {
		return err
	}
	if s.FreshDispatchClaims == nil {
		s.FreshDispatchClaims = make(map[int]*state.FreshDispatchClaim)
	}
	copy := superseded
	s.FreshDispatchClaims[claim.IssueNumber] = &copy
	return nil
}

func (o *Orchestrator) reconcileFreshDispatchClaims(s *state.State, now time.Time) {
	if !o.durableFreshDispatchClaimsEnabled() || s == nil {
		return
	}
	if err := state.Update(o.cfg.StateDir, func(latest *state.State) error {
		if latest.ReconcileFreshDispatchClaims(now) == 0 {
			return state.ErrNoStateChange
		}
		return nil
	}); err != nil {
		log.Printf("[orch] reconcile fresh dispatch claims: %v", err)
	}
	s.ReconcileFreshDispatchClaims(now)
}

func freshDispatchSlotSequence(slot, prefix string) int {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || !strings.HasPrefix(slot, prefix+"-") {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(slot, prefix+"-"))
	return n
}

func newFreshDispatchLeaseID() (string, error) {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create fresh dispatch lease: %w", err)
	}
	return "dispatch:" + hex.EncodeToString(raw[:]), nil
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
	o.reconcileFreshDispatchClaims(s, time.Now().UTC())
	// EMERGENCY STOP (#840): the fleet-wide big red button closes the spawn gate
	// ahead of every other check. It halts ALL new worker spawns (and, since the
	// router is only consulted inside this issue loop, every router LLM call for a
	// spawn) within one poll interval. In-flight workers and attached verify/build
	// children are killed on engage; the daemon stays up so the operator can watch.
	if o.emergencyHaltFn != nil && o.emergencyHaltFn() {
		log.Printf("[orch] EMERGENCY STOP active: not spawning new workers (running=%d)", s.RunningSessionCount())
		return
	}
	if o.fleetSpawnCeilingFn != nil && o.fleetSpawnCeilingFn() {
		log.Printf("[orch] fleet live-worker ceiling reached: not listing or spawning new work (local_running=%d)", s.RunningSessionCount())
		return
	}
	// Host-resource precondition (#1128): the host tmpfs is RAM-backed, so
	// spawning into a nearly-full one is how a space problem becomes an
	// out-of-memory outage. This sits before issue listing so a held cycle also
	// costs no GitHub quota. It is a pause, not a freeze: nothing is persisted,
	// no retry budget is spent, and the next poll re-checks and resumes.
	if o.spawnResourceHoldFn != nil {
		if hold, reason := o.spawnResourceHoldFn(); hold {
			log.Printf("[orch] spawn held on host resources: %s (local_running=%d) — retrying next cycle", reason, s.RunningSessionCount())
			return
		}
	}
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
	if decision := s.LatestSupervisorDecision(); decision != nil && !decision.RecommendationDropped() && decision.RecommendedAction == supervisor.ActionSpawnRepairWorker && decision.Target != nil {
		if !decision.RequiresApproval && decision.Risk != supervisor.RiskApprovalGated {
			issues = o.appendFetchedSupervisorIssue(issues, decision.Target.Issue)
		}
	}
	issues = o.augmentWithSupervisorSelectedIssue(s, issues)
	issues = o.augmentWithFreshDispatchClaims(s, issues)
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
		_, freshDispatchPending := s.FreshDispatchClaimFor(issue.Number)
		if s.IssueInProgress(issue.Number) && !freshDispatchPending {
			if !repairSpawn {
				continue
			}
		} else if freshDispatchPending && s.IssueHasNonFreshClaim(issue.Number) && !repairSpawn {
			// A selected repair must reach dispatchSpawnRepairWorker so its
			// exact-session revalidation can see the earlier fresh claim and
			// terminalize the competing repair authority. Skipping here would
			// leave both claims active, while revoking the startup lease would
			// reopen the duplicate-worker race.
			log.Printf("[orch] refusing fresh dispatch renewal for issue #%d: a canonical session/PR claim appeared", issue.Number)
			continue
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
				reason := fmt.Sprintf("issue #%d gained an excluded label before repair dispatch — approval/intent is stale", issue.Number)
				for _, approval := range s.StaleActiveRepairDispatchApprovals(issue.Number, time.Now().UTC(), reason) {
					log.Printf("[orch] refusing delayed repair for issue #%d (excluded label); staled approval %s", issue.Number, approval.ID)
					o.mirrorRepairApprovalTerminal(approval.ID, time.Now().UTC(), reason)
				}
			}
			log.Printf("[orch] skipping issue #%d (excluded label; repair authority does not bypass current guards)", issue.Number)
			continue
		}
		if label, ok := matchingIssueLabel(issue, o.operatorGateLabels()); ok {
			log.Printf("[orch] skipping issue #%d: held by operator gate label %q", issue.Number, label)
			continue
		}

		// A gate-fail-streak issue is Maestro's own report that a scheduled gate
		// failed N times, not a triaged task — see the supervisor's hold. This
		// loop lists issues itself, so with no issue_labels configured (every
		// open issue eligible) the supervisor's hold is not in the path and the
		// report would be dispatched here. There is no label for an operator to
		// apply in that configuration, so the report is never auto-eligible.
		if len(o.cfg.IssueLabels) == 0 && supervisor.IsGateFailStreakIssue(issue) && !repairSpawn {
			log.Printf("[orch] skipping issue #%d: auto-minted gate-fail-streak report awaits operator triage", issue.Number)
			continue
		}

		// Break the budget-kill mill: a token budget below the floor a worker
		// needs to load its initial context kills every dispatch identically,
		// and budget stops are excluded from the retry budget by design, so
		// nothing else stops the loop (#628 live: 9 spawns in 4.5h, each ~2
		// min at observed≈157k vs max=120k). After a streak of PR-less budget
		// kills the issue is held and the misconfiguration is surfaced once —
		// the operator raises worker_max_tokens (or fixes the measure) instead
		// of watching the fleet re-spawn into the same wall.
		if !repairSpawn {
			kills := s.ConsecutiveTokenBudgetKillsForIssue(issue.Number, o.cfg.WorkerMaxTokens)
			if o.tokenBudgetMillHold(issue.Number, kills) {
				log.Printf("[orch] skipping issue #%d: %d consecutive token-budget stops — worker_max_tokens=%d is likely below the viable floor for this issue",
					issue.Number, kills, o.cfg.WorkerMaxTokens)
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
		// Live backstop recomputed from current state each iteration (retry
		// respawns earlier in the cycle already turned dead sessions running).
		// Uses the same Capacity budget as availableSlots so pr_open PR-gate
		// sessions do not count against live-worker capacity when
		// max_live_workers is set (#814).
		if capNow := s.Capacity(capacityInput(o.cfg)); capNow.Limit > 0 && capNow.AvailableSlots <= 0 {
			log.Printf("[orch] dispatch cap: live=%d pr_gates=%d limit=%d (separated=%t) — queueing issue #%d",
				capNow.LiveWorkers, capNow.PRGates, capNow.Limit, capNow.Separated, issue.Number)
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
		permit, permitOK := o.reserveFleetSpawn()
		if !permitOK {
			log.Printf("[orch] fleet live-worker ceiling reached before routing issue #%d", issue.Number)
			return
		}

		// A classic repair is a same-session maintenance operation, not a fresh
		// issue dispatch. Resolve and revalidate the durable reservation before
		// any backend routing or slot allocation can create a second worktree.
		if repairDispatch := o.resolveSpawnRepairDispatch(s, issue.Number); repairDispatch != nil {
			if o.dispatchSpawnRepairWorker(s, issue, repairDispatch) {
				permit.Commit(repairDispatch.target.Session)
				markSupervisorWorkerRecommendationMaterialized(s, issue.Number, time.Now().UTC())
				started++
			} else {
				permit.Release()
			}
			continue
		}

		// Determine initial phase and backend. A pipeline:full issue label
		// enables the phase pipeline only for this worker's copied config.
		workerCfg, pipelineFull, pipelineAdvised := pipelineConfigForIssue(o.cfg, issue)
		initialPhase := pipeline.InitialPhase(workerCfg)
		var backendName string
		var promptBase string
		// backendReason tracks why this backend was chosen so the dashboard /
		// session record can show provenance: label, role, auto, default,
		// router_error, phase, review_repair. (#427)
		var backendReason string
		var taskType string
		// backendDecision carries the full resolution (incl. policy tier +
		// effort/model override) for the normal path; zero for the
		// review-repair / phase branches (#783).
		var backendDecision router.BackendDecision

		// #565: when the supervisor selected spawn_review_repair for this
		// issue, override backend + prompt with the strong backend and
		// the scoped Greptile-finding prompt. Skip the pipeline preamble
		// — the review-repair worker is a focused, single-phase fixer
		// (not a planner/implementer/validator pass).
		reviewRepair, repairTarget, reviewRepairApprovalID := o.resolveReviewRepairDispatch(s, issue.Number)
		if reviewRepair != nil && repairTarget != nil {
			if !o.tryClaimReviewRepairSlot(s, repairTarget, reviewRepair) {
				permit.Release()
				continue
			}
			backendName = reviewRepair.Backend
			if backendName == "" {
				backendName = o.cfg.Supervisor.ReviewRepair.EffectiveBackend()
			}
			promptBase = supervisor.FormatReviewRepairPromptFromPayload(issue.Number, repairTarget.PR, reviewRepair)
			initialPhase = state.PhaseNone
			backendReason = "review_repair"
			source := "auto"
			if reviewRepairApprovalID != "" {
				source = "approval " + reviewRepairApprovalID
			}
			log.Printf("[orch] starting review-repair worker for issue #%d (PR #%d, head %s, backend=%s, %d findings, source=%s)",
				issue.Number, repairTarget.PR, shortReviewRepairSHA(reviewRepair.HeadSHA), backendName, len(reviewRepair.Findings), source)
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
			decision, dispatchable, retryAt := o.resolveDispatchBackend(s, issue, time.Now().UTC())
			if !dispatchable {
				permit.Release()
				if !dispatchPauseLogged {
					expiry := "no cooldown expiry recorded"
					if retryAt != nil {
						expiry = "earliest cooldown expires " + retryAt.UTC().Format(time.RFC3339)
					}
					if decision.Reason == selectionReasonHoldOnCooldown {
						log.Printf("[orch] dispatch hold: routed backend %s cooling down (%s) — hold_on_cooldown waits for the reset instead of cascading to fallback rungs", decision.Backend, expiry)
					} else {
						log.Printf("[orch] dispatch paused: all backends blocked or cooling down (%s) — not spawning fresh workers this cycle", expiry)
					}
					dispatchPauseLogged = true
				}
				continue
			}
			// #783: apply the per-wave strong-tier budget cap before committing.
			backendDecision = o.applyPolicyBudget(s, decision)
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

		// #783: thread the resolved tier's effort/model override into the
		// dispatched worker's argv (no-op for non-policy decisions); surface the
		// shadow would-pick on the dispatch line when policy shadow mode is on.
		workerCfg = applyTierOverride(workerCfg, backendName, backendDecision)
		// #841: thread the initial pipeline phase's role effort (plan, or implement
		// when no planner) into the dispatched worker's argv. No-op for non-pipeline
		// dispatch (PhaseNone) and when the phase role sets no effort, so today's
		// dispatch is unchanged. Implement/validate phase transitions apply their own
		// effort in worker.StartPhase.
		workerCfg = pipeline.ApplyPhaseEffort(workerCfg, backendName, initialPhase)
		if backendDecision.ShadowTier != "" {
			log.Printf("[orch] issue #%d: policy SHADOW would pick %s — dispatching %s unchanged",
				issue.Number, backendDecision.ShadowReason, backendName)
		}
		var freshClaim *state.FreshDispatchClaim
		if !repairSpawn {
			claimedAt := time.Now().UTC()
			claim, acquired, claimErr := o.claimFreshDispatch(s, issue, claimedAt)
			if claimErr != nil {
				permit.Release()
				log.Printf("[orch] claim fresh dispatch for issue #%d: %v", issue.Number, claimErr)
				continue
			}
			if !acquired {
				permit.Release()
				if claim != nil && claim.Slot != "" {
					log.Printf("[orch] skipping duplicate fresh dispatch for issue #%d: canonical startup lease is %s (generation=%d)", issue.Number, claim.Slot, claim.LeaseGeneration)
				}
				continue
			}
			freshClaim = claim
		}
		log.Printf("[orch] starting worker for issue #%d: %s (backend=%s, reason=%s, phase=%s, long_running=%v)", issue.Number, issue.Title, backendName, backendReason, initialPhase, longRunning)
		var slotName string
		var err error
		if freshClaim != nil {
			slotName, err = o.startClaimedWorker(workerCfg, s, issue, promptBase, backendName, freshClaim.Slot)
		} else {
			slotName, err = o.startWorker(workerCfg, s, issue, promptBase, backendName)
		}
		if err != nil {
			permit.Release()
			log.Printf("[orch] start worker for issue #%d: %v", issue.Number, err)
			// #1100: release the pre-spawn lease immediately so a failed start
			// cannot leave the issue claimed with no worker. The next dispatch
			// reuses this exact reserved slot, branch, and worktree, preventing
			// repeated setup failures from leaking a new worktree each cycle.
			if freshClaim != nil {
				if supersedeErr := o.supersedeFreshDispatch(s, freshClaim, "start_failed", time.Now().UTC()); supersedeErr != nil {
					log.Printf("[orch] supersede failed fresh dispatch for issue #%d on %s: %v (lease remains authoritative)", issue.Number, freshClaim.Slot, supersedeErr)
				}
			}
			o.notifier.Sendf("❌ maestro: failed to start worker for issue #%d (%s): %v",
				issue.Number, issue.Title, err)
			// #874: a review-repair dispatch claimed a (pr,head) attempt via
			// tryClaimReviewRepairSlot BEFORE this start; release it so a failed
			// start does not burn a slot from the bounded repair budget and leave
			// the still-active approval permanently un-dispatchable.
			if reviewRepair != nil && repairTarget != nil {
				o.releaseReviewRepairSlot(s, repairTarget, reviewRepair)
			}
			continue
		}
		permit.Commit(slotName)
		markSupervisorWorkerRecommendationMaterialized(s, issue.Number, time.Now().UTC())

		if longRunning {
			s.Sessions[slotName].LongRunning = true
		}
		if initialPhase != state.PhaseNone {
			s.Sessions[slotName].Phase = initialPhase
		}
		if pipelineFull {
			s.Sessions[slotName].PipelineFull = true
		}
		if pipelineAdvised {
			s.Sessions[slotName].PipelineAdvised = true
		}
		// #427/#783: stamp the backend selection reason on the session so the
		// dashboard / fleet API can show why this backend was chosen (label /
		// auto / default / router_error / phase / review_repair / policy:<tier>).
		if sess := s.Sessions[slotName]; sess != nil {
			if freshClaim != nil && !freshClaim.ClaimedAt.IsZero() && (sess.StartedAt.IsZero() || freshClaim.ClaimedAt.Before(sess.StartedAt)) {
				sess.StartedAt = freshClaim.ClaimedAt.UTC()
			}
			if backendDecision.Tier != "" || backendDecision.ShadowTier != "" {
				sel := o.policyBackendSelection(backendDecision)
				sel.SelectedBackend = backendName
				sess.BackendSelection = sel
			} else {
				sess.BackendSelection = &state.BackendSelection{
					SelectedBackend:      backendName,
					SelectionReason:      backendReason,
					RouteSelectionReason: o.cfg.Model.ResolvedRoute().SelectionReason,
					TaskType:             taskType,
				}
			}
			if taskType != "" && len(sess.Attribution) > 0 {
				sess.Attribution[len(sess.Attribution)-1].TaskType = taskType
			}
		}
		if freshClaim != nil {
			sess := s.Sessions[slotName]
			if slotName != freshClaim.Slot || sess == nil {
				log.Printf("[orch] fresh dispatch for issue #%d returned non-canonical slot %q (want %q); lease remains active", issue.Number, slotName, freshClaim.Slot)
				started++
				continue
			}
			if err := o.completeFreshDispatch(s, freshClaim, sess, time.Now().UTC()); err != nil {
				log.Printf("[orch] persist fresh dispatch for issue #%d on %s: %v (lease remains authoritative)", issue.Number, slotName, err)
				started++
				continue
			}
		}
		if o.syncProject(issue.Number, github.ProjectStatusInProgress) {
			s.MarkProjectStatusSynced(issue.Number, string(github.ProjectStatusInProgress), time.Now().UTC())
		}
		// #874: when this review-repair worker was dispatched from a durable
		// approval (not the live decision path), supersede that approval now
		// that its worker has really started — so the dispatcher does not
		// re-resolve (and re-read the PR head) every cycle, and no active
		// approval is left behind after the repair reaches a worker.
		if reviewRepairApprovalID != "" {
			resolveNow := time.Now().UTC()
			reason := fmt.Sprintf("review-repair worker %s dispatched for PR #%d — approval consumed", slotName, repairTarget.PR)
			if s.ResolveDispatchedReviewRepairApproval(reviewRepairApprovalID, resolveNow, reason) {
				log.Printf("[orch] %s (approval %s)", reason, reviewRepairApprovalID)
				o.mirrorRepairApprovalTerminal(reviewRepairApprovalID, resolveNow, reason)
			}
		}
		o.notifier.Sendf("🚀 maestro: started worker %s for issue #%d: %s", slotName, issue.Number, issue.Title)
		started++
	}

	if started == 0 {
		log.Printf("[orch] no new workers started (%d issues checked)", len(issues))
	}
}

func markSupervisorWorkerRecommendationMaterialized(s *state.State, issueNumber int, now time.Time) bool {
	if s == nil || issueNumber <= 0 {
		return false
	}
	decision := s.LatestSupervisorDecision()
	if decision == nil || decision.RecommendationDropped() || decision.Target == nil || decision.Target.Issue != issueNumber {
		return false
	}
	switch decision.RecommendedAction {
	case supervisor.ActionSpawnWorker, supervisor.ActionSpawnRepairWorker, supervisor.ActionSpawnReviewRepair:
	default:
		return false
	}
	return s.MarkSupervisorRecommendationMaterialized(
		decision.ID,
		state.RecommendationDispositionWorkerStarted,
		now,
	)
}
