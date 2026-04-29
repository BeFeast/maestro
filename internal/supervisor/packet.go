package supervisor

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

const (
	supervisorIssueBodyLimit = 1600
	supervisorLogTailLimit   = 4096
	supervisorRecentLimit    = 3
)

type supervisorStatePacket struct {
	ProjectConfig          supervisorProjectConfigPacket `json:"project_config"`
	Policy                 supervisorPolicyPacket        `json:"supervisor_policy"`
	CurrentSessions        []supervisorSessionPacket     `json:"current_maestro_sessions"`
	OrderedQueue           []supervisorIssuePacket       `json:"ordered_queue_state"`
	GitHub                 supervisorGitHubPacket        `json:"github_metadata"`
	WorkerLogExcerpts      []supervisorLogExcerpt        `json:"recent_worker_log_excerpts,omitempty"`
	StuckStateDetectors    []supervisorDetectorPacket    `json:"stuck_state_detector_outputs"`
	RecentSupervisorEvents []supervisorRecentDecision    `json:"recent_supervisor_decisions,omitempty"`
}

type supervisorProjectConfigPacket struct {
	Repo                       string            `json:"repo"`
	MaxParallel                int               `json:"max_parallel"`
	MaxConcurrentByState       map[string]int    `json:"max_concurrent_by_state,omitempty"`
	MaxRetriesPerIssue         int               `json:"max_retries_per_issue"`
	IssueLabels                []string          `json:"issue_labels,omitempty"`
	ExcludeLabels              []string          `json:"exclude_labels,omitempty"`
	WorkerSilentTimeoutMinutes int               `json:"worker_silent_timeout_minutes,omitempty"`
	WorkerMaxTokens            int               `json:"worker_max_tokens,omitempty"`
	MergeStrategy              string            `json:"merge_strategy"`
	ReviewGate                 string            `json:"review_gate"`
	GitHubProjectsEnabled      bool              `json:"github_projects_enabled"`
	MissionsEnabled            bool              `json:"missions_enabled"`
	Supervisor                 supervisorRuntime `json:"supervisor"`
}

type supervisorRuntime struct {
	Enabled bool   `json:"enabled"`
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	DryRun  bool   `json:"dry_run"`
}

type supervisorPolicyPacket struct {
	AllowedActions          []string `json:"allowed_actions"`
	ApprovalRequiredActions []string `json:"approval_required_actions"`
}

type supervisorSessionPacket struct {
	Name                 string              `json:"name"`
	Issue                int                 `json:"issue"`
	Title                string              `json:"title,omitempty"`
	Status               state.SessionStatus `json:"status"`
	PR                   int                 `json:"pr,omitempty"`
	Branch               string              `json:"branch,omitempty"`
	Backend              string              `json:"backend,omitempty"`
	Phase                state.Phase         `json:"phase,omitempty"`
	RetryCount           int                 `json:"retry_count,omitempty"`
	LastNotifiedStatus   string              `json:"last_notified_status,omitempty"`
	RateLimitHit         bool                `json:"rate_limit_hit,omitempty"`
	TokensUsedAttempt    int                 `json:"tokens_used_attempt,omitempty"`
	TokensUsedTotal      int                 `json:"tokens_used_total,omitempty"`
	StartedAt            time.Time           `json:"started_at,omitempty"`
	FinishedAt           *time.Time          `json:"finished_at,omitempty"`
	LastOutputChangedAt  time.Time           `json:"last_output_changed_at,omitempty"`
	PreviousFeedbackKind string              `json:"previous_attempt_feedback_kind,omitempty"`
}

type supervisorIssuePacket struct {
	Position    int      `json:"position"`
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Labels      []string `json:"labels,omitempty"`
	BodyExcerpt string   `json:"body_excerpt,omitempty"`
}

type supervisorGitHubPacket struct {
	OpenIssues       int                  `json:"open_issues"`
	OpenPullRequests int                  `json:"open_pull_requests"`
	PullRequests     []supervisorPRPacket `json:"pull_requests,omitempty"`
}

