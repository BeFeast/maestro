package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// ghTimeout is the maximum time allowed for a single gh subprocess call.
const ghTimeout = 30 * time.Second

// ProjectStatus represents the status to set on a GitHub Project item.
// Kept for backward compatibility — callers map these to real column names.
type ProjectStatus string

const (
	ProjectStatusTodo       ProjectStatus = "todo"
	ProjectStatusInProgress ProjectStatus = "in_progress"
	ProjectStatusInReview   ProjectStatus = "in_review"
	ProjectStatusBlocked    ProjectStatus = "blocked"
	ProjectStatusDeploying  ProjectStatus = "deploying"
	ProjectStatusLiveVerify ProjectStatus = "live_verification"
	ProjectStatusDone       ProjectStatus = "done"
)

// ProjectStatusCandidates returns the ordered list of column names to try when
// applying a high-level ProjectStatus to a real GitHub Project board. Boards
// often customize column names (e.g. "Review" vs "In Review", "On Hold" vs
// "Blocked"); the first option present in the board's Status field wins.
func ProjectStatusCandidates(status ProjectStatus) []string {
	switch status {
	case ProjectStatusTodo:
		return []string{"Todo", "To do", "To Do", "Backlog"}
	case ProjectStatusInProgress:
		return []string{"In Progress", "In progress", "Doing"}
	case ProjectStatusInReview:
		return []string{"In Review", "In review", "Review", "Reviewing", "Code Review", "In Progress", "In progress"}
	case ProjectStatusBlocked:
		return []string{"Blocked", "On Hold", "On hold", "Stuck", "Needs Attention", "Needs attention"}
	case ProjectStatusDeploying:
		return []string{"Deploying", "Deploy", "Deployment"}
	case ProjectStatusLiveVerify:
		return []string{"Live Verification", "Live verification", "Verification", "Verifying", "QA"}
	case ProjectStatusDone:
		return []string{"Done", "Completed", "Closed"}
	default:
		return nil
	}
}

// ProjectField holds the Status field metadata for a GitHub Project, discovered at runtime.
type ProjectField struct {
	ProjectID string
	FieldID   string
	Options   map[string]string // status name -> option ID (e.g. "In Progress" -> "47fc9ee4")
}

// DiscoverProject finds the GitHub Project board and returns its Status field options.
func (c *Client) DiscoverProject(projectNumber int) (*ProjectField, error) {
	owner := strings.Split(c.Repo, "/")[0]
	query := discoverProjectQuery(owner, projectNumber)

	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		return nil, fmt.Errorf("discover project %d: %w", projectNumber, err)
	}

	pf, err := parseDiscoverProjectResponse(owner, projectNumber, out)
	if err != nil {
		return nil, err
	}

	log.Printf("[projects] discovered project %d for %s: %d status options (%v)", projectNumber, owner, len(pf.Options), keys(pf.Options))
	return pf, nil
}

func discoverProjectQuery(owner string, projectNumber int) string {
	projectFields := `{
				id
				field(name: "Status") {
					... on ProjectV2SingleSelectField {
						id
						options { id name }
					}
				}
			}`

	return fmt.Sprintf(`query {
		repositoryOwner(login: %q) {
			__typename
			... on User {
				projectV2(number: %d) %s
			}
			... on Organization {
				projectV2(number: %d) %s
			}
		}
	}`, owner, projectNumber, projectFields, projectNumber, projectFields)
}

