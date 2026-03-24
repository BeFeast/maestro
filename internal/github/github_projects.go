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
	ProjectStatusDone       ProjectStatus = "done"
)

// ProjectField holds the Status field metadata for a GitHub Project, discovered at runtime.
type ProjectField struct {
	ProjectID string
	FieldID   string
	Options   map[string]string // status name -> option ID (e.g. "In Progress" -> "47fc9ee4")
}

// DiscoverProject finds the GitHub Project board and returns its Status field options.
func (c *Client) DiscoverProject(projectNumber int) (*ProjectField, error) {
	org := strings.Split(c.Repo, "/")[0]

	query := fmt.Sprintf(`query {
		organization(login: %q) {
			projectV2(number: %d) {
				id
				field(name: "Status") {
					... on ProjectV2SingleSelectField {
						id
						options { id name }
					}
				}
			}
		}
	}`, org, projectNumber)

	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		return nil, fmt.Errorf("discover project %d: %w", projectNumber, err)
	}

	var result struct {
		Data struct {
			Organization struct {
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
			} `json:"organization"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse project response: %w", err)
	}

	p := result.Data.Organization.ProjectV2
	if p.ID == "" {
		return nil, fmt.Errorf("project %d not found", projectNumber)
	}

	pf := &ProjectField{
		ProjectID: p.ID,
		FieldID:   p.Field.ID,
		Options:   make(map[string]string),
	}
	for _, opt := range p.Field.Options {
		pf.Options[opt.Name] = opt.ID
	}

	log.Printf("[projects] discovered project %d: %d status options (%v)", projectNumber, len(pf.Options), keys(pf.Options))
	return pf, nil
}

// ProjectItem represents an item on a GitHub Project board with its linked issue info.
type ProjectItem struct {
	IssueNumber int
	IssueClosed bool
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
			IssueClosed: node.Content.State == "CLOSED",
		})
	}

	return items, nil
}

// SyncIssueStatus adds an issue to the project (if not already) and sets its Status.
// Best-effort: errors are logged, never returned.
func (c *Client) SyncIssueStatus(pf *ProjectField, issueNumber int, statusName string) {
	if pf == nil {
		return
	}

	optionID, ok := pf.Options[statusName]
	if !ok {
		log.Printf("[projects] status %q not found in project (have: %v), skipping issue #%d", statusName, keys(pf.Options), issueNumber)
		return
	}

	// Step 1: Get issue node ID
	nodeID, err := c.getIssueNodeID(issueNumber)
	if err != nil {
		log.Printf("[projects] could not get node ID for issue #%d: %v", issueNumber, err)
		return
	}

	// Step 2: Add issue to project (idempotent)
	itemID, err := c.addToProject(pf.ProjectID, nodeID)
	if err != nil {
		log.Printf("[projects] could not add issue #%d to project: %v", issueNumber, err)
		return
	}

	// Step 3: Set status field
	if err := c.setProjectItemStatus(pf.ProjectID, itemID, pf.FieldID, optionID); err != nil {
		log.Printf("[projects] could not set status for issue #%d: %v", issueNumber, err)
		return
	}

	log.Printf("[projects] synced issue #%d status=%q", issueNumber, statusName)
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
