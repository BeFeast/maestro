# `maestro version-bump` — Automatic Version Bumping

`maestro version-bump --pr <number>` reads a merged PR's labels and commit
messages to determine the semver bump level (patch / minor / major), updates
the version in configured files, commits the change, tags it, pushes, and
optionally creates a GitHub release.

## Usage

```bash
maestro version-bump --pr 42
maestro version-bump --pr 42 --config path/to/maestro.yaml
```

## How It Works

### 1. Read current version

Reads the version string from the first file listed in `versioning.files` that
contains a recognizable version pattern:

| File type       | Pattern matched                          |
|-----------------|------------------------------------------|
| `Cargo.toml`    | `version = "X.Y.Z"`                     |
| `package.json`  | `"version": "X.Y.Z"`                    |

### 2. Detect bump type

**Priority order — labels first, then commits, then default:**

1. **PR labels** (highest priority): If the PR has a `version:` label, that
   determines the bump. When multiple version labels exist, the highest wins.

   | Label            | Bump  |
   |------------------|-------|
   | `version:patch`  | patch |
   | `version:minor`  | minor |
   | `version:major`  | major |

2. **Conventional commits** (fallback): If no version label is found, commit
   messages are parsed using conventional commit prefixes:

   | Prefix / pattern           | Bump  |
   |----------------------------|-------|
   | `fix:`                     | patch |
   | `feat:`                    | minor |
   | `feat!:` / `BREAKING CHANGE` | major |

   The highest bump across all commits in the PR wins.

3. **Default bump**: If neither labels nor commits provide a signal, the
   `versioning.default_bump` config value is used (defaults to `patch`).

### 3. Update files

Replaces the old version string with the new one in every file listed in
`versioning.files`.

### 4. Commit, tag, push

- Checks out `main` and pulls latest
- Commits all file changes: `chore: bump version to X.Y.Z`
- Creates an annotated tag: `<tag_prefix>X.Y.Z` (default prefix: `v`)
- Pushes commit and tag to `origin main`

### 5. Create release (optional)

If `versioning.create_release` is `true`, creates a GitHub release for the new
tag with auto-generated release notes.

## Configuration

Add a `versioning` block to your `maestro.yaml`:

```yaml
versioning:
  enabled: true
  files:
    - Cargo.toml       # paths relative to local_path
    - package.json
  default_bump: patch  # "patch", "minor", or "major"
  tag_prefix: v        # prepended to version in git tags
  create_release: true # create GitHub release on bump
```

## Using in GitHub Actions

`maestro version-bump` can run as a CI step triggered after PRs merge to
`main`. This is how panoptikon's `.github/workflows/version-bump.yml` uses it:

```yaml
name: Version Bump
on:
  pull_request:
    types: [closed]
    branches: [main]

jobs:
  bump:
    if: github.event.pull_request.merged == true
    runs-on: ubuntu-latest  # or self-hosted
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
          token: ${{ secrets.GITHUB_TOKEN }}

      - name: Configure git
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"

      - name: Install maestro
        run: |
          curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/install.sh | bash

      - name: Bump version
        run: maestro version-bump --pr ${{ github.event.pull_request.number }}
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

### GitHub Actions environment notes

- `GH_TOKEN` must be set for `gh` CLI commands (PR label/commit reads, release
  creation). The default `GITHUB_TOKEN` works for most operations.
- `fetch-depth: 0` is needed so git has full history for tagging.
- Git user config is required for the commit step.
- The workflow triggers on `pull_request.closed` with a merge check, so it only
  runs for actually merged PRs.

## Automatic mode (orchestrator)

When `versioning.enabled` is `true` in the maestro config, the orchestrator
automatically runs the version bump flow after every successful PR merge — no
CI workflow needed. This happens in-process as part of the merge handler.

## Examples

```
# PR #50 has label "version:minor", version file has "1.2.3"
$ maestro version-bump --pr 50
# Result: 1.2.3 → 1.3.0, tagged v1.3.0

# PR #51 has no version label, commits are ["feat: add search", "fix: typo"]
$ maestro version-bump --pr 51
# Result: 1.3.0 → 1.4.0 (feat: detected → minor)

# PR #52 has no version label, commits are ["chore: update deps"]
$ maestro version-bump --pr 52
# Result: 1.4.0 → 1.4.1 (no signal → default patch)

# PR #53 has label "version:major"
$ maestro version-bump --pr 53
# Result: 1.4.1 → 2.0.0
```
