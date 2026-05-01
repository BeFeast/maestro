# Symphony vs Maestro: Comparison and Improvement Plan

## Executive Summary

**Maestro** is a Go daemon that orchestrates AI coding agents on GitHub Issues using `gh` CLI, tmux sessions, and git worktrees. It has organic growth with tightly coupled tracker logic inside the orchestrator.

**Symphony** is an Elixir service (with a language-agnostic spec) that orchestrates Codex agents on Linear issues via GraphQL, with clean abstraction boundaries, an OTP supervision tree, and a pluggable tracker adapter layer.

The PRD for Maestro's GitHub Project Integration proposes extracting a `tracker` interface similar to what Symphony already implements. This document compares both systems and provides a detailed improvement plan.

---

## Architecture Comparison

| Aspect | Maestro (Go) | Symphony (Elixir) |
|---|---|---|
| **Language** | Go | Elixir (OTP/BEAM) |
| **Issue Tracker** | GitHub Issues (labels) | Linear (GraphQL) |
| **Board Integration** | GitHub Projects V2 (hardcoded IDs) | N/A (tracker states are the board) |
| **Agent Backends** | Multi-backend (Claude, Codex, Gemini, Cline, generic) | Single backend (Codex app-server) |
| **Process Isolation** | tmux sessions + git worktrees | OS directories + Codex subprocess |
| **External CLI deps** | `gh`, `git`, `tmux` | `codex`, `bash` |
| **Config Source** | `maestro.yaml` (external file) | `WORKFLOW.md` front matter (in-repo) |
| **Config Reload** | File-poll (2s) via `configwatch` | File-watch + defensive re-validate |
| **State Persistence** | JSON file (`state.json`, atomic writes) | In-memory only (recovers from tracker on restart) |
| **Concurrency Model** | Sequential single-threaded loop | OTP GenServer + supervised async workers |
| **HTTP Dashboard** | Meta-refresh HTML + JSON API | Phoenix LiveView + JSON API |
| **Notifications** | Telegram (bot API / OpenClaw) | None (logs only) |

---

## Detailed Pros/Cons

### Maestro

| Pros | Cons |
|---|---|
| Multi-backend support (Claude, Codex, Gemini, Cline, generic) | Tracker logic scattered across orchestrator — no interface boundary |
| Mature PR lifecycle (CI checks, Greptile gate, merge sequencing, rebase, conflict resolution) | GitHub Projects integration has hardcoded project IDs and status option IDs |
| Auto-versioning on merge (semver bump + release) | Single retry before permanent failure despite configurable `max_retries_per_issue` |
| LLM-based auto-routing for backend selection | `gh` CLI shelling for every GitHub operation (process spawn overhead) |
| Telegram notifications with digest mode | Sequential orchestration loop — no parallelism within a cycle |
| Git worktree isolation (real branch per issue) | Config has Rust-specific defaults (`auto_resolve_files`) |
| Rich session state (tokens, rate limits, silent detection, PID tracking, review retry lifecycle display) | No tracker abstraction — adding GitHub Projects requires touching orchestrator directly |
| Persistent state across restarts via JSON file | State file grows indefinitely without automatic pruning |
| Blocker detection via regex patterns in issue body | Workspace hook environment is minimal (no issue metadata beyond number) |
| Per-state concurrency limits | No multi-turn agent support within a single worker session |
| Hooks system (after_create, before_run, after_run, before_remove) | Dashboard uses meta-refresh polling, no WebSocket/streaming |

### Symphony

| Pros | Cons |
|---|---|
| Clean tracker abstraction with behaviour callbacks (`Tracker` module) | Single agent backend (Codex only) |
| Multi-turn agent sessions within one worker run (up to `max_turns`) | No PR lifecycle management (merge, CI, rebase) — delegated to agent |
| OTP supervision tree with fault tolerance and concurrent workers | No git worktree isolation — plain directories |
| In-repo config via `WORKFLOW.md` — version-controlled with the project | No notification system |
| LiveView dashboard with real-time updates | Linear-only tracker (no GitHub Issues support) |
| Language-agnostic spec enables multiple implementations | No auto-versioning or release management |
| Tracker writes via optional `linear_graphql` tool (agent-driven) | No LLM-based routing for backend selection |
| Strict prompt template rendering (unknown vars fail) | No persistent state — full re-scan on restart |
| Workspace safety invariants (path containment, key sanitization) | No blocker detection from issue body patterns |
| Per-state concurrency limits (`max_concurrent_agents_by_state`) | Workspace population is entirely hook-driven (no built-in git) |
| Exponential backoff with configurable cap + continuation retries | No rate-limit detection from agent output |
| Dynamic config reload from WORKFLOW.md file watch | No conflict resolution capability |
| Stall detection with configurable timeout | Token tracking is Codex-specific, not multi-backend |
| Clean separation: orchestrator reads tracker, agent writes tracker | Less mature — newer project with fewer battle-tested edge cases |