type supervisorPRPacket struct {
	Number        int    `json:"number"`
	Title         string `json:"title,omitempty"`
	HeadRefName   string `json:"head_ref_name,omitempty"`
	State         string `json:"state,omitempty"`
	Mergeable     string `json:"mergeable,omitempty"`
	CIStatus      string `json:"ci_status,omitempty"`
	ChecksExcerpt string `json:"checks_excerpt,omitempty"`
}

type supervisorLogExcerpt struct {
	Session string `json:"session"`
	Issue   int    `json:"issue"`
	Status  string `json:"status"`
	Excerpt string `json:"excerpt"`
}

type supervisorDetectorPacket struct {
	Name              string                  `json:"name"`
	Status            string                  `json:"status"`
	RecommendedAction string                  `json:"recommended_action,omitempty"`
	Risk              string                  `json:"risk,omitempty"`
	Confidence        float64                 `json:"confidence,omitempty"`
	Target            *state.SupervisorTarget `json:"target,omitempty"`
	Reasons           []string                `json:"reasons,omitempty"`
}

type supervisorRecentDecision struct {
	CreatedAt         time.Time               `json:"created_at"`
	Summary           string                  `json:"summary"`
	RecommendedAction string                  `json:"recommended_action"`
	Target            *state.SupervisorTarget `json:"target,omitempty"`
	Risk              string                  `json:"risk"`
	RequiresApproval  bool                    `json:"requires_approval"`
}

type prChecksReader interface {
	PRCIStatus(prNumber int) (string, error)
}

type prChecksOutputReader interface {
	PRChecksOutput(prNumber int) (string, error)
}

func (e *Engine) buildStatePacket(st *state.State, deterministic state.SupervisorDecision, policy supervisorPolicy) (supervisorStatePacket, error) {
	issues, err := e.reader.ListOpenIssues(nil)
	if err != nil {
		return supervisorStatePacket{}, fmt.Errorf("list open issues for supervisor state packet: %w", err)
	}
	prs, err := e.reader.ListOpenPRs()
	if err != nil {
		return supervisorStatePacket{}, fmt.Errorf("list open PRs for supervisor state packet: %w", err)
	}

	return supervisorStatePacket{
		ProjectConfig:          e.projectConfigPacket(),
		Policy:                 policy.packet(),
		CurrentSessions:        sessionPackets(st),
		OrderedQueue:           issuePackets(issues),
		GitHub:                 e.githubPacket(issues, prs),
		WorkerLogExcerpts:      logExcerpts(st),
		StuckStateDetectors:    e.detectorPackets(st, deterministic),
		RecentSupervisorEvents: recentDecisionPackets(st),
	}, nil
}

func (e *Engine) projectConfigPacket() supervisorProjectConfigPacket {
	return supervisorProjectConfigPacket{
		Repo:                       e.cfg.Repo,
		MaxParallel:                e.cfg.MaxParallel,
		MaxConcurrentByState:       copyStringIntMap(e.cfg.MaxConcurrentByState),
		MaxRetriesPerIssue:         e.cfg.MaxRetriesPerIssue,
		IssueLabels:                append([]string(nil), e.cfg.IssueLabels...),
		ExcludeLabels:              append([]string(nil), e.cfg.ExcludeLabels...),
		WorkerSilentTimeoutMinutes: e.cfg.WorkerSilentTimeoutMinutes,
		WorkerMaxTokens:            e.cfg.WorkerMaxTokens,
		MergeStrategy:              e.cfg.MergeStrategy,
		ReviewGate:                 e.cfg.ReviewGate,
		GitHubProjectsEnabled:      e.cfg.GitHubProjects.Enabled,
		MissionsEnabled:            e.cfg.Missions.Enabled,
		Supervisor: supervisorRuntime{
			Enabled: e.cfg.Supervisor.Enabled,
			Backend: e.cfg.Supervisor.Backend,
			Model:   e.cfg.Supervisor.Model,
			Effort:  e.cfg.Supervisor.Effort,
			DryRun:  e.cfg.Supervisor.DryRun,
		},
	}
}

