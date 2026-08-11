package github

// Visual-evidence helpers for the opt-in verify.visual step (#705): list a
// PR's changed files so the orchestrator can classify it as UI-affecting,
// and detect whether screenshot evidence (an embedded image) is already
// attached to the PR body or any comment.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// PRChangedFiles returns the repo-relative paths changed by a PR.
func (c *Client) PRChangedFiles(prNumber int) ([]string, error) {
	out, err := c.ghAPIWithArgs(fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list PR %d files: %w", prNumber, err)
	}
	files, err := parsePRChangedFiles(out)
	if err != nil {
		return nil, fmt.Errorf("parse PR %d files: %w", prNumber, err)
	}
	return files, nil
}

func parsePRChangedFiles(out []byte) ([]string, error) {
	var entries []struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Filename); name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// PRVisualEvidenceAttached reports whether the PR body or any issue comment
// embeds an image — markdown image syntax, an <img> tag, or a bare link to
// an image file — which is how workers attach screenshot evidence.
func (c *Client) PRVisualEvidenceAttached(prNumber int) (bool, error) {
	pr, err := c.getRESTPull(prNumber)
	if err != nil {
		return false, fmt.Errorf("get PR %d: %w", prNumber, err)
	}
	if pr.Body != nil && ContainsEmbeddedImage(*pr.Body) {
		return true, nil
	}

	out, err := c.ghAPIWithArgs(fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", c.Repo, prNumber), "--paginate")
	if err != nil {
		return false, fmt.Errorf("list PR %d comments: %w", prNumber, err)
	}
	comments, err := parseIssueComments(out)
	if err != nil {
		return false, fmt.Errorf("parse PR %d comments: %w", prNumber, err)
	}
	for _, comment := range comments {
		if ContainsEmbeddedImage(comment.Body) {
			return true, nil
		}
	}
	return false, nil
}

var (
	markdownImageRe = regexp.MustCompile(`!\[[^\]]*\]\([^)]+\)`)
	imageURLRe      = regexp.MustCompile(`(?i)https?://\S+\.(png|jpe?g|gif|webp)(\?\S*)?\b`)
	imgTagRe        = regexp.MustCompile(`(?i)<img\s[^>]*src\s*=`)
)

// ContainsEmbeddedImage reports whether markdown text embeds an image:
// `![alt](url)`, an `<img src=...>` tag, or a link to a *.png/jpg/jpeg/
// gif/webp URL (GitHub renders user-images attachment links inline).
func ContainsEmbeddedImage(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	return markdownImageRe.MatchString(text) ||
		imgTagRe.MatchString(text) ||
		imageURLRe.MatchString(text)
}
