# INT: Maestro vs oh-my-codex (OMX) — PRD Alignment and Implementation Gap Analysis

_Last updated: 2026-04-01 (UTC)._  
_Reference external source: oh-my-codex README (main branch): https://github.com/Yeachan-Heo/oh-my-codex/blob/main/README.md._

## Scope and method

This document compares:

1. **Target architecture** from `docs/PRD-gh-project-integration.md`.
2. **Current maestro implementation status** (code + existing docs).
3. **oh-my-codex (OMX)** capabilities as described in its README.

The goal is to provide a practical **pros/cons table** plus a status-oriented view of where maestro is strong today and where the PRD gap remains.

---

## Quick framing: they solve different layers

- **Maestro** is an **orchestration daemon** for issue→PR→CI→merge automation with persistent runtime/session state and repo operations.
- **OMX** is a **Codex workflow/runtime augmentation layer** focused on in-session skills, role invocations, and team runtime ergonomics.

So comparison is not "which is better overall"; it is "which is better for which layer, and where maestro can adopt OMX-like strengths while executing the PRD".

---

## Current PRD implementation status in maestro (high level)

| PRD area | Target in PRD | Current status in maestro | Evidence |
|---|---|---|---|
| Tracker abstraction (`internal/tracker`) | Orchestrator consumes tracker interface, not raw GitHub tracker semantics | **Not implemented yet** (no `internal/tracker` package) | `docs/PRD-gh-project-integration.md`; no `internal/tracker` directory in repo |
| Config model migration (`tracker.mode`, `tracker.labels`, `tracker.github_project`) | New unified tracker block with backward compatibility | **Not implemented yet**; config still centered on `issue_labels`, `exclude_labels`, plus `github_projects` toggle | `internal/config/config.go` |
| Candidate sourcing via tracker | `ListCandidates` from active tracker source of truth | **Not implemented yet**; orchestrator still calls `ListOpenIssues` directly | `internal/orchestrator/orchestrator.go` |
| Workflow transitions via tracker | `Transition(issue, workflow_state)` for blocked/review/done | **Not implemented yet**; direct `AddIssueLabel` / `CloseIssue` calls remain | `internal/orchestrator/orchestrator.go` |
| GitHub Projects as tracker adapter | Project-specific state mapping hidden behind adapter | **Partially implemented in GitHub client helper form**, not as tracker adapter | `internal/github/github_projects.go` |
| CLI (`status`, `spawn`) source-of-truth alignment | Use tracker reads (not ad-hoc issue listing) | **Not implemented yet**; `spawn` and blocked listing use direct `gh.ListOpenIssues` | `cmd/maestro/main.go` |
| Session vs workflow state separation | Distinct runtime state vs board/workflow state | **Partially separated conceptually in PRD/docs, not in code boundary** | PRD + orchestrator state usage |

---

## Maestro vs OMX — thorough pros/cons comparison table

| Dimension | Maestro (current) — Pros | Maestro (current) — Cons / gaps | OMX (README) — Pros | OMX (README) — Cons / tradeoffs |
|---|---|---|---|---|
| Primary value proposition | End-to-end issue automation including PR lifecycle and merge pipeline | Heavy operational surface; larger setup burden | Fast workflow layer for Codex sessions (`$architect`, `$plan`, `$team`) | Not an autonomous issue→PR orchestration daemon by default |
| Architecture boundary | Strong modules for worker, state, routing, notify, pipeline | Tracker boundary still coupled in orchestrator (PRD gap) | Clear workflow affordances for roles/skills in daily use | README does not present a CI/merge governance engine |
| Source of truth for work | GitHub Issues + labels, optional project sync | GitHub Projects not yet first-class tracker mode | Human/agent-driven invocation model can adapt rapidly in-session | Less explicit issue board governance model in README |
| Workflow state modeling | Rich runtime session states persisted to disk | Workflow/board state operations still mixed with orchestration calls | Emphasizes reusable workflows and escalation paths | State semantics in README are mostly runtime/workflow UX, not board lifecycle API |
| PR/CI/merge automation | Strong: CI checks, merge decisions, rebase handling, close issue flows | Complexity concentrated in orchestrator; difficult to evolve tracker semantics | Can guide implementation workflow via skills | README does not indicate automated merge gatekeeping comparable to maestro |
| Persistence and recovery | Persistent JSON state with restart resilience | State pruning is manual (`history --prune`) rather than automatic loop hygiene | `.omx/` durable guidance/log context in repo workspace | Persistence focus appears centered on OMX runtime artifacts, not merged PR governance |
| Multi-backend support | Built-in backend map (Claude/Codex/Gemini/Cline + routing) | Backend lifecycle still single-shot per worker attempt | Built around Codex execution with layered workflows | Less backend plurality implied in README |
| Operational model | Daemon loop + service mode + multi-project configs | Requires `gh`, `git`, `tmux`, model CLIs, branch protections, etc. | Quick bootstrap (`omx setup`, recommended launch flags) | Team runtime still relies on tmux/psmux for durable parallel mode |
| Team/parallel execution | Parallel workers with worktrees, per-state limits | Orchestrator loop remains centralized and complex | `$team` workflow offers coordinated parallel execution pattern | README frames escalation as optional; not necessarily autonomous scheduling |
| Extensibility surface | Hooks, routing, mission/pipeline features in config | Some historical config debt remains (legacy labels + project toggle split) | Skills-based composition is explicit and discoverable | Skill-driven execution quality depends on prompt/skill quality and user discipline |
| UX surface | CLI commands (`run/status/spawn/watch/history/...`) and notifications | Status UX still tied to internal session view, limited explicit tracker-state output | In-session command vocabulary gives strong human ergonomics | May require users to learn OMX role/skill conventions |
| PRD fit for GitHub Project tracker future | Good baseline repo/PR primitives already exist | Core PRD items (tracker interface/mode migration) still pending | Could inspire a cleaner user-facing workflow language around transitions | OMX README itself does not provide GitHub Project adapter blueprint |

---

## Practical interpretation

### Where maestro is stronger today

- If you need **automated repo operations** (issue pickup, worker spawning, PR checks, merge, rebase, close-issue), maestro is materially ahead.
- If you need **daemonized unattended operation** with persistent state and service mode, maestro is already designed for that.

### Where OMX is stronger today

- If you need **human-in-the-loop ergonomics** and reusable in-session workflows, OMX is clearer and lighter.
- If you need a **workflow language** users can invoke quickly (`$architect`, `$plan`, `$team`), OMX has a clear mental model.

### Most relevant gap vs PRD (and where OMX highlights it)

The biggest outstanding risk is still the PRD's main point: **tracker concerns are not abstracted yet**. Until tracker extraction is complete, adding richer project-board semantics will continue to increase orchestrator coupling. OMX's skill/workflow separation highlights the usability side of the same architecture principle: keep boundaries explicit.

---

## Suggested next documentation follow-up

After implementing PRD Phase 1–2, add an updated version of this table with:

1. `tracker.mode=labels|github_project` status and examples.
2. `status` command output including tracker workflow state.
3. A short "maestro vs OMX: complementary usage" section (e.g., OMX for interactive planning, maestro for autonomous execution).

