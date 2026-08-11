package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ReviewThread is the merge-safety projection of one GitHub review thread.
// Only unresolved, non-outdated threads from the pull request's current head
// are returned by PRUnresolvedReviewThreadsOnHead.
type ReviewThread struct {
	ID     string `json:"id"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Author string `json:"author,omitempty"`
}

type reviewThreadsGraphQLResponse struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				HeadRefOID    string `json:"headRefOid"`
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Path       string `json:"path"`
						Line       int    `json:"line"`
						Comments   struct {
							Nodes []struct {
								Author *struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// PRUnresolvedReviewThreadsOnHead reads GitHub's review-thread connection and
// returns every unresolved, non-outdated thread for the pull request's current
// head. Aggregate review/check conclusions are deliberately not consulted:
// thread resolution is its own live merge gate.
func (c *Client) PRUnresolvedReviewThreadsOnHead(prNumber int) (string, []ReviewThread, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(c.Repo), "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return "", nil, fmt.Errorf("read review threads for PR %d: invalid repo %q", prNumber, c.Repo)
	}
	if prNumber <= 0 {
		return "", nil, fmt.Errorf("read review threads: PR number is required")
	}

	var (
		cursor  string
		headSHA string
		threads []ReviewThread
	)
	for {
		after := "null"
		if cursor != "" {
			after = strconv.Quote(cursor)
		}
		query := fmt.Sprintf(`query {
  repository(owner: %s, name: %s) {
    pullRequest(number: %d) {
      headRefOid
      reviewThreads(first: 100, after: %s) {
        nodes {
          id
          isResolved
          isOutdated
          path
			line
			comments(first: 1) {
			  nodes { author { login } }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`, strconv.Quote(owner), strconv.Quote(name), prNumber, after)

		out, err := c.ghExec("api", "graphql", "-f", "query="+query)
		if err != nil {
			return "", nil, fmt.Errorf("read review threads for PR %d: %w\n%s", prNumber, err, out)
		}
		pageHead, pageThreads, next, hasNext, err := parseReviewThreadsGraphQL(out)
		if err != nil {
			return "", nil, fmt.Errorf("read review threads for PR %d: %w", prNumber, err)
		}
		if headSHA == "" {
			headSHA = pageHead
		} else if pageHead != headSHA {
			return "", nil, fmt.Errorf("PR %d head changed while review threads were paginated (%s -> %s)", prNumber, headSHA, pageHead)
		}
		threads = append(threads, pageThreads...)
		if !hasNext {
			break
		}
		if strings.TrimSpace(next) == "" || next == cursor {
			return "", nil, fmt.Errorf("PR %d review-thread pagination returned an invalid cursor", prNumber)
		}
		cursor = next
	}
	return headSHA, threads, nil
}

func parseReviewThreadsGraphQL(data []byte) (headSHA string, threads []ReviewThread, next string, hasNext bool, err error) {
	var response reviewThreadsGraphQLResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return "", nil, "", false, fmt.Errorf("parse GraphQL response: %w", err)
	}
	if len(response.Errors) > 0 {
		messages := make([]string, 0, len(response.Errors))
		for _, item := range response.Errors {
			if message := strings.TrimSpace(item.Message); message != "" {
				messages = append(messages, message)
			}
		}
		if len(messages) == 0 {
			messages = append(messages, "unknown GraphQL error")
		}
		return "", nil, "", false, fmt.Errorf("GraphQL errors: %s", strings.Join(messages, "; "))
	}
	if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
		return "", nil, "", false, fmt.Errorf("pull request was not found")
	}
	pr := response.Data.Repository.PullRequest
	headSHA = strings.TrimSpace(pr.HeadRefOID)
	if headSHA == "" {
		return "", nil, "", false, fmt.Errorf("pull request head SHA is empty")
	}
	for _, node := range pr.ReviewThreads.Nodes {
		if node.IsResolved || node.IsOutdated {
			continue
		}
		thread := ReviewThread{
			ID:   strings.TrimSpace(node.ID),
			Path: strings.TrimSpace(node.Path),
			Line: node.Line,
		}
		if len(node.Comments.Nodes) > 0 {
			comment := node.Comments.Nodes[0]
			if comment.Author != nil {
				thread.Author = strings.TrimSpace(comment.Author.Login)
			}
		}
		threads = append(threads, thread)
	}
	return headSHA, threads, pr.ReviewThreads.PageInfo.EndCursor, pr.ReviewThreads.PageInfo.HasNextPage, nil
}
