# Morning Operator Digest Runbook

`maestro digest` produces one ranked morning report across every configured fleet project, so the operator does not have to hunt across Mission Control, `maestro history`, and N GitHub repos for "what needs MY decision today" (#703).

This runbook is intentionally safe for shared docs. It uses placeholders for local paths and never requires printing tokens, environment variables, or raw config dumps.

## What the report contains

1. **Decide today** — items that need an operator decision, ranked:
   - pending supervisor approvals (danger risk first),
   - retry-exhausted sessions that still have an open PR,
   - issues carrying the blocked label whose blockers are all resolved (closed or PR merged),
   - PRs whose review-repair budget is exhausted with unresolved findings older than `--stale-review-hours` (default 24h).
2. **Promotable** — open issues without the ready label that look runnable (not epic/meta, not blocked, not already in progress), ranked by how mergeable/self-contained they appear (acceptance criteria, scoped description, non-exploratory title). Capped at 10 per project.
3. **Fleet health (24h)** — one line per project: sessions run, merges, failures, backend distribution.

Every item links to its GitHub issue or PR. GitHub API hiccups degrade gracefully: the affected check is skipped or over-reports, and a "Collection warnings" section flags the gap instead of silently dropping data.

The header carries a **GitHub auth** line so an operator can confirm which rate-limit bucket the fleet spends against (#823):

- `PAT/gh · bucket shared-pat` — the shared personal access token; all projects share one 5 000/hr core bucket.
- `GitHub App installation <id> · bucket installation · token expires <ts>` — authenticated as a GitHub App installation, which has its own bucket independent of the operator PAT. Configure it with a `github_app:` block (`app_id`, `private_key_path`, `installation_id`) on any fleet project; the private key stays on disk and is never logged or stored in the config store.
- `PAT/gh (App fallback active …)` — a `github_app:` block is present but token issuance failed, so the daemon fell back to the PAT. The reason is shown inline and a loud line is written to the daemon journal. The same auth mode is appended to the hourly `[github] REST usage …` journal digest.

## Running it

One command covers the whole fleet using the same `fleet.yaml` that Mission Control uses:

```sh
maestro digest --fleet ~/.maestro/fleet.yaml --out ~/vault/maestro
```

- `--out <dir>` writes `maestro-digest-YYYY-MM-DD.md` into the directory (create the directory inside your Obsidian vault sync root to pick it up automatically). Pass a `*.md` path instead to control the file name. Without `--out` the report goes to stdout only.
- `--notify` (default true) sends a short summary via the existing notifier (Telegram, direct or OpenClaw relay) **only when at least one decide-today item exists**. The first fleet project with a configured `telegram.target` supplies the notifier credentials. Pass `--notify=false` to suppress.
- `--stale-review-hours N` tunes the unresolved-review-findings age threshold (default 24).
- `--json` emits the structured report instead of Markdown — the same data the future Mission Control digest panel will consume (the collection layer lives in `internal/digest` as a reusable API).

Without `--fleet`, the command falls back to the usual config discovery: repeated `--config` flags, a `maestro.d/` directory, or the default single config.

## Scheduling (systemd user timer)

Create `~/.config/systemd/user/maestro-digest.service`:

```ini
[Unit]
Description=Maestro morning operator digest

[Service]
Type=oneshot
ExecStart=/usr/local/bin/maestro digest --fleet %h/.maestro/fleet.yaml --out %h/vault/maestro
```

And `~/.config/systemd/user/maestro-digest.timer`:

```ini
[Unit]
Description=Run the Maestro morning digest every day at 07:00

[Timer]
OnCalendar=*-*-* 07:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

Enable it:

```sh
systemctl --user daemon-reload
systemctl --user enable --now maestro-digest.timer
systemctl --user list-timers maestro-digest.timer
```

`Persistent=true` makes a missed run (machine asleep at 07:00) fire on the next boot. Use `journalctl --user -u maestro-digest.service` to inspect runs.

## Reading the ranking

Decide-today items carry a score used for ordering only (approvals 100+, retry-exhausted PRs 90, unblocked issues 80, stale review PRs 70+, with risk and age nudges). Promotable issues show their 0–100 self-containedness score inline so the operator can promote the top of the list with `maestro-ready` and skip the tail.

## Out of scope (follow-up)

Rendering the digest as a Mission Control dashboard panel is an explicit follow-up. The MVP keeps collection (`internal/digest.Collect`) separate from rendering (`Report.Markdown`, `Report.NotifySummary`) so the panel can reuse the same data without re-aggregating.
