package versioning

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/befeast/maestro/internal/config"
)

// PRClient abstracts the GitHub PR methods needed by the version bump flow.
type PRClient interface {
	PRLabels(prNumber int) ([]string, error)
	PRCommits(prNumber int) ([]string, error)
	CreateRelease(tag, title string) error
}

// BumpType represents a semver bump level.
type BumpType int

const (
	BumpPatch BumpType = iota
	BumpMinor
	BumpMajor
)

func (b BumpType) String() string {
	switch b {
	case BumpMajor:
		return "major"
	case BumpMinor:
		return "minor"
	default:
		return "patch"
	}
}

// ParseBumpType converts a string to a BumpType.
func ParseBumpType(s string) BumpType {
	switch strings.ToLower(s) {
	case "major":
		return BumpMajor
	case "minor":
		return BumpMinor
	default:
		return BumpPatch
	}
}

// Version represents a semver version.
type Version struct {
	Major int
	Minor int
	Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ParseVersion parses a "X.Y.Z" version string.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q: expected X.Y.Z", s)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Version{}, fmt.Errorf("invalid major version %q: %w", parts[0], err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Version{}, fmt.Errorf("invalid minor version %q: %w", parts[1], err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return Version{}, fmt.Errorf("invalid patch version %q: %w", parts[2], err)
	}
	return Version{Major: major, Minor: minor, Patch: patch}, nil
}

// Bump returns a new version bumped by the given type.
func Bump(v Version, bt BumpType) Version {
	switch bt {
	case BumpMajor:
		return Version{Major: v.Major + 1}
	case BumpMinor:
		return Version{Major: v.Major, Minor: v.Minor + 1}
	default:
		return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
	}
}

// DetectBumpFromLabels reads version labels from a label list.
// Labels: version:patch, version:minor, version:major.
// Returns the highest bump found, or the default.
func DetectBumpFromLabels(labels []string, defaultBump string) (BumpType, bool) {
	highest := BumpType(-1)
	for _, label := range labels {
		switch strings.ToLower(label) {
		case "version:major":
			if BumpMajor > highest {
				highest = BumpMajor
			}
		case "version:minor":
			if BumpMinor > highest {
				highest = BumpMinor
			}
		case "version:patch":
			if BumpPatch > highest {
				highest = BumpPatch
			}
		}
	}
	if highest >= 0 {
		return highest, true
	}
	return ParseBumpType(defaultBump), false
}

// DetectBumpFromCommits parses conventional commit prefixes.
// feat!: or BREAKING CHANGE → major, feat: → minor, fix: → patch.
func DetectBumpSignalFromCommits(messages []string) (BumpType, bool) {
	highest := BumpType(-1)
	for _, msg := range messages {
		lower := strings.ToLower(msg)
		switch {
		case strings.Contains(lower, "!:") || strings.Contains(lower, "breaking change"):
			if BumpMajor > highest {
				highest = BumpMajor
			}
		case strings.HasPrefix(lower, "feat"):
			if BumpMinor > highest {
				highest = BumpMinor
			}
		case strings.HasPrefix(lower, "fix"):
			if BumpPatch > highest {
				highest = BumpPatch
			}
		}
	}
	if highest >= 0 {
		return highest, true
	}
	return BumpPatch, false
}

// DetectBumpFromCommits parses conventional commit prefixes and returns the
// configured default when no conventional-commit signal is present.
func DetectBumpFromCommits(messages []string, defaultBump string) BumpType {
	if bump, ok := DetectBumpSignalFromCommits(messages); ok {
		return bump
	}
	return ParseBumpType(defaultBump)
}

// versionPatterns are regex patterns to find version strings in different file types.
var versionPatterns = []*regexp.Regexp{
	// Cargo.toml: version = "X.Y.Z"
	regexp.MustCompile(`(?m)^(\s*version\s*=\s*")(\d+\.\d+\.\d+)(")`),
	// package.json: "version": "X.Y.Z"
	regexp.MustCompile(`(?m)("version"\s*:\s*")(\d+\.\d+\.\d+)(")`),
}

// ReadVersionFromFile reads the first semver version found in a file.
func ReadVersionFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, pat := range versionPatterns {
		if m := pat.FindSubmatch(data); m != nil {
			return string(m[2]), nil
		}
	}
	return "", fmt.Errorf("no version found in %s", path)
}

