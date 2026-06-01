package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
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
// Comparison is case-insensitive and whitespace-tolerant inside
// resolveProjectStatusOption, so this list only needs one entry per logical
// column name.
func ProjectStatusCandidates(status ProjectStatus) []string {
	switch status {
	case ProjectStatusTodo:
		return []string{"Todo", "To do", "Backlog"}
	case ProjectStatusInProgress:
		return []string{"In Progress", "Doing"}
	case ProjectStatusInReview:
		return []string{"In Review", "Review", "Reviewing", "Code Review", "In Progress"}
	case ProjectStatusBlocked:
		return []string{"Blocked", "On Hold", "Stuck", "Needs Attention"}
	case ProjectStatusDeploying:
		return []string{"Deploying", "Deploy", "Deployment"}
	case ProjectStatusLiveVerify:
		return []string{"Live Verification", "Verification", "Verifying", "QA"}
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

	// OptionOrder preserves the Status options in the order returned by the
	// project board so callers (e.g. the fleet API) can render columns in
	// the same order an operator sees them on github.com. New in #529 —
	// kept alongside Options for backward compatibility.
	OptionOrder []string

	// Owner is the login of the project's owner (org or user). OwnerType is
	// the GraphQL __typename ("Organization" or "User") and is required to
	// build the canonical board URL (`/orgs/<o>/projects/N` vs
	// `/users/<o>/projects/N`).
	Owner     string
	OwnerType string
	// Number echoes the project number the field was discovered with so
	// callers don't have to thread it separately.
	Number int
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
		ProjectID:   p.ID,
		FieldID:     p.Field.ID,
		Options:     make(map[string]string),
		OptionOrder: make([]string, 0, len(p.Field.Options)),
		Owner:       owner,
		OwnerType:   result.Data.RepositoryOwner.Typename,
		Number:      projectNumber,
	}
	for _, opt := range p.Field.Options {
		pf.Options[opt.Name] = opt.ID
		pf.OptionOrder = append(pf.OptionOrder, opt.Name)
	}

	return pf, nil
}

// ProjectBoardURL returns the canonical github.com URL for the Projects v2
// board described by pf. Returns "" when pf is nil or the owner data is
// missing. OwnerType is the GraphQL __typename surfaced by DiscoverProject
// ("Organization" or "User"); anything other than "Organization" is treated
// as a user-owned project so personal accounts work without a special case.
func ProjectBoardURL(pf *ProjectField) string {
	if pf == nil {
		return ""
	}
	owner := strings.TrimSpace(pf.Owner)
	if owner == "" || pf.Number <= 0 {
		return ""
	}
	segment := "users"
	if strings.EqualFold(pf.OwnerType, "Organization") {
		segment = "orgs"
	}
	return fmt.Sprintf("https://github.com/%s/%s/projects/%d", segment, owner, pf.Number)
}

// ProjectBoardIssueFilterURL returns a board URL with a filter query that
// surfaces the given issue on the board. GitHub Projects v2 accepts the
// `?filterQuery=#NN` query string and applies it to view 1 (the default),
// which is the closest deep-link to the corresponding project card without
// requiring the per-item GraphQL ID lookup.
func ProjectBoardIssueFilterURL(pf *ProjectField, issueNumber int) string {
	base := ProjectBoardURL(pf)
	if base == "" || issueNumber <= 0 {
		return base
	}
	return fmt.Sprintf("%s?pane=info&filterQuery=%s", base, url.QueryEscape(fmt.Sprintf("#%d", issueNumber)))
}

// ProjectItem represents an item on a GitHub Project board with its linked issue info.
type ProjectItem struct {
	IssueNumber int
	IssueClosed bool
	HasStatus   bool // false when the item has no Status field value (shows as "No Status")
}

// ListNonDoneProjectItems fetches all project items not in Done status
// and returns their linked issue numbers along with whether they are closed.
// "Done" is resolved against every candidate column name (Done/Completed/Closed)
// so boards that label their terminal column differently still filter out
// already-finished items instead of leaking them back into the reconciler.
func (c *Client) ListNonDoneProjectItems(pf *ProjectField) ([]ProjectItem, error) {
	if pf == nil {
		return nil, fmt.Errorf("nil ProjectField")
	}

	_, doneOptionID, _ := resolveProjectStatusOption(pf, ProjectStatusCandidates(ProjectStatusDone))

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
	return parseNonDoneProjectItemsResponse(out, doneOptionID)
}

