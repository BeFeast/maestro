import { parseTimestamp, relTime, slugifyProject } from "./utils.js";
import { managementHomeView } from "./managementHome.js";

const THEME_STORAGE_KEY = "maestro.mc.theme";

export function loadStoredTheme() {
  try {
    const stored = localStorage.getItem(THEME_STORAGE_KEY);
    if (stored === "dark" || stored === "light") return stored;
  } catch (_) {
    /* ignore */
  }
  return "light";
}

export function storeTheme(theme) {
  try {
    localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch (_) {
    /* ignore */
  }
}

export async function fetchFleetRaw() {
  const response = await fetch("/api/v1/fleet", { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

export async function fetchWorkerDetail(project, slot, lines = 260) {
  const url =
    "/api/v1/fleet/worker?project=" +
    encodeURIComponent(project || "") +
    "&slot=" +
    encodeURIComponent(slot || "") +
    "&lines=" +
    encodeURIComponent(String(lines));
  const response = await fetch(url, { cache: "no-store" });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

export function mapFleetResponse(raw, now = Date.now()) {
  const workers = collectWorkers(raw);
  const approvals = collectApprovals(raw);
  // (postApproval / postFleetApproval helpers are exported below.)
  const projects = (raw.projects || []).map(p => mapProject(p, workers, now));
  const summary = raw.summary || {};
  const verdictTone = mapVerdictUiTone(raw.verdict?.tone);
  // #598: subtract convergence-bound items (retry_exhausted with an open
  // PR and no failing-check evidence) so a self-resolving PR does not
  // inflate the operator-facing attention count or the red attention
  // stat card. The raw needs_attention count is still available via
  // summary.needs_attention for diagnostics.
  const selfResolving = Math.max(
    0,
    Math.min(Number(summary.needs_attention || 0), Number(summary.self_resolving || 0)),
  );
  const actionableAttention = Math.max(
    0,
    Number(summary.needs_attention || 0) - selfResolving,
  );
  const actionableApprovals = summary.approvals_actionable == null
    ? Number(summary.approvals_pending || 0)
    : Number(summary.approvals_actionable || 0);
  const attentionCount =
    actionableAttention +
    actionableApprovals +
    Number(summary.errors || 0) +
    Number(summary.stale || 0) +
    Number(summary.dispatch_failures || 0) +
    Number(summary.outcome_drift || 0) +
    Number(summary.no_eligible_issues || 0);

  return {
    raw,
    readOnly: raw.read_only === true,
    emergency: mapEmergency(raw.emergency),
    refreshedAt: raw.refreshed_at || "",
    nextAction: raw.next_action || null,
    verdict: pickVerdictTuple(raw.verdict, raw.operator_brief),
    verdictTone,
    operatorBrief: raw.operator_brief || null,
    summary,
    projects,
    projectOrder: projects.map(p => p.slug),
    workers,
    approvals,
    pendingApprovals: approvals.filter(isPendingApproval),
    historicalApprovals: approvals.filter(a => !isPendingApproval(a)),
    daemonAlive: raw.verdict?.tone !== "daemon-down",
    heartbeatBpm: estimateHeartbeatBpm(summary, raw.verdict?.tone),
    workerCount: Number(summary.running || 0),
    prCount: Number(summary.pr_open || 0),
    attentionCount,
    activeApprovals: Number(summary.approvals_pending || 0),
    selfResolvingCount: selfResolving,
    throughputMerged7d: Number(summary.throughput_merged_7d || 0),
    throughputDaily7d: Array.isArray(summary.throughput_daily_7d)
      ? summary.throughput_daily_7d.map(v => Number(v || 0))
      : [],
    lastDecisionAge: latestSupervisorAge(raw.projects || []),
    supervisorPulse: aggregateSupervisorPulse(raw.projects || []),
    backendHealth: aggregateBackendHealth(raw.projects || []),
    backendQuota: aggregateBackendQuota(raw.projects || []),
    costObservability: mapCostObservability(raw.cost_observability),
  };
}

// mapCostObservability normalizes the server-side cost rollup (#619)
// for SPA consumers. The server computes pricing — the SPA never blends
// rates client-side — so this is a thin shape adapter that fills in
// missing arrays and coerces numbers so React renders cleanly when the
// server returns sparse fields. Returns null when the snapshot lacks
// cost_observability so a legacy server / unconfigured project simply
// hides the panel.
export function mapCostObservability(raw) {
  if (!raw || typeof raw !== "object") return null;
  return {
    today: mapCostWindow(raw.window_today),
    week: mapCostWindow(raw.window_7d),
    lifetime: mapCostWindow(raw.lifetime),
    perBackend: (raw.per_backend || []).map(mapCostBackend),
    perProject: (raw.per_project || []).map(mapCostProject),
    perIssue: (raw.per_issue || []).map(mapCostIssue),
  };
}

function mapCostIssue(raw) {
  return {
    issueNumber: Number(raw?.issue_number || 0),
    issueTitle: String(raw?.issue_title || ""),
    tokens: Number(raw?.tokens || 0),
    usd: Number(raw?.usd || 0),
    sessions: Number(raw?.sessions || 0),
    backends: Array.isArray(raw?.backends) ? raw.backends.map(String) : [],
  };
}

function mapCostWindow(raw) {
  if (!raw || typeof raw !== "object") {
    return { tokens: 0, pricedTokens: 0, unpricedTokens: 0, usd: 0, sessions: 0 };
  }
  return {
    tokens: Number(raw.tokens || 0),
    pricedTokens: Number(raw.priced_tokens || 0),
    unpricedTokens: Number(raw.unpriced_tokens || 0),
    usd: Number(raw.usd || 0),
    sessions: Number(raw.sessions || 0),
  };
}

function mapCostBackend(raw) {
  return {
    backend: String(raw?.backend || ""),
    priceConfigured: raw?.price_configured === true,
    inputUSDPerMtok: Number(raw?.input_usd_per_mtok || 0),
    outputUSDPerMtok: Number(raw?.output_usd_per_mtok || 0),
    today: mapCostWindow(raw?.today),
    week: mapCostWindow(raw?.week),
    lifetime: mapCostWindow(raw?.lifetime),
  };
}

function mapCostProject(raw) {
  return {
    project: String(raw?.project || ""),
    repo: String(raw?.repo || ""),
    today: mapCostWindow(raw?.today),
    week: mapCostWindow(raw?.week),
    lifetime: mapCostWindow(raw?.lifetime),
  };
}

// formatTokens renders an integer token count using a K/M suffix so the
// panel keeps a narrow column width. 1_234 → "1.2K", 12_345_678 → "12.3M".
// Numbers under 1000 render as-is.
export function formatTokens(n) {
  const v = Number(n || 0);
  if (v <= 0) return "0";
  if (v < 1000) return String(v);
  if (v < 1_000_000) return `${(v / 1000).toFixed(v < 10_000 ? 1 : 0)}K`;
  return `${(v / 1_000_000).toFixed(v < 10_000_000 ? 2 : 1)}M`;
}

// formatUSD renders a USD estimate with sensible precision for the
// dashboard: values under $10 keep two decimals, values under $1000
// keep one, larger values drop decimals entirely.
export function formatUSD(n) {
  const v = Number(n || 0);
  if (v <= 0) return "$0";
  if (v < 10) return `$${v.toFixed(2)}`;
  if (v < 1000) return `$${v.toFixed(1)}`;
  return `$${Math.round(v).toLocaleString()}`;
}

// aggregateBackendHealth folds per-project `backend_health` maps (#534)
// into a single fleet-level list the SPA header can render as
// «claude — cooldown until 21:00 UTC · codex — available · opencode —
// available». When the same backend appears across projects, the worst
// state wins (cooldown > available) and the earliest RetryAfter is kept
// so the countdown reflects the soonest recovery.
//
// #600: a cooldown entry whose retry_after is already in the past is
// coerced to "available" client-side. The server normalizes the map on
// every snapshot, but this guard protects against unrefreshed snapshots
// so the panel never reports a working backend as limited.
export function aggregateBackendHealth(rawProjects, now = Date.now()) {
  const out = new Map();
  for (const project of rawProjects || []) {
    const map = project?.backend_health || {};
    for (const [name, entry] of Object.entries(map)) {
      if (!entry || typeof entry !== "object") continue;
      let state = String(entry.state || "");
      const retry = entry.retry_after || null;
      const retryMs = retry ? parseTimestamp(retry) : null;
      if (state === "cooldown" && retryMs != null && retryMs <= now) {
        state = "available";
      }
      const existing = out.get(name);
      if (!existing) {
        out.set(name, {
          backend: name,
          state,
          reason: state === "available" ? "" : entry.reason || "",
          pattern: state === "available" ? "" : entry.pattern || "",
          retryAfter: state === "available" ? null : retry,
          retryAfterMs: state === "available" ? null : retryMs,
          since: entry.since || "",
        });
        continue;
      }
      // Worst-state wins: cooldown > available > unknown empty.
      if (existing.state !== "cooldown" && state === "cooldown") {
        existing.state = "cooldown";
        existing.reason = entry.reason || existing.reason;
        existing.pattern = entry.pattern || existing.pattern;
      }
      if (state === "cooldown" && retryMs != null && (existing.retryAfterMs == null || retryMs < existing.retryAfterMs)) {
        existing.retryAfter = retry;
        existing.retryAfterMs = retryMs;
      }
    }
  }
  return Array.from(out.values()).sort((a, b) => a.backend.localeCompare(b.backend));
}

// backendHealthTone maps a BackendHealth.state value to the SPA's pill
// tone vocabulary so the row uses the same red/green/grey palette as the
// rest of Mission Control.
export function backendHealthTone(state) {
  switch (String(state || "")) {
  case "cooldown": return "stuck";
  case "available": return "ok";
  default: return "idle";
  }
}

// formatBackendHealthSentence renders one row's status as a single
// operator-readable sentence. Returns "" when the backend is healthy
// and the SPA already shows an "available" pill so the row isn't
// double-talked.
export function formatBackendHealthSentence(entry, now = Date.now()) {
  if (!entry) return "";
  if (entry.state !== "cooldown") return "";
  const target = entry.retryAfterMs != null ? entry.retryAfterMs : parseTimestamp(entry.retryAfter);
  if (target == null) {
    return entry.reason
      ? `auto-recovery pending (${entry.reason})`
      : "auto-recovery pending";
  }
  const remainingSec = Math.max(0, Math.round((target - now) / 1000));
  const eta = formatCountdown(remainingSec);
  return `cooldown · auto-recovery in ${eta}`;
}

// aggregateBackendQuota folds per-project `backend_quota` rows (#704)
// into one fleet-level list for the header gauge. Each project's
// orchestrator only meters its own sessions against the shared
// subscription, so when the same backend appears across projects the
// fullest reading wins (max of window/week percent) — the displayed
// gauge is then a lower bound on the true account-level usage.
export function aggregateBackendQuota(rawProjects) {
  const out = new Map();
  for (const project of rawProjects || []) {
    for (const raw of project?.backend_quota || []) {
      if (!raw || typeof raw !== "object") continue;
      const entry = {
        backend: String(raw.backend || ""),
        windowCapTokens: Number(raw.window_cap_tokens || 0),
        windowUsedTokens: Number(raw.window_used_tokens || 0),
        windowPercent: Number(raw.window_percent || 0),
        windowResetAt: raw.window_reset_at || null,
        weeklyCapTokens: Number(raw.weekly_cap_tokens || 0),
        weekUsedTokens: Number(raw.week_used_tokens || 0),
        weekPercent: Number(raw.week_percent || 0),
        weekResetAt: raw.week_reset_at || null,
        dispatchThreshold: Number(raw.dispatch_threshold || 0),
        pressured: raw.pressured === true,
      };
      if (!entry.backend) continue;
      const existing = out.get(entry.backend);
      if (!existing || quotaMaxPercent(entry) > quotaMaxPercent(existing)) {
        out.set(entry.backend, entry);
      }
    }
  }
  return Array.from(out.values()).sort((a, b) => a.backend.localeCompare(b.backend));
}

function quotaMaxPercent(entry) {
  return Math.max(Number(entry.windowPercent || 0), Number(entry.weekPercent || 0));
}

// backendQuotaTone maps a quota row to the SPA's pill palette: red once
// dispatch is being steered away, amber within 10 points of the
// threshold, green otherwise.
export function backendQuotaTone(entry) {
  if (!entry) return "idle";
  if (entry.pressured) return "stuck";
  const threshold = (Number(entry.dispatchThreshold || 0) || 0.85) * 100;
  if (quotaMaxPercent(entry) >= threshold - 10) return "watch";
  return "ok";
}

// formatBackendQuotaSentence renders one quota row as an operator
// sentence: «window 62% · resets in 1h 12m» (plus «week 34%» when a
// weekly cap is calibrated). Pressured rows get an explicit marker so
// the gauge explains why dispatch moved to fallbacks.
export function formatBackendQuotaSentence(entry, now = Date.now()) {
  if (!entry) return "";
  const parts = [];
  if (entry.windowCapTokens > 0) {
    parts.push(`window ${Math.round(entry.windowPercent)}%`);
  }
  if (entry.weeklyCapTokens > 0) {
    parts.push(`week ${Math.round(entry.weekPercent)}%`);
  }
  if (parts.length === 0) return "";
  const resetAt = entry.pressured && entry.weekPercent >= entry.windowPercent && entry.weekResetAt
    ? entry.weekResetAt
    : (entry.windowResetAt || entry.weekResetAt);
  const resetMs = resetAt ? parseTimestamp(resetAt) : null;
  if (resetMs != null && resetMs > now) {
    parts.push(`resets in ${formatCountdown(Math.round((resetMs - now) / 1000))}`);
  }
  if (entry.pressured) {
    parts.push("dispatch → fallback");
  }
  return parts.join(" · ");
}

// formatAttributionSegment renders one BackendAttribution entry as the
// short inline string used on session cards: "claude opus-4.8 xhigh
// (12m)". Backend name is always present; provider/model/variant/effort
// degrade individually when absent. Duration is computed from
// started_at / ended_at; if the segment is still open, ended_at is
// taken as `now`.
export function formatAttributionSegment(segment, now = Date.now()) {
  if (!segment) return "";
  const parts = [];
  if (segment.backend) parts.push(segment.backend);
  const meta = [segment.model, segment.variant, segment.effort]
    .filter(Boolean)
    .filter((v, i, arr) => arr.indexOf(v) === i);
  if (meta.length) parts.push(meta.join(" "));
  const duration = attributionSegmentDuration(segment, now);
  if (duration) parts.push(`(${duration})`);
  return parts.join(" ").trim();
}

export function attributionSegmentDuration(segment, now = Date.now()) {
  const startMs = parseTimestamp(segment?.started_at);
  if (startMs == null) return "";
  const endMs = segment.ended_at ? parseTimestamp(segment.ended_at) : now;
  if (endMs == null || endMs < startMs) return "";
  const seconds = Math.max(1, Math.round((endMs - startMs) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const rem = minutes % 60;
  return rem > 0 ? `${hours}h ${rem}m` : `${hours}h`;
}

// formatAttributionTimeline joins the per-segment summary with arrow
// separators for inline session-card use, e.g. "claude opus-4.8 xhigh
// (12m) → codex gpt-5.5 medium (4m, fallover)". When the previous
// segment carries an end_reason the cause is tucked into the next
// segment's parens so the operator can read the fallover cause inline
// without opening the drawer.
export function formatAttributionTimeline(attribution, now = Date.now()) {
  const segments = Array.isArray(attribution) ? attribution : [];
  if (!segments.length) return "";
  return segments
    .map((seg, i) => {
      const base = formatAttributionSegment(seg, now);
      if (i === 0) return base;
      const prevReason = segments[i - 1]?.end_reason || "";
      if (!prevReason) return base;
      if (/\)$/.test(base)) return base.replace(/\)$/, `, ${prevReason})`);
      return `${base} (${prevReason})`;
    })
    .join(" → ");
}

// aggregateSupervisorPulse rolls the per-project `supervisor_pulse` blocks
// (issue #531) up to a single fleet-level pulse the header verdict card
// can render. The "freshest" run_once timestamp wins (so the countdown
// matches the project that just ticked); the poll interval and mode are
// taken from that same project so the cadence story is consistent; the
// decision sparkline is the merged-then-sliced last 10 verbs across all
// projects, oldest → newest, so idle is visible as a positive signal.
export function aggregateSupervisorPulse(rawProjects) {
  let freshest = null;
  let freshestMs = null;
  let anyStuck = false;
  let stuckReason = "";
  const decisions = [];
  for (const project of rawProjects) {
    const pulse = project?.supervisor_pulse || null;
    if (!pulse) continue;
    if (pulse.stuck) {
      anyStuck = true;
      if (!stuckReason && pulse.stuck_reason) stuckReason = pulse.stuck_reason;
    }
    const latestDecisionAt = parseTimestamp(project?.supervisor?.latest?.created_at);
    for (const verb of pulse.recent_actions || []) {
      if (!verb) continue;
      decisions.push({ verb, t: latestDecisionAt || 0 });
    }
    const ms = parseTimestamp(pulse.last_run_once_at);
    if (ms != null && (freshestMs == null || ms > freshestMs)) {
      freshestMs = ms;
      freshest = { pulse, project };
    }
  }
  // Newest verb-per-project is approximated by the project's latest decision
  // timestamp; stable sort keeps order within a project. Slice to last 10.
  decisions.sort((a, b) => a.t - b.t);
  const recentActions = decisions.slice(-10).map(d => d.verb);
  if (!freshest) {
    return {
      lastRunOnceAt: null,
      lastRunOnceMs: null,
      pollIntervalSeconds: 0,
      mode: "",
      recentActions,
      stuck: anyStuck,
      stuckReason,
    };
  }
  return {
    lastRunOnceAt: freshest.pulse.last_run_once_at || null,
    lastRunOnceMs: freshestMs,
    pollIntervalSeconds: Number(freshest.pulse.poll_interval_seconds || 0),
    mode: String(freshest.pulse.mode || ""),
    recentActions,
    stuck: anyStuck,
    stuckReason,
  };
}

// nextDecisionCountdown returns the seconds remaining until the next
// supervisor cycle is expected to land (issue #531). Negative values
// mean the cycle is overdue. Returns null when the pulse has no
// last_run_once_at or no positive interval so the SPA can fall back
// to the legacy "—" placeholder.
export function nextDecisionCountdown(pulse, now = Date.now()) {
  if (!pulse) return null;
  const lastMs = pulse.lastRunOnceMs != null ? pulse.lastRunOnceMs : parseTimestamp(pulse.lastRunOnceAt);
  if (lastMs == null) return null;
  const interval = Number(pulse.pollIntervalSeconds || 0);
  if (interval <= 0) return null;
  const dueMs = lastMs + interval * 1000;
  return Math.round((dueMs - now) / 1000);
}

// formatCountdown renders a seconds value as "1m 47s" / "12s" / "OVERDUE"
// for the header card. Long durations switch to "Hh Mm" so the header
// doesn't grow when the daemon is wedged on a long interval.
export function formatCountdown(seconds) {
  if (seconds == null || Number.isNaN(seconds)) return "—";
  if (seconds <= 0) return "now";
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) {
    const m = Math.floor(seconds / 60);
    const s = seconds % 60;
    return s > 0 ? `${m}m ${s}s` : `${m}m`;
  }
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  return m > 0 ? `${h}h ${m}m` : `${h}h`;
}

// pulseFreshnessTone maps an age (now - lastRunOnceAt) against the configured
// interval onto the three tones the header pulse-dot uses: "ok" while the
// cycle is on time, "watch" past 2× interval, "stuck" past 3× interval (mirrors
// the watchdog rule in state.SupervisorStuck, see #499).
export function pulseFreshnessTone(pulse, now = Date.now()) {
  if (!pulse) return "idle";
  if (pulse.stuck) return "stuck";
  const lastMs = pulse.lastRunOnceMs != null ? pulse.lastRunOnceMs : parseTimestamp(pulse.lastRunOnceAt);
  if (lastMs == null) return "idle";
  const interval = Number(pulse.pollIntervalSeconds || 0);
  if (interval <= 0) return "ok";
  const ageMs = now - lastMs;
  if (ageMs > interval * 1000 * 3) return "stuck";
  if (ageMs > interval * 1000 * 2) return "watch";
  return "ok";
}

// formatAbsoluteTimestamp renders an ISO timestamp for the tooltip shown on
// hover (issue #531, point 10). Returns "" when the value is missing so
// callers can drop the title attribute entirely.
export function formatAbsoluteTimestamp(value) {
  const ms = parseTimestamp(value);
  if (ms == null) return "";
  try {
    return new Date(ms).toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
  } catch (_) {
    return "";
  }
}

// relTimePrecise renders an age in `hh:mm:ss` once it exceeds an hour
// (issue #531, point 10). Below that it falls back to `relTime`.
export function relTimePrecise(date, now) {
  const ms = now - (date instanceof Date ? date.getTime() : date);
  if (!Number.isFinite(ms) || ms < 0) return relTime(date, now);
  const seconds = Math.floor(ms / 1000);
  if (seconds < 3600) return relTime(date, now);
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  const pad = n => (n < 10 ? "0" + n : "" + n);
  return `${pad(h)}:${pad(m)}:${pad(s)} ago`;
}

function collectWorkers(raw) {
  if (Array.isArray(raw.workers) && raw.workers.length) {
    return raw.workers.map(mapWorker);
  }
  return (raw.projects || []).flatMap(project =>
    (project.active || []).map(worker =>
      mapWorker({
        ...worker,
        project_name: worker.project_name || project.name,
        project_repo: worker.project_repo || project.repo,
        dashboard_url: worker.dashboard_url || project.dashboard_url,
      })
    )
  );
}

function collectApprovals(raw) {
  const approvals = Array.isArray(raw.approvals)
    ? raw.approvals.slice()
    : (raw.projects || []).flatMap(project =>
        (project.approvals || []).map(approval => ({
          ...approval,
          project_name: approval.project_name || project.name,
          project_repo: approval.project_repo || project.repo,
          dashboard_url: approval.dashboard_url || project.dashboard_url,
        }))
      );
  return approvals.map(mapApproval);
}

function mapProject(project, workers, now) {
  const slug = slugifyProject(project.name);
  const queue = project.queue_snapshot || {};
  const outcome = project.outcome || {};
  return {
    slug,
    name: project.name,
    repo: project.repo || "",
    configPath: project.config_path || "",
    dashboardUrl: project.dashboard_url || "",
    backend: project.supervisor?.latest?.mode || "claude",
    runtime: outcome.runtime_target || "",
    goal: outcome.configured ? outcome.goal || "" : null,
    open: Number(queue.open || 0),
    eligible: Number(queue.eligible || 0),
    held: Number(queue.held || 0),
    blocked: Number(queue.blocked_by_dependency || 0),
    running: Number(project.running || 0),
    prOpen: Number(project.pr_open || 0),
    failed: Number(project.failed || 0),
    sessions: Number(project.sessions || 0),
    needsAttention: Number(project.needs_attention || 0),
    operatorState: project.operator_state || {},
    outcome,
    queueSnapshot: queue,
    freshness: project.freshness || {},
    supervisor: project.supervisor || {},
    error: project.error || "",
    readOnly: project.read_only === true,
    paused: project.paused === true,
    maxParallel: Number(project.max_parallel || 0),
    state: mapProjectState(project),
    summaryLine: projectSummaryLine(project),
    tapeEvents: deriveTapeEvents(project, workers, now),
    projectBoard: mapProjectBoard(project.project_board),
    costObservability: mapCostObservability(project.cost_observability),
    effectiveConfig: mapEffectiveConfig(project.effective_config),
    projectId: String(project.project_id || ""),
    managementHome: managementHomeView(project.management_home),
    raw: project,
  };
}

function mapEffectiveConfig(raw) {
  if (!raw || typeof raw !== "object") return null;
  const policy = raw.model_policy || {};
  const labels = raw.labels || {};
  const retention = raw.retention || {};
  const costCaps = raw.cost_caps || {};
  const gate = raw.supervisor_gate || {};
  return {
    modelPolicy: {
      default: String(policy.default || ""),
      fallbackBackends: Array.isArray(policy.fallback_backends) ? policy.fallback_backends.map(String) : [],
      backends: Array.isArray(policy.backends) ? policy.backends.map(mapEffectiveBackend) : [],
      routing: {
        mode: String((policy.routing || {}).mode || ""),
        routerModel: String((policy.routing || {}).router_model || ""),
        routerModelName: String((policy.routing || {}).router_model_name || ""),
        allowMeteredBackend: (policy.routing || {}).allow_metered_backend === true,
      },
    },
    maxParallel: Number(raw.max_parallel || 0),
    reviewGate: String(raw.review_gate || ""),
    labels: {
      issue: arrayOfString(labels.issue),
      exclude: arrayOfString(labels.exclude),
      ready: String(labels.ready || ""),
      blocked: String(labels.blocked || ""),
      supervisorExcluded: arrayOfString(labels.supervisor_excluded),
      allowIssueTypes: arrayOfString(labels.allow_issue_types),
      mission: arrayOfString(labels.mission),
      completionRequired: arrayOfString(labels.completion_required),
      verification: String(labels.verification || ""),
    },
    retention: {
      enabled: retention.enabled === true,
      keepLast: Number(retention.keep_last || 0),
      minAge: String(retention.min_age || ""),
      archiveEnabled: retention.archive_enabled === true,
      archiveFilePresent: retention.archive_file_present === true,
    },
    costCaps: {
      workerMaxTokens: Number(costCaps.worker_max_tokens || 0),
      workerSoftTokenThreshold: costCaps.worker_soft_token_threshold == null ? null : Number(costCaps.worker_soft_token_threshold),
      backendPricingConfigured: Number(costCaps.backend_pricing_configured || 0),
      backendPricingTotal: Number(costCaps.backend_pricing_total || 0),
    },
    supervisorGate: {
      mode: String(gate.mode || ""),
      dryRun: gate.dry_run === true,
      safeActions: arrayOfString(gate.safe_actions),
      approvalRequired: arrayOfString(gate.approval_required),
      allowedActions: arrayOfString(gate.allowed_actions),
      approvalRequiredActions: arrayOfString(gate.approval_required_actions),
      completionGatesActive: gate.completion_gates_active === true,
      handoffPlannerActive: gate.handoff_planner_active === true,
      reviewRepairActive: gate.review_repair_active === true,
      reviewRepairBackend: String(gate.review_repair_backend || ""),
      reviewRepairMaxRetries: Number(gate.review_repair_max_retries || 0),
      allowMeteredBackend: gate.allow_metered_backend === true,
      meteredBackendRefused: gate.metered_backend_refused === true,
      meteredBackend: String(gate.metered_backend || ""),
    },
    approvalAction: String(raw.approval_action || "change_global_config"),
    settings: Array.isArray(raw.settings) ? raw.settings.map(mapSettingSource) : [],
  };
}

// mapSettingSource maps one fleet-controllable cost/LLM knob (#839): its
// effective value and the layer it came from (builtin/fleet/project). isDefault
// drives the "non-default override" highlight in the Settings panel.
function mapSettingSource(raw) {
  return {
    key: String(raw?.key || ""),
    value: String(raw?.value == null ? "" : raw.value),
    source: String(raw?.source || "builtin"),
    isDefault: raw?.is_default === true,
  };
}

function mapEffectiveBackend(raw) {
  return {
    name: String(raw?.name || ""),
    enabled: raw?.enabled !== false,
    provider: String(raw?.provider || ""),
    model: String(raw?.model || ""),
    variant: String(raw?.variant || ""),
    effort: String(raw?.effort || ""),
    promptMode: String(raw?.prompt_mode || ""),
    nonAgentic: raw?.non_agentic === true,
    priceConfigured: raw?.price_configured === true,
    inputUSDPerMtok: Number(raw?.input_usd_per_mtok || 0),
    outputUSDPerMtok: Number(raw?.output_usd_per_mtok || 0),
    pricingClass: String(raw?.pricing_class || ""),
    metered: raw?.metered === true,
  };
}

function arrayOfString(v) {
  return Array.isArray(v) ? v.map(String).filter(Boolean) : [];
}

// mapProjectBoard normalizes the optional /api/v1/fleet `project_board` payload
// for the SPA. Returns null when the project has no GitHub Project board
// surfaced — callers use that as a "hide widget" signal.
export function mapProjectBoard(raw) {
  if (!raw || typeof raw !== "object") return null;
  const columns = Array.isArray(raw.columns)
    ? raw.columns.map(c => ({
      name: String(c?.name || ""),
      optionId: String(c?.option_id || ""),
      count: Number(c?.count || 0),
    }))
    : [];
  return {
    number: Number(raw.number || 0),
    url: raw.url || "",
    owner: raw.owner || "",
    ownerType: raw.owner_type || "",
    columns,
    totalItems: Number(raw.total_items || 0),
    fetchedAt: raw.fetched_at || "",
    error: raw.error || "",
  };
}

// projectBoardIssueURL deep-links to the project board with a #N filter so
// the corresponding card surfaces on click-through from a session card.
// Returns "" when the board is not surfaced or the issue number is missing.
export function projectBoardIssueURL(board, issueNumber) {
  if (!board || !board.url) return "";
  const n = Number(issueNumber || 0);
  if (n <= 0) return board.url;
  const sep = board.url.includes("?") ? "&" : "?";
  return `${board.url}${sep}pane=info&filterQuery=${encodeURIComponent("#" + n)}`;
}

export function mapProjectState(project) {
  if (!(project.outcome || {}).configured) {
    return { state: "idle", label: "unconfigured" };
  }
  if (project.error) {
    return { state: "unknown", label: "error" };
  }
  if (project.freshness?.stale) {
    return { state: "unknown", label: "stale" };
  }
  const op = project.operator_state || {};
  const label = op.label || "idle";
  const kind = op.kind || "idle";

  if (Number(project.running || 0) > 0 || kind === "working") {
    return { state: "live", label, count: project.running };
  }
  // #683: an intentional operator pause renders as a calm policy-toned
  // "paused" state — never dead/missing. The server only emits kind=paused
  // once no in-flight work remains, so a still-running worker keeps its
  // live/watch state (it finishes normally); the dedicated paused badge
  // shows alongside either way.
  if (kind === "paused") {
    return { state: "policy", label };
  }
  // #598: auto_merging is convergence-bound and calm — render alongside
  // monitoring_pr (watch tone) rather than stuck/red.
  if (kind === "auto_merging") {
    return { state: "watch", label, pr: op.pr_number ? { num: op.pr_number, label } : undefined };
  }
  if (
    kind === "stale_worker" ||
    kind === "dispatch_failure" ||
    Number(project.needs_attention || 0) > Number(project.self_resolving || 0)
  ) {
    return { state: "stuck", label };
  }
  if (kind === "monitoring_pr") {
    return { state: "watch", label, pr: op.pr_number ? { num: op.pr_number, label } : undefined };
  }
  if (kind === "no_eligible_issues" || kind === "queue_blocked" || kind === "idle") {
    return { state: "policy", label };
  }
  if (kind === "outcome_drift" || kind === "outcome_missing") {
    return { state: "stuck", label };
  }
  return { state: "ok", label };
}

function projectSummaryLine(project) {
  const op = project.operator_state || {};
  if (!(project.outcome || {}).configured) {
    return "No outcome configured";
  }
  if (project.error) return project.error;
  if (project.freshness?.stale) {
    return project.freshness.reason || "Snapshot stale";
  }
  if (op.summary) return op.summary;
  const queue = project.queue_snapshot || {};
  if (queue.idle_reason) return queue.idle_reason;
  if (Number(project.running || 0) > 0) {
    return `${project.running} worker${project.running === 1 ? "" : "s"} active`;
  }
  return op.next_action || "";
}

function mapWorker(worker) {
  const taxonomy = workerStatusTaxonomy(worker);
  const status = taxonomy.label;
  return {
    ...worker,
    project: worker.project_name || "",
    issue: {
      num: worker.issue_number || 0,
      title: worker.issue_title || "",
      url: worker.issue_url || "",
    },
    rawStatus: worker.status || "",
    displayStatus: worker.display_status || "",
    status,
    tone: taxonomy.tone,
    section: taxonomy.section,
    age: parseTimestamp(worker.started_at) || Date.now(),
    summary: worker.next_action || worker.status_reason || status.replace(/_/g, " "),
    pr: worker.pr_number || null,
    branch: worker.branch || "",
    live: worker.live === true,
    done: worker.status === "done",
    stuck: taxonomy.section === "stuck",
    stuckReason: taxonomy.section === "stuck" ? worker.status_reason || status : "",
  };
}

// workerStatusTaxonomy is the single source of truth for how a worker's raw
// `status` (and optional `display_status`) maps to a pill label, pill tone,
// and the section it should render under in the Workers screen.
//
// Sections:
//   - "running": actively in flight (running, queued, review_retry_*)
//   - "recent":  not running but live in the last 24h (pr_open, code_landed,
//                blocked-by-project, idle awaiting reconciliation)
//   - "stuck":   needs operator attention (dead, failed, conflict_failed,
//                retry_exhausted, backend_rate_limited, backend_auth_failure,
//                backend_model_unavailable)
//   - "done":    `status === "done"` only — true completion
//
// Pill tones map to CSS classes in mc.css:
//   ok=green, watch=amber, stuck=red, info=blue, policy=purple, idle=grey.
//
// Rules of thumb:
//   - The pill colour follows the *real* SessionStatus, not the server-side
//     project-blocked override. A `dead` worker must render red, never green.
//   - "blocked" (grey) only applies when the underlying session is not stuck,
//     i.e. the issue is genuinely waiting on a project-status block.
//   - Tests should pin: a `dead` session never ends up in section "done".
export function workerStatusTaxonomy(worker) {
  const status = String(worker.status || "");
  const display = String(worker.display_status || "");

  if (display.startsWith("review_retry_")) {
    return {
      label: display.replace(/_/g, " "),
      tone: display === "review_retry_recheck" ? "watch" : "info",
      section: "running",
    };
  }

  if (display === "backend_rate_limited") {
    return { label: "rate limited", tone: "watch", section: "stuck" };
  }

  // backend_auth_failure (#693): the worker died because its backend failed
  // authentication (credential outage), not because the work failed. The
  // retry budget is preserved; the operator needs to fix credentials. Must
  // not degrade to the generic red "dead" pill.
  if (display === "backend_auth_failure") {
    return { label: "auth failure", tone: "watch", section: "stuck" };
  }

  // backend_model_unavailable (#713): the worker died because its backend's
  // configured model is gone — pulled, renamed, or no longer accessible — not
  // because the work failed. The retry budget is preserved; the operator
  // swaps the model id (distinct remediation from an auth outage).
  if (display === "backend_model_unavailable") {
    return { label: "model unavailable", tone: "watch", section: "stuck" };
  }

  if (display === "blocked" && !isStuckStatus(status)) {
    return { label: "blocked", tone: "idle", section: "recent" };
  }

  switch (status) {
  case "running":
    return { label: "running", tone: "info", section: "running" };
  case "queued":
    return { label: "queued", tone: "info", section: "running" };
  case "pr_open":
    return { label: "pr_open", tone: "policy", section: "recent" };
  case "code_landed":
    return { label: "code_landed", tone: "ok", section: "recent" };
  case "done":
    return { label: "done", tone: "ok", section: "done" };
  case "failed":
  case "conflict_failed":
    return { label: status, tone: "stuck", section: "stuck" };
  case "dead":
    return { label: "dead", tone: "stuck", section: "stuck" };
  case "retry_exhausted":
    return { label: "retry_exhausted", tone: "watch", section: "stuck" };
  default:
    return { label: status || "—", tone: "idle", section: "recent" };
  }
}

function isStuckStatus(status) {
  return status === "dead"
    || status === "failed"
    || status === "conflict_failed"
    || status === "retry_exhausted";
}

// workerNextAction returns the operator-facing copy and the buttons the SPA
// should offer for a given worker, keyed off the real session status. This is
// the UI side of issue #540: the «open log» / «retry» / «reset budget» /
// «mark blocked» backend handlers may not all exist yet — buttons render with
// an action key so the drawer can wire them up as endpoints land.
export function workerNextAction(worker) {
  const status = String(worker.rawStatus || worker.status || "");
  const display = String(worker.displayStatus || worker.display_status || "");
  const fallback = worker.next_action || worker.status_reason || "";

  if (display === "backend_rate_limited") {
    const backend = String(worker.provider_limit_backend || worker.backend || "the backend");
    const resetAt = worker.next_retry_at || worker.rate_limit_reset_at || "";
    const tail = resetAt ? ` Auto-recovery at ${resetAt}.` : "";
    return {
      text: `Worker hit provider limit on ${backend}.${tail}`,
      buttons: [{ label: "Open backend health →", action: "openBackendHealth" }],
    };
  }

  if (display === "backend_auth_failure") {
    const backend = String(worker.provider_limit_backend || worker.backend || "the backend");
    return {
      text: `Backend ${backend} failed authentication. Fix credentials; the retry budget is preserved.`,
      buttons: [{ label: "Open backend health →", action: "openBackendHealth" }],
    };
  }

  if (display === "backend_model_unavailable") {
    const backend = String(worker.provider_limit_backend || worker.backend || "the backend");
    return {
      text: `Backend ${backend} cannot load its configured model (unavailable or no access). Swap the model id or restore access; the retry budget is preserved.`,
      buttons: [{ label: "Open backend health →", action: "openBackendHealth" }],
    };
  }

  if (display === "blocked" && !isStuckStatus(status)) {
    return {
      text: fallback || "Issue is blocked in GitHub Project. Resolve the project-status block before starting new work.",
      buttons: worker.issue_url ? [{ label: "Open issue →", href: worker.issue_url }] : [],
    };
  }

  switch (status) {
  case "dead":
    return {
      text: "Worker exited unexpectedly. Open log to see why.",
      buttons: [{ label: "Open log →", action: "openLog" }],
    };
  case "failed":
  case "conflict_failed":
    return {
      text: "Worker terminated with error. Retry or open log.",
      buttons: [
        { label: "Retry", action: "retry" },
        { label: "Open log →", action: "openLog" },
      ],
    };
  case "retry_exhausted":
    return {
      text: "Retry budget exhausted. Manual triage required.",
      buttons: [
        { label: "Reset budget", action: "resetBudget" },
        { label: "Mark issue blocked", action: "markBlocked" },
      ],
    };
  case "pr_open":
    return {
      text: fallback || "Waiting for CI, review, or the merge gate.",
      buttons: worker.pr_url ? [{ label: "Open PR →", href: worker.pr_url }] : [],
    };
  case "code_landed":
    return {
      text: fallback || "PR merged. Runtime verification still required before closing the issue.",
      buttons: worker.pr_url ? [{ label: "Open PR →", href: worker.pr_url }] : [],
    };
  case "done":
    return {
      text: fallback || worker.status_reason || "Issue complete.",
      buttons: worker.pr_url ? [{ label: "Open PR →", href: worker.pr_url }] : [],
    };
  default:
    return { text: fallback, buttons: [] };
  }
}

function mapApproval(approval) {
  const ageMin = Math.max(
    0,
    Math.floor(Number(approval.updated_age_seconds || approval.created_age_seconds || 0) / 60)
  );
  return {
    ...approval,
    project: approval.project_name || "",
    pr: approval.pr_number || 0,
    title: approval.summary || actionLabel(approval.action),
    author: approval.session || approval.decision_id || "supervisor",
    reviewer: approval.risk || "operator",
    ageMin,
    sla: 30,
    state: approvalTone(approval),
    suggestion: String(approval.action || "").trim() === "apply_lesson_proposal",
    body: approval.summary || "",
    stage: actionLabel(approval.action),
  };
}

export function isPendingApproval(approval) {
  return (approval.status || "") === "pending";
}

// isExecutionSkippedApproval is the SPA-side predicate for the post-#492
// "operationally honest skip" rendering: an approval the operator already
// approved, where the executor returned execution_skipped with a summary
// describing why no side effect ran. The audit row surfaces this distinctly
// from a true `executed` outcome (premortem failure mode #8).
export function isExecutionSkippedApproval(approval) {
  return (approval && approval.status) === "execution_skipped";
}

// manualFollowupForApproval returns the operator follow-up payload the SPA
// renders alongside an execution_skipped approval. For change_global_config
// (the YAML mutation pipeline that hasn't landed yet) the executor returns
// `execution_skipped` with a manual-edit summary; this helper builds the
// concrete shell command the operator still has to run so the audit row is
// operationally honest rather than reading like a fait accompli.
//
// `project` is the project name (fleet project slug). Returns null when the
// approval doesn't need a manual follow-up (most execution_skipped cases —
// merge_pr behind main, stop_worker no live session, etc. — are recoverable
// by the next supervisor cycle and don't need an operator command).
export function manualFollowupForApproval(approval) {
  if (!isExecutionSkippedApproval(approval)) return null;
  const action = String((approval && approval.action) || "").trim();
  if (action !== "change_global_config") return null;
  const project = slugifyProjectForCommand(
    (approval && (approval.project || approval.project_name)) || ""
  );
  const configName = project || "<project>";
  const serviceProject = project || "<project>";
  const command =
    `vim ~/.maestro/maestro.d/${configName}.yaml && ` +
    `systemctl --user restart maestro-supervisor-${serviceProject}.service`;
  return {
    headline: "Manual follow-up required",
    detail:
      "The supervisor recorded the intent but did not apply the change. " +
      "Edit the project's config file and restart the supervisor unit to land it.",
    command,
  };
}

// slugifyProjectForCommand normalises a project name into the lowercase,
// shell-safe slug used in config filenames and systemd unit names. Mirrors
// utils.slugifyProject but kept local so the helper has no surprise import
// cycle.
function slugifyProjectForCommand(name) {
  return String(name || "")
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

function approvalTone(approval) {
  if (String(approval.action || "").trim() === "apply_lesson_proposal") return "idle";
  if (approval.past_sla) return "stuck";
  if ((approval.status || "") === "pending") return "watch";
  return "idle";
}


export function mapVerdictUiTone(apiTone) {
  switch (String(apiTone || "").trim()) {
  case "healthy":
  case "busy":
    return "ok";
  case "attention":
    return "watch";
  case "daemon-down":
  case "error":
    return "stuck";
  default:
    return "ok";
  }
}

export function splitVerdict(sentence) {
  const text = String(sentence || "").trim();
  if (!text) return ["All quiet.", " Nothing needs you right now."];
  const dot = text.indexOf(". ");
  if (dot > 0 && dot < text.length - 2) {
    return [text.slice(0, dot + 1) + " ", text.slice(dot + 2)];
  }
  const firstSpace = text.indexOf(" ");
  if (firstSpace > 0 && firstSpace < 40) {
    return [text.slice(0, firstSpace), text.slice(firstSpace)];
  }
  return [text, ""];
}

function estimateHeartbeatBpm(summary, verdictTone) {
  if (verdictTone === "daemon-down") return 0;
  const running = Number(summary?.running || 0);
  if (running >= 2) return Math.min(90, 24 + running * 24);
  if (running === 1) return 45;
  if (Number(summary?.monitoring_pr || 0) > 0) return 28;
  if (verdictTone === "attention") return 42;
  return 18;
}

function latestSupervisorAge(projects) {
  let latest = null;
  for (const project of projects) {
    const ms = parseTimestamp(project.supervisor?.latest?.created_at);
    if (ms && (latest === null || ms > latest)) latest = ms;
  }
  return latest;
}

export function deriveTapeEvents(project, workers, now) {
  const windowMs = 60 * 60 * 1000;
  const events = [];
  const projectName = project.name;

  for (const worker of workers) {
    if (worker.project_name !== projectName && worker.project !== projectName) continue;
    const started = parseTimestamp(worker.started_at);
    if (!started) continue;
    const age = now - started;
    if (age > windowMs) continue;
    const x = 1 - age / windowMs;
    const runtimeSec = Number(worker.runtime_seconds || 300);
    const w = Math.min(0.28, Math.max(0.04, runtimeSec / windowMs));
    let kind = "run";
    if (worker.status === "pr_open" || worker.display_status === "pr_open") kind = "pr";
    else if (worker.needs_attention || worker.status === "failed") kind = "stuck";
    else if (worker.status === "done") kind = "merge";
    events.push({ kind, x: Math.max(0.02, Math.min(0.98, x)), w, live: worker.live === true });
  }

  const op = project.operator_state || {};
  if (Number(project.pr_open || 0) > 0 && op.kind === "monitoring_pr") {
    events.push({ kind: "pr", x: 0.82, w: 0.1 });
  }
  if (op.kind === "no_eligible_issues" || op.kind === "queue_blocked") {
    events.push({ kind: "held", x: 0.55, w: 0.18 });
  }
  return events.sort((a, b) => a.x - b.x);
}

export function workerSessionsFromFleet(fleet, now) {
  // Session-status taxonomy (issue #540):
  //   - running = `rawStatus === "running"` — actually executing right now.
  //   - recent  = live (24-h window from the server) AND not stuck. Includes
  //               pr_open, code_landed, blocked-by-project, etc. so the
  //               "in flight" group reflects active flow only.
  //   - stuck   = any session whose taxonomy section is "stuck" (dead, failed,
  //               conflict_failed, retry_exhausted, backend_rate_limited,
  //               backend_auth_failure, backend_model_unavailable). Both
  //               live and terminal stuck sessions land here — they never
  //               hide under DONE.
  //   - today   = `rawStatus === "done"` finished today (true completion).
  //   - older   = terminal finished within the 7-day audit window but not today.
  //
  // stuckToday / doneToday counters surface in the rollups so an operator can
  // tell "2 completed today" apart from "2 stuck today".
  const running = [];
  const recent = [];
  const stuck = [];
  const done = [];
  const startOfDay = new Date(now);
  startOfDay.setHours(0, 0, 0, 0);
  const dayStart = startOfDay.getTime();
  const sevenDayCutoff = dayStart - 6 * 24 * 60 * 60 * 1000;
  let olderCount = 0;
  let stuckToday = 0;

  for (const worker of fleet.workers || []) {
    const section = worker.section || workerStatusTaxonomy(worker).section;
    const finished = parseTimestamp(worker.finished_at) || worker.age;
    const rawStatus = worker.rawStatus || worker.status || "";

    if (rawStatus === "running") {
      running.push(worker);
    }

    if (section === "stuck") {
      stuck.push(worker);
      if (finished && finished >= dayStart) stuckToday += 1;
      continue;
    }

    if (worker.live) {
      recent.push(worker);
      continue;
    }

    if (!finished) continue;
    if (rawStatus === "done" && finished >= dayStart) {
      done.push(worker);
    } else if (finished >= sevenDayCutoff) {
      olderCount += 1;
    }
  }

  return {
    running,
    runningCount: running.length,
    recent,
    recentCount: recent.length,
    // Backward-compat alias: `live` === `recent`.
    live: recent,
    today: done,
    todayCount: done.length,
    stuck,
    stuckCount: stuck.length,
    stuckToday,
    olderCount,
  };
}

export function supervisorDecisionsFromProject(project, now) {
  const latest = project.supervisor?.latest;
  if (!latest) return [];
  const created = parseTimestamp(latest.created_at);
  if (!created) return [];
  return [{
    t: created,
    verb: latest.recommended_action || latest.status || "decision",
    note: latest.summary || latest.operator_sentence || "",
    conf: Number(latest.confidence || 0),
    warn: project.needsAttention > 0 || project.operatorState?.kind === "monitoring_pr",
  }];
}

export function actionLabel(action) {
  switch (String(action || "").trim()) {
  case "none": return "Skipped tick";
  case "monitor_open_pr": return "Watching PR";
  case "merge_pr": return "Merging PR";
  case "approve_merge": return "Ready to merge PR";
  case "skip_wave": return "Skipped tick";
  case "spawn_worker": return "Starting worker";
  case "label_issue_ready": return "Mark issue ready";
  case "add_issue_comment": return "Comment on issue";
  default:
    return String(action || "-").replace(/_/g, " ");
  }
}

// approvalSlotLabel renders the leftmost "slot" column on an approval card.
// The column conceptually holds the *worker slot* (session id like "01")
// when the approval is tied to a running worker — merge_pr, close_issue,
// delete_worktree, etc. For approvals minted BEFORE a worker is allocated
// (spawn_worker, label_issue_ready, add_issue_comment), target.session is
// empty; the legacy fallback rendered "#—" which reads as a missing
// value instead of "this is the action, not a session". Issue #537 / gap 6:
// show the action verb in that case so the column conveys *what* the
// approval will do.
//
// Priority: PR # → issue # → action verb. Session id is the conceptual
// identity but the existing UI shows PR/issue numbers operators are used
// to scanning, so those win when present; the action-verb fallback only
// fires when neither a PR, an issue, nor a session is bound.
export function approvalSlotLabel(approval) {
  const a = approval || {};
  const pr = Number(a.pr || a.pr_number || 0);
  if (pr > 0) return `#${pr}`;
  const issue = Number(a.issue_number || 0);
  if (issue > 0) return `#${issue}`;
  const session = typeof a.session === "string" ? a.session.trim() : "";
  if (session !== "") return session;
  return actionLabel(a.action);
}

// approvalCTA returns the primary-button label for an approval card. The
// label names the *effect* that will run when the operator approves, not a
// generic "Approve" — so for a merge_pr approval on PR #123 it reads
// "Merge PR #123", not "Approve". Issue #535 / spec gap 4.
export function approvalCTA(action, prNumber, issueNumber) {
  switch (String(action || "").trim()) {
  case "merge_pr":
    return prNumber ? `Merge PR #${prNumber}` : "Merge PR";
  case "close_issue":
    return issueNumber ? `Close issue #${issueNumber}` : "Close issue";
  case "delete_worktree":
    return "Delete worktree";
  case "change_global_config":
    return "Apply config change";
  case "spawn_worker":
    return "Start worker";
  case "label_issue_ready":
    return "Mark ready";
  default:
    return "Approve";
  }
}

// approvalRejectLabel returns the secondary-button label for an approval
// card. Mirrors approvalCTA — "Don't merge" / "Don't close" reads more
// honestly than a generic "Reject" once the primary CTA is verb-specific.
export function approvalRejectLabel(action) {
  switch (String(action || "").trim()) {
  case "merge_pr":
    return "Don't merge";
  case "close_issue":
    return "Don't close";
  case "delete_worktree":
    return "Keep worktree";
  case "change_global_config":
    return "Reject change";
  default:
    return "Reject";
  }
}

// approvalReasonPlaceholder returns the textarea placeholder shown in the
// approve/reject confirmation modal. The hint differs by verb so the
// operator has a sensible example for the action they are about to take.
export function approvalReasonPlaceholder(action, verb) {
  const v = String(verb || "approve");
  const a = String(action || "").trim();
  if (v === "approve") {
    switch (a) {
    case "merge_pr":
      return "e.g. CI green, manual smoke ok";
    case "close_issue":
      return "e.g. duplicate / not reproducible";
    case "delete_worktree":
      return "e.g. worker abandoned, cleaning up";
    case "change_global_config":
      return "e.g. rolling out new policy";
    default:
      return "e.g. CI green, manual smoke ok";
    }
  }
  switch (a) {
  case "merge_pr":
    return "e.g. failing test on review, needs another pass";
  case "close_issue":
    return "e.g. still reproducible, keep open";
  case "delete_worktree":
    return "e.g. still investigating, keep state";
  case "change_global_config":
    return "e.g. needs more eyes before rollout";
  default:
    return "e.g. failing on review item X";
  }
}

// isApprovalActionMergePR / isApprovalActionCloseIssue are tiny predicates
// used by the dashboard to pick the right card variant. Keeping them here
// (rather than inlining strings in JSX) makes the verb→label mapping easy
// to keep in sync with the supervisor's approval-required action set.
export function isApprovalActionMergePR(action) {
  return String(action || "").trim() === "merge_pr";
}

export function isApprovalActionCloseIssue(action) {
  return String(action || "").trim() === "close_issue";
}

export function formatRefreshAge(refreshedAt, now) {
  const ms = parseTimestamp(refreshedAt);
  if (!ms) return "—";
  return relTime(ms, now);
}

export function projectBySlug(fleet, slug) {
  return (fleet?.projects || []).find(p => p.slug === slug || p.name === slug) || null;
}

export function refreshFleetTapeEvents(fleet, now) {
  if (!fleet) return fleet;
  return {
    ...fleet,
    projects: (fleet.projects || []).map(project => ({
      ...project,
      tapeEvents: deriveTapeEvents(project.raw || project, fleet.workers || [], now),
    })),
  };
}

// postFleetApproval / postProjectApproval drive the new HTTP approve/reject
// endpoints (#476A). They return the updated approval JSON on success and
// throw an Error with the server-supplied message on any non-2xx, so the
// caller can show it inline in the dashboard.
export async function postFleetApproval({ approvalId, project, verb, actor, reason }) {
  if (!approvalId) throw new Error("approvalId is required");
  if (!project) throw new Error("project is required");
  if (verb !== "approve" && verb !== "reject") throw new Error("verb must be approve|reject");
  const url = `/api/v1/fleet/approvals/${encodeURIComponent(approvalId)}/${verb}?project=${encodeURIComponent(project)}`;
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ actor: actor || "dashboard", reason: reason || "" }),
  });
  return parseApprovalResponse(res, verb);
}

export async function postProjectApproval({ approvalId, verb, actor, reason }) {
  if (!approvalId) throw new Error("approvalId is required");
  if (verb !== "approve" && verb !== "reject") throw new Error("verb must be approve|reject");
  const url = `/api/v1/approvals/${encodeURIComponent(approvalId)}/${verb}`;
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ actor: actor || "dashboard", reason: reason || "" }),
  });
  return parseApprovalResponse(res, verb);
}

