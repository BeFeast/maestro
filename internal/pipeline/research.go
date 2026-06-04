package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// researchDir is the directory under the worktree where research files are written.
const researchDir = ".maestro/research"

var goplsBinary = "gopls"

var goplsAggregateTimeout = 20 * time.Second

var (
	markdownCodeBlockRe      = regexp.MustCompile("```[\\s\\S]*?```")
	markdownInlineCodeRe     = regexp.MustCompile("`[^`]+`")
	markdownURLRe            = regexp.MustCompile(`https?://\S+`)
	markdownFormattingRe     = regexp.MustCompile(`[#*_\[\]()>~]`)
	goCodeSpanSymbolRe       = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*)`")
	goIdentifierRe           = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)
	goSymbolCandidateShapeRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	goplsSpanRe              = regexp.MustCompile(`^(.+):([0-9]+):([0-9]+)`)
)

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

	symbols, err := findSymbolContexts(worktreePath, issueTitle, issueBody)
	if err != nil {
		log.Printf("[pipeline] research: symbol context unavailable: %v (falling back to filename search only)", err)
	} else if len(symbols) > 0 {
		log.Printf("[pipeline] research: found symbol context for %d symbols", len(symbols))
		appendSymbolContexts(&sb, worktreePath, symbols)
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

type symbolContext struct {
	Symbol          string
	LookupPath      string
	LookupLine      int
	LookupCharacter int
	Definitions     []symbolLocation
	References      []symbolLocation
	Implementations []symbolLocation
}

type symbolLocation struct {
	Path      string
	Line      int
	Character int
}

// extractKeywords extracts meaningful keywords from issue title and body.
func extractKeywords(title, body string) []string {
	// Combine title and body, split into words
	text := title + " " + body

	// Remove markdown formatting, URLs, code blocks
	text = markdownCodeBlockRe.ReplaceAllString(text, "")
	text = markdownInlineCodeRe.ReplaceAllString(text, "")
	text = markdownURLRe.ReplaceAllString(text, "")
	text = markdownFormattingRe.ReplaceAllString(text, " ")

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

func findSymbolContexts(worktreePath, issueTitle, issueBody string) ([]symbolContext, error) {
	if _, err := os.Stat(filepath.Join(worktreePath, "go.mod")); err != nil {
		return nil, fmt.Errorf("not a Go module")
	}
	if _, err := exec.LookPath(goplsBinary); err != nil {
		return nil, fmt.Errorf("%s not available: %w", goplsBinary, err)
	}

	candidates := extractSymbolCandidates(issueTitle, issueBody)
	if len(candidates) == 0 {
		return nil, nil
	}

	goFiles := listGoSourceFiles(worktreePath)
	if len(goFiles) == 0 {
		return nil, nil
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), goplsAggregateTimeout)
	defer cancel()

	var contexts []symbolContext
	for _, symbol := range candidates {
		if queryCtx.Err() != nil {
			return contexts, nil
		}
		path, line, char, ok := findSymbolTokenPosition(worktreePath, goFiles, symbol)
		if !ok {
			continue
		}
		ctx := symbolContext{
			Symbol:          symbol,
			LookupPath:      path,
			LookupLine:      line,
			LookupCharacter: char,
		}
		var err error
		ctx.Definitions, err = runGoplsLocationQuery(queryCtx, worktreePath, "definition", path, line, char)
		if err != nil {
			log.Printf("[pipeline] research: gopls definition failed for %s: %v", symbol, err)
			if queryCtx.Err() != nil {
				return contexts, nil
			}
			continue
		}
		ctx.References, err = runGoplsLocationQuery(queryCtx, worktreePath, "references", path, line, char)
		if err != nil {
			log.Printf("[pipeline] research: gopls references failed for %s: %v", symbol, err)
			if queryCtx.Err() != nil {
				return contexts, nil
			}
		}
		ctx.Implementations, err = runGoplsLocationQuery(queryCtx, worktreePath, "implementation", path, line, char)
		if err != nil {
			log.Printf("[pipeline] research: gopls implementation failed for %s: %v", symbol, err)
			if queryCtx.Err() != nil {
				return contexts, nil
			}
		}
		if len(ctx.Definitions)+len(ctx.References)+len(ctx.Implementations) > 0 {
			contexts = append(contexts, ctx)
		}
		if len(contexts) >= 3 {
			break
		}
	}

	return contexts, nil
}

func appendSymbolContexts(sb *strings.Builder, worktreePath string, contexts []symbolContext) {
	sb.WriteString("\n## Symbol Context (gopls)\n\n")
	sb.WriteString("Language-server context for primary Go symbols named in the issue.\n\n")
	for _, ctx := range contexts {
		sb.WriteString(fmt.Sprintf("### `%s`\n\n", ctx.Symbol))
		sb.WriteString(fmt.Sprintf("Lookup position: `%s:%d:%d`\n\n", ctx.LookupPath, ctx.LookupLine, ctx.LookupCharacter))
		appendSymbolLocationList(sb, "Definitions", ctx.Definitions, 5)
		appendSymbolLocationList(sb, "References", ctx.References, 10)
		appendSymbolLocationList(sb, "Implementations", ctx.Implementations, 10)
		appendSymbolSnippets(sb, worktreePath, ctx)
	}
}

func appendSymbolLocationList(sb *strings.Builder, title string, locs []symbolLocation, limit int) {
	if len(locs) == 0 {
		return
	}
	sb.WriteString(fmt.Sprintf("**%s**\n", title))
	for i, loc := range locs {
		if i >= limit {
			sb.WriteString(fmt.Sprintf("- ...and %d more.\n", len(locs)-limit))
			break
		}
		sb.WriteString(fmt.Sprintf("- `%s:%d:%d`\n", loc.Path, loc.Line, loc.Character))
	}
	sb.WriteString("\n")
}

func appendSymbolSnippets(sb *strings.Builder, worktreePath string, ctx symbolContext) {
	locs := dedupeSymbolLocations(append(append([]symbolLocation{}, ctx.Definitions...), ctx.References...))
	if len(locs) == 0 {
		return
	}
	sb.WriteString("**Snippets**\n\n")
	limit := len(locs)
	if limit > 3 {
		limit = 3
	}
	for _, loc := range locs[:limit] {
		snippet := readFileWindow(filepath.Join(worktreePath, loc.Path), loc.Line, 4)
		if snippet == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("`%s:%d`\n```go\n%s\n```\n\n", loc.Path, loc.Line, snippet))
	}
}

func extractSymbolCandidates(title, body string) []string {
	var candidates []string
	seen := map[string]bool{}
	add := func(s string) {
		if !isUsefulGoSymbolCandidate(s) || seen[s] {
			return
		}
		seen[s] = true
		candidates = append(candidates, s)
	}

	for _, m := range goCodeSpanSymbolRe.FindAllStringSubmatch(title+" "+body, -1) {
		add(m[1])
	}

	for _, m := range goIdentifierRe.FindAllString(title+" "+body, -1) {
		if hasGoIdentifierShape(m) {
			add(m)
		}
	}

	if len(candidates) > 8 {
		candidates = candidates[:8]
	}
	return candidates
}

func isUsefulGoSymbolCandidate(s string) bool {
	if len(s) < 3 {
		return false
	}
	goKeywords := map[string]bool{
		"break": true, "case": true, "chan": true, "const": true, "continue": true,
		"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
		"func": true, "go": true, "goto": true, "if": true, "import": true,
		"interface": true, "map": true, "package": true, "range": true, "return": true,
		"select": true, "struct": true, "switch": true, "type": true, "var": true,
	}
	lower := strings.ToLower(s)
	if goKeywords[lower] {
		return false
	}
	return goSymbolCandidateShapeRe.MatchString(s)
}

func hasGoIdentifierShape(s string) bool {
	if strings.Contains(s, "_") {
		return true
	}
	if len(s) >= 5 && s[0] >= 'a' && s[0] <= 'z' {
		for _, r := range s[1:] {
			if r >= 'A' && r <= 'Z' {
				return true
			}
		}
	}
	return len(s) >= 3 && s[0] >= 'A' && s[0] <= 'Z'
}

func listGoSourceFiles(worktreePath string) []string {
	var files []string
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, ".maestro": true,
		"target": true, "dist": true, "build": true, "__pycache__": true,
	}
	_ = filepath.Walk(worktreePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(info.Name(), ".go") {
			rel, err := filepath.Rel(worktreePath, path)
			if err == nil {
				files = append(files, rel)
			}
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func findSymbolTokenPosition(worktreePath string, files []string, symbol string) (string, int, int, bool) {
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(worktreePath, rel))
		if err != nil {
			continue
		}
		for lineIndex, line := range strings.Split(string(data), "\n") {
			if loc := findIdentifierTokenIndex(line, symbol); loc >= 0 {
				return rel, lineIndex + 1, loc + 1, true
			}
		}
	}
	return "", 0, 0, false
}

func findIdentifierTokenIndex(line, symbol string) int {
	for offset := 0; offset < len(line); {
		idx := strings.Index(line[offset:], symbol)
		if idx < 0 {
			return -1
		}
		idx += offset
		end := idx + len(symbol)
		if isIdentifierBoundary(line, idx-1) && isIdentifierBoundary(line, end) {
			return idx
		}
		offset = end
	}
	return -1
}

func isIdentifierBoundary(s string, idx int) bool {
	if idx < 0 || idx >= len(s) {
		return true
	}
	c := s[idx]
	return !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_')
}

func runGoplsLocationQuery(parentCtx context.Context, worktreePath, verb, relPath string, line, char int) ([]symbolLocation, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()

	pos := fmt.Sprintf("%s:%d:%d", filepath.Join(worktreePath, relPath), line, char)
	cmd := exec.CommandContext(ctx, goplsBinary, verb, "-json", pos)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return parseGoplsLocations(worktreePath, out), nil
}

func parseGoplsLocations(worktreePath string, data []byte) []symbolLocation {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var locs []symbolLocation
	walkGoplsJSON(worktreePath, raw, func(uri string, line, character int) {
		path := uriToPath(uri)
		if path == "" {
			return
		}
		rel, err := filepath.Rel(worktreePath, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			return
		}
		locs = append(locs, symbolLocation{
			Path:      filepath.ToSlash(rel),
			Line:      line + 1,
			Character: character + 1,
		})
	})
	return dedupeSymbolLocations(locs)
}

func walkGoplsJSON(worktreePath string, v any, emit func(uri string, line, character int)) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			walkGoplsJSON(worktreePath, item, emit)
		}
	case map[string]any:
		if span, ok := x["span"].(string); ok {
			if loc, ok := parseGoplsSpan(worktreePath, span); ok {
				emit("file://"+loc.Path, loc.Line-1, loc.Character-1)
			}
		}
		uri, _ := x["uri"].(string)
		if uri == "" {
			if target, ok := x["targetUri"].(string); ok {
				uri = target
			}
		}
		if uri != "" {
			if line, character, ok := jsonRangeStart(x); ok {
				emit(uri, line, character)
			}
		}
		for _, item := range x {
			walkGoplsJSON(worktreePath, item, emit)
		}
	case string:
		if loc, ok := parseGoplsSpan(worktreePath, x); ok {
			emit("file://"+loc.Path, loc.Line-1, loc.Character-1)
		}
	}
}

func jsonRangeStart(m map[string]any) (int, int, bool) {
	rangeValue := m["range"]
	if rangeValue == nil {
		rangeValue = m["targetRange"]
	}
	rangeMap, ok := rangeValue.(map[string]any)
	if !ok {
		return 0, 0, false
	}
	start, ok := rangeMap["start"].(map[string]any)
	if !ok {
		return 0, 0, false
	}
	line, lineOK := start["line"].(float64)
	char, charOK := start["character"].(float64)
	return int(line), int(char), lineOK && charOK
}

func uriToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return u.Path
}

func parseGoplsSpan(worktreePath, span string) (symbolLocation, bool) {
	match := goplsSpanRe.FindStringSubmatch(span)
	if match == nil {
		return symbolLocation{}, false
	}
	line, err := parsePositiveInt(match[2])
	if err != nil {
		return symbolLocation{}, false
	}
	character, err := parsePositiveInt(match[3])
	if err != nil {
		return symbolLocation{}, false
	}
	path := match[1]
	if worktreePath != "" && !filepath.IsAbs(path) {
		path = filepath.Join(worktreePath, path)
	}
	return symbolLocation{Path: path, Line: line, Character: character}, true
}

func parsePositiveInt(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a positive integer: %s", s)
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("not a positive integer: %s", s)
	}
	return n, nil
}

func dedupeSymbolLocations(locs []symbolLocation) []symbolLocation {
	seen := map[string]bool{}
	var out []symbolLocation
	for _, loc := range locs {
		key := fmt.Sprintf("%s:%d:%d", loc.Path, loc.Line, loc.Character)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, loc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Character < out[j].Character
	})
	return out
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

func readFileWindow(path string, centerLine, radius int) string {
	data, err := os.ReadFile(path)
	if err != nil || centerLine <= 0 {
		return ""
	}
	allLines := strings.Split(string(data), "\n")
	start := centerLine - radius
	if start < 1 {
		start = 1
	}
	end := centerLine + radius
	if end > len(allLines) {
		end = len(allLines)
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		b.WriteString(fmt.Sprintf("%4d: %s", i, allLines[i-1]))
		if i < end {
			b.WriteString("\n")
		}
	}
	return b.String()
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
