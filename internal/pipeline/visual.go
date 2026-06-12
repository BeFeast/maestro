package pipeline

// Visual-evidence verify step (#705). Opt-in per project via verify.visual:
// a project-specific capture command produces screenshots for UI-affecting
// changes, classified by glob patterns over the PR's changed files. The
// helpers here are deterministic and side-effect free except for
// RunVisualCapture, which executes the configured command.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/befeast/maestro/internal/config"
)

// visualImageExtensions are the file extensions counted as screenshot output.
var visualImageExtensions = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".gif":  true,
	".webp": true,
}

// MatchesVisualPath reports whether a repo-relative file path matches one
// doublestar-style glob pattern. A `**` segment matches any number of path
// segments (including zero); other segments use path.Match semantics
// (`*`, `?`, `[...]` within a single segment). A pattern with a trailing
// slash is treated as a directory prefix (`web/` ≡ `web/**`).
func MatchesVisualPath(pattern, file string) bool {
	pattern = normalizeVisualPath(pattern)
	file = normalizeVisualPath(file)
	if pattern == "" || file == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return matchVisualSegments(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

func normalizeVisualPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	return strings.TrimPrefix(p, "./")
}

func matchVisualSegments(pattern, parts []string) bool {
	if len(pattern) == 0 {
		return len(parts) == 0
	}
	if pattern[0] == "**" {
		for skip := 0; skip <= len(parts); skip++ {
			if matchVisualSegments(pattern[1:], parts[skip:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], parts[0])
	if err != nil || !ok {
		return false
	}
	return matchVisualSegments(pattern[1:], parts[1:])
}

// MatchUIAffectingFiles returns the subset of changed files that match any
// of the configured UI path globs, preserving input order. An empty result
// means the change is not UI-affecting.
func MatchUIAffectingFiles(patterns, changedFiles []string) []string {
	if len(patterns) == 0 || len(changedFiles) == 0 {
		return nil
	}
	var matched []string
	for _, file := range changedFiles {
		for _, pattern := range patterns {
			if MatchesVisualPath(pattern, file) {
				matched = append(matched, file)
				break
			}
		}
	}
	return matched
}

// RunVisualCapture executes the configured capture command from the worktree
// root and returns the worktree-relative paths of image files found in the
// output directory afterwards, sorted lexically. The command sees the
// absolute output directory as $MAESTRO_SCREENSHOT_DIR. A non-zero exit or
// timeout returns an error together with whatever screenshots already exist —
// callers treat both as advisory (warning + finding), never as a merge block.
func RunVisualCapture(v config.VerifyVisualConfig, worktreePath string) ([]string, error) {
	command := strings.TrimSpace(v.Command)
	if command == "" {
		return nil, fmt.Errorf("verify.visual: no capture command configured")
	}

	outputDir := filepath.Join(worktreePath, v.ResolvedOutputDir())
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("verify.visual: create output dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), v.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = worktreePath
	cmd.Env = append(os.Environ(), "MAESTRO_SCREENSHOT_DIR="+outputDir)
	out, err := cmd.CombinedOutput()

	shots := ListScreenshots(worktreePath, v.ResolvedOutputDir())
	if ctx.Err() == context.DeadlineExceeded {
		return shots, fmt.Errorf("verify.visual: capture command timed out after %s", v.Timeout())
	}
	if err != nil {
		return shots, fmt.Errorf("verify.visual: capture command failed: %v: %s", err, firstOutputLine(out))
	}
	return shots, nil
}

// ListScreenshots returns worktree-relative paths of image files under the
// output directory (recursive), sorted lexically. A missing directory yields
// an empty list.
func ListScreenshots(worktreePath, outputDir string) []string {
	root := filepath.Join(worktreePath, outputDir)
	var shots []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort scan: skip unreadable entries
		}
		if !visualImageExtensions[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if rel, relErr := filepath.Rel(worktreePath, p); relErr == nil {
			shots = append(shots, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(shots)
	return shots
}

func firstOutputLine(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "(no output)"
	}
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	const maxLen = 200
	if len(text) > maxLen {
		text = text[:maxLen] + "…"
	}
	return text
}
