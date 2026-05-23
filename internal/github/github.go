package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type greptileCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Text    string `json:"text"`
	} `json:"output"`
}

type checkRunsResponse struct {
	CheckRuns []greptileCheckRun `json:"check_runs"`
}

type combinedStatusResponse struct {
	State    string `json:"state"`
	Statuses []struct {
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	} `json:"statuses"`
}

type greptileReviewComment struct {
	Body             string `json:"body"`
	Path             string `json:"path"`
	Line             int    `json:"line"`
	CommitID         string `json:"commit_id"`
	OriginalCommitID string `json:"original_commit_id"`
	User             struct {
		Login string `json:"login"`
	} `json:"user"`
}

type issueComment struct {
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	ProjectItems []IssueProjectItem `json:"projectItems,omitempty"`
}

type IssueProjectItem struct {
	Title  string                  `json:"title,omitempty"`
	Status *IssueProjectItemStatus `json:"status,omitempty"`
}

type IssueProjectItemStatus struct {
	Name     string `json:"name,omitempty"`
	OptionID string `json:"optionId,omitempty"`
}

type PR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
	Mergeable   string `json:"mergeable"`
	Title       string `json:"title"`
	Body        string `json:"body,omitempty"`
	IsDraft     bool   `json:"isDraft"`
	MergedAt    string `json:"mergedAt,omitempty"`
}

type Client struct {
	Repo string
}

type RateLimitBucket struct {
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
	Reset     int `json:"reset"`
	Used      int `json:"used"`
}

type RateLimitStatus struct {
	Core    RateLimitBucket `json:"core"`
	GraphQL RateLimitBucket `json:"graphql"`
}

type restIssue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   *string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

type restPull struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	Mergeable      *bool   `json:"mergeable"`
	MergeableState string  `json:"mergeable_state"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	MergedAt *string `json:"merged_at"`
}

type prLabel struct {
	Name string `json:"name"`
}

type prCommit struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

func (ri restIssue) issue() Issue {
	body := ""
	if ri.Body != nil {
		body = *ri.Body
	}
	return Issue{
		Number: ri.Number,
		Title:  ri.Title,
		Body:   body,
		Labels: ri.Labels,
	}
}

func (rp restPull) pr() PR {
	body := ""
	if rp.Body != nil {
		body = *rp.Body
	}
	mergedAt := ""
	if rp.MergedAt != nil {
		mergedAt = *rp.MergedAt
	}
	return PR{
		Number:      rp.Number,
		HeadRefName: rp.Head.Ref,
		State:       strings.ToUpper(rp.State),
		Title:       rp.Title,
		Body:        body,
		IsDraft:     rp.Draft,
		MergedAt:    mergedAt,
	}
}

func New(repo string) *Client {
	return &Client{Repo: repo}
}

func ghAPI(endpoint string) ([]byte, error) {
	return ghAPIWithArgs(endpoint)
}

func ghAPIWithArgs(endpoint string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"api", endpoint}, args...)
	out, err := exec.Command("gh", cmdArgs...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w", endpoint, err)
	}
	return out, nil
}

func parseRateLimitStatus(out []byte) (RateLimitStatus, error) {
	var payload struct {
		Resources struct {
			Core    RateLimitBucket `json:"core"`
			GraphQL RateLimitBucket `json:"graphql"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return RateLimitStatus{}, err
	}
	return RateLimitStatus{
		Core:    payload.Resources.Core,
		GraphQL: payload.Resources.GraphQL,
	}, nil
}

func (c *Client) RateLimit() (RateLimitStatus, error) {
	out, err := ghAPI("rate_limit")
	if err != nil {
		return RateLimitStatus{}, err
	}
	status, err := parseRateLimitStatus(out)
	if err != nil {
		return RateLimitStatus{}, fmt.Errorf("parse rate limit: %w", err)
	}
	return status, nil
}

func parseRESTIssues(out []byte) ([]Issue, error) {
	var restIssues []restIssue
	if err := json.Unmarshal(out, &restIssues); err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(restIssues))
	for _, issue := range restIssues {
		if issue.PullRequest != nil {
			continue
		}
		issues = append(issues, issue.issue())
	}
	return issues, nil
}

func parseRESTIssue(out []byte) (Issue, error) {
	var issue restIssue
	if err := json.Unmarshal(out, &issue); err != nil {
		return Issue{}, err
	}
	return issue.issue(), nil
}

