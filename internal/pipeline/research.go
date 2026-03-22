package pipeline

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// researchDir is the directory under the worktree where research files are written.
const researchDir = ".maestro/research"

// runResearch performs the pre-coding research phase.
// It scans the codebase for files relevant to the issue keywords and writes
// a context file to .maestro/research/<issue-number>.md.
// Returns the research context as a string.
func runResearch(worktreePath string, issueNumber int, issueTitle, issueBody string) (string, error) {
	keywords := extractKeywords(issueTitle, issueBody)
	if len(keywords) == 0 {
		return "", fmt.Errorf("no keywords extracted from issue")
	}
	log.Printf("[pipeline] research: extracted %d keywords: %v", len(keywords), keywords)

	// Scan codebase for relevant files
	relevantFiles := findRelevantFiles(worktreePath, keywords)
	log.Printf("[pipeline] research: found %d relevant files", len(relevantFiles))

	// Build the research context document
	var sb strings.Builder
	sb.WriteString("# Pre-coding Research Context\n\n")
	sb.WriteString(fmt.Sprintf("## Keywords: %s\n\n", strings.Join(keywords, ", ")))

	if len(relevantFiles) == 0 {
		sb.WriteString("No directly relevant files found. The worker should explore the codebase.\n")
	} else {
		sb.WriteString("## Relevant Files\n\n")
		// Cap at 20 files to keep context focused
		limit := len(relevantFiles)
		if limit > 20 {
			limit = 20
		}
		for _, rf := range relevantFiles[:limit] {
			sb.WriteString(fmt.Sprintf("- `%s` — matches: %s\n", rf.Path, strings.Join(rf.MatchedKeywords, ", ")))
		}
		if len(relevantFiles) > 20 {
			sb.WriteString(fmt.Sprintf("\n...and %d more files.\n", len(relevantFiles)-20))
		}

		// Read snippets from top relevant files
		sb.WriteString("\n## Key File Snippets\n\n")
		snippetLimit := 5
		if snippetLimit > len(relevantFiles) {
			snippetLimit = len(relevantFiles)
		}
		for _, rf := range relevantFiles[:snippetLimit] {
			snippet := readFileHead(filepath.Join(worktreePath, rf.Path), 30)
			if snippet != "" {
				sb.WriteString(fmt.Sprintf("### %s\n```\n%s\n```\n\n", rf.Path, snippet))
			}
		}
	}

	// Discover project structure patterns
	patterns := discoverPatterns(worktreePath)
	if len(patterns) > 0 {
		sb.WriteString("## Project Patterns\n\n")
		for _, p := range patterns {
			sb.WriteString(fmt.Sprintf("- %s\n", p))
		}
	}

	context := sb.String()

	// Write research file
	researchPath := filepath.Join(worktreePath, researchDir)
	if err := os.MkdirAll(researchPath, 0755); err != nil {
		return context, fmt.Errorf("create research dir: %w", err)
	}
	outFile := filepath.Join(researchPath, fmt.Sprintf("%d.md", issueNumber))
	if err := os.WriteFile(outFile, []byte(context), 0644); err != nil {
		return context, fmt.Errorf("write research file: %w", err)
	}
	log.Printf("[pipeline] research: wrote context to %s (%d bytes)", outFile, len(context))

	return context, nil
}

// relevantFile represents a file that matched research keywords.
type relevantFile struct {
	Path            string
	MatchedKeywords []string
}

