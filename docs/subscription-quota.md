# Subscription quota telemetry & quota-aware dispatch (#704)

The whole fleet typically burns one Claude subscription account. The
subscription meters a **5-hour session window** (starts at first use,
resets 5 hours later) and a **weekly cap** — and the provider exposes no
programmatic usage API, so hitting either limit used to be discovered
reactively: workers start dying and dispatch degrades to fallback
backends (#694/#695).

With quota calibration configured, maestro instead:

1. **Tracks usage** — every cycle, newly observed session tokens accrue
   into per-backend rolling windows (`backend_quota_usage` in state):
   a 5-hour window and a 7-day window, each anchored at the first
   tokens observed after the previous window expired.
2. **Exposes it** — `/api/v1/fleet` carries a `backend_quota` row per
   calibrated backend (percent used, reset ETA, pressured flag), and
   Mission Control renders a gauge next to the backend health pills.
   Provider/model cooldowns are shown alongside the gauge with aggregate
   credential-pool availability. A low account-level percentage does not prove
   that a specific model is enabled or currently usable on that credential.
3. **Steers dispatch** — once estimated usage crosses
   `dispatch_threshold` (default **0.85**), the backend gets a
   BackendHealth cooldown with reason `quota_pressure` and `RetryAfter`
   at the window reset. The existing dispatch gate (#695) then routes
   fresh dispatches to the next healthy backend in
   `model.fallback_backends`; running workers are not touched. A
   `backend_quota_pressure` supervisor finding (warning) is emitted once
   per pressure episode and self-clears at reset.

Out of scope (by design): keep-alive pings to start the window early. Credential
rotation remains owned by CLIProxyAPI; Maestro consumes only its aggregate,
secret-free result and applies model-scoped fallback.

## Configuration

```yaml
model:
  default: claude
  fallback_backends: [codex]
  backends:
    claude:
      cmd: claude
      quota:
        window_tokens: 12000000   # est. token capacity of one 5h window
        weekly_tokens: 80000000   # est. token capacity of one week
        dispatch_threshold: 0.85  # fraction (0..1); default 0.85
    codex:
      cmd: codex
```

- A backend without a `quota:` block is not quota-tracked and never
  quota-gated.
- Either capacity may be omitted; only configured windows are checked.
- `dispatch_threshold` must be a fraction in `(0, 1]` — `85` is
  rejected at config parse so a percent-style typo cannot silently
  disable steering.

## Calibrating window_tokens / weekly_tokens

Maestro estimates usage from its own per-session token counters (the
same counters behind the MC cost panel, #619). The provider's metering
is opaque and not token-for-token identical, so the capacities are
deliberately operator-calibrated knobs:

1. Run the fleet for a stretch where the subscription's own usage
   readout is visible (run `claude /status` interactively, or check the
   provider dashboard) and note the percent used for the current 5h
   session and for the week.
2. Read maestro's token burn for the same stretch: the MC **Cost &
   usage** panel (today/7d per backend) or `backend_quota_usage` in the
   fleet snapshot.
3. Divide: `window_tokens ≈ tokens_burned / fraction_used`. Example: if
   maestro metered 4.2M tokens while `claude /status` shows 35% of the
   session window, calibrate `window_tokens: 12000000`. Same arithmetic
   for `weekly_tokens` against the weekly readout.
4. Re-check after a week. If the gauge consistently reads low (pressure
   never fires before the provider limits hit), lower the capacities;
   if dispatch is steered away while `claude /status` still shows
   headroom, raise them.

Estimation caveats to keep in mind:

- Each project's orchestrator only meters its **own** sessions. When
  several projects share one subscription, either give each project a
  proportional share of the capacity, or treat the MC gauge (which
  shows the fullest per-project reading per backend) as a lower bound.
- Tokens are observed from worker logs with some lag, and supervisor /
  router calls outside worker sessions are not metered. The threshold
  (15% headroom by default) is the buffer for this drift.
- Window anchoring is approximate: maestro anchors a window at the
  first tokens it observes, which can lag the provider's anchor by up
  to one poll cycle.

## Operations

- **MC gauge** — green below `threshold − 10pp`, amber when
  approaching, red once pressured (`dispatch → fallback`). Tooltip
  shows exact window/week percentages and the threshold.
- **Pressure episode** — one `backend_quota_pressure` warning finding
  per episode; evidence carries the usage pattern and `reset_at`. No
  operator action is required: the gate self-clears when the window
  resets or usage drops below threshold. If episodes recur every
  window, recalibrate the capacities or reduce `max_parallel`.
- **Interaction with hard failures** — a quota gate never overwrites an
  auth-failure or provider-limit cooldown, and unlike those, PR
  evidence does not clear it (the backend keeps working while
  pressured; only the reset clock ends the episode).

## Reactive usage-limit classification (#805)

Calibration is predictive and can miss (mis-calibrated capacities,
another consumer draining the same account). When the provider itself
starts refusing work — the CLI prints a quota error and exits — maestro
classifies the death reactively instead of burning
`max_retries_per_issue` respawning onto a dead backend:

1. **Parseable reset** — a dead (or live) worker whose log matches a
   rate-limit signature *and* states a reset time is a high-confidence
   provider limit (#663). Reset hints may be date-bearing ("try again
   at May 30, 2026 8:13 PM"), **time-only** ("try again at 12:30 PM",
   the live codex phrasing), or the Claude subscription **"resets
   `<clock>` (`<tz>`)"** shape ("You've hit your session limit · resets
   9am (UTC)", "You're out of extra usage · resets 4:10pm (UTC)", #808)
   — a clock-only hint resolves to the next occurrence of that
   wall-clock time. The backend is gated with reason `provider_limit`
   and `RetryAfter` at the stated reset.
2. **No parseable reset** — a worker that died within the early-death
   window (~10 min of spawn) whose log **tail** matches an
   account-quota exhaustion signature ("You've hit your `<usage|
   session>` limit", "You've reached your `<plan>` limit" / the
   `/usage-credits` marker (Claude/Fable, #808), "You're out of extra
   usage", the codex settings/usage URL, claude "usage limit reached")
   is gated with reason `usage_limit` and a fixed 30-minute re-probe
   cooldown. Generic signals (bare 429, "too many requests") stay
   excluded — they are transient and acting on them is the #663
   false-positive class.

Either way the attempt respawns on the next healthy backend from
`model.fallback_backends` in the same cycle, the session is excluded
from `FailedAttemptsForIssue` (retry budget preserved), a
`backend_usage_limit` supervisor finding is emitted (blocking when the
default backend is gated), and `maestro status` prints the gate under
"Backend health". Scheduled retries also consult `backend_health`
before respawning: a retry whose backend is gated substitutes the first
healthy fallback, or defers to the cooldown expiry when none exists.

Backends with quota phrasings maestro does not know can extend the
classifier per backend:

```yaml
model:
  backends:
    mycli:
      cmd: mycli
      usage_limit_patterns:
        - "(?i)monthly spend cap reached"
```

Entries are regexes (validated at config parse) matched against the
dead worker's log tail. Keep them high-precision: a pattern that
matches ordinary work output causes false backend gating.