func restIssueStateClosed(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "closed")
}

func parseRESTPulls(out []byte) ([]PR, error) {
	var restPulls []restPull
	if err := json.Unmarshal(out, &restPulls); err != nil {
		return nil, err
	}
	prs := make([]PR, 0, len(restPulls))
	for _, pr := range restPulls {
		prs = append(prs, pr.pr())
	}
	return prs, nil
}

func issueRefRegexp(issueNumber int) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?i)(^|[^0-9])#%d([^0-9]|$)`, issueNumber))
}

func prReferencesIssue(pr PR, issueNumber int) bool {
	if issueNumber <= 0 {
		return false
	}
	return issueRefRegexp(issueNumber).MatchString(pr.Title + "\n" + pr.Body)
}

func parseCheckRuns(out []byte) ([]greptileCheckRun, error) {
	var payload checkRunsResponse
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, err
	}
	return payload.CheckRuns, nil
}

func parseCombinedStatus(out []byte) (combinedStatusResponse, error) {
	var payload combinedStatusResponse
	if err := json.Unmarshal(out, &payload); err != nil {
		return combinedStatusResponse{}, err
	}
	return payload, nil
}

func ciStatusFromREST(checks []greptileCheckRun, combined combinedStatusResponse) string {
	if len(combined.Statuses) > 0 && strings.EqualFold(combined.State, "pending") {
		return "pending"
	}
	if len(combined.Statuses) > 0 && (strings.EqualFold(combined.State, "failure") || strings.EqualFold(combined.State, "error")) {
		return "failure"
	}

	hasSignal := len(checks) > 0 || len(combined.Statuses) > 0
	for _, check := range checks {
		status := strings.ToLower(strings.TrimSpace(check.Status))
		conclusion := strings.ToLower(strings.TrimSpace(check.Conclusion))
		if status == "queued" || status == "in_progress" || status == "waiting" || status == "requested" || (status != "completed" && conclusion == "") {
			return "pending"
		}
		switch conclusion {
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
			return "failure"
		}
	}
	if !hasSignal {
		return "success"
	}
	return "success"
}

func formatChecksOverview(checks []greptileCheckRun, combined combinedStatusResponse) string {
	var lines []string
	for _, check := range checks {
		state := strings.TrimSpace(check.Conclusion)
		if state == "" {
			state = strings.TrimSpace(check.Status)
		}
		if state == "" {
			state = "unknown"
		}
		lines = append(lines, fmt.Sprintf("%s\t%s", check.Name, state))
	}
	for _, status := range combined.Statuses {
		name := status.Context
		if name == "" {
			name = "commit-status"
		}
		state := status.State
		if state == "" {
			state = "unknown"
		}
		if status.Description != "" {
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s", name, state, status.Description))
		} else {
			lines = append(lines, fmt.Sprintf("%s\t%s", name, state))
		}
	}
	if len(lines) == 0 {
		return "no checks\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func mergeableFromRESTPull(pr restPull) string {
	if pr.Mergeable != nil {
		if *pr.Mergeable {
			return "MERGEABLE"
		}
		return "CONFLICTING"
	}
	switch strings.ToLower(strings.TrimSpace(pr.MergeableState)) {
	case "dirty":
		return "CONFLICTING"
	case "", "unknown":
		return "UNKNOWN"
	default:
		return "MERGEABLE"
	}
}

func parseIssueComments(out []byte) ([]issueComment, error) {
	var comments []issueComment
	if err := json.Unmarshal(out, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

func parsePRLabels(out []byte) ([]string, error) {
	var labels []prLabel
	if err := json.Unmarshal(out, &labels); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, label.Name)
	}
	return names, nil
}

func parsePRCommits(out []byte) ([]string, error) {
	var commits []prCommit
	if err := json.Unmarshal(out, &commits); err != nil {
		return nil, err
	}
	msgs := make([]string, 0, len(commits))
	for _, commit := range commits {
		message := strings.TrimSpace(commit.Commit.Message)
		if message == "" {
			continue
		}
		headline := strings.SplitN(message, "\n", 2)[0]
		msgs = append(msgs, headline)
	}
	return msgs, nil
}

// ListOpenIssues returns open issues matching any of the given labels (OR filter).
// If labels is empty, all open issues are returned.
func (c *Client) ListOpenIssues(labels []string) ([]Issue, error) {
	if len(labels) <= 1 {
		// Single label or no labels — one call suffices
		label := ""
		if len(labels) == 1 {
			label = labels[0]
		}
		return c.listOpenIssuesByLabel(label)
	}

	// Multiple labels: fetch per-label and deduplicate (OR semantics)
	seen := make(map[int]struct{})
	var result []Issue
	for _, label := range labels {
		issues, err := c.listOpenIssuesByLabel(label)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			if _, ok := seen[issue.Number]; !ok {
				seen[issue.Number] = struct{}{}
				result = append(result, issue)
			}
		}
	}
	return result, nil
}