// ReadCurrentVersion reads the version from the first configured file that has one.
func ReadCurrentVersion(files []string) (string, error) {
	for _, f := range files {
		v, err := ReadVersionFromFile(f)
		if err == nil {
			return v, nil
		}
	}
	return "", fmt.Errorf("no version found in any configured file")
}

// replaceVersionField replaces the first version field whose value equals
// oldVer, using the same anchored patterns as ReadVersionFromFile. It must
// not touch other occurrences of the version string (e.g. a dependency pin
// like "yt-dlp-ejs==0.8.0" in pyproject.toml, or a nested "version" key in
// a lockfile dependency that coincidentally matches the project version).
func replaceVersionField(content, oldVer, newVer string) (string, bool) {
	for _, pat := range versionPatterns {
		for _, m := range pat.FindAllStringSubmatchIndex(content, -1) {
			// m[4]:m[5] bound the version capture group.
			if content[m[4]:m[5]] != oldVer {
				continue
			}
			return content[:m[4]] + newVer + content[m[5]:], true
		}
	}
	return content, false
}

// UpdateVersionInFile updates the project version field from oldVer to newVer
// in the file, leaving any other occurrences of oldVer untouched.
func UpdateVersionInFile(path, oldVer, newVer string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	updated, replaced := replaceVersionField(string(data), oldVer, newVer)
	if !replaced {
		return fmt.Errorf("version field %s not found in %s", oldVer, path)
	}
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// CommitAndTag creates a version bump commit and tag in the given repo.
func CommitAndTag(repoPath, version, tagPrefix string, files []string) error {
	tag := tagPrefix + version
	commitMsg := fmt.Sprintf("chore: bump version to %s", version)

	addArgs := append([]string{"add", "--"}, files...)
	if out, err := runGit(repoPath, addArgs...); err != nil {
		return fmt.Errorf("git add: %w\n%s", err, out)
	}

	// Commit
	if out, err := runGit(repoPath, "commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("git commit: %w\n%s", err, out)
	}

	// Tag
	if out, err := runGit(repoPath, "tag", "-a", tag, "-m", tag); err != nil {
		return fmt.Errorf("git tag: %w\n%s", err, out)
	}

	// Push commit and tag
	if out, err := runGit(repoPath, "push", "origin", "main", "--follow-tags"); err != nil {
		return fmt.Errorf("git push: %w\n%s", err, out)
	}

	return nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ResolveFiles resolves version file paths relative to the repo local path.
func ResolveFiles(localPath string, files []string) []string {
	resolved := make([]string, len(files))
	for i, f := range files {
		if strings.HasPrefix(f, "/") {
			resolved[i] = f
		} else {
			resolved[i] = localPath + "/" + f
		}
	}
	return resolved
}

// BumpResult holds the result of a version bump detection.
type BumpResult struct {
	OldVersion Version
	NewVersion Version
	BumpType   BumpType
}

// BatchBumpResult holds the result of a batched version bump.
type BatchBumpResult struct {
	BumpResult
	LastTag     string
	CommitCount int
	PRNumbers   []int
	NoChanges   bool
}

// DetectBump reads PR labels and commits to determine the bump type,
// then computes the new version. It reads the current version from the
// given files and uses labels-first with commit-message fallback.
func DetectBump(gh PRClient, prNumber int, files []string, defaultBump string) (BumpResult, error) {
	// Read current version
	currentStr, err := ReadCurrentVersion(files)
	if err != nil {
		return BumpResult{}, fmt.Errorf("read current version: %w", err)
	}
	current, err := ParseVersion(currentStr)
	if err != nil {
		return BumpResult{}, fmt.Errorf("parse current version: %w", err)
	}

	// Detect bump type from PR labels
	labels, err := gh.PRLabels(prNumber)
	if err != nil {
		return BumpResult{}, fmt.Errorf("get PR labels: %w", err)
	}

	bumpType, fromLabel := DetectBumpFromLabels(labels, defaultBump)

	// Fallback to conventional commits if no version label found
	if !fromLabel {
		commits, err := gh.PRCommits(prNumber)
		if err != nil {
			log.Printf("[versioning] warn: could not read PR commits: %v, using default bump", err)
		} else {
			bumpType = DetectBumpFromCommits(commits, defaultBump)
		}
	}

	newVer := Bump(current, bumpType)
	return BumpResult{OldVersion: current, NewVersion: newVer, BumpType: bumpType}, nil
}

var prNumberPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\(#(\d+)\)`),
	regexp.MustCompile(`(?i)\bpull request #(\d+)\b`),
}

func extractPRNumbers(messages []string) []int {
	seen := make(map[int]struct{})
	for _, msg := range messages {
		for _, pat := range prNumberPatterns {
			for _, match := range pat.FindAllStringSubmatch(msg, -1) {
				if len(match) < 2 {
					continue
				}
				n, err := strconv.Atoi(match[1])
				if err != nil || n <= 0 {
					continue
				}
				seen[n] = struct{}{}
			}
		}
	}
	prs := make([]int, 0, len(seen))
	for n := range seen {
		prs = append(prs, n)
	}
	sort.Ints(prs)
	return prs
}

func maxBump(a, b BumpType) BumpType {
	if b > a {
		return b
	}
	return a
}

// DetectBatchBump computes one bump for all commits and PR labels in a range.
func DetectBatchBump(gh PRClient, files []string, defaultBump string, messages []string) (BumpResult, []int, error) {
	currentStr, err := ReadCurrentVersion(files)
	if err != nil {
		return BumpResult{}, nil, fmt.Errorf("read current version: %w", err)
	}
	current, err := ParseVersion(currentStr)
	if err != nil {
		return BumpResult{}, nil, fmt.Errorf("parse current version: %w", err)
	}

	highest := BumpType(-1)
	prNumbers := extractPRNumbers(messages)
	for _, prNumber := range prNumbers {
		labels, err := gh.PRLabels(prNumber)
		if err != nil {
			return BumpResult{}, nil, fmt.Errorf("get PR %d labels: %w", prNumber, err)
		}
		if bump, ok := DetectBumpFromLabels(labels, defaultBump); ok {
			highest = maxBump(highest, bump)
		}
	}

	if bump, ok := DetectBumpSignalFromCommits(messages); ok {
		highest = maxBump(highest, bump)
	}
	if highest < 0 {
		highest = ParseBumpType(defaultBump)
	}

	newVer := Bump(current, highest)
	return BumpResult{OldVersion: current, NewVersion: newVer, BumpType: highest}, prNumbers, nil
}

func latestTag(repoPath, tagPrefix string) (string, error) {
	match := tagPrefix + "*"
	out, err := runGit(repoPath, "describe", "--tags", "--match", match, "--abbrev=0")
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

func commitMessagesSince(repoPath, tag string) ([]string, error) {
	rangeArg := "HEAD"
	if tag != "" {
		rangeArg = tag + "..HEAD"
	}
	out, err := runGit(repoPath, "log", "--format=%B%x00", rangeArg)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	messages := make([]string, 0, len(parts))
	for _, part := range parts {
		msg := strings.TrimSpace(part)
		if msg == "" {
			continue
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

// RunSinceLastTag executes one batched version bump for commits since the
// latest matching tag. If no commits exist in the range, it returns NoChanges.
func RunSinceLastTag(cfg *config.Config, gh PRClient) (BatchBumpResult, error) {
	if !cfg.Versioning.Enabled {
		log.Printf("[versioning] disabled, skipping")
		return BatchBumpResult{NoChanges: true}, nil
	}

	if len(cfg.Versioning.Files) == 0 {
		return BatchBumpResult{}, fmt.Errorf("versioning enabled but no files configured")
	}

	if out, err := runGit(cfg.LocalPath, "checkout", "main"); err != nil {
		return BatchBumpResult{}, fmt.Errorf("git checkout main: %w\n%s", err, out)
	}
	if out, err := runGit(cfg.LocalPath, "pull", "origin", "main"); err != nil {
		return BatchBumpResult{}, fmt.Errorf("git pull: %w\n%s", err, out)
	}

	lastTag, err := latestTag(cfg.LocalPath, cfg.Versioning.TagPrefix)
	if err != nil {
		return BatchBumpResult{}, fmt.Errorf("find latest tag: %w", err)
	}
	messages, err := commitMessagesSince(cfg.LocalPath, lastTag)
	if err != nil {
		return BatchBumpResult{}, fmt.Errorf("list commits since %q: %w", lastTag, err)
	}
	if len(messages) == 0 {
		log.Printf("[versioning] no commits since latest %s* tag, skipping", cfg.Versioning.TagPrefix)
		return BatchBumpResult{LastTag: lastTag, NoChanges: true}, nil
	}

	files := ResolveFiles(cfg.LocalPath, cfg.Versioning.Files)
	result, prs, err := DetectBatchBump(gh, files, cfg.Versioning.DefaultBump, messages)
	if err != nil {
		return BatchBumpResult{}, err
	}
	log.Printf("[versioning] bumping %s → %s (%s) from %d commit(s) since %s", result.OldVersion, result.NewVersion, result.BumpType, len(messages), lastTag)

	oldStr := result.OldVersion.String()
	newStr := result.NewVersion.String()
	for _, f := range files {
		if err := UpdateVersionInFile(f, oldStr, newStr); err != nil {
			log.Printf("[versioning] warn: %v", err)
			continue
		}
		log.Printf("[versioning] updated %s", f)
	}

	if err := CommitAndTag(cfg.LocalPath, newStr, cfg.Versioning.TagPrefix, files); err != nil {
		return BatchBumpResult{}, fmt.Errorf("commit and tag: %w", err)
	}
	log.Printf("[versioning] committed and tagged %s%s", cfg.Versioning.TagPrefix, result.NewVersion)

	if cfg.Versioning.CreateRelease {
		tag := cfg.Versioning.TagPrefix + newStr
		if err := gh.CreateRelease(tag, tag); err != nil {
			return BatchBumpResult{}, fmt.Errorf("create release: %w", err)
		}
		log.Printf("[versioning] created release %s", tag)
	}

	return BatchBumpResult{
		BumpResult:  result,
		LastTag:     lastTag,
		CommitCount: len(messages),
		PRNumbers:   prs,
	}, nil
}

// Run executes the full version bump flow for a merged PR.
func Run(cfg *config.Config, gh PRClient, prNumber int) error {
	if !cfg.Versioning.Enabled {
		log.Printf("[versioning] disabled, skipping")
		return nil
	}

	if len(cfg.Versioning.Files) == 0 {
		return fmt.Errorf("versioning enabled but no files configured")
	}

	files := ResolveFiles(cfg.LocalPath, cfg.Versioning.Files)

	result, err := DetectBump(gh, prNumber, files, cfg.Versioning.DefaultBump)
	if err != nil {
		return err
	}
	log.Printf("[versioning] bumping %s → %s (%s)", result.OldVersion, result.NewVersion, result.BumpType)

	oldStr := result.OldVersion.String()
	newStr := result.NewVersion.String()

	// Pull latest main before modifying
	if out, err := runGit(cfg.LocalPath, "checkout", "main"); err != nil {
		return fmt.Errorf("git checkout main: %w\n%s", err, out)
	}
	if out, err := runGit(cfg.LocalPath, "pull", "origin", "main"); err != nil {
		return fmt.Errorf("git pull: %w\n%s", err, out)
	}

	// Update version in all configured files
	for _, f := range files {
		if err := UpdateVersionInFile(f, oldStr, newStr); err != nil {
			log.Printf("[versioning] warn: %v", err)
			continue
		}
		log.Printf("[versioning] updated %s", f)
	}

	// Commit, tag, push
	if err := CommitAndTag(cfg.LocalPath, newStr, cfg.Versioning.TagPrefix, files); err != nil {
		return fmt.Errorf("commit and tag: %w", err)
	}
	log.Printf("[versioning] committed and tagged %s%s", cfg.Versioning.TagPrefix, result.NewVersion)

	// Optionally create GitHub release
	if cfg.Versioning.CreateRelease {
		tag := cfg.Versioning.TagPrefix + newStr
		if err := gh.CreateRelease(tag, tag); err != nil {
			return fmt.Errorf("create release: %w", err)
		}
		log.Printf("[versioning] created release %s", tag)
	}

	return nil
}
