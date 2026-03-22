package pipeline

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// testMapping represents a mapping from a requirement to its verification command.
type testMapping struct {
	Requirement string
	VerifyCmd   string
}

// mapTests generates verification commands for each requirement and writes
// a verify.sh script to the worktree. Returns the path to the verify script
// and a markdown summary of the test mapping.
func mapTests(issueNumber int, issueTitle, issueBody, worktreePath, plan string) (string, string, error) {
	requirements := extractRequirements(issueBody)
	if len(requirements) == 0 {
		requirements = []string{issueTitle}
	}

	// Detect project test infrastructure
	testCmds := detectTestInfrastructure(worktreePath)
	log.Printf("[pipeline] test-mapping: detected test commands: %v", testCmds)

	// Build test mappings
	mappings := buildTestMappings(requirements, worktreePath, testCmds)

	// Generate verify.sh
	verifyPath := filepath.Join(worktreePath, ".maestro", "verify.sh")
	if err := os.MkdirAll(filepath.Dir(verifyPath), 0755); err != nil {
		return "", "", fmt.Errorf("create verify dir: %w", err)
	}

	script := generateVerifyScript(mappings, testCmds)
	if err := os.WriteFile(verifyPath, []byte(script), 0755); err != nil {
		return "", "", fmt.Errorf("write verify script: %w", err)
	}
	log.Printf("[pipeline] test-mapping: wrote verify.sh (%d bytes, %d mappings)", len(script), len(mappings))

	// Build markdown summary for prompt injection
	summary := buildTestMappingSummary(mappings, verifyPath)

	return verifyPath, summary, nil
}

// detectTestInfrastructure identifies the test framework(s) used in the project.
func detectTestInfrastructure(worktreePath string) []string {
	var cmds []string

	// Go
	if _, err := os.Stat(filepath.Join(worktreePath, "go.mod")); err == nil {
		cmds = append(cmds, "go test ./...")
	}

	// Rust
	if _, err := os.Stat(filepath.Join(worktreePath, "Cargo.toml")); err == nil {
		cmds = append(cmds, "cargo test")
	}

	// Node.js — check package.json for test script
	pkgJSON := filepath.Join(worktreePath, "package.json")
	if data, err := os.ReadFile(pkgJSON); err == nil {
		content := string(data)
		if strings.Contains(content, `"test"`) {
			cmds = append(cmds, "npm test")
		}
	}

	// Python
	for _, f := range []string{"setup.py", "pyproject.toml"} {
		if _, err := os.Stat(filepath.Join(worktreePath, f)); err == nil {
			cmds = append(cmds, "python -m pytest")
			break
		}
	}

	// Makefile
	if _, err := os.Stat(filepath.Join(worktreePath, "Makefile")); err == nil {
		cmds = append(cmds, "make test")
	}

	return cmds
}

// buildTestMappings creates test mappings for each requirement.
func buildTestMappings(requirements []string, worktreePath string, testCmds []string) []testMapping {
	var mappings []testMapping

	// Find existing test files
	testFiles := findTestFiles(worktreePath)

	for _, req := range requirements {
		mapping := testMapping{
			Requirement: req,
		}

		// Try to find a specific test file related to this requirement
		keywords := extractKeywords(req, "")
		specificTest := findRelatedTestFile(testFiles, keywords)

		if specificTest != "" {
			// Use the specific test file
			if strings.HasSuffix(specificTest, "_test.go") {
				pkg := filepath.Dir(specificTest)
				mapping.VerifyCmd = fmt.Sprintf("go test ./%s/...", pkg)
			} else {
				mapping.VerifyCmd = fmt.Sprintf("# Run tests in %s", specificTest)
			}
		} else if len(testCmds) > 0 {
			// Fall back to project-wide test command
			mapping.VerifyCmd = testCmds[0]
		} else {
			mapping.VerifyCmd = "# No automated test found — manual verification required"
		}

		mappings = append(mappings, mapping)
	}

	return mappings
}

// findTestFiles walks the worktree looking for test files.
func findTestFiles(worktreePath string) []string {
	var testFiles []string

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, ".maestro": true,
		"target": true, "dist": true, "build": true,
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

		name := info.Name()
		relPath, relErr := filepath.Rel(worktreePath, path)
		if relErr != nil {
			return nil
		}

		// Match common test file patterns
		isTest := false
		switch {
		case strings.HasSuffix(name, "_test.go"):
			isTest = true
		case strings.HasSuffix(name, ".test.js") || strings.HasSuffix(name, ".test.ts"):
			isTest = true
		case strings.HasSuffix(name, ".spec.js") || strings.HasSuffix(name, ".spec.ts"):
			isTest = true
		case strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py"):
			isTest = true
		case strings.HasSuffix(name, "_test.rs"):
			isTest = true
		}

		if isTest {
			testFiles = append(testFiles, relPath)
		}
		return nil
	})

	return testFiles
}

// findRelatedTestFile finds a test file related to the given keywords.
func findRelatedTestFile(testFiles []string, keywords []string) string {
	bestMatch := ""
	bestScore := 0

	for _, tf := range testFiles {
		lowerPath := strings.ToLower(tf)
		score := 0
		for _, kw := range keywords {
			if strings.Contains(lowerPath, kw) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestMatch = tf
		}
	}

	return bestMatch
}

// generateVerifyScript creates the verify.sh script content.
func generateVerifyScript(mappings []testMapping, testCmds []string) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Auto-generated by maestro pipeline — verification commands for this task\n")
	sb.WriteString("# Run this script to verify all requirements are met.\n")
	sb.WriteString("set -e\n\n")

	// Add project-level checks first
	if len(testCmds) > 0 {
		sb.WriteString("echo '=== Project Test Suite ==='\n")
		for _, cmd := range testCmds {
			sb.WriteString(fmt.Sprintf("%s\n", cmd))
		}
		sb.WriteString("\n")
	}

	// Add per-requirement verification
	sb.WriteString("echo '=== Requirement Verification ==='\n")
	for i, m := range mappings {
		sb.WriteString(fmt.Sprintf("echo 'Requirement %d: %s'\n", i+1, shellEscape(truncate(m.Requirement, 80))))
		if strings.HasPrefix(m.VerifyCmd, "#") {
			sb.WriteString(fmt.Sprintf("echo 'WARN: %s'\n", m.VerifyCmd[2:]))
		} else {
			sb.WriteString(fmt.Sprintf("%s\n", m.VerifyCmd))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("echo 'All verifications passed.'\n")
	return sb.String()
}

// shellEscape escapes a string for safe use in a shell echo command.
func shellEscape(s string) string {
	s = strings.ReplaceAll(s, "'", "'\\''")
	return s
}

// buildTestMappingSummary creates a markdown summary of test mappings for prompt injection.
func buildTestMappingSummary(mappings []testMapping, verifyPath string) string {
	var sb strings.Builder
	sb.WriteString("# Test Mapping\n\n")
	sb.WriteString("Each requirement has been mapped to a verification command.\n")
	sb.WriteString(fmt.Sprintf("A verify script has been generated at: `%s`\n\n", verifyPath))

	sb.WriteString("| # | Requirement | Verify Command |\n")
	sb.WriteString("|---|-------------|----------------|\n")
	for i, m := range mappings {
		sb.WriteString(fmt.Sprintf("| %d | %s | `%s` |\n",
			i+1, truncate(m.Requirement, 50), truncate(m.VerifyCmd, 50)))
	}

	sb.WriteString("\n**After implementing changes, run the verify script to confirm all requirements are met.**\n")
	return sb.String()
}
