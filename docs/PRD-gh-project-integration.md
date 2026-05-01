# PRD: GitHub Project Integration

## Status

Draft

## Summary

`maestro` should support GitHub Projects as the source of truth for task selection and workflow state, while keeping GitHub Issues and PRs as the execution substrate.

The clean architecture target is:

- `orchestrator` owns local runtime/session state only
- `tracker` owns task sourcing and workflow transitions
- `github` keeps repo/PR/CI operations
- GitHub Projects becomes an implementation of `tracker`, not a special case inside `orchestrator`

This document describes the architecture required to get there cleanly, not the smallest possible patch.

## Problem

Today, task selection and workflow state are coupled to GitHub issue labels and scattered GitHub calls:

- candidate selection is label-driven in [`internal/orchestrator/orchestrator.go`](../internal/orchestrator/orchestrator.go)
- config only models `issue_labels` / `exclude_labels` in [`internal/config/config.go`](../internal/config/config.go)
- tracker-like writes are ad hoc: `AddIssueLabel("blocked")`, `CloseIssue(...)`, `IsIssueClosed(...)`
- some GitHub mutations bypass orchestrator wrappers and call `o.gh` directly
- CLI commands such as `status` and `spawn` instantiate `github.Client` directly

That works for labels, but it does not scale to a kanban-like workflow where the real lifecycle is:

- Backlog / Ready
- In Progress
- In Review
- Blocked
- Done

## Goals

- Make GitHub Projects the canonical workflow source when enabled.
- Preserve the existing local session state machine in `internal/state`.
- Keep PR/CI/merge/version/deploy logic intact.
- Make board-state transitions explicit and idempotent.
- Allow the old label-based mode to remain available behind the same interface.
- Remove direct tracker semantics from `orchestrator`.

## Non-goals

- Replacing GitHub Issues with project draft items.
- Replacing PR/CI/merge orchestration.
- Redesigning `worker` or `state` packages.
- Adding a generic provider abstraction beyond what is needed for GitHub labels and GitHub Projects.

## Design Principles

1. `orchestrator` should consume `WorkItem` and workflow transitions, not GitHub CLI details.
2. Project field IDs and option IDs must stay inside the adapter.
3. Runtime session status and tracker workflow status are different things and must remain separate.
4. Tracker operations must be idempotent because the orchestration loop is periodic.
5. Backward compatibility should come from a label-based tracker implementation, not from branching all over the orchestrator.

## Proposed Architecture

### Layer split

Introduce a new package:

- `internal/tracker`

Keep:

- `internal/orchestrator` for local scheduling and session lifecycle
- `internal/github` for repo/PR/CI/release operations
- `internal/state` for persisted local runtime state

Add two tracker implementations:

- `internal/tracker/labels`
- `internal/tracker/githubproject`

### Responsibilities by package

#### `internal/tracker`

Owns:

- candidate discovery
- work item hydration
- workflow transition API
- board/item metadata cache
- project-specific status mapping

Does not own:

- PR checks
- merging
- rebasing
- releases
- deployment

#### `internal/github`

Owns:

- `ListOpenPRs`
- `PRCIStatus`
- `PRMergeable`
- `PRGreptileApproved`
- `MergePR`
- `CreateRelease`
- repo-level issue/PR fetches needed outside workflow tracking

It may still expose issue reads, but tracker-facing selection and workflow state should not depend on raw `github.Client` from the orchestrator.

## Config Design

Supervisor policy is a separate local safety layer from tracker configuration. It may be set in the top-level `supervisor:` block or in `.maestro/supervisor.yaml`, and covers queue order, dynamic wave selection, ready/blocked labels, held parent/meta work, blocker-dependency skips, excluded issue types, safe actions, and approval-gated actions.

Add a new top-level block:

```yaml
tracker:
  mode: github_project   # "labels" | "github_project"

  labels:
    include:
      - enhancement
    exclude:
      - blocked

  github_project:
    owner: BeFeast
    number: 12
    status_field: Status
    candidate_statuses:
      - Backlog
      - Ready
    in_progress_status: In progress
    review_status: In review
    blocked_status: Blocked
    done_status: Done
    require_project_item: true
    auto_add_missing_issue: false
    close_issue_on_done: true
    sync_blocked_label: false
```

### New config structs