func (c *Client) listOpenIssuesByLabel(label string) ([]Issue, error) {
	endpoint := fmt.Sprintf("repos/%s/issues?state=open&per_page=100", c.Repo)
	if label != "" {
		endpoint += "&labels=" + url.QueryEscape(label)
	}

	out, err := ghAPI(endpoint)
	if err != nil {
		return nil, fmt.Errorf("list open issues: %w", err)
	}

	issues, err := parseRESTIssues(out)
	if err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}
	return issues, nil
}

// GetIssue fetches a single issue by number
func (c *Client) GetIssue(number int) (Issue, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/issues/%d", c.Repo, number))
	if err != nil {
		return Issue{}, fmt.Errorf("get issue %d: %w", number, err)
	}
	issue, err := parseRESTIssue(out)
	if err != nil {
		return Issue{}, fmt.Errorf("parse issue %d: %w", number, err)
	}
	return issue, nil
}

// IsIssueClosed returns true if the issue is closed
func (c *Client) IsIssueClosed(number int) (bool, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/issues/%d", c.Repo, number))
	if err != nil {
		return false, fmt.Errorf("get issue %d: %w", number, err)
	}
	var result struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return false, err
	}
	return restIssueStateClosed(result.State), nil
}

// ListOpenPRs returns all open PRs
func (c *Client) ListOpenPRs() ([]PR, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/pulls?state=open&per_page=100", c.Repo))
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	prs, err := parseRESTPulls(out)
	if err != nil {
		return nil, fmt.Errorf("parse prs: %w", err)
	}
	return prs, nil
}

func (c *Client) listClosedPRs() ([]PR, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/pulls?state=closed&per_page=100&sort=updated&direction=desc", c.Repo))
	if err != nil {
		return nil, fmt.Errorf("list closed PRs: %w", err)
	}
	prs, err := parseRESTPulls(out)
	if err != nil {
		return nil, fmt.Errorf("parse closed prs: %w", err)
	}
	return prs, nil
}

func (c *Client) getRESTPull(prNumber int) (restPull, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/pulls/%d", c.Repo, prNumber))
	if err != nil {
		return restPull{}, err
	}
	var pr restPull
	if err := json.Unmarshal(out, &pr); err != nil {
		return restPull{}, err
	}
	return pr, nil
}

func (c *Client) pullHeadSHA(prNumber int) (string, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(pr.Head.SHA)
	if sha == "" {
		return "", fmt.Errorf("empty head sha for PR %d", prNumber)
	}
	return sha, nil
}

func (c *Client) checkRunsForSHA(sha string) ([]greptileCheckRun, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", c.Repo, sha), "--paginate")
	if err != nil {
		return nil, err
	}
	checks, err := parseCheckRuns(out)
	if err != nil {
		return nil, fmt.Errorf("parse check runs for %s: %w", sha, err)
	}
	return checks, nil
}

func (c *Client) combinedStatusForSHA(sha string) (combinedStatusResponse, error) {
	out, err := ghAPI(fmt.Sprintf("repos/%s/commits/%s/status", c.Repo, sha))
	if err != nil {
		return combinedStatusResponse{}, err
	}
	status, err := parseCombinedStatus(out)
	if err != nil {
		return combinedStatusResponse{}, fmt.Errorf("parse combined status for %s: %w", sha, err)
	}
	return status, nil
}

// CreatePR opens a pull request and returns its number.
func (c *Client) CreatePR(title, body, base, head string) (int, error) {
	args := []string{
		"pr", "create",
		"--repo", c.Repo,
		"--title", title,
		"--body", body,
		"--base", base,
		"--head", head,
	}
	out, err := exec.Command("gh", args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("gh pr create: %w\n%s", err, out)
	}

	output := strings.TrimSpace(string(out))
	match := regexp.MustCompile(`/pull/([0-9]+)`).FindStringSubmatch(output)
	if len(match) != 2 {
		return 0, fmt.Errorf("unexpected gh pr create output: %s", output)
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("parse PR number from %q: %w", output, err)
	}
	return n, nil
}