---

## Key Architectural Differences

### 1. Tracker Abstraction

**Symphony** has a clean `Tracker` behaviour (interface) with `fetch_candidate_issues/0`, `fetch_issues_by_states/1`, `fetch_issue_states_by_ids/1`, `create_comment/2`, and `update_issue_state/2`. The `Linear.Adapter` implements this, and a `Memory` adapter exists for testing. The orchestrator never touches tracker internals.

**Maestro** has no tracker interface. Issue selection, label filtering, blocker checking, and status writes (`AddIssueLabel`, `CloseIssue`) are all done directly in the orchestrator via `github.Client`. The PRD proposes extracting a `Tracker` interface with `ListCandidates`, `GetByIssueNumber`, `IsClosed`, and `Transition` — this is the right direction and aligns with Symphony's approach.

### 2. Workflow State vs Runtime State

**Symphony** cleanly separates:
- **Tracker state** (Linear issue states: Todo, In Progress, Done) — owned by the tracker
- **Orchestration state** (Unclaimed, Claimed, Running, RetryQueued, Released) — owned by the orchestrator

**Maestro** conflates these:
- Session states (`running`, `pr_open`, `queued`, `done`, `failed`) mix runtime status with workflow progression
- Label mutations (`blocked`) are both workflow signals and orchestrator actions

The PRD's `WorkflowState` enum (candidate, in_progress, review, blocked, done) is the right fix.

### 3. Agent Lifecycle

**Symphony** runs multi-turn sessions: one worker can execute up to `max_turns` coding agent turns on the same thread, re-checking tracker state between turns. The agent subprocess stays alive across turns.

**Maestro** runs single-shot sessions: one tmux session per issue attempt. If the agent exits, the orchestrator decides whether to retry with a fresh session.

### 4. Config Philosophy

**Symphony** uses `WORKFLOW.md` — a markdown file with YAML front matter that lives in the repo. This means the workflow prompt and runtime config are version-controlled together.

**Maestro** uses `maestro.yaml` — an external config file, typically per-deployment. The prompt template is a separate file referenced by path. This gives operational flexibility but means config isn't co-located with the codebase being worked on.

Maestro configs can also carry an `outcome` brief. Without that brief, Maestro should say the project has no configured runtime goal instead of treating issue throughput as success.

### 5. Board/Tracker Writes

**Symphony** follows a "service reads, agent writes" philosophy. The orchestrator polls the tracker for state but never mutates tracker state. State transitions are done by the agent using tools (e.g., the `linear_graphql` tool).

**Maestro** writes to the tracker directly from the orchestrator (close issues, add labels, sync project board). The PRD proposes moving these writes behind a `Tracker.Transition()` method, which is a good middle ground — the orchestrator still drives transitions but through an abstraction.

---

## Detailed Improvement Plan for Maestro

### Phase 0: Preparatory Cleanup (1-2 days)

**Goal:** Remove tech debt that will complicate the tracker extraction.

1. **Remove hardcoded project IDs** from `github_projects.go`
   - Replace `knownProjects` map with dynamic resolution via `gh api graphql` to fetch project ID, status field ID, and status options by project number
   - This is a prerequisite for any configurable GitHub Projects support

2. **Fix single-retry bug** in orchestrator
   - The `sess.RetryCount < 1` check limits retries to 1 despite `max_retries_per_issue` config (default 3)
   - Align the check with `cfg.MaxRetriesPerIssue`

3. **Remove Rust-specific defaults** from `auto_resolve_files`
   - Move default `auto_resolve_files` to empty; let configs opt in

4. **Add automatic state pruning** to the orchestration loop
   - Call `PruneOldSessions` periodically (e.g., every 10 cycles) instead of only via `maestro history --prune`

### Phase 1: Tracker Interface Extraction (3-5 days)