```go
type TrackerConfig struct {
    Mode          string                    `yaml:"mode"` // "labels" | "github_project"
    Labels        TrackerLabelsConfig       `yaml:"labels"`
    GitHubProject GitHubProjectTrackerConfig `yaml:"github_project"`
}

type TrackerLabelsConfig struct {
    Include []string `yaml:"include"`
    Exclude []string `yaml:"exclude"`
}

type GitHubProjectTrackerConfig struct {
    Owner              string   `yaml:"owner"`
    Number             int      `yaml:"number"`
    StatusField        string   `yaml:"status_field"`
    CandidateStatuses  []string `yaml:"candidate_statuses"`
    InProgressStatus   string   `yaml:"in_progress_status"`
    ReviewStatus       string   `yaml:"review_status"`
    BlockedStatus      string   `yaml:"blocked_status"`
    DoneStatus         string   `yaml:"done_status"`
    RequireProjectItem bool     `yaml:"require_project_item"`
    AutoAddMissingIssue bool    `yaml:"auto_add_missing_issue"`
    CloseIssueOnDone   bool     `yaml:"close_issue_on_done"`
    SyncBlockedLabel   bool     `yaml:"sync_blocked_label"`
}
```

### Backward compatibility

For one release, keep:

- `issue_label`
- `issue_labels`
- `exclude_labels`

`config.parse()` should map legacy fields into `tracker.labels.*` when `tracker.mode` is unset.

Target default:

- legacy configs become `tracker.mode=labels`
- new configs should use only `tracker.*`

## Tracker Domain Model

### `WorkItem`

```go
type WorkItem struct {
    IssueNumber  int
    Title        string
    Body         string
    Labels       []string
    Repository   string
    URL          string

    TrackerState WorkflowState

    ExternalID   string // project item id when applicable
    ProjectTitle string
}
```

### `WorkflowState`

```go
type WorkflowState string

const (
    WorkflowCandidate  WorkflowState = "candidate"
    WorkflowInProgress WorkflowState = "in_progress"
    WorkflowReview     WorkflowState = "review"
    WorkflowBlocked    WorkflowState = "blocked"
    WorkflowDone       WorkflowState = "done"
    WorkflowUnknown    WorkflowState = "unknown"
)
```

The adapter maps concrete provider values to these domain states.

Example:

- `Backlog`, `Ready` -> `WorkflowCandidate`
- `In progress` -> `WorkflowInProgress`
- `In review` -> `WorkflowReview`
- `Blocked` -> `WorkflowBlocked`
- `Done` -> `WorkflowDone`

## Tracker Interface

```go
type Tracker interface {
    ListCandidates(ctx context.Context) ([]WorkItem, error)
    GetByIssueNumber(ctx context.Context, issueNumber int) (WorkItem, error)
    IsClosed(ctx context.Context, issueNumber int) (bool, error)
    Transition(ctx context.Context, issueNumber int, target WorkflowState, opts TransitionOptions) error
}

type TransitionOptions struct {
    Comment string
    Force   bool
}
```

### Why this shape

- `ListCandidates` replaces label-based selection inside `orchestrator`.
- `GetByIssueNumber` replaces direct `gh issue view/list` usage in `spawn`, retries, and fallback flows.
- `IsClosed` keeps the zombie-cleanup behavior without exposing raw GitHub issue APIs.
- `Transition` centralizes status sync and optional side effects.

The orchestrator should not know:

- project item IDs
- project IDs
- field IDs
- single-select option IDs
- whether the backend is labels or GitHub Projects

## Adapter Design

### `labels` implementation

Purpose:

- preserve current behavior
- use legacy include/exclude label filtering
- treat transitions as mostly no-op except:
  - `blocked` -> optionally add blocked label
  - `done` -> optionally close issue

This lets the orchestrator switch to the interface first.

### `githubproject` implementation

Purpose:

- fetch candidates from a GitHub Project
- read current project status for each issue
- move project items across statuses
- optionally keep issue closure and legacy label sync behavior

### Internal adapter cache

The GitHub Project adapter should resolve and cache:

- `projectID`
- `statusFieldID`
- `status option name -> optionID`
- `issueNumber -> project item`

Suggested internal type:

```go
type projectCache struct {
    ProjectID      string
    StatusFieldID  string
    StatusOptions  map[string]string
    ItemsByIssue   map[int]projectItem
    RefreshedAt    time.Time
}
```

