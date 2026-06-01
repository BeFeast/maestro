import { parseTimestamp, relTime, slugifyProject } from "./utils.js";

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
  const attentionCount =
    Number(summary.needs_attention || 0) +
    Number(summary.approvals_pending || 0) +
    Number(summary.errors || 0) +
    Number(summary.stale || 0) +
    Number(summary.dispatch_failures || 0) +
    Number(summary.outcome_drift || 0) +
    Number(summary.no_eligible_issues || 0);

  return {
    raw,
    readOnly: raw.read_only === true,
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
    throughputMerged7d: Number(summary.throughput_merged_7d || 0),
    throughputDaily7d: Array.isArray(summary.throughput_daily_7d)
      ? summary.throughput_daily_7d.map(v => Number(v || 0))
      : [],
    lastDecisionAge: latestSupervisorAge(raw.projects || []),
  };
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
    maxParallel: Number(project.max_parallel || 0),
    state: mapProjectState(project),
    summaryLine: projectSummaryLine(project),
    tapeEvents: deriveTapeEvents(project, workers, now),
    raw: project,
  };
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
  if (
    kind === "stale_worker" ||
    kind === "dispatch_failure" ||
    Number(project.needs_attention || 0) > 0
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
  const status = worker.display_status || worker.status || "";
  return {
    ...worker,
    project: worker.project_name || "",
    issue: {
      num: worker.issue_number || 0,
      title: worker.issue_title || "",
      url: worker.issue_url || "",
    },
    status,
    tone: workerTone(worker),
    age: parseTimestamp(worker.started_at) || Date.now(),
    summary: worker.next_action || worker.status_reason || status.replace(/_/g, " "),
    pr: worker.pr_number || null,
    branch: worker.branch || "",
    live: worker.live === true,
    done: status === "done" || worker.status === "done",
    stuckReason: worker.needs_attention ? worker.status_reason || worker.status : "",
  };
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
    body: approval.summary || "",
    stage: actionLabel(approval.action),
  };
}

export function isPendingApproval(approval) {
  return (approval.status || "") === "pending";
}

function approvalTone(approval) {
  if (approval.past_sla) return "stuck";
  if ((approval.status || "") === "pending") return "watch";
  return "idle";
}

function workerTone(worker) {
  if (worker.needs_attention || worker.status === "retry_exhausted" || worker.status === "failed") {
    return "stuck";
  }
  if (worker.status === "pr_open" || worker.display_status === "pr_open") return "watch";
  if (worker.live) return "ok";
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
  // Three distinct concepts (issue #496):
  //   - running  = `worker.status === "running"` — actually executing right now.
  //   - recent   = `worker.live` (24-h activity window from the server).
  //                Includes terminal sessions that finished in the last 24 h.
  //   - today    = terminal worker that finished today.
  //   - older    = terminal worker finished within the last 7 days but not today.
  //
  // The "N workers in flight" hero MUST read `running`, not `recent`.
  // `live` is kept as an alias of `recent` for backward compatibility
  // with older callers / future-removed; new code should use `running`
  // or `recent` explicitly.
  const running = [];
  const recent = [];
  const today = [];
  const startOfDay = new Date(now);
  startOfDay.setHours(0, 0, 0, 0);
  const dayStart = startOfDay.getTime();
  // Real 7-day cutoff: today + the previous 6 days. olderCount counts
  // only sessions finished within that window but before today (#473).
  const sevenDayCutoff = dayStart - 6 * 24 * 60 * 60 * 1000;
  let olderCount = 0;

  for (const worker of fleet.workers || []) {
    if (worker.status === "running") {
      running.push(worker);
    }
    if (worker.live) {
      recent.push(worker);
      continue;
    }
    const finished = parseTimestamp(worker.finished_at) || worker.age;
    if (!finished) continue;
    if (finished >= dayStart) {
      today.push(worker);
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
    today,
    todayCount: today.length,
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
  default:
    return String(action || "-").replace(/_/g, " ");
  }
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