// extractKeywords extracts meaningful keywords from issue title and body.
func extractKeywords(title, body string) []string {
	// Combine title and body, split into words
	text := title + " " + body

	// Remove markdown formatting, URLs, code blocks
	text = regexp.MustCompile("```[\\s\\S]*?```").ReplaceAllString(text, "")
	text = regexp.MustCompile("`[^`]+`").ReplaceAllString(text, "")
	text = regexp.MustCompile(`https?://\S+`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`[#*_\[\]()>~]`).ReplaceAllString(text, " ")

	words := strings.Fields(strings.ToLower(text))

	// Filter: keep words that are meaningful (>3 chars, not stop words)
	stopWords := map[string]bool{
		"the": true, "and": true, "for": true, "that": true, "this": true,
		"with": true, "from": true, "are": true, "was": true, "were": true,
		"been": true, "have": true, "has": true, "had": true, "not": true,
		"but": true, "what": true, "all": true, "can": true, "will": true,
		"each": true, "which": true, "their": true, "there": true, "when": true,
		"should": true, "would": true, "could": true, "does": true, "into": true,
		"before": true, "after": true, "about": true, "between": true,
		"true": true, "false": true, "default": true, "also": true,
		"more": true, "than": true, "then": true, "them": true, "they": true,
		"some": true, "other": true, "every": true, "must": true, "only": true,
	}

	seen := make(map[string]bool)
	var keywords []string
	for _, w := range words {
		// Clean non-alphanumeric edges
		w = strings.Trim(w, ".,;:!?\"'()-/")
		if len(w) < 3 || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		keywords = append(keywords, w)
	}

	// Limit to top 15 keywords (prefer shorter list for focused search)
	if len(keywords) > 15 {
		keywords = keywords[:15]
	}

	return keywords
}

// findRelevantFiles walks the worktree and finds files matching keywords.
func findRelevantFiles(worktreePath string, keywords []string) []relevantFile {
	var results []relevantFile
	seen := make(map[string]bool)

	// Skip common non-source directories
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, ".maestro": true,
		"target": true, "dist": true, "build": true, "__pycache__": true,
	}

	filepath.Walk(worktreePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		// Only consider source files
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if !isSourceExt(ext) {
			return nil
		}

		relPath, err := filepath.Rel(worktreePath, path)
		if err != nil {
			return nil
		}

		// Check if filename or path matches any keyword
		lowerPath := strings.ToLower(relPath)
		var matched []string
		for _, kw := range keywords {
			if strings.Contains(lowerPath, kw) {
				matched = append(matched, kw)
			}
		}

		if len(matched) > 0 && !seen[relPath] {
			seen[relPath] = true
			results = append(results, relevantFile{Path: relPath, MatchedKeywords: matched})
		}

		return nil
	})

	// Sort by number of matched keywords (more matches = more relevant)
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if len(results[j].MatchedKeywords) > len(results[i].MatchedKeywords) {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	return results
}

// isSourceExt returns true for common source file extensions.
func isSourceExt(ext string) bool {
	switch ext {
	case ".go", ".rs", ".py", ".js", ".ts", ".tsx", ".jsx",
		".java", ".rb", ".c", ".h", ".cpp", ".hpp",
		".yaml", ".yml", ".toml", ".json", ".md",
		".sh", ".bash", ".zsh":
		return true
	}
	return false
}

// readFileHead reads the first N lines of a file.
func readFileHead(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	allLines := strings.Split(string(data), "\n")
	if len(allLines) > lines {
		allLines = allLines[:lines]
	}
	return strings.Join(allLines, "\n")
}

// discoverPatterns identifies project structure patterns in the worktree.
func discoverPatterns(worktreePath string) []string {
	var patterns []string

	// Check for Go project
	if _, err := os.Stat(filepath.Join(worktreePath, "go.mod")); err == nil {
		patterns = append(patterns, "Go project (go.mod found)")
	}

	// Check for Rust project
	if _, err := os.Stat(filepath.Join(worktreePath, "Cargo.toml")); err == nil {
		patterns = append(patterns, "Rust project (Cargo.toml found)")
	}

	// Check for Node project
	if _, err := os.Stat(filepath.Join(worktreePath, "package.json")); err == nil {
		patterns = append(patterns, "Node.js project (package.json found)")
	}

	// Check for Python project
	for _, f := range []string{"setup.py", "pyproject.toml", "requirements.txt"} {
		if _, err := os.Stat(filepath.Join(worktreePath, f)); err == nil {
			patterns = append(patterns, fmt.Sprintf("Python project (%s found)", f))
			break
		}
	}

	// Check for common directories
	dirs := []struct {
		name    string
		pattern string
	}{
		{"cmd", "CLI entry points in cmd/"},
		{"internal", "Internal packages in internal/"},
		{"pkg", "Public packages in pkg/"},
		{"src", "Source code in src/"},
		{"test", "Tests in test/"},
		{"tests", "Tests in tests/"},
	}
	for _, d := range dirs {
		if info, err := os.Stat(filepath.Join(worktreePath, d.name)); err == nil && info.IsDir() {
			patterns = append(patterns, d.pattern)
		}
	}

	return patterns
}