The cache is process-local and can be refreshed lazily each loop.

## GitHub Project Implementation Details

### Candidate discovery

Use `gh project item-list` as the primary source:

- scope to one project via `owner + number`
- filter to issue-backed items only
- filter by configured candidate statuses
- ignore closed issues

The adapter should return `WorkItem` with enough issue data for:

- routing via labels
- prompt selection via labels
- worker naming / notifications

### Transition writes

Use `gh project item-edit` to move a project item to the configured status.

Rules:

- if item is already in target status, return success
- if the issue is not in the project:
  - fail when `require_project_item=true`
  - attempt add-then-transition when `auto_add_missing_issue=true`
- if `close_issue_on_done=true`, closing the issue happens inside the adapter on `WorkflowDone`
- if `sync_blocked_label=true`, add `blocked` label on `WorkflowBlocked`

### Adapter-owned side effects

The adapter may perform these side effects because they are tracker semantics:

- move project item status
- close issue on done
- add blocked label for backward compatibility

The adapter must not:

- merge PRs
- inspect CI
- rebase branches

## Orchestrator Changes

### Construction

Replace:

- `gh *github.Client` as the only external workflow dependency

With:

- `tracker tracker.Tracker`
- `repo *github.Client`

Suggested shape:

```go
type Orchestrator struct {
    cfg      *config.Config
    notifier *notify.Notifier
    tracker  tracker.Tracker
    repo     *github.Client
    router   *router.Router
    // existing test hooks remain
}
```

### Call-site changes

#### `startNewWorkers`

Current behavior:

- reads `cfg.IssueLabels`
- filters with `cfg.ExcludeLabels`

New behavior:

- `items, err := o.tracker.ListCandidates(ctx)`
- no direct label filtering in orchestrator
- still use `WorkItem.Labels` for:
  - backend routing
  - prompt selection
  - `long-running` detection

On successful worker start:

- call `o.tracker.Transition(issueNumber, WorkflowInProgress, ...)`

#### `checkSessions`

Keep local runtime logic:

- dead PID
- stale tmux
- rate limit
- silent timeout
- max runtime

Change tracker writes:

- do not call `AddIssueLabel("blocked")` directly
- on terminal manual-review outcomes, call `Transition(..., WorkflowBlocked, ...)`
- when a running session is closed externally, treat the item as done locally and optionally sync `WorkflowDone`

#### PR detection

When branch has an open PR and session transitions to `pr_open`:

- call `Transition(..., WorkflowReview, ...)`

This applies in both:

- `reconcileRunningSessions`
- `checkSessions`

#### `autoMergePRs` and `mergeReadyPR`

Keep:

- PR selection
- CI checks
- Greptile gate
- merge sequencing
- version bump
- deploy hook

Change tracker writes:

- Greptile rejection -> `Transition(..., WorkflowBlocked, ...)`
- successful merge -> `Transition(..., WorkflowDone, ...)`

Issue close should move behind the tracker adapter when tracker mode is `github_project`.

#### `rebaseConflicts`

Keep rebase mechanics.

On unresolvable conflict:

- call `Transition(..., WorkflowBlocked, ...)`

### Wrapper cleanup

The following orchestrator helpers should be tracker-facing where applicable:

- `getIssue`
- `isIssueClosed`
- `addIssueLabel`
- `closeIssue`

Target:

- `getIssue` becomes `tracker.GetByIssueNumber`
- `isIssueClosed` remains on tracker
- `addIssueLabel` and `closeIssue` leave orchestrator entirely for workflow-state writes

## CLI Changes

### `maestro spawn`

Current:

- `gh.ListOpenIssues(nil)`

New:

- `tracker.GetByIssueNumber(issueNum)`

This keeps manual spawn aligned with the active tracker source of truth.

### `maestro status`

Current:

- local session state + PR CI state

New:

- local session state
- PR CI state
- tracker workflow state when available

Example extra column:

```text
SESSION  ISSUE  TRACKER       STATUS    PR   CI
pan-1    #154   In progress   running   -    -
pan-2    #155   In review     pr_open   91   pending
```

This prevents confusion when local runtime and board state diverge.

## Transition Mapping

### Canonical mapping

