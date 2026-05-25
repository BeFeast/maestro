---
title: Design Export To UX Issues Runbook
type: runbook
updated: 2026-05-24
tags: [dev, runbook, ux, design, github, agents]
area: Dev
status: active
---

# Design Export To UX Issues Runbook

This runbook describes how to turn a design export (zip, repo, prototype) from any design tool — Claude Design, Figma Dev Mode, Penpot, v0, hand-built reference app — into a PRD addendum and a GitHub issue wave for a real product UX redesign.

The important rule: a design export is the visual and interaction specification, not a vague moodboard. Do not create issues that merely mount the exported app under a demo route, and do not ask workers for an "inspired restyle". Convert the design into production behavior, route ownership, acceptance criteria, and small mergeable slices, while requiring strict fidelity to the exported source files.

## Fidelity Contract

When a design export includes source files such as `*.html`, `*.jsx`, `*.tsx`, `*.css`, icons, or SVGs, those files are **the implementation**, not a specification of one. The job is to wire production data and routes through that implementation, not to re-author it from screenshots or token names.

**The literal source is the canonical recipe. There is no "spec" separate from the code.** The design system stylesheet (tokens + primitives) plus the component sources together define every pixel; if a value can be resolved from those files, the resolved value is the only acceptable output. "Close" colors, "similar" shadows, "approximate" spacing, "equivalent" SVG geometry are all forbidden — they are drift.

Workers must:

