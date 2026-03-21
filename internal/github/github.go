package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type greptileCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type greptileReviewComment struct {
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
	User     struct {
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
}

type PR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
	Mergeable   string `json:"mergeable"`
	Title       string `json:"title"`
}

type Client struct {
	Repo string
}

func New(repo string) *Client {
	return &Client{Repo: repo}
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
	args := []string{
		"issue", "list",
		"--repo", c.Repo,
		"--state", "open",
		"--json", "number,title,body,labels",
		"--limit", "100",
	}
	if label != "" {
		args = append(args, "--label", label)
	}

	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}

	var issues []Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse issues: %w", err)
	}
	return issues, nil
}

// GetIssue fetches a single issue by number
func (c *Client) GetIssue(number int) (Issue, error) {
	out, err := exec.Command("gh", "issue", "view", fmt.Sprint(number),
		"--repo", c.Repo,
		"--json", "number,title,body,labels").Output()
	if err != nil {
		return Issue{}, fmt.Errorf("gh issue view %d: %w", number, err)
	}
	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return Issue{}, fmt.Errorf("parse issue %d: %w", number, err)
	}
	return issue, nil
}

// IsIssueClosed returns true if the issue is closed
func (c *Client) IsIssueClosed(number int) (bool, error) {
	out, err := exec.Command("gh", "issue", "view", fmt.Sprint(number),
		"--repo", c.Repo,
		"--json", "state").Output()
	if err != nil {
		return false, fmt.Errorf("gh issue view %d: %w", number, err)
	}
	var result struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return false, err
	}
	return result.State == "CLOSED", nil
}

// ListOpenPRs returns all open PRs
func (c *Client) ListOpenPRs() ([]PR, error) {
	out, err := exec.Command("gh", "pr", "list",
		"--repo", c.Repo,
		"--state", "open",
		"--json", "number,headRefName,state,mergeable,title",
		"--limit", "100").Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}

	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse prs: %w", err)
	}
	return prs, nil
}

// PRCIStatus returns "success", "failure", "pending", or "unknown"
func (c *Client) PRCIStatus(prNumber int) (string, error) {
	out, err := exec.Command("gh", "pr", "checks",
		fmt.Sprint(prNumber),
		"--repo", c.Repo).CombinedOutput()
	if err != nil {
		// gh pr checks exits non-zero when checks fail
		outStr := string(out)
		if strings.Contains(outStr, "fail") || strings.Contains(outStr, "✗") {
			return "failure", nil
		}
		if strings.Contains(outStr, "pending") || strings.Contains(outStr, "in_progress") {
			return "pending", nil
		}
		// No checks configured
		if strings.Contains(outStr, "no checks") {
			return "success", nil
		}
		return "unknown", nil
	}
	outStr := string(out)
	if strings.Contains(outStr, "fail") || strings.Contains(outStr, "✗") {
		return "failure", nil
	}
	if strings.Contains(outStr, "pending") || strings.Contains(outStr, "in_progress") {
		return "pending", nil
	}
	return "success", nil
}

