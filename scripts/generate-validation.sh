#!/bin/bash
# generate-validation.sh — after_create hook that generates VALIDATION.md in the worktree.
#
# Usage in maestro.yaml:
#   hooks:
#     after_create: /path/to/scripts/generate-validation.sh
#
# Environment variables (set by maestro):
#   ISSUE_ID        — "owner/repo#123"
#   ISSUE_NUMBER    — "123"
#   WORKSPACE_PATH  — absolute path to the worktree

set -euo pipefail

VALIDATION_FILE="${WORKSPACE_PATH}/VALIDATION.md"

cat > "$VALIDATION_FILE" <<'EOF'
# Validation Contract

This file defines the "done" criteria for the current issue.
The worker agent reads this before starting implementation.

## Required Assertions

<!-- List specific, testable behaviors derived from the issue requirements.
     Each assertion should be verifiable by running a test or command. -->

- [ ] All new/changed behavior has a corresponding test
- [ ] Tests were written before or alongside implementation (not after)

## Quality Gates

- [ ] `go build ./...` (or equivalent) passes
- [ ] `go test ./...` (or equivalent) passes — all tests green
- [ ] `go vet ./...` (or equivalent) clean
- [ ] No lint warnings introduced

## Done vs Partial

- **Done**: All required assertions pass, all quality gates green, PR opened
- **Partial**: Some assertions pass but others are missing or failing — document what's missing in PR body
EOF

echo "[generate-validation] wrote $VALIDATION_FILE"