**Goal:** Introduce the `tracker` package and migrate all tracker reads/writes behind it.

#### 1.1 Define the interface and domain types

Create `internal/tracker/tracker.go`:

```go
package tracker

type WorkflowState string

const (
    WorkflowCandidate  WorkflowState = "candidate"
    WorkflowInProgress WorkflowState = "in_progress"
    WorkflowReview     WorkflowState = "review"
    WorkflowBlocked    WorkflowState = "blocked"
    WorkflowDone       WorkflowState = "done"
    WorkflowUnknown    WorkflowState = "unknown"
)

type WorkItem struct {
    IssueNumber  int
    Title        string
    Body         string
    Labels       []string
    Repository   string
    URL          string
    TrackerState WorkflowState
    ExternalID   string
    ProjectTitle string
}

type TransitionOptions struct {
    Comment string
    Force   bool
}

type Tracker interface {
    ListCandidates(ctx context.Context) ([]WorkItem, error)
    GetByIssueNumber(ctx context.Context, issueNumber int) (WorkItem, error)
    IsClosed(ctx context.Context, issueNumber int) (bool, error)
    Transition(ctx context.Context, issueNumber int, target WorkflowState, opts TransitionOptions) error
}
```

#### 1.2 Implement `labels` adapter

Create `internal/tracker/labels/tracker.go`:
- Wraps existing `github.Client` methods
- `ListCandidates` → `gh.ListOpenIssues(includeLabels)` + exclude-label filtering
- `GetByIssueNumber` → `gh.GetIssue(n)`
- `IsClosed` → `gh.IsIssueClosed(n)`
- `Transition` → label add/remove + optional `CloseIssue` for done state
- Preserves 100% current behavior

#### 1.3 Wire into orchestrator

Modify `Orchestrator` struct:
```go
type Orchestrator struct {
    cfg      *config.Config
    notifier *notify.Notifier
    tracker  tracker.Tracker   // NEW
    repo     *github.Client    // renamed from gh
    router   *router.Router
    ...
}
```

Replace in `startNewWorkers`:
- `o.gh.ListOpenIssues(...)` → `o.tracker.ListCandidates(ctx)`
- Label filtering moves into the labels adapter

Replace in `checkSessions`:
- `o.gh.AddIssueLabel("blocked")` → `o.tracker.Transition(..., WorkflowBlocked, ...)`
- `o.gh.CloseIssue(...)` → `o.tracker.Transition(..., WorkflowDone, ...)`

Replace in `autoMergePRs`:
- Post-merge `CloseIssue` → `o.tracker.Transition(..., WorkflowDone, ...)`
- Greptile rejection → `o.tracker.Transition(..., WorkflowBlocked, ...)`

Replace in `rebaseConflicts`:
- Conflict failure → `o.tracker.Transition(..., WorkflowBlocked, ...)`

#### 1.4 Update tests

Refactor `orchestrator_test.go` to use a fake `Tracker` implementation instead of asserting on direct GitHub label mutations. Introduce `internal/tracker/fake/tracker.go` for test use.

### Phase 2: Config Migration (1-2 days)

**Goal:** Add `tracker` config block while preserving backward compatibility.

#### 2.1 Add new config structs

Add to `internal/config/config.go`:
```go
type TrackerConfig struct {
    Mode          string                     `yaml:"mode"`
    Labels        TrackerLabelsConfig        `yaml:"labels"`
    GitHubProject GitHubProjectTrackerConfig `yaml:"github_project"`
}
```

#### 2.2 Backward compatibility in `parse()`

When `tracker.mode` is unset:
- If `issue_labels` or `issue_label` exists → `tracker.mode = "labels"`, map into `tracker.labels.*`
- Emit deprecation warning for top-level `issue_labels`, `exclude_labels`

#### 2.3 Validation

- `tracker.mode = "github_project"` requires `github_project.owner` and `github_project.number`
- `tracker.mode = "labels"` is the default

### Phase 3: GitHub Project Adapter (3-5 days)

**Goal:** Implement the `githubproject` tracker adapter.

#### 3.1 Dynamic project metadata resolution

Create `internal/tracker/githubproject/cache.go`:
- Resolve `projectID`, `statusFieldID`, `statusOptions` via GraphQL queries at startup
- Cache results with TTL (e.g., 10 minutes)
- Map config status names (e.g., "Backlog", "In progress") to GraphQL option IDs

