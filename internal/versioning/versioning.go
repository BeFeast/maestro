package versioning

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

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
func DetectBumpFromCommits(messages []string, defaultBump string) BumpType {
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
		return highest
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

// UpdateVersionInFile replaces oldVer with newVer in the file.
func UpdateVersionInFile(path, oldVer, newVer string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := string(data)
	updated := strings.ReplaceAll(content, oldVer, newVer)
	if updated == content {
		return fmt.Errorf("version %s not found in %s", oldVer, path)
	}
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// CommitAndTag creates a version bump commit and tag in the given repo.
func CommitAndTag(repoPath, version, tagPrefix string) error {
	tag := tagPrefix + version
	commitMsg := fmt.Sprintf("chore: bump version to %s", version)

	// Stage all changes
	if out, err := runGit(repoPath, "add", "-A"); err != nil {
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

// Run executes the full version bump flow for a merged PR.
func Run(cfg *config.Config, gh *github.Client, prNumber int) error {
	if !cfg.Versioning.Enabled {
		log.Printf("[versioning] disabled, skipping")
		return nil
	}

	if len(cfg.Versioning.Files) == 0 {
		return fmt.Errorf("versioning enabled but no files configured")
	}

	// Resolve file paths relative to repo local path
	files := make([]string, len(cfg.Versioning.Files))
	for i, f := range cfg.Versioning.Files {
		if strings.HasPrefix(f, "/") {
			files[i] = f
		} else {
			files[i] = cfg.LocalPath + "/" + f
		}
	}

	// Read current version
	currentStr, err := ReadCurrentVersion(files)
	if err != nil {
		return fmt.Errorf("read current version: %w", err)
	}
	current, err := ParseVersion(currentStr)
	if err != nil {
		return fmt.Errorf("parse current version: %w", err)
	}
	log.Printf("[versioning] current version: %s", current)

	// Detect bump type from PR labels
	labels, err := gh.PRLabels(prNumber)
	if err != nil {
		return fmt.Errorf("get PR labels: %w", err)
	}

	bumpType, fromLabel := DetectBumpFromLabels(labels, cfg.Versioning.DefaultBump)

	// Fallback to conventional commits if no version label found
	if !fromLabel {
		commits, err := gh.PRCommits(prNumber)
		if err != nil {
			log.Printf("[versioning] warn: could not read PR commits: %v, using default bump", err)
		} else {
			bumpType = DetectBumpFromCommits(commits, cfg.Versioning.DefaultBump)
			log.Printf("[versioning] no version label, detected %s from commits", bumpType)
		}
	} else {
		log.Printf("[versioning] detected %s from PR labels", bumpType)
	}

	// Bump version
	newVer := Bump(current, bumpType)
	log.Printf("[versioning] bumping %s → %s (%s)", current, newVer, bumpType)

	// Pull latest main before modifying
	if out, err := runGit(cfg.LocalPath, "checkout", "main"); err != nil {
		return fmt.Errorf("git checkout main: %w\n%s", err, out)
	}
	if out, err := runGit(cfg.LocalPath, "pull", "origin", "main"); err != nil {
		return fmt.Errorf("git pull: %w\n%s", err, out)
	}

	// Update version in all configured files
	for _, f := range files {
		if err := UpdateVersionInFile(f, currentStr, newVer.String()); err != nil {
			log.Printf("[versioning] warn: %v", err)
			continue
		}
		log.Printf("[versioning] updated %s", f)
	}

	// Commit, tag, push
	if err := CommitAndTag(cfg.LocalPath, newVer.String(), cfg.Versioning.TagPrefix); err != nil {
		return fmt.Errorf("commit and tag: %w", err)
	}
	log.Printf("[versioning] committed and tagged %s%s", cfg.Versioning.TagPrefix, newVer)

	// Optionally create GitHub release
	if cfg.Versioning.CreateRelease {
		tag := cfg.Versioning.TagPrefix + newVer.String()
		if err := gh.CreateRelease(tag, tag); err != nil {
			return fmt.Errorf("create release: %w", err)
		}
		log.Printf("[versioning] created release %s", tag)
	}

	return nil
}