// IsPRMerged returns true if the PR has been merged.
func (c *Client) IsPRMerged(prNumber int) (bool, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return false, fmt.Errorf("get pull %d: %w", prNumber, err)
	}
	return strings.EqualFold(pr.State, "closed") && pr.MergedAt != nil, nil
}

// HasMergedPRForIssue returns true if a merged PR references the issue.
func (c *Client) HasMergedPRForIssue(issueNumber int) (bool, error) {
	prs, err := c.listClosedPRs()
	if err != nil {
		return false, err
	}
	for _, pr := range prs {
		if pr.MergedAt != "" && prReferencesIssue(pr, issueNumber) {
			return true, nil
		}
	}
	return false, nil
}

// PRCIStatus returns "success", "failure", "pending", or "unknown"
func (c *Client) PRCIStatus(prNumber int) (string, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return "unknown", fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	checks, checksErr := c.checkRunsForSHA(sha)
	combined, statusErr := c.combinedStatusForSHA(sha)
	if checksErr != nil && statusErr != nil {
		return "unknown", fmt.Errorf("get checks for PR %d: check-runs: %v; statuses: %v", prNumber, checksErr, statusErr)
	}
	return ciStatusFromREST(checks, combined), nil
}

// PRMergeable returns the mergeable state: "MERGEABLE", "CONFLICTING", "UNKNOWN"
func (c *Client) PRMergeable(prNumber int) (string, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return "", fmt.Errorf("get pull %d: %w", prNumber, err)
	}
	return mergeableFromRESTPull(pr), nil
}

// PRGreptileApproved checks whether Greptile has approved the PR.
//
// Primary path: reads GitHub Check Runs for the PR's head SHA.
//   - Looks for a check whose name contains "greptile" (case-insensitive).
//   - conclusion == "success" or "neutral" approves when there are no high
//     severity Greptile inline review comments on the current head SHA.
//   - check found, other conclusion → approved=false, pending=false
//   - check not found → falls through to comment-based fallback
//
// Fallback path: reads PR comments for legacy Greptile comment-mode setups.
//   - "safe to merge" or confidence 4/5 / 5/5 → approved=true
//   - comment found but not approving → approved=false, pending=false
//   - no greptile signal at all → pending=true
func (c *Client) PRGreptileApproved(prNumber int) (approved bool, pending bool, err error) {
	// --- 1. Get head SHA of the PR ---
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return false, false, fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}

	// --- 2. Get check runs for the head SHA ---
	checkRuns, err := c.checkRunsForSHA(sha)
	if err != nil {
		// Non-fatal: fall through to comment fallback
		goto commentFallback
	}

	{
		found, approved, pending := greptileCheckDecision(checkRuns)
		if found {
			if pending {
				return false, true, nil
			}
			if !approved {
				return false, false, nil
			}

			// Greptile check run passed, but high-severity inline comments on
			// the current head are still actionable and should block the gate.
			comments, err := c.greptileReviewComments(prNumber)
			if err == nil && hasGreptileInlineCommentOnHead(comments, sha) {
				return false, false, nil
			}
			return true, false, nil
		}
		// No greptile check run found → fall through to comment fallback
	}

commentFallback:
	// --- 3. Fallback: check PR comments (legacy Greptile comment-mode) ---
	commentsOut, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return false, false, fmt.Errorf("list issue comments for PR %d: %w", prNumber, err)
	}

	comments, err := parseIssueComments(commentsOut)
	if err != nil {
		return false, false, fmt.Errorf("parse pr %d comments: %w", prNumber, err)
	}

	foundGreptile := false
	for _, comment := range comments {
		bodyLower := strings.ToLower(comment.Body)
		if !strings.Contains(bodyLower, "greptile") {
			continue
		}

		foundGreptile = true

		if strings.Contains(bodyLower, "not safe to merge") || strings.Contains(bodyLower, "unsafe to merge") {
			return false, false, nil
		}

		if strings.Contains(bodyLower, "safe to merge") {
			return true, false, nil
		}

		if strings.Contains(bodyLower, "confidence score:") && (strings.Contains(bodyLower, "5/5") || strings.Contains(bodyLower, "4/5")) {
			return true, false, nil
		}
	}

	if !foundGreptile {
		return false, true, nil
	}

	return false, false, nil
}

