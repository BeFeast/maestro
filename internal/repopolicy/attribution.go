package repopolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var conventionFiles = []string{"AGENTS.md", "CLAUDE.md", "CONTRIBUTING.md"}

// ProhibitsPublicAIAttribution reports whether the repository's effective
// convention file explicitly keeps AI/backend attribution out of public git or
// GitHub artifacts. The precedence matches worker prompt assembly: AGENTS.md,
// then CLAUDE.md, then CONTRIBUTING.md.
func ProhibitsPublicAIAttribution(root string) (bool, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return false, nil
	}

	for _, name := range conventionFiles {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("read repository policy %s: %w", name, err)
		}
		if strings.TrimSpace(string(data)) == "" {
			continue
		}
		return textProhibitsPublicAIAttribution(string(data)), nil
	}
	return false, nil
}

func textProhibitsPublicAIAttribution(text string) bool {
	text = strings.ToLower(strings.ReplaceAll(text, "‑", "-"))
	paragraphs := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	for _, paragraph := range paragraphs {
		line := strings.Join(strings.Fields(paragraph), " ")
		if line == "" || !mentionsAttribution(line) {
			continue
		}
		if explicitlyProhibits(line) {
			return true
		}
	}
	return false
}

func mentionsAttribution(line string) bool {
	for _, marker := range []string{
		"ai attribution", "agent attribution", "backend attribution", "model attribution",
		"maestro-backend", "co-authored-by", "generated with",
	} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

func explicitlyProhibits(line string) bool {
	for _, marker := range []string{
		"do not", "don't", "must not", "never", "forbid", "prohibit",
		"not allowed", "no ai attribution", "keep backend attribution internal",
		"keep model attribution internal", "attribution-free", "attribution free",
	} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

// ContainsForbiddenPublicAttribution identifies attribution records that must
// not be published when repository policy prohibits AI attribution. Human
// co-author trailers are preserved; only known AI-agent co-authors are blocked.
func ContainsForbiddenPublicAttribution(text string) bool {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "maestro-backend:"),
			strings.HasPrefix(lower, "maestro-model:"),
			strings.HasPrefix(lower, "maestro-agent:"),
			strings.Contains(lower, "generated with claude"),
			strings.Contains(lower, "generated with codex"),
			strings.Contains(lower, "generated with chatgpt"),
			strings.Contains(lower, "generated with gemini"):
			return true
		case strings.HasPrefix(lower, "co-authored-by:") && aiCoauthor(lower):
			return true
		}
	}
	return false
}

func aiCoauthor(line string) bool {
	for _, marker := range []string{
		"claude", "codex", "chatgpt", "openai", "anthropic", "gemini", "copilot", "maestro",
	} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}
