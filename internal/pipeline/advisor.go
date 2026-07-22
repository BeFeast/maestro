package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// AdvisorReviewFile is the only worktree artifact the Advisor may create.
	AdvisorReviewFile = "MAESTRO_PLAN_REVIEW.md"

	AdvisorVerdictApproved = "PLAN_APPROVED"
	AdvisorVerdictRevise   = "PLAN_REVISE"
	AdvisorVerdictInvalid  = "PLAN_INVALID"
	AdvisorVerdictBypassed = "PLAN_BYPASSED"
)

// AdvisorResult is the strict, parsed review artifact.
type AdvisorResult struct {
	Verdict  string
	Findings string
	Raw      string
}

// ReadAdvisorResult reads MAESTRO_PLAN_REVIEW.md and requires its first byte to
// begin an exact verdict line. Leading blank space, commentary before the
// marker, unknown markers, and PLAN_REVISE without findings are all invalid.
func ReadAdvisorResult(worktreePath string) (AdvisorResult, error) {
	path := filepath.Join(worktreePath, AdvisorReviewFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return AdvisorResult{}, fmt.Errorf("read advisor review: %w", err)
	}
	raw := string(data)
	firstLine := raw
	rest := ""
	if idx := strings.IndexByte(raw, '\n'); idx >= 0 {
		firstLine = raw[:idx]
		rest = raw[idx+1:]
	}
	firstLine = strings.TrimSuffix(firstLine, "\r")
	result := AdvisorResult{Raw: raw, Findings: strings.TrimSpace(rest)}
	switch firstLine {
	case AdvisorVerdictApproved:
		result.Verdict = AdvisorVerdictApproved
		return result, nil
	case AdvisorVerdictRevise:
		result.Verdict = AdvisorVerdictRevise
		if result.Findings == "" {
			return result, fmt.Errorf("advisor verdict %s requires non-empty findings", AdvisorVerdictRevise)
		}
		return result, nil
	default:
		result.Findings = strings.TrimSpace(raw)
		return result, fmt.Errorf("advisor review must start with an exact %s or %s line", AdvisorVerdictApproved, AdvisorVerdictRevise)
	}
}

// AdvisorWorkspaceSnapshot captures the invariants that make the Advisor
// review-only: canonical plan contents, git HEAD, and all worktree changes other
// than its dedicated review artifact.
type AdvisorWorkspaceSnapshot struct {
	Head             string
	Worktree         string
	RemoteRefs       string
	PlanDigest       string
	ValidationDigest string
}

func CaptureAdvisorWorkspace(worktreePath string) (AdvisorWorkspaceSnapshot, error) {
	planDigest, err := fileDigest(filepath.Join(worktreePath, PlanFile))
	if err != nil {
		return AdvisorWorkspaceSnapshot{}, fmt.Errorf("snapshot plan: %w", err)
	}
	validationDigest, err := fileDigest(filepath.Join(worktreePath, ValidationFile))
	if err != nil {
		return AdvisorWorkspaceSnapshot{}, fmt.Errorf("snapshot validation: %w", err)
	}
	head, err := gitOutput(worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return AdvisorWorkspaceSnapshot{}, fmt.Errorf("snapshot git head: %w", err)
	}
	status, err := gitOutput(worktreePath, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return AdvisorWorkspaceSnapshot{}, fmt.Errorf("snapshot worktree: %w", err)
	}
	remoteRefs, err := gitOutput(worktreePath, "for-each-ref", "--format=%(refname)=%(objectname)", "refs/remotes")
	if err != nil {
		return AdvisorWorkspaceSnapshot{}, fmt.Errorf("snapshot remote refs: %w", err)
	}
	return AdvisorWorkspaceSnapshot{
		Head:             strings.TrimSpace(head),
		Worktree:         advisorWorktreeStatus(status),
		RemoteRefs:       strings.TrimSpace(remoteRefs),
		PlanDigest:       planDigest,
		ValidationDigest: validationDigest,
	}, nil
}

// ValidateAdvisorWorkspace proves that the Advisor created at most its
// dedicated review artifact. The returned reason is stable state/API data.
func ValidateAdvisorWorkspace(worktreePath string, before AdvisorWorkspaceSnapshot) (string, error) {
	after, err := CaptureAdvisorWorkspace(worktreePath)
	if err != nil {
		return "workspace_check_failed", err
	}
	if after.PlanDigest != before.PlanDigest || after.ValidationDigest != before.ValidationDigest {
		return "canonical_artifact_mutated", fmt.Errorf("Advisor changed %s or %s", PlanFile, ValidationFile)
	}
	if after.Head != before.Head {
		return "advisor_commit_detected", fmt.Errorf("Advisor changed git HEAD from %s to %s", before.Head, after.Head)
	}
	if after.RemoteRefs != before.RemoteRefs {
		return "advisor_push_detected", fmt.Errorf("Advisor changed locally tracked remote refs")
	}
	if after.Worktree != before.Worktree {
		return "advisor_worktree_mutation", fmt.Errorf("Advisor changed worktree files outside %s", AdvisorReviewFile)
	}
	return "", nil
}

// RemoveAdvisorReview removes a previous round's artifact before another role
// starts, preventing stale approval from being mistaken for a new verdict.
func RemoveAdvisorReview(worktreePath string) error {
	err := os.Remove(filepath.Join(worktreePath, AdvisorReviewFile))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// AppendAdvisorFindings adds one compact, bounded review entry for the planner.
// Findings themselves are retained exactly after surrounding whitespace is
// removed; only fixed per-entry metadata is added.
func AppendAdvisorFindings(ledger string, planVersion, reviewRound int, findings string) string {
	entry := fmt.Sprintf("Plan v%d, review round %d:\n%s", planVersion, reviewRound, strings.TrimSpace(findings))
	if strings.TrimSpace(ledger) == "" {
		return entry
	}
	return strings.TrimSpace(ledger) + "\n\n" + entry
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func gitOutput(worktreePath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", worktreePath}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func advisorWorktreeStatus(status string) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimSuffix(status, "\n"), "\n") {
		if line == "" {
			continue
		}
		path := line
		if len(line) > 3 {
			path = strings.TrimSpace(line[3:])
		}
		if strings.HasPrefix(line, "?? ") && path == AdvisorReviewFile {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