func greptileCheckDecision(checkRuns []greptileCheckRun) (found bool, approved bool, pending bool) {
	for _, cr := range checkRuns {
		if !strings.Contains(strings.ToLower(cr.Name), "greptile") {
			continue
		}
		found = true
		if cr.Conclusion == "success" || cr.Conclusion == "neutral" {
			return true, true, false
		}
		if cr.Status == "in_progress" || cr.Status == "queued" || cr.Status == "waiting" || cr.Conclusion == "" {
			return true, false, true
		}
		return true, false, false
	}
	return false, false, false
}

func isGreptileLogin(login string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(login)), "greptile")
}

func isReviewBotLogin(login string) bool {
	lower := strings.ToLower(strings.TrimSpace(login))
	return strings.Contains(lower, "greptile") || strings.Contains(lower, "codex")
}

func hasGreptileInlineCommentOnHead(comments []greptileReviewComment, sha string) bool {
	for _, comment := range comments {
		if !isGreptileLogin(comment.User.Login) {
			continue
		}
		if !reviewCommentTargetsHead(comment, sha) {
			continue
		}
		// Only block on P0 or P1 severity — P2/P3 are non-blocking
		if isHighSeverity(comment.Body) {
			return true
		}
	}
	return false
}

func reviewCommentTargetsHead(comment greptileReviewComment, sha string) bool {
	head := strings.TrimSpace(sha)
	if head == "" {
		return true
	}
	original := strings.TrimSpace(comment.OriginalCommitID)
	if original != "" {
		return original == head
	}
	commit := strings.TrimSpace(comment.CommitID)
	if commit == "" {
		return true
	}
	return commit == head
}

// isHighSeverity checks if a review comment is P0 or P1 severity.
// P2/P3 comments are informational and should not block merge.
func isHighSeverity(body string) bool {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "alt=\"p0\"") || strings.Contains(lower, "alt=\"p1\"") {
		return true
	}
	if strings.Contains(lower, "/p0") || strings.Contains(lower, "/p1") {
		return true
	}
	if strings.Contains(lower, "badge/p0") || strings.Contains(lower, "badge/p1") {
		return true
	}
	return false
}

func (c *Client) greptileReviewComments(prNumber int) ([]greptileReviewComment, error) {
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d/comments", c.Repo, prNumber),
		"--paginate").Output()
	if err != nil {
		return nil, err
	}
	var comments []greptileReviewComment
	if err := json.Unmarshal(out, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// ClosePR closes a PR without merging and leaves a comment explaining why.
func (c *Client) ClosePR(prNumber int, comment string) error {
	if comment != "" {
		out, err := exec.Command("gh", "pr", "comment",
			fmt.Sprint(prNumber),
			"--repo", c.Repo,
			"--body", comment).CombinedOutput()
		if err != nil {
			return fmt.Errorf("gh pr comment %d: %w\n%s", prNumber, err, out)
		}
	}
	out, err := exec.Command("gh", "pr", "close",
		fmt.Sprint(prNumber),
		"--repo", c.Repo).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr close %d: %w\n%s", prNumber, err, out)
	}
	return nil
}

// PRChecksOutput returns a REST-derived check overview for a PR, useful for
// capturing CI failure details to pass to retry workers.
func (c *Client) PRChecksOutput(prNumber int) (string, error) {
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil {
		return "", fmt.Errorf("get pull %d head sha: %w", prNumber, err)
	}
	checks, checksErr := c.checkRunsForSHA(sha)
	combined, statusErr := c.combinedStatusForSHA(sha)
	if checksErr != nil && statusErr != nil {
		return "", fmt.Errorf("get checks for PR %d: check-runs: %v; statuses: %v", prNumber, checksErr, statusErr)
	}
	return formatChecksOverview(checks, combined), nil
}

// MergePR squash-merges a PR
func (c *Client) MergePR(prNumber int) error {
	out, err := exec.Command("gh", "pr", "merge",
		fmt.Sprint(prNumber),
		"--repo", c.Repo,
		"--squash",
		"--delete-branch").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge %d: %w\n%s", prNumber, err, out)
	}
	return nil
}