func parseDiscoverProjectResponse(owner string, projectNumber int, out []byte) (*ProjectField, error) {
	var result struct {
		Data struct {
			RepositoryOwner struct {
				Typename  string `json:"__typename"`
				ProjectV2 struct {
					ID    string `json:"id"`
					Field struct {
						ID      string `json:"id"`
						Options []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"options"`
					} `json:"field"`
				} `json:"projectV2"`
			} `json:"repositoryOwner"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse project response: %w", err)
	}

	p := result.Data.RepositoryOwner.ProjectV2
	if p.ID == "" {
		return nil, fmt.Errorf("project %d not found for owner %q", projectNumber, owner)
	}

	pf := &ProjectField{
		ProjectID: p.ID,
		FieldID:   p.Field.ID,
		Options:   make(map[string]string),
	}
	for _, opt := range p.Field.Options {
		pf.Options[opt.Name] = opt.ID
	}

	return pf, nil
}

// ProjectItem represents an item on a GitHub Project board with its linked issue info.
type ProjectItem struct {
	IssueNumber int
	IssueClosed bool
	HasStatus   bool // false when the item has no Status field value (shows as "No Status")
}

// ListNonDoneProjectItems fetches all project items not in Done status
// and returns their linked issue numbers along with whether they are closed.
func (c *Client) ListNonDoneProjectItems(pf *ProjectField) ([]ProjectItem, error) {
	if pf == nil {
		return nil, fmt.Errorf("nil ProjectField")
	}

	doneOptionID := pf.Options["Done"]

	query := fmt.Sprintf(`{
  node(id: %q) {
    ... on ProjectV2 {
      items(first: 100) {
        nodes {
          fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue {
              optionId
            }
          }
          content {
            ... on Issue {
              number
              state
            }
          }
        }
      }
    }
  }
}`, pf.ProjectID)

	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("graphql project items: %w\nstderr: %s", err, exitErr.Stderr)
		}
		return nil, fmt.Errorf("graphql project items: %w", err)
	}

	var resp struct {
		Data struct {
			Node struct {
				Items struct {
					Nodes []struct {
						FieldValueByName *struct {
							OptionID string `json:"optionId"`
						} `json:"fieldValueByName"`
						Content *struct {
							Number int    `json:"number"`
							State  string `json:"state"`
						} `json:"content"`
					} `json:"nodes"`
				} `json:"items"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse project items response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}

	var items []ProjectItem
	for _, node := range resp.Data.Node.Items.Nodes {
		if node.Content == nil || node.Content.Number == 0 {
			continue
		}
		if doneOptionID != "" && node.FieldValueByName != nil && node.FieldValueByName.OptionID == doneOptionID {
			continue
		}
		items = append(items, ProjectItem{
			IssueNumber: node.Content.Number,
			IssueClosed: strings.EqualFold(strings.TrimSpace(node.Content.State), "closed"),
			HasStatus:   node.FieldValueByName != nil && node.FieldValueByName.OptionID != "",
		})
	}

	return items, nil
}

// SyncIssueStatus adds an issue to the project (if not already) and sets its Status.
// Best-effort: errors are logged, never returned.
func (c *Client) SyncIssueStatus(pf *ProjectField, issueNumber int, statusName string) {
	c.SyncIssueStatusOneOf(pf, issueNumber, statusName)
}

// SyncIssueStatusOneOf is SyncIssueStatus that tries each candidate column name
// in order and uses the first that exists on the project board. This lets
// callers express logical intent (e.g. "In Review") while tolerating boards
// that use a different label ("Review", "Code Review"). When no candidate
// matches, the call is a no-op so the worker runtime keeps making progress.
func (c *Client) SyncIssueStatusOneOf(pf *ProjectField, issueNumber int, candidates ...string) {
	_ = c.TrySyncIssueStatusOneOf(pf, issueNumber, candidates...)
}

func (c *Client) TrySyncIssueStatusOneOf(pf *ProjectField, issueNumber int, candidates ...string) bool {
	if pf == nil {
		return false
	}

	statusName, optionID, ok := resolveProjectStatusOption(pf, candidates)
	if !ok {
		log.Printf("[projects] none of statuses %v found in project (have: %v), skipping issue #%d", candidates, keys(pf.Options), issueNumber)
		return false
	}

	// Step 1: Get issue node ID
	nodeID, err := c.getIssueNodeID(issueNumber)
	if err != nil {
		log.Printf("[projects] could not get node ID for issue #%d: %v", issueNumber, err)
		return false
	}

	// Step 2: Add issue to project (idempotent)
	itemID, err := c.addToProject(pf.ProjectID, nodeID)
	if err != nil {
		log.Printf("[projects] could not add issue #%d to project: %v", issueNumber, err)
		return false
	}

	// Step 3: Set status field
	if err := c.setProjectItemStatus(pf.ProjectID, itemID, pf.FieldID, optionID); err != nil {
		log.Printf("[projects] could not set status for issue #%d: %v", issueNumber, err)
		return false
	}

	log.Printf("[projects] synced issue #%d status=%q", issueNumber, statusName)
	return true
}

// getIssueNodeID retrieves the GraphQL node ID for an issue.
func (c *Client) getIssueNodeID(issueNumber int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "issue", "view", fmt.Sprint(issueNumber),
		"--repo", c.Repo,
		"--json", "id").Output()
	if err != nil {
		return "", fmt.Errorf("gh issue view %d --json id: %w", issueNumber, err)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("parse issue %d node id: %w", issueNumber, err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("empty node ID for issue #%d", issueNumber)
	}
	return result.ID, nil
}

// addToProject adds an issue to a GitHub Project and returns the project item ID.
func (c *Client) addToProject(projectID, contentID string) (string, error) {
	query := fmt.Sprintf(`mutation {
  addProjectV2ItemById(input: {projectId: %q, contentId: %q}) {
    item { id }
  }
}`, projectID, contentID)

	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("graphql addProjectV2ItemById: %w\nstderr: %s\nstdout: %s", err, exitErr.Stderr, out)
		}
		return "", fmt.Errorf("graphql addProjectV2ItemById: %w", err)
	}

	var resp struct {
		Data struct {
			AddProjectV2ItemById struct {
				Item struct {
					ID string `json:"id"`
				} `json:"item"`
			} `json:"addProjectV2ItemById"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("parse addProjectV2ItemById response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return "", fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}
	itemID := resp.Data.AddProjectV2ItemById.Item.ID
	if itemID == "" {
		return "", fmt.Errorf("empty item ID in addProjectV2ItemById response")
	}
	return itemID, nil
}

// setProjectItemStatus sets the Status field on a project item.
func (c *Client) setProjectItemStatus(projectID, itemID, fieldID, optionID string) error {
	query := fmt.Sprintf(`mutation {
  updateProjectV2ItemFieldValue(input: {
    projectId: %q,
    itemId: %q,
    fieldId: %q,
    value: { singleSelectOptionId: %q }
  }) { projectV2Item { id } }
}`, projectID, itemID, fieldID, optionID)

	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("graphql updateProjectV2ItemFieldValue: %w\nstderr: %s\nstdout: %s", err, exitErr.Stderr, out)
		}
		return fmt.Errorf("graphql updateProjectV2ItemFieldValue: %w", err)
	}

	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return fmt.Errorf("parse updateProjectV2ItemFieldValue response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}
	return nil
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// resolveProjectStatusOption finds the first candidate status name that exists
// on the project's Status field, comparing case-insensitively and ignoring
// surrounding whitespace. Returns the canonical (board) name and its option ID.
func resolveProjectStatusOption(pf *ProjectField, candidates []string) (string, string, bool) {
	if pf == nil || len(pf.Options) == 0 {
		return "", "", false
	}

	normalized := make(map[string]string, len(pf.Options))
	for name := range pf.Options {
		normalized[normalizeProjectStatusKey(name)] = name
	}

	for _, candidate := range candidates {
		key := normalizeProjectStatusKey(candidate)
		if key == "" {
			continue
		}
		// Exact match (case sensitive) first to preserve historical behavior.
		if id, ok := pf.Options[candidate]; ok {
			return candidate, id, true
		}
		if name, ok := normalized[key]; ok {
			return name, pf.Options[name], true
		}
	}
	return "", "", false
}

func normalizeProjectStatusKey(name string) string {
	out := strings.ToLower(strings.TrimSpace(name))
	out = strings.ReplaceAll(out, "_", " ")
	out = strings.ReplaceAll(out, "-", " ")
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return out
}