- read the primary HTML/index file named in the export README before implementation;
- follow all imports and read the relevant component/style source files end-to-end before editing production code;
- resolve every `var(--X)` (or analogous design token reference) in the design's CSS against the design's token file and use the literal resolved value;
- copy each exported component as the new production component (see [Source Code Port Protocol](#source-code-port-protocol));
- reuse exported SVG/vector assets verbatim (same viewBox, node coordinates, stroke widths, dash patterns) — do not redraw from a screenshot;
- preserve the design's information architecture as a whole: navigation groups, topbar context, status pills, metric rows, panels, tables, footer composition, user-pill placement, empty/error states. Do not re-arrange.

Workers must not:

- treat the export as "inspiration" for a generic color/theme refresh;
- substitute framework color names that "sound close" (e.g. a Tailwind family name that matches the token alias word but not the hex) — see [Forbidden Substitutions](#forbidden-substitutions);
- redraw logos or icons when the export provides vector source;
- compose recipes from individual utility classes at consumer sites when the design ships a named recipe (`.card`, `.card-2`, `.hairline`, `.divider`, etc.) — port the recipe as a single utility class in the project's global stylesheet and use it everywhere;
- nest a recipe inside itself (two outer-tier cards) — the design defines one outer and one inner tier only;
- edit `className` on the old component as a shortcut for porting the new one — see [Replace, Don't Repaint](#replace-dont-repaint);
- mark work done because broad design tokens are present while the screen structure, logo, typography, density, or navigation still differs.

If strict fidelity conflicts with production data, accessibility, responsiveness, or missing backend fields, record the deviation explicitly in the issue/PR and keep the item visible as `Blocked` or follow-up work. Silent deviation is not allowed.

### Source Code Port Protocol

When a production target has a matching design source file (`shell.*` → app shell, `dashboard.*` → dashboard route, etc.), the implementation workflow is:

1. **Copy verbatim.** Place the design source file inside the repo at a deterministic location, unchanged, as the baseline. The git history must show the literal copy as commit N before any adaptation lands as commit N+1. Reviewers and CI use that diff to prove the implementation is a port, not a rewrite.
2. **Adapt only the integration layer.** The only edits allowed on top of the copy are:
   - swap framework primitives where required (e.g., raw `<div>` → component-library primitive that compiles to the same DOM/styling);
   - replace fake data arrays with real query/fetch hooks;
   - wire route navigation, params, links;
   - add accessibility attributes the design omitted (aria-labels, focus rings) without changing visual output.
3. **Anything else is a rewrite, not a port.** Re-styling, re-laying-out, "improving" the design, picking different color names, re-spacing, "matching the spirit" — all forbidden. If the design source is wrong, file an issue against the design, do not silently fix it in the port.
4. **Bundle recipes behind a single utility class.** When the design ships a recipe like `.card { border: var(--hairline) solid var(--border); background: var(--surface-1); ... }`, resolve every var against the design's token file and write the literal recipe as one utility class in the project's global stylesheet (e.g., `globals.css`). Use that class at every consumer site. Never compose the recipe ad-hoc from individual utility classes at the consumer — that is how drift starts.
5. **Sweep, don't patch.** When a forbidden pattern is discovered on one route, the bug is systemic. Audit every route + component file and apply the correct utility class everywhere in one PR. Do not fix only the route the user happened to screenshot.

### Replace, Don't Repaint

When the design ships a component for a production target, the workflow is **delete the old component and put the design's component in its place**, then wire data on top. It is not "open the old component and change its `className` values to match the design".

Mechanical workflow:

1. `git rm` (or overwrite) the old production component file.
2. Drop in the design source file as the new implementation (see [Source Code Port Protocol](#source-code-port-protocol)).
3. Wire data/routing/primitives on top in a follow-up commit.
4. If you must keep an old export name for callers, re-export from the new file — do not preserve the old internal markup.

The "repaint" anti-pattern is what produces inspired-but-wrong UI: old DOM tree, old class composition, design tokens sprinkled on top. The design's DOM tree is part of the design. Replace, do not repaint.

### Forbidden Substitutions

These are concrete, recurring drift patterns. Each is forbidden — the only acceptable resolution is the literal value from the design source.

| Wrong pattern | Why it's wrong | Required action |
|---|---|---|
| A framework color name (`cyan-500`, `sky-400`, `slate-800`, Material `blue.700`, Bootstrap `$indigo-500`) because the design token has a similar word | Framework hue families ship 4-6 shades per name; a name match almost never matches the source hex. `cyan-500 = #06b6d4` (teal) ≠ `--accent-cyan = #38bdf8` (sky). The hue family difference is visible. | Resolve the design token in the source file. Use the literal hex (`text-[#38bdf8]`) or define a project token alias once. |
| `border-X-strong` / `bg-X-2` because a named recipe "looks close" | A named recipe like `.card` resolves to specific `var(--hairline) solid var(--border)` + `var(--surface-1)`. The resolved values may be `1px` + a different rgba than the `*-strong` alias. | Resolve every var against the design's token file. Port the literal recipe as a single utility class. |
| Redraw a logo SVG from a screenshot | The design ships a vector source with exact viewBox, node coords, stroke widths, dash patterns. | Paste the SVG markup verbatim. |
| Nest a recipe inside itself (e.g., two outer-tier cards) producing concentric borders | The design defines one outer recipe and one inner recipe. Nesting two outers creates a double border that does not exist in the source. | Outer = `.<recipe>`, first-level inner = `.<recipe>-2`. No deeper nesting unless the design source nests deeper. |
| Compose a recipe ad-hoc from `bg-X border-Y rounded-Z p-W` at consumer sites | Five different consumers will drift apart over time. | Single utility class in the global stylesheet. CI guard forbids the ad-hoc combo. |
| Re-arrange information architecture: move a user pill from sidebar footer to topbar dropdown, rename nav groups, promote a workspace tab to a sidebar item, split a recipe across pages | IA is part of the design. The design specifies where every pill sits, which routes are first-class nav items, and which are tabs inside a workspace. | Port the IA exactly. Workspace-internal variants stay inside the workspace — they do not become sidebar items. |
| Hardcode a topbar breadcrumb or page title to one literal label because the screenshot shows it on one page | The design's topbar derives the breadcrumb from the current route. A static label is a missing feature, not "the same as design". | Derive from `usePathname()` (or router equivalent) with an explicit mapping table. |
| Use a `<Spinner>` because "loading is loading" when the design ships skeletons | Loading state is part of the design. A spinner is a different design. | Port the skeleton primitive from the design source. |
| Use the framework default Button/Input/Card because the design's primitive is "basically the same" | If the design ships its own primitive, the framework default has different padding, radius, border, focus ring, hover state. All visible. | Port the design's primitive. Update the project's component-library primitive to match — once, then reuse everywhere. |

When in doubt: open the design source file, read it end-to-end, copy the literal values. "I'll use the framework default because it's close" is the failure pattern.

## CI Guardrails (Required)

A repo cannot be considered redesign-ready without these guardrails. Each one prevents a recurring drift pattern that humans demonstrably miss in review.

1. **Raw framework color literals are blocked.** A shell script in the repo (e.g., `scripts/check-design-tokens.sh`) greps production source for raw `(cyan|sky|teal|slate|indigo|emerald|rose|amber|fuchsia|violet|blue|green|yellow|red|orange|pink|purple|stone|zinc|neutral|gray)-[0-9]` color classes (adjust the alternation for your CSS framework) and fails the build when any are found. Only project token aliases or literal hex copy-pasted from the design source are allowed.
2. **Ad-hoc recipe compositions are blocked.** A grep guard fails the build when consumer files use the constituent pieces of a named recipe (e.g., `bg-<surface-token>.*border-<border-token>`) instead of the single utility class (`.<recipe>`).
3. **Nested recipes are blocked.** A grep guard fails the build when two outer-tier recipe classes appear on nested elements without the second-tier class between them.
4. **Forbidden primitives are blocked.** `window.alert|confirm|prompt`, native `<dialog>` in projects with a modal primitive, native `<select>` in projects with a custom select, etc., per project conventions.
5. **Each guard runs in the Frontend Build CI job before the production build,** so a violation fails CI on the first push. A guard that only runs locally is not a guard.

Sample skeleton for guard 1 (adapt the alternation to your stack):

```bash
#!/usr/bin/env bash
set -euo pipefail
ROOT="${1:-web/src}"
PATTERN='(cyan|sky|teal|slate|indigo|emerald|rose|amber|fuchsia|violet|blue|green|yellow|red|orange|pink|purple|stone|zinc|neutral|gray)-[0-9]'
HITS=$(rg -n --no-heading -g '!*.test.*' -g '!*.spec.*' "$PATTERN" "$ROOT" || true)
if [ -n "$HITS" ]; then
  echo "ERROR: raw framework color literals found. Use project token aliases or literal hex from the design source." >&2
  echo "$HITS" >&2
  exit 1
fi
echo "OK: no raw framework color literals in production code"
```

Wire it into CI:

```yaml
# .github/workflows/ci.yml (excerpt)
jobs:
  frontend-build:
    steps:
      - uses: actions/checkout@v4
      - name: Design token guard
        run: bash scripts/check-design-tokens.sh
      - name: Build
        run: <build command>
```

When porting a new design system into a repo, the **first PR** lands the global stylesheet (with the byte-exact recipes), the token alias config, and these CI guards together. Route-by-route ports come after. Without the guards, the route ports will drift before the wave is finished.

## When To Use

Use this when:

- there is an existing app with real users, data, routes, auth, jobs, metrics, or deployment;
- a design tool produced a zip, static HTML, React/Vue/Svelte prototype, or style sheet bundle;
- the goal is to replace or extend production UX, not to publish a design demo;
- a coding agent (manual or automated supervisor) will implement the work from GitHub issues.

Do not use this as a substitute for product judgment. The design may contain variant pickers, fake data, debug panels, placeholder commands, or demo-only routes. These must be filtered before filing issues. Filtering demo-only controls is different from restyling the product loosely: the accepted production screens and source files still have to be followed strictly.

## Inputs

| Input | Example |
|---|---|
| Design export zip | `/mnt/storage/src/Maestro.zip` |
| Existing repo | `BeFeast/maestro` |
| Local checkout | `/mnt/storage/src/maestro` on workshop |
| PRD | Obsidian PRD, Notion doc, or project spec |
| Runtime URL | https://mastro.oklabs.uk |
| Current app screenshots | before state |
| Desired variant decision | all, defaults to light, tape |

If the design export includes many variants, choose one production direction before filing implementation issues. Do not make the implementation issue "support every design variant" unless that is an actual product requirement.

## Extract And Inventory

Extract the zip to a temporary folder:

```bash
EXPORT_ZIP="/Users/<user>/Downloads/<design>.zip"
WORKDIR="/tmp/<slug>-design-export"
rm -rf "$WORKDIR"
mkdir -p "$WORKDIR"
unzip -q "$EXPORT_ZIP" -d "$WORKDIR"
find "$WORKDIR" -maxdepth 3 -type f | sort
```

Inventory the export:

```bash
rg -n "variant|theme|density|tweak|debug|mock|sample|fake|route|href|api|fetch|localStorage" "$WORKDIR"
find "$WORKDIR" -maxdepth 3 -type f \( -name "*.jsx" -o -name "*.tsx" -o -name "*.vue" -o -name "*.svelte" -o -name "*.css" -o -name "*.html" \) -print
```

Create a short design inventory:

| Area | Questions |
|---|---|
| Routes/pages | Which screens exist? Which production routes do they replace? |
| Components | Which reusable widgets are real product components? |
| Data | What is fake data vs real API shape? |
| Variants | Which theme/layout variant is selected for production? |
| Debug UI | What must not ship? |
| Assets | Are images/icons local, generated, external, or placeholders? |
| Accessibility | Are keyboard/focus/contrast states represented? |
| Responsiveness | Are mobile/tablet/desktop states included? |

Also create a fidelity inventory before filing issues:

| Source file | Production target | Fidelity requirements |
|---|---|---|
| `logos.*` / SVG assets | shared brand/logo component | exact geometry, stroke width, node size, glow/dash treatment |
| `shell.*` | authenticated app shell | sidebar groups, topbar context, status pills, search, footer/user status |
| `dashboard.*` | dashboard route | first viewport layout, KPI row, charts, primary panel, event/activity list |
| `tokens.css` / `base.css` (or equivalent design system files) | global theme and primitives | CSS variables, typography, radius, borders, spacing, density |

If this table is missing, the issue wave is not ready for implementation.

## Stage Export On Execution Surface

Before filing implementation issues, make the design export available on the machine that will actually run workers, builds, tests, and commits. A local laptop path is not a valid handoff path for a remote worker.

1. Resolve the execution surface and repo checkout first. For example, `workshop:/mnt/storage/src/<repo>`.
2. Copy the export archive to a stable path on that host, for example `/mnt/storage/src/<Design>.zip` or `/mnt/storage/src/<repo>/design-inputs/<Design>.zip`.
3. Verify checksum on both sides and record it in the PRD addendum and Wave 0 issue.
4. Extract and inventory from the execution surface as well as locally. The worker should be able to run `unzip -Z -1 <staged-zip>` without guessing where the source lives.
5. Name both paths in issues when useful:
   - original submitter path: `/Users/<user>/Downloads/<design>.zip`;
   - worker path: `<host>:<absolute path to staged zip>`;
   - repo baseline target: `<repo-relative design source directory>`.

Do not create a Maestro-ready issue that only references a Mac-local path when Maestro runs on a Linux host. If the staged archive is missing or checksum verification fails, the issue is `blocked` until the artifact is staged.

## Separate Design Demo From Product UI

Most design exports contain both product UI and design-control UI. Treat these separately.

Ship:

- production pages;
- production navigation;
- selected visual variant;
- reusable components that support real workflows;
- API-backed data states;
- loading/empty/error states;
- accessible interactions.

Do not ship unless explicitly requested:

- variant/theme/density pickers;
- design debug toggles;
- fake data switchers;
- route playgrounds;
- local-only style controls;
- explanatory text saying how the UI works;
- demo-only routes (e.g., `/__spa__`, `/preview`) as the final user surface.

Production route mapping should be explicit. Example:

| Design screen | Production route | Notes |
|---|---|---|
| List/library | `/` | Replace existing list page in place |
| Item detail | `/items/{id}` | Preserve old link semantics |
| Queue | `/queue` | Real jobs, not fake pipeline cards |
| Ops | `/ops` | Real metrics/health API |
| Settings | `/settings` | Real config surface or read-only placeholders |

If a demo route is needed temporarily, label it as scaffolding and create a follow-up issue to remove or hide it.

## Compare Against Current App

Before filing issues, capture current state:

```bash
# local or remote, depending on the app
open http://<current-app>/
```

Document:

- current production routes;
- current API endpoints used by pages;
- data fields already available;
- missing API fields needed by the design;
- legacy views that must be preserved temporarily;
- deployment and healthcheck constraints.

The issue wave should replace production UX in-place. A user should not need to know that a new SPA/demo exists.

## PRD Addendum Shape

Add a PRD section named something like:

```markdown
## UX Redesign Addendum: <date>

### Source Materials

- Design export: `<zip path or attachment>`
- Selected production variant: `<variant>`
- Existing runtime: `<url>`
- Existing repo: `<repo>`

### Product Direction

Short statement of the desired UX and what it optimizes for.

### Non-Goals

- Do not ship design tweak/debug controls.
- Do not make a separate demo-only app the user-facing product.
- Do not rewrite unrelated backend behavior.
- Do not deploy automatically unless explicitly approved.

### Route Replacement Map

| Existing route | New UX source | Data/API dependency | Compatibility requirement |
|---|---|---|---|

### Implementation Waves

Wave 0: foundation — global stylesheet (recipes as utility classes), token aliases, primitives, CI guards.
Wave 1: app shell — navigation, layout, selected theme, responsive shell.
Wave 2: primary user workflows.
Wave 3: ops/settings/live behavior.
Wave 4: polish and content quality.

### Acceptance Criteria

List page-by-page acceptance criteria.
```

Keep the PRD addendum product-facing. Implementation details belong in GitHub issues, not in prose-only PRD text.

## Issue Slicing

Slice by production route or coherent workflow, not by design file.

Good issue slices:

- **Wave 0 / Foundation**: land global stylesheet with byte-exact recipes as utility classes, token aliases, framework primitive overrides, CI guards. **Must merge before any route port.**
- App shell: navigation, layout, selected theme, responsive shell, user pill placement.
- List/library page: real list/search/tags/in-flight jobs.
- Detail page: real entity/summary/detail route.
- Queue + job detail: real job status, retry/cancel, log tail.
- Ops dashboard: real health/metrics/backup state.
- Settings: real settings sections and token/prompt surfaces.
- Command palette: navigation/search/submit.
- Live updates: polling/SSE hooks.
- Content quality: generated tags, descriptions, summaries.

Bad issue slices:

- "Implement the design zip."
- "Add `/__spa__` with the new app."
- "Copy all design files into the repo."
- "Support all themes and densities."
- "Make the UI match screenshot" with no route or data criteria.
- "Apply a redesign-inspired theme."
- "Use the same colors as the design."
- "Refactor frontend" without acceptance criteria.

## Issue Body Template

Use this structure:

```markdown
## Context

This implements `<route/workflow>` from the design export as production UX. The export source files **are the implementation** — not a spec, not inspiration. Wire data through them. Do not re-author.

Selected production variant: `<variant>`.
Do not ship variant/density/debug controls.

## Design Source Access

- Original submitter path: `<local path>`
- Worker/execution-surface path: `<host>:<absolute path>`
- SHA-256: `<checksum>`
- Repo baseline target: `<repo-relative path>`

## Scope

- Replace route `<route>` by following the Source Code Port Protocol:
  1. Land the literal copy of `<file.jsx>` (and dependent `<file.css>`, `<asset.svg>`) as commit N.
  2. Wire data/routing/primitives on top as commit N+1.
- Resolve every `var(--X)` in the design CSS against `<tokens-file>` and use the literal resolved value.
- Use real API/data from `<endpoint>`.
- Preserve `<compatibility requirement>`.

## Acceptance Criteria

- [ ] `<route>` renders the new UX in production.
- [ ] **Literal port evidence**: PR contains a commit that is the byte-for-byte copy of `<file>` from the design export, with adaptation commits on top. A reviewer can diff "design source vs first commit" and see no semantic changes.
- [ ] **Recipe utility classes used everywhere**: any card/hairline/divider/shell recipe uses the project's single utility class from the global stylesheet, not ad-hoc utility-class compositions.
- [ ] **No raw framework color literals** anywhere in production code. `bash scripts/check-design-tokens.sh` passes.
- [ ] **No nested same-recipe cards.** Outer recipe + inner second-tier recipe only.
- [ ] **IA preserved**: navigation groups, topbar context, status pills, footer/user-pill placement match the design's app-shell file exactly. Workspace-internal tabs live inside the workspace, not in the sidebar.
- [ ] **Logos/SVGs are verbatim** — same viewBox, node coords, stroke widths, dash patterns. No redraws.
- [ ] **Breadcrumbs/labels derive from current route**, not hardcoded.
- [ ] **Loading/empty/error states** use the design's primitives (skeletons, empty illustrations, error blocks), not generic spinners.
- [ ] Uses real data; no fake design data remains.
- [ ] Mobile and desktop layouts do not overlap or clip text.
- [ ] Existing links/bookmarks for `<route>` still work.
- [ ] No variant/density/debug controls are visible.
- [ ] Verification commands pass.
- [ ] The worker used the staged execution-surface design archive, not a laptop-only path.

## Verification

```bash
<lint command>
<test command>
bash scripts/check-design-tokens.sh
<frontend build command>
```

## Notes

- Design source (literal port targets): `<specific files and components>`.
- Resolved token values used: `<list each --var and its resolved literal>`.
- Related PRD addendum section: `<section>`.
- Forbidden substitutions for this route: `<list any palette/recipe substitutions that look tempting but are not allowed>`.
```

Suggested labels (adapt to your project's label scheme):

```text
ui, design-handoff, p1
```

Add `blocked` to future wave issues until the current wave is ready.

## Check For API Gaps

Before filing a page issue, identify whether the design requires data the backend does not expose.

Examples:

| Design need | Issue handling |
|---|---|
| count/aggregation field on a list | include API schema/query change in same issue only if small |
| in-flight job stages | create Queue/Job API issue first if missing |
| backup/heartbeat indicator | Ops issue can include endpoint if localized |
| short fluent description | separate content-quality issue |
| semantic tags/slugs | separate summarizer/prompt issue |

Do not let a page issue silently invent fake client-only data to satisfy the screenshot.

## Priority And Wave Policy

Suggested priority scheme (adapt to your project):

- urgent production breakage: `p0`;
- current main UX wave: `p1`;
- ordinary follow-up: `p2`;
- polish/content quality: `p3`.

Use a `blocked` label to control what an automated supervisor (or human picker) can pull from the queue.

For a new redesign:

1. Start with one issue in flight at a time.
2. After the first one or two PRs merge cleanly, allow 2-3 concurrent issues.
3. Open parallel issues only when they touch mostly separate files/routes.
4. Keep shared shell/routing/style issues small and early.
5. Do not open four route-heavy SPA issues at once unless the repo has proven conflict handling.

If you use an automated supervisor (e.g., Maestro) with a dynamic-wave mode that picks one issue at a time, keep that default. The supervisor should own wave handoff: all issues in the wave should be visible on the GitHub Project from the start, future issues should remain `blocked`, dependencies should be written in a parseable form, and the supervisor should remove `blocked` / add the ready label when upstream issues are merged and checks are green. Do not leave unblocking as an implicit human memory task. If the supervisor cannot yet perform dependency-based unblocking, create an explicit temporary handoff owner (automation or tracking issue) and record that it is a workaround, not the target operating model.

## Review Checklist Before Filing Issues

Use this checklist:

- [ ] The export README and primary HTML/index were read.
- [ ] All imported source files relevant to the issue were identified.
- [ ] The design archive is staged on the execution surface where workers run, with checksum verified and recorded.
- [ ] The selected production variant is stated.
- [ ] **The issue uses literal-port language**: "copy verbatim then wire data", "byte-exact", "literal port", "replace, don't repaint". The word "faithfully" alone is too soft — it gets read as "best-effort match". Add the explicit recipe (copy as commit N, adapt as commit N+1).
- [ ] **The first PR in the wave lands the global stylesheet (recipes as utility classes), token aliases, and CI guards** before any route ports.
- [ ] **CI guards exist in the repo** for raw framework color literals, ad-hoc recipe compositions, and nested same-recipe cards. Each runs in the Frontend Build CI job before the production build.
- [ ] Demo/debug controls are explicitly excluded.
- [ ] Every design page maps to a production route.
- [ ] Every issue names the exact design source files it must follow AND lists the design tokens whose resolved values the implementation must use literally.
- [ ] Logo/SVG/vector assets are called out as verbatim-paste components, not "recreate equivalent".
- [ ] Every issue has real data/API expectations.
- [ ] No issue says only "copy design export".
- [ ] No issue leaves the redesign hidden under a demo path.
- [ ] No issue can pass by changing only broad color tokens.
- [ ] **No issue is acceptable that lets the worker keep old DOM/markup and re-style it.** The expected workflow is `git rm` old + drop in design source.
- [ ] Each issue has test/build commands including the design-token guard.
- [ ] Live/visual healthcheck expectations compare structure and fidelity, not only HTTP status or palette tokens.
- [ ] Future/polish issues are `blocked` or `p3`.
- [ ] Current wave has at most 1-3 runnable issues.
- [ ] Automated-supervisor handoff is explicit: every issue is on the Project, dependencies are parseable, and either the supervisor owns unblocking or a temporary handoff owner is recorded.
- [ ] PRD addendum records non-goals and compatibility requirements.

## Commands For GitHub Issue Creation

Create body files locally, then:

```bash
gh issue create --repo <org>/<repo> \
  --title "Page: <name> production UX" \
  --body-file /tmp/issue-<slug>.md \
  --label ui \
  --label design-handoff \
  --label p1
```

Create future issues blocked:

```bash
gh issue create --repo <org>/<repo> \
  --title "<future polish item>" \
  --body-file /tmp/issue-<slug>.md \
  --label ui \
  --label api \
  --label p3 \
  --label blocked
```

Unblock only the next wave:

```bash
gh issue edit <issue> --repo <org>/<repo> --remove-label blocked
gh issue comment <issue> --repo <org>/<repo> --body "Opening this as part of the current UX wave. Keep scope to <route/workflow>."
```

## Anti-Pattern Catalog

These are the recurring drift patterns the guardrails in this runbook exist to prevent. Each anti-pattern is followed by the rule that catches it. Use this catalog at PR review time — if a PR exhibits any of these, the recipe is not yet "done" regardless of how the screenshots look in isolation.

- **Inspired restyle.** The export contains concrete source files, but the implementation is an approximate dark/<accent> restyle of existing components instead of replacing the old components with the design source. → Caught by: literal port evidence requirement (PR must contain a verbatim-copy commit).
- **Soft language drift.** Issue wording allows "renewed", "visual system", or "inspired by" interpretations instead of strict source-file fidelity. The word "faithfully" alone gets read as "best-effort match". → Caught by: literal-port language requirement in the issue template.
- **Redrawn vector.** A logo or icon is redrawn as a different SVG instead of pasting the exported source verbatim. → Caught by: "logos/SVGs are verbatim" acceptance criterion.
- **Recipe drift.** Consumer files use ad-hoc utility-class compositions (`bg-X border-Y rounded-Z p-W`) instead of a single named utility class ported byte-exact from the design source. Multiple consumers drift apart over time. → Caught by: CI guard for ad-hoc recipe compositions.
- **Double borders from nested recipes.** An outer recipe wraps another instance of itself, producing concentric borders that do not exist in the source. → Caught by: CI guard for nested same-recipe cards.
- **Framework color name substitution.** A worker picks `cyan-500` (or any framework family name) because a token alias has a similar word. The hue family is wrong. → Caught by: CI guard for raw framework color literals.
- **Wrong information architecture.** User pill moved from sidebar footer to topbar dropdown; workspace-internal tabs promoted to sidebar items; nav groups renamed or split. → Caught by: IA preservation acceptance criterion comparing against the design's app-shell file.
- **Hardcoded labels.** Topbar breadcrumb or page title set to one literal value because the screenshot showed it on one page. Wrong on every other route. → Caught by: "breadcrumbs/labels derive from current route" acceptance criterion.
- **Wrong primitive for the same job.** `<Spinner>` instead of skeletons; framework default Button instead of the design's primitive. The design's primitive is part of the design. → Caught by: "loading/empty/error states use design's primitives" criterion plus Wave 0 primitive ports.
- **Fix-only-what-was-screenshotted.** Worker fixes the route the user sent a screenshot of, leaving the same bug live on three other routes. → Caught by: sweep mandate — any forbidden pattern found on one route is systemic and must be fixed everywhere in one PR.
- **Healthcheck false-green.** Healthcheck checks broad tokens and overflow, reports `healthy` while the UI is objectively different from the design. → Caught by: healthchecks must fail on structural mismatches (wrong shared logo, missing topbar context, wrong sidebar groups, wrong user-pill placement, wrong first-viewport composition, palette-only restyles).
- **Demo route as final user surface.** Design exported as an app gets mounted under `/preview` or `/__spa__` and shipped that way instead of replacing the production route. → Caught by: route replacement map plus "no issue leaves the redesign hidden under a demo path".

Corrective rule (enforced by the above):

- For redesign work, `Done` requires (a) literal-port commit evidence in the PR, (b) all CI guards green, (c) live visual verification against the design source, not against the previous implementation.
- Healthchecks must fail on structural mismatches.
- If the live product differs from the accepted source files, the GitHub Project must show visible `In Progress`, `In Review`, or `Blocked` work.

## Handoff Prompt Template

Use this prompt when asking an agent to turn a design export into a PRD addendum and issue wave:

```text
I have a design export for <project>.

Repo: <org>/<repo>
Local checkout: <path>
PRD: <path or URL>
Design export: <zip path>
Worker-accessible design export: <host>:<absolute staged zip path>
Design export SHA-256: <checksum>
Current runtime URL: <url>
Selected production variant: <variant>

Analyze the export and produce:
1. A PRD addendum for the UX redesign.
2. A route replacement map.
3. A GitHub issue wave with small production-route/workflow issues.
4. Labels and priority/wave recommendations.

Critical rules — these are ORDERS, not suggestions:

- The export source files (*.jsx, *.tsx, *.vue, *.svelte, *.css, *.svg) ARE the implementation. Your job is to wire data through them. Do not re-author them.
- Before creating Maestro-ready issues, stage the design export on the worker execution surface and record the worker-accessible path plus SHA-256. Do not hand a remote worker only a laptop-local path.
- Workflow is "copy verbatim then wire", not "faithfully recreate". Every route port must land as two commits: (N) byte-for-byte copy of the design source, (N+1) data/routing adaptation on top. PR reviewers must be able to diff commit N against the design source and see no semantic changes.
- "Replace, don't repaint." When the design provides a component for a production target, `git rm` the old component and drop in the design source as the new component. Do not edit className on the old component as a shortcut.
- Resolve every var(--X) in the design CSS against the design's token file and use the literal resolved value. Never pick a "close" framework color name (e.g. Tailwind cyan-500 for a token whose word matches).
- Recipes named in the design (.card, .card-2, .hairline, .divider, .shell, etc.) port as single utility classes in the project's global stylesheet, byte-exact from the design source. Use the utility class at every consumer site. Never compose the recipe ad-hoc from individual utility classes.
- Never nest a recipe inside itself (two outer-tier cards). Outer + first-level inner only, per the design source.
- Preserve information architecture exactly: nav groups, topbar context, status pills, user-pill placement, footer composition. Do not "improve" the design's IA. Workspace-internal tabs stay inside their workspace.
- SVGs/logos are pasted verbatim. Same viewBox, node coords, stroke widths, dash patterns. No redraws.
- Breadcrumbs/labels derive from current route, never hardcoded.
- The first PR in the wave lands the global stylesheet (recipes as utility classes), the token alias config, and CI guards (raw color literals blocked, ad-hoc recipe compositions blocked, nested same-recipe cards blocked). Route ports come AFTER. Without the guards, the route ports will drift before the wave finishes.
- When a forbidden pattern is found on one route, the bug is systemic — fix every route + component in one sweep PR. Do not fix only the screenshot the user happened to send.
- Do not file an issue that only mounts the design under a demo route.
- Do not include variant pickers, density pickers, fake data switchers, or design debug controls in production scope.
- Use real app routes and real API/data expectations.
- Name the exact source files each issue must follow AND list the design tokens whose resolved values must be used literally.
- Separate API gaps from page work when the API change is not tiny.
- Mark future/polish issues blocked or p3.
- Current wave should have at most 1-3 runnable issues.

Before filing issues, report the proposed route map and issue list.
After approval, create the GitHub issues.
```

## Done Definition

This design-to-issues pass is done when:

- the PRD addendum exists;
- the selected production variant is recorded;
- the primary HTML/index and imported source files were read and mapped;
- the design export is staged on the execution surface, checksum-verified, and recorded in issues;
- demo-only controls are explicitly excluded;
- the route replacement map exists;
- GitHub issues exist with acceptance criteria;
- every UX issue uses literal-port language ("copy verbatim then wire", "byte-exact", "replace, don't repaint") and names fidelity-critical source files plus the design tokens whose literal resolved values must be used;
- a Wave 0 issue exists for landing the global stylesheet (recipes as utility classes), token aliases, framework primitive overrides, and CI guards before any route ports;
- only the intended first wave is runnable;
- future/polish issues are blocked or low priority;
- the implementing agent can start work without guessing which parts of the design export are mandatory.

A route port is done (per-PR) when:

- the PR includes a commit that is the byte-for-byte copy of the design source file, with adaptation commits on top — reviewers can diff "design source vs first commit" and see no semantic changes;
- all CI guards (raw color literals, ad-hoc recipe compositions, nested same-recipe cards) are green;
- a live screenshot of the deployed route is attached and visually matches the design source — not "looks similar", but matches.