// CloseIssue closes a GitHub issue and leaves a comment explaining why
func (c *Client) CloseIssue(number int, comment string) error {
	if comment != "" {
		out, err := exec.Command("gh", "issue", "comment",
			fmt.Sprint(number),
			"--repo", c.Repo,
			"--body", comment).CombinedOutput()
		if err != nil {
			return fmt.Errorf("gh issue comment %d: %w\n%s", number, err, out)
		}
	}
	out, err := exec.Command("gh", "issue", "close",
		fmt.Sprint(number),
		"--repo", c.Repo).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue close %d: %w\n%s", number, err, out)
	}
	return nil
}

// AddIssueLabel adds a label to an issue.
func (c *Client) AddIssueLabel(issueNumber int, label string) error {
	out, err := exec.Command("gh", "issue", "edit",
		strconv.Itoa(issueNumber),
		"--repo", c.Repo,
		"--add-label", label,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit --add-label: %w\n%s", err, out)
	}
	return nil
}

// RemoveIssueLabel removes a label from an issue.
func (c *Client) RemoveIssueLabel(issueNumber int, label string) error {
	out, err := exec.Command("gh", "issue", "edit",
		strconv.Itoa(issueNumber),
		"--repo", c.Repo,
		"--remove-label", label,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit --remove-label: %w\n%s", err, out)
	}
	return nil
}

// CommentIssue leaves a comment on an issue.
func (c *Client) CommentIssue(issueNumber int, body string) error {
	out, err := exec.Command("gh", "issue", "comment",
		strconv.Itoa(issueNumber),
		"--repo", c.Repo,
		"--body", body,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue comment: %w\n%s", err, out)
	}
	return nil
}

// PRLabels returns the labels on a PR.
func (c *Client) PRLabels(prNumber int) ([]string, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/labels?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list PR %d labels: %w", prNumber, err)
	}
	names, err := parsePRLabels(out)
	if err != nil {
		return nil, err
	}
	return names, nil
}

// PRCommits returns commit messages for a PR.
func (c *Client) PRCommits(prNumber int) ([]string, error) {
	out, err := ghAPIWithArgs(fmt.Sprintf("repos/%s/pulls/%d/commits?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list PR %d commits: %w", prNumber, err)
	}
	msgs, err := parsePRCommits(out)
	if err != nil {
		return nil, err
	}
	return msgs, nil
}

// CreateRelease creates a GitHub release for the given tag.
func (c *Client) CreateRelease(tag, title string) error {
	out, err := exec.Command("gh", "release", "create",
		tag,
		"--repo", c.Repo,
		"--title", title,
		"--generate-notes").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh release create %s: %w\n%s", tag, err, out)
	}
	return nil
}

// HasOpenPRForIssue returns true if there is at least one open PR that
// references the given issue number (e.g. "closes #N") in its body or title.
func (c *Client) HasOpenPRForIssue(issueNumber int) (bool, error) {
	prs, err := c.ListOpenPRs()
	if err != nil {
		return false, err
	}
	for _, pr := range prs {
		if prReferencesIssue(pr, issueNumber) {
			return true, nil
		}
	}
	return false, nil
}

// FindBlockers scans an issue body for blocker references matching the given
// regex patterns. Each pattern must contain a capture group for the issue number.
// Returns deduplicated issue numbers referenced as blockers.
func FindBlockers(body string, patterns []string) []int {
	seen := make(map[int]struct{})
	var blockers []int
	for _, pat := range patterns {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			continue
		}
		for _, match := range re.FindAllStringSubmatch(body, -1) {
			if len(match) < 2 {
				continue
			}
			n, err := strconv.Atoi(match[1])
			if err != nil || n <= 0 {
				continue
			}
			if _, ok := seen[n]; !ok {
				seen[n] = struct{}{}
				blockers = append(blockers, n)
			}
		}
	}
	return blockers
}

// CreateIssue creates a new GitHub issue and returns its number.
func (c *Client) CreateIssue(title, body string, labels []string) (int, error) {
	args := []string{
		"issue", "create",
		"--repo", c.Repo,
		"--title", title,
		"--body", body,
	}
	for _, l := range labels {
		args = append(args, "--label", l)
	}

	out, err := exec.Command("gh", args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("gh issue create: %w\n%s", err, out)
	}

	// gh issue create prints the URL; extract issue number from last path segment
	url := strings.TrimSpace(string(out))
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return 0, fmt.Errorf("unexpected gh issue create output: %s", url)
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("parse issue number from %q: %w", url, err)
	}
	return n, nil
}