func (p supervisorPolicy) packet() supervisorPolicyPacket {
	return supervisorPolicyPacket{
		AllowedActions:          withActionAliases(sortedKeys(p.allowed)),
		ApprovalRequiredActions: withActionAliases(sortedKeys(p.approvalRequired)),
	}
}

func sessionPackets(st *state.State) []supervisorSessionPacket {
	packets := make([]supervisorSessionPacket, 0, len(st.Sessions))
	for _, name := range sortedSessionNames(st) {
		sess := st.Sessions[name]
		if sess == nil {
			continue
		}
		packets = append(packets, supervisorSessionPacket{
			Name:                 name,
			Issue:                sess.IssueNumber,
			Title:                RedactSensitive(sess.IssueTitle),
			Status:               sess.Status,
			PR:                   sess.PRNumber,
			Branch:               sess.Branch,
			Backend:              sess.Backend,
			Phase:                sess.Phase,
			RetryCount:           sess.RetryCount,
			LastNotifiedStatus:   sess.LastNotifiedStatus,
			RateLimitHit:         sess.RateLimitHit,
			TokensUsedAttempt:    sess.TokensUsedAttempt,
			TokensUsedTotal:      sess.TokensUsedTotal,
			StartedAt:            sess.StartedAt,
			FinishedAt:           sess.FinishedAt,
			LastOutputChangedAt:  sess.LastOutputChangedAt,
			PreviousFeedbackKind: sess.PreviousAttemptFeedbackKind,
		})
	}
	return packets
}

func issuePackets(issues []github.Issue) []supervisorIssuePacket {
	packets := make([]supervisorIssuePacket, 0, len(issues))
	for i, issue := range issues {
		packets = append(packets, supervisorIssuePacket{
			Position:    i + 1,
			Number:      issue.Number,
			Title:       RedactSensitive(issue.Title),
			Labels:      issueLabelNames(issue),
			BodyExcerpt: truncateText(RedactSensitive(issue.Body), supervisorIssueBodyLimit),
		})
	}
	return packets
}

func (e *Engine) githubPacket(issues []github.Issue, prs []github.PR) supervisorGitHubPacket {
	packet := supervisorGitHubPacket{
		OpenIssues:       len(issues),
		OpenPullRequests: len(prs),
		PullRequests:     make([]supervisorPRPacket, 0, len(prs)),
	}
	checksReader, canReadChecks := e.reader.(prChecksReader)
	checksOutputReader, canReadChecksOutput := e.reader.(prChecksOutputReader)
	for _, pr := range prs {
		prPacket := supervisorPRPacket{
			Number:      pr.Number,
			Title:       RedactSensitive(pr.Title),
			HeadRefName: pr.HeadRefName,
			State:       pr.State,
			Mergeable:   pr.Mergeable,
		}
		if canReadChecks {
			if status, err := checksReader.PRCIStatus(pr.Number); err == nil {
				prPacket.CIStatus = status
			} else {
				prPacket.CIStatus = "unknown"
			}
		}
		if canReadChecksOutput {
			if output, err := checksOutputReader.PRChecksOutput(pr.Number); err == nil {
				prPacket.ChecksExcerpt = truncateText(RedactSensitive(output), supervisorLogTailLimit)
			}
		}
		packet.PullRequests = append(packet.PullRequests, prPacket)
	}
	return packet
}

func logExcerpts(st *state.State) []supervisorLogExcerpt {
	excerpts := make([]supervisorLogExcerpt, 0, len(st.Sessions))
	for _, name := range sortedSessionNames(st) {
		sess := st.Sessions[name]
		if sess == nil || strings.TrimSpace(sess.LogFile) == "" {
			continue
		}
		data, err := os.ReadFile(sess.LogFile)
		if err != nil || len(data) == 0 {
			continue
		}
		excerpt := tailText(string(data), supervisorLogTailLimit)
		excerpt = strings.TrimSpace(RedactSensitive(excerpt))
		if excerpt == "" {
			continue
		}
		excerpts = append(excerpts, supervisorLogExcerpt{
			Session: name,
			Issue:   sess.IssueNumber,
			Status:  string(sess.Status),
			Excerpt: excerpt,
		})
	}
	return excerpts
}