func parseNonDoneProjectItemsResponse(out []byte, doneOptionID string) ([]ProjectItem, error) {
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

// ProjectBoardColumn captures one Status column on the board with its current
// item count. Used by the fleet API to render a WIP rollup (#529).
type ProjectBoardColumn struct {
	Name     string `json:"name"`
	OptionID string `json:"option_id,omitempty"`
	Count    int    `json:"count"`
}

// ListProjectItemStatusCounts returns one ProjectBoardColumn per Status option
// on the board (preserving board order) plus the total item count. The
// "No Status" bucket is appended only when at least one item lacks a Status
// value, so empty boards stay tidy.
//
// The query fetches up to 100 items in one page; that ceiling matches the
// rest of this package's ProjectV2 helpers and is more than enough for the
// maestro coordination board (#5). Larger boards would need pagination; left
// as a known follow-up since the rollup is operator-glance, not authoritative.
func (c *Client) ListProjectItemStatusCounts(pf *ProjectField) ([]ProjectBoardColumn, int, error) {
	if pf == nil {
		return nil, 0, fmt.Errorf("nil ProjectField")
	}

	query := fmt.Sprintf(`{
  node(id: %q) {
    ... on ProjectV2 {
      items(first: 100) {
        totalCount
        nodes {
          fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue {
              optionId
              name
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
			return nil, 0, fmt.Errorf("graphql project status counts: %w\nstderr: %s", err, exitErr.Stderr)
		}
		return nil, 0, fmt.Errorf("graphql project status counts: %w", err)
	}
	return parseProjectItemStatusCountsResponse(out, pf)
}

func parseProjectItemStatusCountsResponse(out []byte, pf *ProjectField) ([]ProjectBoardColumn, int, error) {
	var resp struct {
		Data struct {
			Node struct {
				Items struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						FieldValueByName *struct {
							OptionID string `json:"optionId"`
							Name     string `json:"name"`
						} `json:"fieldValueByName"`
					} `json:"nodes"`
				} `json:"items"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, 0, fmt.Errorf("parse project status counts: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return nil, 0, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}

	byOption := make(map[string]int)
	noStatus := 0
	for _, node := range resp.Data.Node.Items.Nodes {
		if node.FieldValueByName == nil || strings.TrimSpace(node.FieldValueByName.OptionID) == "" {
			noStatus++
			continue
		}
		byOption[node.FieldValueByName.OptionID]++
	}

	columns := make([]ProjectBoardColumn, 0, len(pf.OptionOrder)+1)
	for _, name := range pf.OptionOrder {
		optionID := pf.Options[name]
		columns = append(columns, ProjectBoardColumn{
			Name:     name,
			OptionID: optionID,
			Count:    byOption[optionID],
		})
	}
	if noStatus > 0 {
		columns = append(columns, ProjectBoardColumn{Name: "No Status", Count: noStatus})
	}

	total := resp.Data.Node.Items.TotalCount
	if total == 0 {
		// Fall back to the summed page when totalCount is missing so the
		// rollup is at least as accurate as the page we saw.
		for _, col := range columns {
			total += col.Count
		}
	}
	return columns, total, nil
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
	out, err := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/issues/%d", c.Repo, issueNumber)).Output()
	if err != nil {
		return "", fmt.Errorf("gh api issue %d: %w", issueNumber, err)
	}
	nodeID, err := parseIssueNodeID(issueNumber, out)
	if err != nil {
		return "", err
	}
	return nodeID, nil
}

func parseIssueNodeID(issueNumber int, out []byte) (string, error) {
	var result struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("parse issue %d node id: %w", issueNumber, err)
	}
	if result.NodeID == "" {
		return "", fmt.Errorf("empty node ID for issue #%d", issueNumber)
	}
	return result.NodeID, nil
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

// HasProjectStatusCandidate reports whether any candidate can be represented by
// the board's Status field. Callers use this to avoid repeated ProjectV2 item
// lookups for lifecycle states the board does not support.
func HasProjectStatusCandidate(pf *ProjectField, candidates []string) bool {
	_, _, ok := resolveProjectStatusOption(pf, candidates)
	return ok
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