#### 3.2 Candidate discovery

Create `internal/tracker/githubproject/tracker.go`:
- `ListCandidates` → query project items, filter by configured candidate statuses, return only issue-backed items
- Use `gh project item-list` or GraphQL query
- Map project item status to `WorkflowState`

#### 3.3 Transition writes

- `Transition` → `gh project item-edit` to move items across statuses
- Handle `require_project_item`, `auto_add_missing_issue`, `close_issue_on_done`, `sync_blocked_label` options
- Idempotent: same target status = no-op

#### 3.4 Tests

- Unit tests for status mapping
- Unit tests for transition idempotency
- Adapter tests with faked `gh` command output

### Phase 4: CLI and Observability (1-2 days)

**Goal:** Update CLI commands and status display.

#### 4.1 `maestro spawn`

- Use `tracker.GetByIssueNumber(issueNum)` instead of `gh.ListOpenIssues`
- Validate issue is in candidate state before spawning

#### 4.2 `maestro status`

Add tracker state column to output:
```
SESSION  ISSUE  TRACKER       STATUS    PR   CI
pan-1    #154   In progress   running   -    -
pan-2    #155   In review     pr_open   91   pending
```

#### 4.3 HTTP API

- Add `tracker_state` field to `/api/v1/state` and `/api/v1/{issue}` responses

### Phase 5: Lessons from Symphony (2-3 days)

**Goal:** Adopt specific patterns from Symphony that improve Maestro.

#### 5.1 Multi-turn agent support (HIGH VALUE)

Symphony's multi-turn model is significantly more efficient:
- Single worker can run up to N turns, re-checking tracker state between turns
- Avoids workspace setup/teardown overhead on each attempt
- Agent maintains context across turns

Implementation sketch:
- Add `max_turns` config option
- Modify `worker.Start()` to support continuation turns (for backends that support it, e.g., Codex with app-server protocol)
- Between turns, check if issue is still active before continuing
- This is backend-specific — only applicable to backends supporting multi-turn (Codex app-server)

#### 5.2 Workspace safety invariants (MEDIUM VALUE)

Adopt Symphony's explicit workspace safety checks:
- Validate `workspace_path` is under `worktree_base` before agent launch
- Sanitize workspace directory names (Symphony uses `[A-Za-z0-9._-]` only)
- These are defense-in-depth measures currently missing from Maestro

#### 5.3 Strict prompt template rendering (LOW-MEDIUM VALUE)

Symphony fails on unknown template variables. Maestro currently uses simple string replacement which silently ignores missing variables. Consider:
- Adding a template validation step before worker launch
- Failing the attempt if required variables are missing

#### 5.4 Stall detection improvement (MEDIUM VALUE)

Symphony has explicit stall detection (`stall_timeout_ms`) based on event timestamps. Maestro's `worker_silent_timeout_minutes` is similar but:
- Only checks tmux output hash changes, not protocol-level events
- Could be improved to also monitor actual agent activity timestamps when available

---

## Summary: Priority Matrix

| Item | Effort | Impact | Priority |
|---|---|---|---|
| Phase 0: Remove hardcoded project IDs | 0.5 days | Blocker | P0 |
| Phase 0: Fix single-retry bug | 0.5 days | High | P0 |
| Phase 1: Tracker interface + labels adapter | 3-5 days | Critical | P1 |
| Phase 2: Config migration | 1-2 days | High | P1 |
| Phase 3: GitHub Project adapter | 3-5 days | Critical | P1 |
| Phase 4: CLI + observability | 1-2 days | Medium | P2 |
| Phase 5.1: Multi-turn agent support | 2-3 days | High | P2 |
| Phase 5.2: Workspace safety invariants | 0.5 days | Medium | P3 |
| Phase 5.3: Strict prompt templates | 0.5 days | Low-Medium | P3 |
| Phase 5.4: Stall detection improvement | 1 day | Medium | P3 |

**Total estimated effort: 13-22 days**

---

## Key Takeaway

The PRD's proposed architecture is well-aligned with Symphony's proven patterns. The main risk is Phase 1 (tracker extraction) due to the large orchestrator file (~55K lines) with interleaved tracker logic. Breaking this into careful, test-backed incremental refactors is essential. The labels adapter should be a pure behavioral no-op — existing tests must pass unchanged before moving to Phase 3.
