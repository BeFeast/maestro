# UX Redesign Addendum: 2026-05-25

Parent epic: [#445 Epic: Mission Control literal port](https://github.com/BeFeast/maestro/issues/445)

## Source Materials

- Design export: `workshop:/mnt/storage/src/Maestro.zip`
- SHA-256: `1d646c5b60004429dd04e403b35e1c47f19422e4f42dfca3ffca16f67d9aee41`
- Primary index: `Maestro Mission Control.html` → `mc/*`
- Selected production variant: **light theme (primary), topbar theme toggle included, tape fleet layout**
- Existing runtime: Fleet MC at `http://127.0.0.1:8787` (read-only)
- Existing repo: `BeFeast/maestro`
- Repo baseline target: `internal/server/web/design-source/mc/`

## Product Direction

Replace the current header-and-rail fleet page with the Mission Control SPA shell from the design export. Operators should answer these questions in under 10 seconds from a single sidebar-navigated surface:

- Is Maestro alive?
- Is anything running now?
- Is anything actually blocked or waiting for me?
- If nothing is running, is that expected queue policy or a failure?
- Which project needs attention first?
- Which PR/issue/session explains the current state?

The design export source files (`mc/*.jsx`, `mc/mc.css`) **are the implementation** — wire production data through them; do not re-author from screenshots or token names.

## Non-Goals

- Do not ship the tweaks panel, scenario switcher, or fleet layout variant picker (tape / rail / cards).
- Do not mount MC under a demo route (`/__spa__`, `/preview`).
- Do not enable write actions in V1 (settings toggles render read-only/disabled).
- Do not rewrite supervisor or backend behavior.
- Brand exploration, UI audit, and mock HTML files in the zip are reference only — not production routes.
- Single-project `dashboard.html` unification is **deferred** to Wave 4; per-project `dashboard_url` ports were retired in #516 — every project is now reachable through the aggregator at `/project/<name>`.

## Route Replacement Map

| Existing route | New UX source | Data/API dependency | Compatibility requirement |
|---|---|---|---|
| `/`, `/fleet` | `mc/mc-fleet.jsx` → `FleetScreen` (tape layout only) | `GET /api/v1/fleet` | Same URLs; MC shell replaces inline layout |
| (none) | `mc/mc-screens.jsx` → `WorkersScreen` | `GET /api/v1/fleet` (workers, attention) | New first-class route `/workers` |
| (none) | `mc/mc-screens.jsx` → `ProjectScreen` | Per-project slice from fleet API | New route `/project/{slug}`; replaces the legacy `dashboard_url` redirect (#516) |
| needs-you rail + `/approvals/audit` | `mc/mc-screens.jsx` → `ApprovalsScreen` | `GET /api/v1/fleet` (approvals) | Preserve audit access; integrate active vs history |
| (none) | `mc/mc-screens.jsx` → `SettingsScreen` | New read-only config summary API (small) | Display-only in V1 |
| Partial Cmd-K (`fleet.js`) | `mc/mc-shell.jsx` → `CommandPalette` | Loaded fleet data | Strip scenario/theme demo items; keep topbar theme toggle |
| Per-project `dashboard_url` | Aggregator route `/project/<name>` | `/api/v1/fleet` (filtered slice) | Retired in #516; legacy `dashboard.html` is no longer the operator surface |

## Implementation Waves

| Wave | Scope | Runnable when |
|---|---|---|
| 0 | [#446](https://github.com/BeFeast/maestro/issues/446) | Global stylesheet (byte-exact recipes), React build pipeline, design-source verbatim copies, CI guards | Immediately |
| 1 | [#447](https://github.com/BeFeast/maestro/issues/447) | App shell: `SidebarV2`, `Topbar` (with theme toggle), `CommandPalette`, path routing, `BrandMark` | Wave 0 merged |
| 2 | [#448](https://github.com/BeFeast/maestro/issues/448) | Fleet overview: `FleetScreen` + `FleetTape`, stat strip, previews | Wave 1 merged |
| 3 | [#449](https://github.com/BeFeast/maestro/issues/449) | Workers + Approvals + `WorkerDrawer` | Wave 2 merged |
| 4 | [#450](https://github.com/BeFeast/maestro/issues/450) | Project dashboard + Settings (read-only); begin single-project deprecation planning | Wave 3 merged |
| 5 | [#451](https://github.com/BeFeast/maestro/issues/451) | Tape timeline API enrichment, polish, content quality | Wave 4 merged |

## Fidelity-Critical Source Files

| Source file | Production target |
|---|---|
| `mc/mc.css` | Global theme tokens + recipes (`.hb`, `.sb`, `.stat`, `.panel`, `.tape`, `.card`, `.appv`, …) |
| `mc/mc-atoms.jsx` | Primitives: `Icon.*`, `Heartbeat`, `Stat`, `Panel`, pills |
| `mc/mc-shell.jsx` | `SidebarV2`, `Topbar`, `CommandPalette`, `BrandMark` |
| `mc/mc-fleet.jsx` | `FleetScreen`, `FleetTape` (layout switcher removed) |
| `mc/mc-screens.jsx` | `ProjectScreen`, `WorkersScreen`, `ApprovalsScreen`, `SettingsScreen`, `WorkerDrawer` |
| `mc/mc-app.jsx` | Router composition (demo controls stripped) |

## Resolved Token Values (light theme — must use literally)

From `mc/mc.css` `:root[data-theme="light"]`:

| Token | Resolved value |
|---|---|
| `--bg-0` | `#f5f6f9` |
| `--bg-1` | `#ffffff` |
| `--bg-2` | `#eef0f4` |
| `--border-1` | `#e4e7ec` |
| `--fg-0` | `#0c0d10` |
| `--fg-1` | `#2a2d33` |
| `--accent` | `#0d9488` |
| `--accent-2` | `#0891b2` |
| `--ok` | `#047857` |
| `--watch` | `#b45309` |
| `--stuck` | `#b91c1c` |
| `--policy` | `#7c3aed` |
| `--info` | `#1d4ed8` |

All recipe classes must resolve every `var(--X)` against this file and port as single utility classes.

## Acceptance Criteria (page-level)

### Fleet overview (`/`, `/fleet`)

- [ ] First viewport: heartbeat hero (`.hb`), stat strip, tape timeline — matches `mc-fleet.jsx` + `mc.css` at light theme, tape layout.
- [ ] Sidebar IA matches `SidebarV2` exactly (Fleet / Workers / Approvals groups, project list, System / Settings, `sb-foot`).
- [ ] Topbar includes theme toggle; breadcrumbs derive from current route.
- [ ] Uses real `/api/v1/fleet` data; no scenario fake data.
- [ ] Literal-port commit evidence in PR (byte-for-byte copy commit N, adaptation N+1).
- [ ] CI design guards green.

### App shell (all routes)

- [ ] `BrandMark` SVG pasted verbatim (viewBox `0 0 96 96`, same node coords and stroke widths).
- [ ] Command palette excludes scenario switcher items; includes navigation/search only.
- [ ] No tweaks panel, layout picker, or scenario controls visible.

## Relationship to Epic #335

Epic #335 tracked incremental UX fixes against UI Audit mocks. This literal-port wave supersedes that approach for shell, routing, and visual fidelity. Completed M-series items remain valid where they do not conflict; open items (#336, #344, #345, #346) fold into Waves 1–3.
