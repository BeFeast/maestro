package pipeline

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxPlanRevisions = 3

// planIssue represents a validation issue found in a plan.
type planIssue struct {
	Severity string // "error" or "warning"
	Message  string
}

// validatePlan extracts requirements from the issue, builds a plan,
// validates it against the codebase, and returns the validated plan text.
// Up to maxPlanRevisions revision loops are attempted.
func validatePlan(issueNumber int, issueTitle, issueBody, worktreePath, researchContext string) (string, error) {
	requirements := extractRequirements(issueBody)
	if len(requirements) == 0 {
		log.Printf("[pipeline] plan-validation: no structured requirements found, using issue body as single requirement")
		requirements = []string{issueTitle}
	}
	log.Printf("[pipeline] plan-validation: extracted %d requirements", len(requirements))

	var plan string
	for revision := 0; revision < maxPlanRevisions; revision++ {
		plan = buildPlan(requirements, worktreePath, researchContext)
		issues := validatePlanContent(plan, requirements, worktreePath)

		errors := 0
		for _, issue := range issues {
			if issue.Severity == "error" {
				errors++
			}
			log.Printf("[pipeline] plan-validation [rev %d]: %s: %s", revision, issue.Severity, issue.Message)
		}

		if errors == 0 {
			log.Printf("[pipeline] plan-validation: plan validated after %d revision(s)", revision)
			return plan, nil
		}

		// Append validation feedback for next revision
		researchContext += "\n\nPlan validation issues:\n"
		for _, issue := range issues {
			researchContext += fmt.Sprintf("- [%s] %s\n", issue.Severity, issue.Message)
		}
	}

	// Return the best plan we have even after max revisions
	log.Printf("[pipeline] plan-validation: returning plan after %d revisions (may have unresolved issues)", maxPlanRevisions)
	return plan, nil
}

// extractRequirements parses the issue body for structured requirements.
// Looks for bullet points, numbered lists, and checkbox items.
func extractRequirements(body string) []string {
	var requirements []string
	seen := make(map[string]bool)

	lines := strings.Split(body, "\n")
	// Patterns for requirement-like lines
	bulletRe := regexp.MustCompile(`^\s*[-*+]\s+(.+)`)
	numberedRe := regexp.MustCompile(`^\s*\d+\.\s+(.+)`)
	checkboxRe := regexp.MustCompile(`^\s*-\s*\[[ x]\]\s+(.+)`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var req string
		if m := checkboxRe.FindStringSubmatch(line); len(m) > 1 {
			req = strings.TrimSpace(m[1])
		} else if m := numberedRe.FindStringSubmatch(line); len(m) > 1 {
			req = strings.TrimSpace(m[1])
		} else if m := bulletRe.FindStringSubmatch(line); len(m) > 1 {
			req = strings.TrimSpace(m[1])
		}

		if req != "" && len(req) > 10 && !seen[req] {
			// Skip lines that look like headers or config examples
			if strings.HasPrefix(req, "#") || strings.HasPrefix(req, "```") {
				continue
			}
			seen[req] = true
			requirements = append(requirements, req)
		}
	}

	return requirements
}

// buildPlan generates a structured plan mapping requirements to implementation steps.
func buildPlan(requirements []string, worktreePath, researchContext string) string {
	var sb strings.Builder
	sb.WriteString("# Implementation Plan\n\n")

	// Discover project info for context
	patterns := discoverPatterns(worktreePath)
	if len(patterns) > 0 {
		sb.WriteString("## Project Context\n")
		for _, p := range patterns {
			sb.WriteString(fmt.Sprintf("- %s\n", p))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Requirements & Approach\n\n")
	for i, req := range requirements {
		sb.WriteString(fmt.Sprintf("### %d. %s\n", i+1, req))

		// Try to find related files
		keywords := extractKeywords(req, "")
		relFiles := findRelevantFiles(worktreePath, keywords)
		if len(relFiles) > 0 {
			sb.WriteString("**Related files:**\n")
			limit := 5
			if limit > len(relFiles) {
				limit = len(relFiles)
			}
			for _, rf := range relFiles[:limit] {
				sb.WriteString(fmt.Sprintf("  - `%s`\n", rf.Path))
			}
		}
		sb.WriteString("\n")
	}

	if researchContext != "" {
		sb.WriteString("## Research Context Available\n")
		sb.WriteString("Pre-coding research has been performed. See the research context section in the prompt.\n\n")
	}

	return sb.String()
}

// validatePlanContent checks a plan for common issues.
func validatePlanContent(plan string, requirements []string, worktreePath string) []planIssue {
	var issues []planIssue

	// Check 1: All requirements should be referenced in the plan
	planLower := strings.ToLower(plan)
	for i, req := range requirements {
		// Check if key words from the requirement appear in the plan
		words := strings.Fields(strings.ToLower(req))
		matchCount := 0
		for _, w := range words {
			if len(w) > 3 && strings.Contains(planLower, w) {
				matchCount++
			}
		}
		coverage := float64(0)
		if len(words) > 0 {
			coverage = float64(matchCount) / float64(len(words))
		}
		if coverage < 0.3 {
			issues = append(issues, planIssue{
				Severity: "warning",
				Message:  fmt.Sprintf("requirement %d may not be fully addressed: %q", i+1, truncate(req, 60)),
			})
		}
	}

	// Check 2: Referenced files should exist
	fileRe := regexp.MustCompile("`([^`]+\\.[a-zA-Z]+)`")
	matches := fileRe.FindAllStringSubmatch(plan, -1)
	missingCount := 0
	for _, m := range matches {
		filePath := m[1]
		// Skip if it looks like a pattern or contains wildcards
		if strings.Contains(filePath, "*") || strings.Contains(filePath, "...") {
			continue
		}
		fullPath := filepath.Join(worktreePath, filePath)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			missingCount++
		}
	}
	if missingCount > 0 && len(matches) > 0 {
		ratio := float64(missingCount) / float64(len(matches))
		if ratio > 0.5 {
			issues = append(issues, planIssue{
				Severity: "warning",
				Message:  fmt.Sprintf("%d of %d referenced files do not exist (may need to be created)", missingCount, len(matches)),
			})
		}
	}

	// Check 3: Scope check — flag if plan references too many files
	if len(matches) > 30 {
		issues = append(issues, planIssue{
			Severity: "warning",
			Message:  fmt.Sprintf("plan references %d files — scope may be too broad", len(matches)),
		})
	}

	return issues
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