async function parseApprovalResponse(res, verb) {
  let payload = null;
  try {
    payload = await res.json();
  } catch (_) {
    // non-JSON body; fall through with payload=null
  }
  if (!res.ok) {
    const msg =
      (payload && (payload.error || payload.message)) ||
      `${verb} request failed with status ${res.status}`;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return payload;
}

// postFleetAction drives the fleet snapshot's per-worker / per-project
// action buttons (#567): mark_issue_ready, mark_issue_blocked,
// restart_worker, stop_worker, approve_merge. The server translates UI
// verbs to the underlying safe-action / cautious-gate verb and either
// executes synchronously (200 for safe) or enqueues a pending Approval
// (202 for cautious-gate). The caller surfaces the resolved
// action_id / approval_id and any server-supplied error message.
export async function postFleetAction({
  actionId,
  project,
  slot,
  issueNumber,
  prNumber,
  actor,
  reason,
}) {
  if (!actionId) throw new Error("actionId is required");
  const body = { action_id: actionId };
  if (project) body.project = project;
  if (slot) body.slot = slot;
  if (issueNumber) body.issue_number = Number(issueNumber);
  if (prNumber) body.pr_number = Number(prNumber);
  if (actor) body.actor = actor;
  if (reason) body.reason = reason;
  const res = await fetch("/api/v1/fleet/actions", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  let payload = null;
  try {
    payload = await res.json();
  } catch (_) {
    /* non-JSON body */
  }
  if (!res.ok) {
    const msg =
      (payload && (payload.error || payload.message)) ||
      `action ${actionId} failed with status ${res.status}`;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return payload;
}

// mapEmergency normalizes the fleet-wide EMERGENCY STOP block (#840). It is
// always present in the wire format (level "none" when inactive), but tolerate a
// missing block so a legacy server simply reports "not active".
export function mapEmergency(raw) {
  if (!raw || typeof raw !== "object") {
    return { active: false, level: "none", since: "", actor: "", reason: "" };
  }
  return {
    active: raw.active === true,
    level: raw.level || "none",
    since: raw.since || "",
    actor: raw.actor || "",
    reason: raw.reason || "",
  };
}

// postFleetEmergency engages or clears the fleet-wide EMERGENCY STOP switch
// (#840). level is "llm_stopped" | "all_stopped" (engage) or "none" (resume).
// Returns the new switch state; throws on a non-2xx with the server message.
export async function postFleetEmergency({ level, actor, reason }) {
  if (!level) throw new Error("level is required");
  const body = { level };
  if (actor) body.actor = actor;
  if (reason) body.reason = reason;
  const res = await fetch("/api/v1/fleet/emergency", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  let payload = null;
  try {
    payload = await res.json();
  } catch (_) {
    /* non-JSON body */
  }
  if (!res.ok) {
    const msg =
      (payload && (payload.error || payload.message)) ||
      `emergency ${level} failed with status ${res.status}`;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return mapEmergency(payload);
}

// pickVerdictTuple returns the [headline, detail] pair the SPA hero
// renders. Prefers the server-built short form (verdict.headline +
// verdict.detail, issue #474); falls back to splitting the legacy
// run-on sentence so older servers keep working unchanged.
export function pickVerdictTuple(verdict, brief) {
  const h = verdict && typeof verdict.headline === "string" ? verdict.headline.trim() : "";
  const d = verdict && typeof verdict.detail === "string" ? verdict.detail.trim() : "";
  if (h) {
    return [h, d ? " " + d : ""];
  }
  return splitVerdict((verdict && verdict.sentence) || (brief && brief.sentence) || "");
}

// Command palette search (issue #345 / M-010).
//
// buildSearchIndex turns the already-loaded fleet payload into a flat list
// of palette-ready entries (one per project, project dashboard, worker
// session, issue, PR, approval, plus a handful of static pages and read-only
// actions). The index is pure: it does not call the network and exposes no
// write controls — read-only is enforced by giving every entry a navigation
// `to` (path or external URL) and nothing else. Callers (CommandPalette)
// then filter the index against the typed query with searchFleetItems.
//
// Each entry shape:
//   { id, kind, title, meta, to, external, project, slot, rank, terms }
//
// `kind` strings are the user-visible category labels in the result row.
// `external: true` means `to` is an absolute URL the palette should open
// in a new tab instead of pushing onto history.
function searchNormalize(value) {
  return String(value == null ? "" : value).toLowerCase();
}

function searchCompact(value) {
  return searchNormalize(value).replace(/[^a-z0-9]+/g, "");
}

function searchNumberAliases(label, number) {
  const value = Number(number || 0);
  if (!Number.isFinite(value) || value <= 0) return [];
  const text = String(value);
  const prefix = String(label || "").trim();
  return [text, "#" + text, prefix + " " + text, prefix + " #" + text, prefix + text];
}

function searchMetaText(parts) {
  return parts
    .map(value => String(value == null ? "" : value).trim())
    .filter(Boolean)
    .join(" · ");
}

function pushSearchItem(out, seen, item) {
  if (!item || !item.id || seen.has(item.id)) return;
  seen.add(item.id);
  const terms = Array.isArray(item.terms) ? item.terms : [];
  const blob = [item.kind, item.title, item.meta, item.project, item.slot, item.to]
    .concat(terms)
    .map(searchNormalize)
    .join(" ");
  out.push({
    id: item.id,
    kind: item.kind,
    title: item.title,
    meta: item.meta || "",
    to: item.to || "",
    external: item.external === true,
    project: item.project || "",
    slot: item.slot || "",
    rank: Number(item.rank || 0),
    searchText: blob,
    searchCompact: searchCompact(blob),
  });
}

export function buildSearchIndex(fleet) {
  const out = [];
  const seen = new Set();
  const projects = (fleet && fleet.projects) || [];
  const workers = (fleet && fleet.workers) || [];
  const approvals = (fleet && fleet.approvals) || [];

  // Static pages — keyboard-friendly jumps even when the fleet is empty.
  pushSearchItem(out, seen, {
    id: "page:fleet", kind: "Page", title: "Fleet overview",
    meta: "Jump to fleet", to: "/fleet", rank: 20,
    terms: ["fleet", "overview", "home"],
  });
  pushSearchItem(out, seen, {
    id: "page:workers", kind: "Page", title: "Workers",
    meta: "Jump to workers", to: "/workers", rank: 20,
    terms: ["workers", "sessions"],
  });
  pushSearchItem(out, seen, {
    id: "page:approvals", kind: "Page", title: "Approvals",
    meta: "Jump to approvals", to: "/approvals", rank: 20,
    terms: ["approvals", "pending"],
  });
  pushSearchItem(out, seen, {
    id: "page:settings", kind: "Page", title: "Settings",
    meta: "Jump to settings", to: "/settings", rank: 10,
    terms: ["settings", "config"],
  });

  for (const project of projects) {
    const slug = project.slug || project.name || "";
    if (!slug) continue;
    const stateLabel = (project.state && project.state.label) || "";
    pushSearchItem(out, seen, {
      id: "project:" + slug,
      kind: "Project",
      title: slug,
      meta: searchMetaText([project.repo, stateLabel, project.summaryLine]),
      to: "/project/" + encodeURIComponent(slug),
      project: slug,
      rank: 40,
      terms: ["project", "slug", project.repo, project.name],
    });
    if (project.dashboardUrl) {
      pushSearchItem(out, seen, {
        id: "dashboard:" + slug,
        kind: "Dashboard",
        title: slug + " dashboard",
        meta: searchMetaText([project.repo, project.dashboardUrl]),
        to: project.dashboardUrl,
        external: true,
        project: slug,
        rank: 35,
        terms: ["dashboard", "project dashboard", slug, project.repo],
      });
    }
  }

  for (const worker of workers) {
    const project = worker.project_name || worker.project || "";
    const slot = worker.slot || "";
    if (!project && !slot) continue;
    const issueNumber = (worker.issue && worker.issue.num) || worker.issue_number || 0;
    const issueTitle = (worker.issue && worker.issue.title) || worker.issue_title || "";
    const issueURL = (worker.issue && worker.issue.url) || worker.issue_url || "";
    const prNumber = worker.pr || worker.pr_number || 0;
    const prURL = worker.pr_url || "";
    const status = worker.status || worker.rawStatus || "";
    const issueAliases = searchNumberAliases("issue", issueNumber);
    const prAliases = searchNumberAliases("pr", prNumber);
    const rank = (worker.needs_attention ? 70 : 0) + (worker.live ? 55 : 20);
    const sessionParams = new URLSearchParams();
    if (project) sessionParams.set("project", project);
    if (slot) sessionParams.set("slot", slot);
    const sessionTo = "/workers" + (sessionParams.toString() ? "?" + sessionParams.toString() : "");

    pushSearchItem(out, seen, {
      id: "session:" + project + ":" + slot,
      kind: "Session",
      title: searchMetaText([project, slot]) || "Worker session",
      meta: searchMetaText([
        issueNumber ? "Issue #" + issueNumber : "",
        prNumber ? "PR #" + prNumber : "",
        status,
        worker.branch,
      ]),
      to: sessionTo,
      project,
      slot,
      rank,
      terms: ["worker", "session", "slot", issueTitle, worker.branch].concat(issueAliases, prAliases),
    });

    if (issueNumber) {
      pushSearchItem(out, seen, {
        id: "issue:" + project + ":" + issueNumber + ":" + slot,
        kind: "Issue",
        title: "Issue #" + issueNumber,
        meta: searchMetaText([project, slot, issueTitle]),
        to: issueURL || sessionTo,
        external: Boolean(issueURL),
        project,
        slot,
        rank: rank - 5,
        terms: [issueTitle, "issue"].concat(issueAliases),
      });
    }

    if (prNumber) {
      pushSearchItem(out, seen, {
        id: "pr:" + project + ":" + prNumber + ":" + slot,
        kind: "PR",
        title: "PR #" + prNumber,
        meta: searchMetaText([project, slot, issueNumber ? "Issue #" + issueNumber : ""]),
        to: prURL || sessionTo,
        external: Boolean(prURL),
        project,
        slot,
        rank: rank - 10,
        terms: [issueTitle, "pull request", "pr"].concat(prAliases),
      });
    }
  }

  for (const approval of approvals) {
    const project = approval.project_name || approval.project || "";
    const issueNumber = approval.issue_number || 0;
    const prNumber = approval.pr_number || approval.pr || 0;
    const session = approval.session || "";
    const targets = [];
    if (issueNumber) targets.push("Issue #" + issueNumber);
    if (prNumber) targets.push("PR #" + prNumber);
    if (session) targets.push("Session " + session);
    const externalURL = approval.pr_url || approval.issue_url || "";
    pushSearchItem(out, seen, {
      id: "approval:" + (approval.id || targets.join(":") || project),
      kind: "Approval",
      title: targets.length ? "Approval " + targets.join(" / ") : "Approval " + (approval.id || "target"),
      meta: searchMetaText([project, actionLabel(approval.action), approval.status, approval.summary]),
      to: externalURL || "/approvals",
      external: Boolean(externalURL),
      project,
      slot: session,
      rank: (approval.status || "") === "pending" ? 65 : 15,
      terms: [approval.id, approval.decision_id, approval.summary, approval.action]
        .concat(searchNumberAliases("issue", issueNumber), searchNumberAliases("pr", prNumber)),
    });
  }

  // Read-only action: theme toggle. We intentionally surface no write
  // actions through the palette in V1 — see acceptance criteria for #345.
  pushSearchItem(out, seen, {
    id: "action:theme",
    kind: "Action",
    title: "Toggle theme",
    meta: "Switch light / dark",
    to: "theme",
    rank: 5,
    terms: ["theme", "dark", "light"],
  });

  return out;
}

export function fuzzySearchMatch(haystack, needle) {
  if (!needle) return true;
  let index = 0;
  for (const ch of haystack) {
    if (ch === needle[index]) index++;
    if (index === needle.length) return true;
  }
  return false;
}

function scoreSearchItem(item, normalized, compact) {
  if (!normalized) return item.rank;
  if (item.searchText.includes(normalized)) return item.rank + 100;
  if (compact && item.searchCompact.includes(compact)) return item.rank + 60;
  if (compact && fuzzySearchMatch(item.searchCompact, compact)) return item.rank + 25;
  return -1;
}

export function searchFleetItems(fleet, query, limit) {
  const max = Number.isFinite(Number(limit)) && Number(limit) > 0 ? Number(limit) : 12;
  const index = buildSearchIndex(fleet);
  const normalized = searchNormalize(query);
  const compact = searchCompact(query);
  if (!normalized) {
    return index.slice().sort((a, b) => b.rank - a.rank).slice(0, max);
  }
  const scored = [];
  for (const item of index) {
    const score = scoreSearchItem(item, normalized, compact);
    if (score >= 0) scored.push({ item, score });
  }
  scored.sort((a, b) => {
    if (a.score !== b.score) return b.score - a.score;
    return a.item.title.localeCompare(b.item.title);
  });
  return scored.slice(0, max).map(entry => entry.item);
}
