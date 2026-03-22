#!/usr/bin/env bash
# generate-validation.sh — after_create hook that generates VALIDATION.md in the worktree.
#
# Fetches issue details from GitHub and produces a structured validation contract
# that the worker agent reads before starting implementation.
#
# Environment (set by maestro):
#   ISSUE_NUMBER    GitHub issue number
#   WORKSPACE_PATH  Path to the worktree directory
#   ISSUE_ID        Full issue identifier (owner/repo#number)
#
# Usage in maestro.yaml:
#   hooks:
#     after_create: ./scripts/generate-validation.sh
#
# Requires: gh (GitHub CLI), authenticated

set -euo pipefail

ISSUE_NUMBER="${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
WORKSPACE_PATH="${WORKSPACE_PATH:?WORKSPACE_PATH is required}"

# Extract repo from ISSUE_ID (format: owner/repo#number)
REPO="${ISSUE_ID%#*}"

if [ -z "$REPO" ]; then
  echo "[generate-validation] WARN: could not extract repo from ISSUE_ID=$ISSUE_ID, skipping" >&2
  exit 0
fi

echo "[generate-validation] Generating VALIDATION.md for ${REPO}#${ISSUE_NUMBER}"

# Fetch issue title and body
ISSUE_TITLE=$(gh issue view "$ISSUE_NUMBER" --repo "$REPO" --json title --jq '.title' 2>/dev/null || echo "")
ISSUE_BODY=$(gh issue view "$ISSUE_NUMBER" --repo "$REPO" --json body --jq '.body' 2>/dev/null || echo "")

if [ -z "$ISSUE_TITLE" ]; then
  echo "[generate-validation] WARN: could not fetch issue #${ISSUE_NUMBER}, skipping" >&2
  exit 0
fi

# Generate the validation contract
cat > "${WORKSPACE_PATH}/VALIDATION.md" <<EOF
# Validation Contract — Issue #${ISSUE_NUMBER}
## ${ISSUE_TITLE}

### Required Assertions
_Extract specific, testable behaviors from the issue description below and list them as checkboxes._

<!-- Issue body for reference:
${ISSUE_BODY}
-->

- [ ] All changes described in the issue are implemented
- [ ] New/modified code has corresponding test coverage
- [ ] No unrelated changes included

### Quality Gates
- [ ] Build passes (\`go build ./...\` or equivalent)
- [ ] Tests pass (\`go test ./...\` or equivalent)
- [ ] Lint clean (\`go vet ./...\` or equivalent)
- [ ] Code formatted (\`go fmt ./...\` or equivalent)

### Completion Criteria
- **Done**: All required assertions pass + all quality gates green + PR opened
- **Partial**: Some assertions blocked — open draft PR with explanation
EOF

echo "[generate-validation] Created ${WORKSPACE_PATH}/VALIDATION.md"