// EditIssueBody updates the body of a GitHub issue.
func (c *Client) EditIssueBody(number int, body string) error {
	out, err := exec.Command("gh", "issue", "edit",
		strconv.Itoa(number),
		"--repo", c.Repo,
		"--body", body,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit %d --body: %w\n%s", number, err, out)
	}
	return nil
}

// FindOpenPRForIssue returns the first open PR that references the given issue number.
// Returns pr number, branch name, and whether one was found.
func (c *Client) FindOpenPRForIssue(issueNumber int) (prNumber int, branch string, found bool, err error) {
	prs, err := c.ListOpenPRs()
	if err != nil {
		return 0, "", false, err
	}
	for _, pr := range prs {
		if prReferencesIssue(pr, issueNumber) {
			return pr.Number, pr.HeadRefName, true, nil
		}
	}
	return 0, "", false, nil
}

// ReviewComment is an exported review comment (from Greptile, Codex, or any reviewer).
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
	User string `json:"user"`
}

var reviewLocationPattern = regexp.MustCompile(`(?mi)(^|\s)([A-Za-z0-9_./-]+\.[A-Za-z0-9]+:\d+|file:\s*\S+)`)

// CollectReviewFeedback collects actionable inline review comments from Greptile and Codex on a PR.
func (c *Client) CollectReviewFeedback(prNumber int) ([]ReviewComment, error) {
	// Get HEAD SHA — only return comments on the latest commit
	prOut, _ := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d", c.Repo, prNumber),
		"--jq", ".head.sha").Output()
	headSHA := strings.TrimSpace(string(prOut))

	comments, err := c.greptileReviewComments(prNumber)
	if err != nil {
		return nil, err
	}
	var result []ReviewComment
	for _, cm := range comments {
		login := cm.User.Login
		if !isReviewBotLogin(login) {
			continue
		}
		// Skip comments that were originally left on older commits — they may already be fixed.
		if !reviewCommentTargetsHead(cm, headSHA) {
			continue
		}
		comment := ReviewComment{
			Path: cm.Path,
			Line: cm.Line,
			Body: cm.Body,
			User: login,
		}
		if !isActionableReviewComment(comment) {
			continue
		}
		result = append(result, comment)
	}
	return result, nil
}

func isActionableReviewComment(comment ReviewComment) bool {
	body := strings.TrimSpace(comment.Body)
	if body == "" || isNonActionableReviewText(body) {
		return false
	}
	if strings.TrimSpace(comment.Path) != "" || comment.Line > 0 {
		return true
	}
	return isActionableReviewSummary(body)
}

func isActionableReviewSummary(body string) bool {
	body = strings.TrimSpace(body)
	if body == "" || isNonActionableReviewText(body) {
		return false
	}
	return hasActionableReviewMarker(body)
}

func isNonActionableReviewText(body string) bool {
	lower := normalizedReviewText(body)
	if lower == "" {
		return true
	}
	if strings.Contains(lower, "not safe to merge") || strings.Contains(lower, "unsafe to merge") {
		return false
	}

	nonActionable := []string{
		"no actionable comments",
		"no actionable feedback",
		"no actionable issues",
		"no blocking issues",
		"no bugs found",
		"no changes requested",
		"no findings",
		"no issues found",
		"no issues were found",
		"no review comments",
		"nothing to fix",
		"review complete with no findings",
		"review passed",
		"safe to merge",
		"looks good to me",
		"looks good",
		"lgtm",
		"found 0 issues",
		"0 issues found",
	}
	for _, phrase := range nonActionable {
		if strings.Contains(lower, phrase) {
			return true
		}
	}

	if strings.Contains(lower, "codex") && strings.Contains(lower, "reviewed") &&
		(strings.Contains(lower, "left comments") || strings.Contains(lower, "review comments")) &&
		!hasActionableReviewMarker(body) {
		return true
	}

	return false
}

func hasActionableReviewMarker(body string) bool {
	if reviewLocationPattern.MatchString(body) {
		return true
	}
	lower := normalizedReviewText(body)
	markers := []string{
		"not safe to merge",
		"unsafe to merge",
		"changes requested",
		"must fix",
		"please fix",
		"needs fix",
		"action required",
		"blocking",
		"regression",
		"bug",
		"crash",
		"panic",
		"nil pointer",
		"data race",
		"security",
		"vulnerability",
		"incorrect",
		"broken",
		"failing",
		"leak",
		"deadlock",
		"p0",
		"p1",
		"p2",
		"p3",
		"severity:",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func normalizedReviewText(body string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(body)), " "))
}

