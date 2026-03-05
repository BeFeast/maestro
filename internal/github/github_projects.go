package github

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// ProjectStatus represents the status to set on a GitHub Project item.
type ProjectStatus string

const (
	ProjectStatusTodo       ProjectStatus = "todo"
	ProjectStatusInProgress ProjectStatus = "in_progress"
	ProjectStatusDone       ProjectStatus = "done"
)

// projectConfig holds the IDs needed to interact with a specific GitHub Project.
type projectConfig struct {
	ProjectID     string // GraphQL node ID (e.g. "PVT_kwDODsWD3M4BQ3Zd")
	StatusFieldID string // Status field ID (e.g. "PVTSSF_lADODsWD3M4BQ3Zdzg-3LbA")
	StatusOptions map[ProjectStatus]string
}

// knownProjects maps project numbers to their configs.
var knownProjects = map[int]projectConfig{
	4: {
		ProjectID:     "PVT_kwDODsWD3M4BQ3Zd",
		StatusFieldID: "PVTSSF_lADODsWD3M4BQ3Zdzg-3LbA",
		StatusOptions: map[ProjectStatus]string{
			ProjectStatusTodo:       "f75ad846",
			ProjectStatusInProgress: "47fc9ee4",
			ProjectStatusDone:       "98236657",
		},
	},
	5: {
		ProjectID:     "PVT_kwDODsWD3M4BQ3Z7",
		StatusFieldID: "PVTSSF_lADODsWD3M4BQ3Z7zg-3Lwo",
		StatusOptions: map[ProjectStatus]string{
			ProjectStatusTodo:       "f75ad846",
			ProjectStatusInProgress: "47fc9ee4",
			ProjectStatusDone:       "98236657",
		},
	},
}

// SyncIssueToProject adds an issue to the GitHub Project and sets its status.
// It is graceful: errors are logged but not returned, so callers are never blocked.
func (c *Client) SyncIssueToProject(issueNumber int, projectNumber int, status ProjectStatus) {
	cfg, ok := knownProjects[projectNumber]
	if !ok {
		log.Printf("[projects] unknown project number %d, skipping sync for issue #%d", projectNumber, issueNumber)
		return
	}

	optionID, ok := cfg.StatusOptions[status]
	if !ok {
		log.Printf("[projects] unknown status %q for project %d, skipping sync for issue #%d", status, projectNumber, issueNumber)
		return
	}

	// Step 1: Get issue node ID
	nodeID, err := c.getIssueNodeID(issueNumber)
	if err != nil {
		log.Printf("[projects] could not get node ID for issue #%d: %v", issueNumber, err)
		return
	}

	// Step 2: Add issue to project (idempotent — returns existing item if already added)
	itemID, err := c.addToProject(cfg.ProjectID, nodeID)
	if err != nil {
		log.Printf("[projects] could not add issue #%d to project %d: %v", issueNumber, projectNumber, err)
		return
	}

	// Step 3: Set status field
	if err := c.setProjectItemStatus(cfg.ProjectID, itemID, cfg.StatusFieldID, optionID); err != nil {
		log.Printf("[projects] could not set status for issue #%d in project %d: %v", issueNumber, projectNumber, err)
		return
	}

	log.Printf("[projects] synced issue #%d to project %d with status %q", issueNumber, projectNumber, status)
}

// getIssueNodeID retrieves the GraphQL node ID for an issue.
func (c *Client) getIssueNodeID(issueNumber int) (string, error) {
	out, err := exec.Command("gh", "issue", "view", fmt.Sprint(issueNumber),
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

	out, err := exec.Command("gh", "api", "graphql", "-f", "query="+query).Output()
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

	out, err := exec.Command("gh", "api", "graphql", "-f", "query="+query).Output()
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