func (e *Engine) detectorPackets(st *state.State, deterministic state.SupervisorDecision) []supervisorDetectorPacket {
	detectors := []supervisorDetectorPacket{{
		Name:              "deterministic_supervisor",
		Status:            "ok",
		RecommendedAction: deterministic.RecommendedAction,
		Risk:              deterministic.Risk,
		Confidence:        deterministic.Confidence,
		Target:            deterministic.Target,
		Reasons:           deterministic.Reasons,
	}}
	if e.cfg.WorkerSilentTimeoutMinutes <= 0 {
		return detectors
	}
	timeout := time.Duration(e.cfg.WorkerSilentTimeoutMinutes) * time.Minute
	now := e.now().UTC()
	for _, name := range sortedSessionNames(st) {
		sess := st.Sessions[name]
		if sess == nil || sess.Status != state.StatusRunning {
			continue
		}
		status := "unknown"
		if !sess.LastOutputChangedAt.IsZero() {
			status = "not_stuck"
			if now.Sub(sess.LastOutputChangedAt) > timeout {
				status = "stuck"
			}
		}
		detectors = append(detectors, supervisorDetectorPacket{
			Name:   "worker_silent_timeout",
			Status: status,
			Target: &state.SupervisorTarget{Issue: sess.IssueNumber, PR: sess.PRNumber, Session: name},
			Reasons: []string{
				fmt.Sprintf("worker_silent_timeout_minutes=%d", e.cfg.WorkerSilentTimeoutMinutes),
			},
		})
	}
	return detectors
}

func recentDecisionPackets(st *state.State) []supervisorRecentDecision {
	if len(st.SupervisorDecisions) == 0 {
		return nil
	}
	decisions := append([]state.SupervisorDecision(nil), st.SupervisorDecisions...)
	sort.Slice(decisions, func(i, j int) bool {
		return decisions[i].CreatedAt.Before(decisions[j].CreatedAt)
	})
	if len(decisions) > supervisorRecentLimit {
		decisions = decisions[len(decisions)-supervisorRecentLimit:]
	}
	packets := make([]supervisorRecentDecision, 0, len(decisions))
	for _, decision := range decisions {
		packets = append(packets, supervisorRecentDecision{
			CreatedAt:         decision.CreatedAt,
			Summary:           RedactSensitive(decision.Summary),
			RecommendedAction: decision.RecommendedAction,
			Target:            decision.Target,
			Risk:              decision.Risk,
			RequiresApproval:  decision.RequiresApproval,
		})
	}
	return packets
}

var sensitiveRedactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\bAuthorization\s*:\s*[^\n\r]+`), "Authorization: [REDACTED]"},
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`), "Bearer [REDACTED]"},
	{regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|PRIVATE[_-]?KEY)[A-Z0-9_]*)\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s\n]+)`), "${1}=[REDACTED]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`), "[REDACTED_GITHUB_TOKEN]"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`), "[REDACTED_API_KEY]"},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`), "[REDACTED_SLACK_TOKEN]"},
}

// RedactSensitive removes common credential shapes before data reaches prompts or state records.
func RedactSensitive(text string) string {
	redacted := text
	for _, item := range sensitiveRedactions {
		redacted = item.re.ReplaceAllString(redacted, item.repl)
	}
	return redacted
}

func issueLabelNames(issue github.Issue) []string {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.Name)
	}
	return labels
}

func truncateText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "\n... (truncated)"
}

func tailText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return "... (truncated)\n" + string(runes[len(runes)-limit:])
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func withActionAliases(actions []string) []string {
	for _, action := range actions {
		if action == ActionLabelIssueReady {
			return append(actions, "add_ready_label")
		}
	}
	return actions
}

func copyStringIntMap(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	copy := make(map[string]int, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
