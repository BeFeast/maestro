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
