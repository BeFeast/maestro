# `maestro version-bump` — Automatic Version Bumping

`maestro version-bump --pr <number>` reads a merged PR's labels and commit
messages to determine the semver bump level (patch / minor / major), updates
the version in configured files, commits the change, tags it, pushes, and
optionally creates a GitHub release.

`maestro version-bump --since-last-tag` performs the same update once for the
whole batch of commits on `main` since the latest matching version tag. If the
range is empty, it exits without committing or tagging.

## Usage

```bash
maestro version-bump --pr 42
maestro version-bump --pr 42 --config path/to/maestro.yaml
maestro version-bump --since-last-tag --config path/to/maestro.yaml
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

**For `--pr`, priority order is labels first, then commits, then default:**

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

For `--since-last-tag`, Maestro reads all commit messages since the latest
`<tag_prefix>*` tag and extracts PR numbers from squash/merge commit messages
such as `(#123)` and `Merge pull request #123`. It then computes one bump for
the whole range from all discovered `version:*` PR labels plus conventional
commit prefixes. The highest bump wins; if the range has commits but no signal,
`versioning.default_bump` is used. If the range has no commits, no release is
created.

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
    - VERSION          # paths relative to local_path
  default_bump: patch  # "patch", "minor", or "major"
  tag_prefix: v        # prepended to version in git tags
  create_release: true # create GitHub release on bump
```

## Using in GitHub Actions

`maestro version-bump --since-last-tag` can run on a cadence and can also be
replayed manually with `workflow_dispatch`. This produces at most one release
per workflow run and produces none when there are no commits after the latest
version tag:

```yaml
name: Version Bump
on:
  schedule:
    - cron: '17 6 * * *'
  workflow_dispatch:

permissions:
  contents: write
  issues: write
  pull-requests: read

concurrency:
  group: version-bump-main
  cancel-in-progress: false

jobs:
  bump:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: main
          fetch-depth: 0
          token: ${{ secrets.GITHUB_TOKEN }}

      - name: Configure git
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: Ensure version labels
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          gh label create "version:major" --color "B60205" --description "Major version bump" --force
          gh label create "version:minor" --color "0E8A16" --description "Minor version bump" --force
          gh label create "version:patch" --color "1D76DB" --description "Patch version bump" --force

      - name: Bump version
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: go run ./cmd/maestro/ version-bump --since-last-tag --config .github/maestro-release.yaml
```

### GitHub Actions environment notes

- `GH_TOKEN` must be set for `gh` CLI commands (PR label/commit reads, release
  creation). The default `GITHUB_TOKEN` works for most operations.
- `fetch-depth: 0` is needed so git has full history and tags.
- Git user config is required for the commit step.
- The workflow checks the commit range itself, so scheduled runs with no merged
  work after the latest tag are idempotent.

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