| Trigger | Local session state | Tracker transition |
|---|---|---|
| candidate selected and worker started | `running` | `candidate -> in_progress` |
| worker respawned after retry/rate-limit fallback | `running` | stay `in_progress` |
| PR detected while worker alive | `pr_open` | `in_progress -> review` |
| worker exits but PR already exists | `pr_open` | `in_progress -> review` |
| rebase succeeds and session becomes `queued` | `queued` | stay `review` |
| CI pending/failing on open PR | `pr_open` / `queued` | stay `review` |
| Greptile rejects PR | `pr_open` / `queued` | `review -> blocked` |
| retry budget exhausted | `retry_exhausted` | `* -> blocked` |
| max runtime failure needing manual review | `failed` | `* -> blocked` |
| unresolved rebase conflict | `conflict_failed` | `review -> blocked` |
| issue closed externally | `done` | `* -> done` when tracker still open |
| PR merged successfully | `done` | `review -> done` |

### What should not move board state

These events are runtime noise, not workflow-state changes:

- first dead-worker retry
- rate-limit fallback switch
- temporary CI failure before human review decision
- silent output change tracking

## Failure Semantics

### Idempotency

`Transition()` must be safe to call multiple times:

- same issue, same target state -> no-op
- missing project item with `require_project_item=false` -> no-op or best-effort add

### Degraded mode

If the tracker cannot update board state:

- orchestrator must continue its local session transition
- log a warning
- send notification only for repeated failures or terminal workflow desyncs

Do not block merges or worker cleanup solely because project status sync failed.

## Suggested File Layout

```text
internal/tracker/
  tracker.go              # interfaces and domain types
  labels/
    tracker.go
  githubproject/
    tracker.go
    cache.go
    mapping.go
```

Expected edits:

- `internal/config/config.go`
- `internal/orchestrator/orchestrator.go`
- `cmd/maestro/main.go`
- `cmd/maestro/init.go` later, not phase 1
- README and runbook docs after implementation

## Implementation Plan

### Phase 1: Boundary extraction

- add `tracker` config structs
- add `internal/tracker` interface and `labels` implementation
- refactor orchestrator to use tracker for:
  - `ListCandidates`
  - `GetByIssueNumber`
  - `IsClosed`
  - workflow transitions

This phase should preserve current behavior.

### Phase 2: GitHub Project adapter

- add `githubproject` implementation
- resolve project metadata and status field options
- wire `tracker.mode=github_project`
- update `spawn` and `status`

### Phase 3: UX and migration

- deprecate top-level label fields in docs
- add tracker status to `status` output
- optionally update `maestro init` to support project mode

## Testing Strategy

### Unit tests

Add new tests for:

- label tracker candidate filtering
- GitHub Project state mapping
- GitHub Project transition idempotency
- missing project item behavior
- config compatibility from legacy label fields

### Orchestrator tests

Refactor existing tests to use a fake tracker.

Important suites:

- `TestStartNewWorkers_*`
- `TestCheckSessions_*`
- `TestAutoMergePRs_*`
- `TestReconcileRunningSessions_*`

The goal is for orchestrator tests to stop asserting on direct GitHub label mutations and instead assert on tracker transitions.

### Adapter tests

The GitHub Project adapter needs dedicated tests because current `internal/github` coverage is minimal.

Preferred approach:

- introduce a small command-runner seam inside the adapter
- fake `gh` JSON responses for:
  - `project item-list`
  - `project field-list`
  - `project item-edit`
  - `issue view`
  - optional `issue edit` / `issue close`

## Open Questions

1. Should `CI failure` stay in `In review`, or should there be an optional `Changes requested` project state?
2. Should `Blocked` be set only on permanent/manual-review outcomes, or also on first serious failure?
3. Do we want optional priority sorting from another project field in phase 2, or keep current source order?
4. Should `auto_add_missing_issue` exist at all, or should project membership be mandatory for safety?

## Recommendation

Implement the boundary first, even if GitHub Projects is the immediate goal.

If GitHub Projects support is added directly into the current `orchestrator`/`github.Client` shape, the code will gain a second set of tracker semantics without any real separation. That will make future workflow changes harder than the current label-only model.

The clean path is:

1. extract `tracker`
2. preserve existing label behavior behind `tracker/labels`
3. add `tracker/githubproject`
4. move all workflow transitions behind the new interface

That gives `maestro` a proper workflow layer without destabilizing the existing worker and PR pipeline.
