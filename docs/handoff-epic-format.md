# Handoff Epic Format and Child Issue Template

This document describes the recommended GitHub issue layout for unattended
overnight development of a design / product handoff (for example, the
Scribe redesign described in issue [#443](https://github.com/BeFeast/maestro/issues/443)).

Maestro's supervisor handoff planner (`supervisor.handoff_planner`) reads
the open handoff/epic issue, picks the next concrete slice, and either
opens the next child issue or emits an actionable `open_child_issue`
recommendation. The supervisor never silently idles on `none` while an
open handoff epic remains.

## When to use a handoff epic

Use a handoff epic when:

- a single PR cannot deliver the whole change (multi-route UI rewrite,
  multi-service migration, design-system rollout);
- the work is partitionable into 1-day-or-less slices;
- you want the overnight supervisor to keep dispatching slices after the
  first one merges, without an operator at the keyboard.

A handoff epic is identified by either:

- a title prefixed `Epic:` (e.g. `Epic: Scribe Redesign 2026-05`), or
- one of the labels listed in
  `supervisor.handoff_planner.source_issue_labels` (default:
  `["epic", "design-handoff"]`).

## Recommended epic layout

```markdown
# Epic: <product-area> redesign

## Outcome

One paragraph stating the runtime goal, the target user, and what
"done" looks like for the whole epic (not for a single PR).

## Issue Wave

Ordered list of concrete slices. Each item is one runnable child issue.
Maestro reads the first unchecked line as the next slice to schedule.

- [ ] Slice 1: route shell + new layout primitives
- [ ] Slice 2: replace `/inbox` route with redesigned shell
- [ ] Slice 3: replace `/settings` route with redesigned shell
- [ ] Slice 4: live visual gate + production preflight wiring

## Route Replacement Map

(optional, useful for UI epics) Map of old route → new route and
which slice owns the migration.

| Old route          | New route        | Owning slice |
|--------------------|------------------|--------------|
| /inbox             | /v2/inbox        | Slice 2      |
| /settings          | /v2/settings     | Slice 3      |

## Done For The Epic

The whole epic is Done only when:

1. every slice in `Issue Wave` is checked off;
2. the live visual verification on the staging runtime passes
   (`completion_gates.live_visual_command`);
3. the runtime health gate passes for at least one full deploy cycle
   (`outcome.health_check` is `healthy`);
4. the operator confirms the design parity sign-off comment.

## Source Materials

- design zip checksum / location
- design system spec / Figma link
- runtime URL the slice must verify against

## Preflight Contract

Before any worker starts on a slice from this epic, Maestro runs the
configured `supervisor.handoff_planner.preflight_command` (or the
top-level `supervisor.preflight_command`). It must verify, at minimum:

- the execution checkout path exists and is clean;
- `bun`, `gh`, `uv`, `docker`, `maestro`, `git`, `curl`, `unzip`,
  `sha256sum`, `ssh` are on `$PATH` for the worker shell;
- `gh auth status` passes;
- required handoff assets exist and checksum-match
  (e.g. `/mnt/storage/src/Scribe.redesign.zip`);
- direct runtime SSH works (e.g. `ssh god@10.10.0.13`);
- Docker works on the execution host and the runtime host;
- required runtime / control-plane URLs return success;
- repo-specific env/secret lookup is wired.

Failure blocks both `open_child_issue` and `spawn_worker` and surfaces
a visible operator action item (`stuck_state: preflight_failed`).
```

## Recommended child issue template

Save this as `.maestro/templates/design-handoff-child.md` (or wherever
`supervisor.handoff_planner.issue_template` points) and reference it
from the planner config. Every supervisor-opened child issue should
inline these sections so a fresh worker has everything it needs.

```markdown
## Parent Epic

References parent: #<EPIC_NUMBER> (`Epic: ...`).

## Scope

What this slice should change, in two or three sentences.

## Non-goals

Routes / components / surfaces this slice MUST NOT touch.

## Source Materials

- design zip: `/mnt/storage/src/<project>.redesign.zip` (sha256: ...)
- design ref: `<design-system route or asset id>`
- runtime URL: `<staging URL the worker must verify against>`

## Preflight

Maestro will run `supervisor.handoff_planner.preflight_command` before
any worker spawns. The slice author does NOT need to re-run preflight
manually; failures will be surfaced as a P0 stuck-state on the dashboard.

## Acceptance Criteria

- [ ] code change matches design intent and design tokens
- [ ] no regression on routes outside the slice's scope
- [ ] live visual verification command passes against staging
- [ ] healthz passes for the deployed PR
- [ ] manual sign-off comment from the operator on the parent epic

## Validation Commands

Listed verbatim so the worker can call them and so reviewers can
re-run them locally.

```bash
# repo-local
go test ./...
bun test
# runtime
maestro outcome check --project <name>
<custom live-visual command>
```

## Expected PR Evidence

- screenshots / screen recordings against the staging runtime
- diff that touches only the slice scope above
- comment on the parent epic checking off this slice from the
  `Issue Wave` list
```

## Completion gates

Healthcheck passing is evidence, not the whole "Done" gate. When the
issue body carries any of the configured
`supervisor.completion_gates.body_markers` (default: none — opt-in per
project) or one of the `completion_gates.required_labels`, the
supervisor refuses to collapse the Done check to healthz alone. The
issue must move to verification (or stay open) until the configured
`live_visual_command` / `deployment_status_command` proves the runtime
matches the slice's acceptance criteria.

Example config:

```yaml
supervisor:
  handoff_planner:
    enabled: true
    source_issue_labels: [epic, design-handoff]
    issue_template: .maestro/templates/design-handoff-child.md
    parse_sections:
      - "## Issue Wave"
      - "## Route Replacement Map"
      - "## Done For The Epic"
    preflight_command: /home/god/.maestro/bin/scribe-redesign-preflight.sh
    require_preflight_before_create: true
    require_preflight_before_spawn: true
    max_children_per_cycle: 1
    max_open_children: 3
  completion_gates:
    required_labels: [needs-visual-verification, ui-redesign]
    body_markers: ["## Live Visual Verification"]
    live_visual_command: /home/god/.maestro/bin/scribe-live-visual.sh
    deployment_status_command: /home/god/.maestro/bin/scribe-deploy-status.sh
    verification_label: awaiting-verification
```

See also: [`ux-redesign-addendum-2026-05-25.md`](./ux-redesign-addendum-2026-05-25.md)
for the Scribe redesign incident timeline that motivated this design.

## Dependency-based dynamic waves (#442)

A handoff epic that opens its wave of child issues up front (rather than
one-at-a-time via the planner) needs a way for the supervisor to keep the
board moving without an external cron. The dependency-unblock controller
solves this:

1. Every child issue in the wave is created with the configured `blocked`
   label and a parseable dependency reference in its body.
2. The supervisor scans blocked wave members each cycle, parses the
   dependency references, and unblocks issues whose dependencies are all
   complete (issue closed or linked PR merged).
3. When `github_projects.enabled` is true, the supervisor enrolls every
   blocked wave member onto the configured GitHub Project so operators can
   see the upcoming wave before any item goes runnable.

### Dependency reference shapes the supervisor understands

```markdown
Depends on: #147
```

```markdown
Depends on: #148, #149
```

```markdown
## Dependencies

- #147 — design export landed
- #148 — UX wave 1 issues filed
```

All three are recognised by `github.FindDependencies`. The structured
section variant is preferred for wave epics because it survives template
edits cleanly.

### Config

```yaml
supervisor:
  ready_label: maestro-ready
  blocked_label: blocked
  safe_actions:
    - add_ready_label
    - remove_ready_label
    - remove_blocked_label
    - add_issue_comment
    - merge_pr
    - close_issue
  dynamic_wave:
    enabled: true
    owns_ready_label: false
    runnable_project_statuses: [Todo, To Do, Ready, Backlog, New]
    dependency_unblock:
      enabled: true             # opt-in; default false
      max_runnable: 5           # cap concurrent runnable wave members
      enroll_in_project: true   # default true when projects are configured
      announce_with_comment: true  # default true; comment lists dep evidence

github_projects:
  enabled: true
  project_number: 6
```

### What the supervisor does each cycle

1. List open issues.
2. Pick out the ones carrying the configured `blocked_label` — those are
   the blocked wave members. They are evaluated separately from runnable
   pickup, so they never appear as worker candidates.
3. If GitHub Projects is enabled, ensure every blocked wave member is on
   the configured Project board (best-effort; subprocess errors are
   logged, not raised).
4. For each blocked member, parse its body for dependencies. Skip the
   member if no parseable dependency reference exists.
5. Resolve dependencies: every dependency must either be closed or have a
   merged linked PR. If any dependency is still open, the member stays
   blocked this cycle.
6. When the runnable cap (`max_runnable`) is already met, skip further
   unblocks for this cycle.
7. When a member is ready to unblock, recommend `unblock_issue` with three
   mutations: `remove_blocked_label`, `add_ready_label`, and an
   `add_issue_comment` whose body lists the dependency evidence (e.g.
   `- #147 closed`, `- #149 PR merged`).
8. Mutations are idempotent: if supervisor state already records a
   succeeded mutation for the same issue/label, no duplicate is planned.

### Why the supervisor (and not the worker)

The implementing worker prompt stops after PR creation by design — the
worker is scoped to a single slice. Continuation across the wave belongs
to the supervisor:

- The supervisor sees every issue and PR in the repo, including the
  blocked wave's dependency PRs.
- Cron-based unblockers (the temporary `scribe-redesign-handoff-unblocker`
  used during the Scribe redesign) duplicate this loop with weaker
  guarantees: no idempotency against supervisor state, no project
  enrollment, no max-runnable cap.
- The supervisor's safe-actions policy is the single audit trail for
  label changes; routing the same change through an external cron splits
  the evidence between two systems.

If your repo previously ran an external cron for this purpose, you can
retire it once `supervisor.dynamic_wave.dependency_unblock.enabled` is
true and `safe_actions` grants `remove_blocked_label`, `add_ready_label`,
and `add_issue_comment`.