// FormatReviewFeedback formats review comments into a text block for worker prompts.
func FormatReviewFeedback(comments []ReviewComment) string {
	if len(comments) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## Review Feedback (fix these issues)\n\n")
	sb.WriteString("The following review comments were left on your PR. Fix each one, commit, and push to the same branch.\n\n")
	for i, c := range comments {
		sb.WriteString(fmt.Sprintf("### Comment %d", i+1))
		if c.User != "" {
			sb.WriteString(fmt.Sprintf(" (from %s)", c.User))
		}
		sb.WriteString("\n")
		if c.Path != "" {
			sb.WriteString(fmt.Sprintf("File: %s", c.Path))
			if c.Line > 0 {
				sb.WriteString(fmt.Sprintf(", Line: %d", c.Line))
			}
			sb.WriteString("\n")
		}
		sb.WriteString(c.Body)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// CIFailureSummary gets the CI check run failure summary for a PR.
func (c *Client) CIFailureSummary(prNumber int) (string, error) {
	// 1. Get check overview
	overview, err := c.PRChecksOutput(prNumber)
	if err != nil {
		overview = err.Error()
	}

	// 2. Find failed job IDs and fetch their logs
	sha, err := c.pullHeadSHA(prNumber)
	if err != nil || sha == "" {
		return overview, nil
	}

	checks, err := c.checkRunsForSHA(sha)
	if err != nil {
		return overview, nil
	}
	var failed []greptileCheckRun
	for _, check := range checks {
		switch strings.ToLower(strings.TrimSpace(check.Conclusion)) {
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
			failed = append(failed, check)
		}
	}
	if len(failed) == 0 {
		return overview, nil
	}

	var result strings.Builder
	result.WriteString("CI Check Overview:\n")
	result.WriteString(overview)
	result.WriteString("\n\n")

	for _, check := range failed {
		result.WriteString(fmt.Sprintf("=== Failed check: %s ===\n", check.Name))
		if check.Output.Summary != "" {
			result.WriteString(check.Output.Summary)
			result.WriteString("\n")
		}
		if check.Output.Text != "" {
			result.WriteString(check.Output.Text)
			result.WriteString("\n")
		}
		if check.DetailsURL != "" {
			result.WriteString("Details: ")
			result.WriteString(check.DetailsURL)
			result.WriteString("\n")
		} else if check.HTMLURL != "" {
			result.WriteString("Details: ")
			result.WriteString(check.HTMLURL)
			result.WriteString("\n")
		}
		result.WriteString("\n")
	}

	s := result.String()
	if len(s) > 8000 {
		s = s[:8000] + "\n... (truncated)"
	}
	return s, nil
}

// CollectPRReviewFeedback collects actionable Greptile/Codex review feedback
// from a PR, including inline review comments and issue-level summary comments.
// Returns a formatted string ready to inject into a worker prompt, or empty
// string if no actionable review feedback exists.
func (c *Client) CollectPRReviewFeedback(prNumber int) (string, error) {
	var sections []string

	// 1. Fetch issue-level comments (Greptile summary with confidence score)
	issueCommentsOut, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/issues/%d/comments", c.Repo, prNumber),
		"--paginate").Output()
	if err == nil {
		var comments []struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if json.Unmarshal(issueCommentsOut, &comments) == nil {
			for _, cm := range comments {
				if isReviewBotLogin(cm.User.Login) && isActionableReviewSummary(cm.Body) {
					sections = append(sections, cm.Body)
				}
			}
		}
	}

	// 2. Fetch inline review comments
	inlineComments, err := c.CollectReviewFeedback(prNumber)
	if err == nil && len(inlineComments) > 0 {
		sections = append(sections, FormatReviewFeedback(inlineComments))
	}

	if len(sections) == 0 {
		return "", nil
	}

	return strings.Join(sections, "\n\n"), nil
}

// HasLabel returns true if any of the issue's labels match
func HasLabel(issue Issue, labels []string) bool {
	for _, l := range issue.Labels {
		for _, excl := range labels {
			if strings.EqualFold(l.Name, excl) {
				return true
			}
		}
	}
	return false
}
