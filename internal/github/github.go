package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

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

// PRGreptileApproved checks PR comments for Greptile review status.
// Returns:
//   - approved=true when Greptile marked it safe to merge (or confidence 4/5, 5/5)
//   - pending=true when no Greptile comment exists yet
//   - approved=false,pending=false when Greptile commented but did not approve
func (c *Client) PRGreptileApproved(prNumber int) (approved bool, pending bool, err error) {
	out, err := exec.Command("gh", "pr", "view",
		fmt.Sprint(prNumber),
		"--repo", c.Repo,
		"--comments",
		"--json", "comments").Output()
	if err != nil {
		return false, false, fmt.Errorf("gh pr view %d comments: %w", prNumber, err)
	}

	var result struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return false, false, fmt.Errorf("parse pr %d comments: %w", prNumber, err)
	}

	foundGreptile := false
	for _, comment := range result.Comments {
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