// PRMergeable returns the mergeable state: "MERGEABLE", "CONFLICTING", "UNKNOWN"
func (c *Client) PRMergeable(prNumber int) (string, error) {
	out, err := exec.Command("gh", "pr", "view",
		fmt.Sprint(prNumber),
		"--repo", c.Repo,
		"--json", "mergeable").Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view %d: %w", prNumber, err)
	}
	var result struct {
		Mergeable string `json:"mergeable"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", err
	}
	return result.Mergeable, nil
}

// PRGreptileApproved checks whether Greptile has approved the PR.
//
// Primary path: reads GitHub Check Runs for the PR's head SHA.
//   - Looks for a check whose name contains "greptile" (case-insensitive).
//   - conclusion == "success" or "neutral" only approves when there are no
//     Greptile inline review comments on the current head SHA.
//   - check found, other conclusion → approved=false, pending=false
//   - check not found → falls through to comment-based fallback
//
// Fallback path: reads PR comments for legacy Greptile comment-mode setups.
//   - "safe to merge" or confidence 4/5 / 5/5 → approved=true
//   - comment found but not approving → approved=false, pending=false
//   - no greptile signal at all → pending=true
func (c *Client) PRGreptileApproved(prNumber int) (approved bool, pending bool, err error) {
	// --- 1. Get head SHA of the PR ---
	prOut, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d", c.Repo, prNumber)).Output()
	if err != nil {
		return false, false, fmt.Errorf("gh api pulls/%d: %w", prNumber, err)
	}
	var prData struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(prOut, &prData); err != nil {
		return false, false, fmt.Errorf("parse pr %d head sha: %w", prNumber, err)
	}
	sha := prData.Head.SHA

	// --- 2. Get check runs for the head SHA ---
	checksOut, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/commits/%s/check-runs", c.Repo, sha),
		"--paginate").Output()
	if err != nil {
		// Non-fatal: fall through to comment fallback
		goto commentFallback
	}

	{
		var checksData struct {
			CheckRuns []greptileCheckRun `json:"check_runs"`
		}
		if err := json.Unmarshal(checksOut, &checksData); err != nil {
			goto commentFallback
		}

		found, approved, pending := greptileCheckDecision(checksData.CheckRuns)
		if found {
			if pending {
				return false, true, nil
			}
			if !approved {
				return false, false, nil
			}

			reviewComments, err := c.greptileReviewComments(prNumber)
			if err != nil {
				return false, false, fmt.Errorf("gh api pulls/%d/comments: %w", prNumber, err)
			}
			if hasGreptileInlineCommentOnHead(reviewComments, sha) {
				return false, false, nil
			}
			return true, false, nil
		}
		// No greptile check run found → fall through to comment fallback
	}

commentFallback:
	// --- 3. Fallback: check PR comments (legacy Greptile comment-mode) ---
	commentsOut, err := exec.Command("gh", "pr", "view",
		fmt.Sprint(prNumber),
		"--repo", c.Repo,
		"--comments",
		"--json", "comments").Output()
	if err != nil {
		return false, false, fmt.Errorf("gh pr view %d comments: %w", prNumber, err)
	}

	var commentsResult struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(commentsOut, &commentsResult); err != nil {
		return false, false, fmt.Errorf("parse pr %d comments: %w", prNumber, err)
	}

	foundGreptile := false
	for _, comment := range commentsResult.Comments {
		bodyLower := strings.ToLower(comment.Body)
		if !strings.Contains(bodyLower, "greptile") {
			continue
		}

		foundGreptile = true

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

func hasGreptileInlineCommentOnHead(comments []greptileReviewComment, sha string) bool {
	for _, comment := range comments {
		if !isGreptileLogin(comment.User.Login) {
			continue
		}
		if strings.TrimSpace(sha) == "" || strings.TrimSpace(comment.CommitID) == strings.TrimSpace(sha) {
			return true
		}
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

// ClosePR closes a pull request with an optional comment explaining why
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

// PRChecksOutput returns the raw output of `gh pr checks` for a PR
func (c *Client) PRChecksOutput(prNumber int) string {
	out, _ := exec.Command("gh", "pr", "checks",
		fmt.Sprint(prNumber),
		"--repo", c.Repo).CombinedOutput()
	return string(out)
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

// PRLabels returns the labels on a PR.
func (c *Client) PRLabels(prNumber int) ([]string, error) {
	out, err := exec.Command("gh", "pr", "view",
		fmt.Sprint(prNumber),
		"--repo", c.Repo,
		"--json", "labels").Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d labels: %w", prNumber, err)
	}
	var result struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	names := make([]string, len(result.Labels))
	for i, l := range result.Labels {
		names[i] = l.Name
	}
	return names, nil
}

// PRCommits returns commit messages for a PR.
func (c *Client) PRCommits(prNumber int) ([]string, error) {
	out, err := exec.Command("gh", "pr", "view",
		fmt.Sprint(prNumber),
		"--repo", c.Repo,
		"--json", "commits").Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d commits: %w", prNumber, err)
	}
	var result struct {
		Commits []struct {
			MessageHeadline string `json:"messageHeadline"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	msgs := make([]string, len(result.Commits))
	for i, c := range result.Commits {
		msgs[i] = c.MessageHeadline
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
// Uses GitHub search so it works regardless of branch naming.
func (c *Client) HasOpenPRForIssue(issueNumber int) (bool, error) {
	query := fmt.Sprintf("#%d", issueNumber)
	out, err := exec.Command("gh", "pr", "list",
		"--repo", c.Repo,
		"--state", "open",
		"--search", query,
		"--json", "number",
		"--limit", "1").Output()
	if err != nil {
		return false, fmt.Errorf("gh pr list --search: %w", err)
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return false, fmt.Errorf("parse pr search results: %w", err)
	}
	return len(prs) > 0, nil
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
