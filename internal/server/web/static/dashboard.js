const state = {
  workers: [],
  supervisor: null,
  outcome: null,
  selected: "",
  filter: "",
  lastLog: null,
  readOnly: true
};

const statusRank = {
  review_retry_running: 0,
  running: 0,
  review_retry_recheck: 1,
  pr_open: 1,
  review_retry_pending: 2,
  review_retry_backoff: 2,
  queued: 2,
  dead: 3,
  failed: 4,
  conflict_failed: 5,
  retry_exhausted: 6,
  done: 7
};

const repoEl = document.getElementById("repo");
const statsEl = document.getElementById("stats");
const workersEl = document.getElementById("workers");
const filterEl = document.getElementById("filter");
const logEl = document.getElementById("log");
const logTitleEl = document.getElementById("log-title");
const logMetaEl = document.getElementById("log-meta");
const statusNoteEl = document.getElementById("status-note");
const supervisorPanelEl = document.getElementById("supervisor-panel");
const outcomePanelEl = document.getElementById("outcome-panel");

repoEl.textContent = window.MAESTRO_REPO || "";

filterEl.addEventListener("input", () => {
  state.filter = filterEl.value.toLowerCase();
  renderWorkers();
});

function escapeText(value) {
  return String(value ?? "").replace(/[&<>"']/g, ch => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "\"": "&quot;",
    "'": "&#39;"
  }[ch]));
}

function compactNumber(value) {
  const n = Number(value || 0);
  if (!n) return "-";
  if (n < 1000) return String(n);
  if (n < 1000000) return (n / 1000).toFixed(n < 10000 ? 1 : 0).replace(/\.0$/, "") + "k";
  return (n / 1000000).toFixed(1).replace(/\.0$/, "") + "M";
}

function linkHTML(url, label) {
  if (!url) return escapeText(label);
  return '<a href="' + escapeText(url) + '" target="_blank" rel="noreferrer">' + escapeText(label) + '</a>';
}

function issueSummaryHTML(worker) {
  const issue = worker.issue_number ? "#" + worker.issue_number : "-";
  return '<span class="issue-main">' + linkHTML(worker.issue_url, issue) +
    '<span class="issue-title">' + escapeText(worker.issue_title || "") + '</span></span>';
}

function issueSummaryText(worker) {
  const issue = worker.issue_number ? "#" + worker.issue_number : "-";
  return (issue + " " + (worker.issue_title || "")).trim();
}

function actionLabel(action) {
  switch (String(action || "").trim()) {
  case "none": return "Skip tick";
  case "monitor_open_pr": return "Watch PR";
  case "approve_merge": return "Merge PR";
  case "spawn_worker": return "Start worker";
  case "label_issue_ready": return "Mark issue ready";
  case "review_retry_exhausted": return "Review retry-exhausted work";
  case "check_outcome_health": return "Check runtime health";
  case "wait_for_running_worker":
  case "wait_for_worker": return "Wait for worker";
  case "wait_for_capacity": return "Wait for free slot";
  case "wait_for_ordered_queue": return "Wait for queue order";
  default:
    return String(action || "-").replace(/_/g, " ");
  }
}

function rawSupervisorAction(action) {
  const raw = String(action || "").trim();
  return raw || "none";
}

function supervisorDecisionMetaText(item) {
  const risk = rawSupervisorAction(item && item.risk);
  const confidence = Number(item && item.confidence);
  const confidencePct = Number.isFinite(confidence) && confidence > 0
    ? Math.round(confidence * 100) + "%"
    : "";
  switch (risk) {
  case "safe":
    return confidencePct ? "Maestro is confident this is safe (" + confidencePct + ")." : "Maestro marked this as safe.";
  case "mutating":
    return confidencePct ? "Maestro expects a mutating step here (" + confidencePct + " confidence)." : "Maestro expects a mutating step here.";
  case "approval_gated":
    return confidencePct ? "Maestro expects an approval-required step here (" + confidencePct + " confidence)." : "Maestro expects an approval-required step here.";
  default:
    return confidencePct ? "Confidence " + confidencePct + "." : "";
  }
}

function supervisorOperatorSentence(item) {
  if (item && item.operator_sentence) return item.operator_sentence;
  const raw = rawSupervisorAction(item && (item.recommended_action || item.action));
  if (raw === "none") return "Skipped this tick because no safe action was available.";
  const summary = String(item && item.summary || "").trim();
  if (summary) return "Supervisor chose " + raw + ". " + summary;
  return "Supervisor chose " + raw + ". Inspect diagnostics for details.";
}

function actionDisabledReason(actions) {
  const action = (actions || []).find(item => item.disabled_reason);
  return action ? action.disabled_reason : "Write actions require approval-backed configuration.";
}

function actionTargetText(action) {
  const parts = [];
  if (action.target) parts.push(action.target);
  if (action.issue_number) parts.push("issue #" + action.issue_number);
  if (action.pr_number) parts.push("PR #" + action.pr_number);
  return parts.length ? parts.join(" · ") : "project";
}

function actionPolicyText(action) {
  if (action.approval_policy) return actionLabel(action.approval_policy);
  return action.requires_approval ? "manual approval required" : "none";
}

function actionDetailHTML(action) {
  const description = action.description ? '<div><strong>Would</strong> ' + escapeText(action.description.replace(/^Would\s+/i, "")) + '</div>' : "";
  return '<div class="action-detail">' + description +
    '<div><strong>Scope</strong> ' + escapeText(actionLabel(action.scope || "unknown")) + '</div>' +
    '<div><strong>Target</strong> ' + escapeText(actionTargetText(action)) + '</div>' +
    '<div><strong>Approval</strong> ' + escapeText(actionPolicyText(action)) + '</div>' +
    '<div><strong>Disabled</strong> ' + escapeText(action.disabled_reason || "Write action unavailable") + '</div>' +
    '</div>';
}

function renderActionButtons(actions, showDetails) {
  const items = actions || [];
  if (!items.length) return "";
  return '<div class="action-list">' + items.map(action =>
    '<div class="action-item"><button type="button" class="action-btn" disabled aria-disabled="true" title="' +
    escapeText(action.disabled_reason || "Write action unavailable") + '">' +
    escapeText(action.label || actionLabel(action.id)) + '</button>' +
    (showDetails ? actionDetailHTML(action) : "") + '</div>'
  ).join("") + '</div>';
}

function renderWorkerActions(actions) {
  if (state.readOnly) {
    return '<div class="project-actions-readonly">Write controls disabled in read-only mode.</div>';
  }
  if (!actions || !actions.length) return "";
  return '<div class="worker-actions"><span>Actions</span>' + renderActionButtons(actions, false) + '</div>' +
    '<div class="action-note">' + escapeText(actionDisabledReason(actions)) + '</div>';
}

function formatTimestamp(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  let relative = seconds + "s ago";
  if (seconds >= 86400) relative = Math.floor(seconds / 86400) + "d ago";
  else if (seconds >= 3600) relative = Math.floor(seconds / 3600) + "h ago";
  else if (seconds >= 60) relative = Math.floor(seconds / 60) + "m ago";
  return date.toLocaleString() + " (" + relative + ")";
}

function targetLinksHTML(links) {
  return (links || []).map(link => linkHTML(link.url, link.label)).join("");
}

function queueText(queue) {
  if (!queue || !queue.enabled) return "";
  const label = queue.label || "Queue";
  if (queue.position && queue.total) return label + ": " + queue.position + " of " + queue.total;
  if (queue.total) return label + ": " + queue.total + " item" + (queue.total === 1 ? "" : "s");
  return label + ": empty";
}

function displayStatus(worker) {
  return worker.display_status || worker.status || "-";
}

function statusLabel(worker) {
  if (worker.status === "running" && worker.alive === false) return "running stale";
  return displayStatus(worker);
}

function pillClass(worker) {
  const base = "pill s-" + escapeText(displayStatus(worker) || "unknown");
  if (worker.needs_attention || (worker.status === "running" && worker.alive === false)) {
    return base + " pill-attention";
  }
  return base;
}

function workerMatches(worker) {
  if (!state.filter) return true;
  const text = [worker.slot, worker.issue_number, worker.issue_title, worker.status, displayStatus(worker), worker.backend, worker.pr_number]
    .join(" ")
    .toLowerCase();
  return text.includes(state.filter);
}

function sortWorkers(workers) {
  return [...workers].sort((a, b) => {
    const ar = statusRank[displayStatus(a)] ?? 99;
    const br = statusRank[displayStatus(b)] ?? 99;
    if (ar !== br) return ar - br;
    return String(b.started_at || "").localeCompare(String(a.started_at || ""));
  });
}

function renderStats(summary, total, maxParallel, readOnly) {
  const running = (summary.running || 0) + (summary.review_retry_running || 0);
  const prOpen = (summary.pr_open || 0) + (summary.review_retry_recheck || 0);
  const failed = (summary.dead || 0) + (summary.failed || 0) + (summary.retry_exhausted || 0) + (summary.conflict_failed || 0);
  const items = [
    ["Running", running + " / " + maxParallel],
    ["PR open", prOpen],
    ["Failed", failed],
    ["Sessions", total],
    ["Mode", readOnly ? "Read-only" : "Control"]
  ];
  statsEl.innerHTML = items.map(([label, value]) =>
    '<div class="stat"><strong>' + escapeText(value) + '</strong><span>' + escapeText(label) + '</span></div>'
  ).join("");
}

function renderSupervisor(info) {
  if (!info || !info.has_run || !info.latest) {
    const empty = info && info.empty_state ? info.empty_state : "No Supervisor has run yet.";
    supervisorPanelEl.innerHTML = '<div class="supervisor-head">' +
      '<span class="supervisor-title">Supervisor</span>' +
      '<span class="supervisor-time">empty</span>' +
      '</div>' +
      '<div class="supervisor-empty">' + escapeText(empty) + '</div>';
    return;
  }

  const latest = info.latest;
  const links = targetLinksHTML(latest.target_links);
  const queue = queueText(latest.queue);
  const meta = [
    supervisorDecisionMetaText(latest),
    queue
  ].filter(Boolean);
  const reasons = (latest.stuck_reasons && latest.stuck_reasons.length ? latest.stuck_reasons : latest.reasons || []).slice(0, 3);
  const reasonHTML = reasons.length ? '<ul class="supervisor-reasons">' + reasons.map(reason =>
    '<li>' + escapeText(reason) + '</li>'
  ).join("") + '</ul>' : "";
  const lastSafeRaw = info.last_safe_action ? rawSupervisorAction(info.last_safe_action.action) : "";
  const lastSafe = info.last_safe_action ? '<div class="supervisor-meta">' +
    '<span title="Raw action: ' + escapeText(lastSafeRaw) + '">Last safe action: ' + escapeText(supervisorOperatorSentence(info.last_safe_action)) + '</span>' +
    '<span>' + escapeText(formatTimestamp(info.last_safe_action.created_at)) + '</span>' +
    '</div>' : "";
  const approvals = (info.approval_actions || []).length ? '<div class="supervisor-actions">' +
    '<span>Requires approval:</span>' +
    (info.approval_actions || []).map(action =>
      '<button class="supervisor-approval" disabled title="Raw action: ' + escapeText(rawSupervisorAction(action.action)) + ' · ' + escapeText(action.disabled_reason || "Controls not available yet") + '">' +
      escapeText(supervisorOperatorSentence(action)) +
      '</button>'
    ).join("") +
    '</div>' : "";

  const rawAction = rawSupervisorAction(latest.recommended_action);
  const operatorSentence = supervisorOperatorSentence(latest);

  supervisorPanelEl.innerHTML = '<div class="supervisor-head">' +
    '<span class="supervisor-title">Supervisor</span>' +
    '<span class="supervisor-time">' + escapeText(formatTimestamp(latest.created_at)) + '</span>' +
    '</div>' +
    '<div class="supervisor-main">' +
    '<span class="supervisor-action" title="Raw action: ' + escapeText(rawAction) + '">' + escapeText(operatorSentence) + '</span>' +
    (links ? '<span class="supervisor-links">' + links + '</span>' : "") +
    '</div>' +
    (latest.summary ? '<div class="supervisor-summary">' + escapeText(latest.summary) + '</div>' : "") +
    (meta.length ? '<div class="supervisor-meta">' + meta.map(item => '<span>' + escapeText(item) + '</span>').join("") + '</div>' : "") +
    reasonHTML + lastSafe + approvals;
}

function renderOutcome(outcome) {
  const o = outcome || {};
  const configured = o.configured === true;
  const goal = configured ? (o.goal || "Configured outcome") : "No outcome brief configured";
  const target = o.runtime_target || "-";
  const health = o.health_state || (configured ? "unknown" : "not_configured");
  const next = o.next_action || (configured ? "Verify runtime health." : "Add an outcome brief to config.");
  const host = o.runtime_host ? " · " + o.runtime_host : "";
  const checked = o.health_checked_at ? formatTimestamp(o.health_checked_at) : "-";
  const summary = o.health_summary || "";
  outcomePanelEl.innerHTML = '<div class="supervisor-head">' +
    '<span class="supervisor-title">Outcome</span>' +
    '<span class="supervisor-time">' + escapeText(health.replace(/_/g, " ")) + '</span>' +
    '</div>' +
    '<div class="outcome-grid">' +
      '<div class="outcome-line" title="' + escapeText(goal) + '"><strong>Goal</strong> ' + escapeText(goal) + '</div>' +
      '<div class="outcome-line" title="' + escapeText(target + host) + '"><strong>Runtime</strong> ' + escapeText(target + host) + '</div>' +
      '<div class="outcome-line" title="' + escapeText(next) + '"><strong>Next</strong> ' + escapeText(next) + '</div>' +
      '<div class="outcome-line"><strong>Health</strong> ' + escapeText(health.replace(/_/g, " ")) + '</div>' +
      '<div class="outcome-line"><strong>Checked</strong> ' + escapeText(checked) + '</div>' +
      (summary ? '<div class="outcome-line" title="' + escapeText(summary) + '"><strong>Signal</strong> ' + escapeText(summary) + '</div>' : "") +
    '</div>';
}

function renderWorkers() {
  const visible = sortWorkers(state.workers).filter(workerMatches);
  if (visible.length === 0) {
    workersEl.innerHTML = '<tr><td colspan="7" class="empty">No workers.</td></tr>';
    return;
  }
  workersEl.innerHTML = visible.map(worker => {
    const selected = worker.slot === state.selected ? " selected" : "";
    const attention = worker.needs_attention ? " row-attention" : "";
    const pr = worker.pr_number ? "#" + worker.pr_number : "-";
    return '<tr class="' + selected + attention + '" data-slot="' + escapeText(worker.slot) + '">' +
      '<td class="slot">' + escapeText(worker.slot) + '</td>' +
      '<td class="issue" title="' + escapeText(issueSummaryText(worker)) + '">' + issueSummaryHTML(worker) + '</td>' +
      '<td class="status"><span class="' + pillClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td class="backend">' + escapeText(worker.backend || "-") + '</td>' +
      '<td class="pr">' + linkHTML(worker.pr_url, pr) + '</td>' +
      '<td class="runtime">' + escapeText(worker.runtime || "-") + '</td>' +
      '<td class="tokens">' + compactNumber(worker.tokens_used_total) + '</td>' +
    '</tr>';
  }).join("");
  workersEl.querySelectorAll("tr[data-slot]").forEach(row => {
    row.addEventListener("click", () => selectWorker(row.dataset.slot));
  });
}

function selectWorker(slot) {
  state.selected = slot;
  state.lastLog = null;
  renderWorkers();
  renderSelectedDetails();
  loadLog();
}

function emptyLogText(worker) {
  if (!worker) return "No log output yet.";
  if (worker.status === "running" && worker.backend === "claude") {
    return "No log output yet. Claude print mode may stay quiet until it finishes.";
  }
  if (worker.status === "running") return "No log output yet. Worker is still running.";
  return "No log output.";
}

function renderSelectedDetails() {
  const worker = state.workers.find(item => item.slot === state.selected);
  if (!worker) {
    statusNoteEl.classList.remove("visible");
    statusNoteEl.innerHTML = "";
    return;
  }

  const links = [];
  if (worker.issue_url) links.push(linkHTML(worker.issue_url, "Issue #" + worker.issue_number));
  if (worker.pr_url) links.push(linkHTML(worker.pr_url, "PR #" + worker.pr_number));
  const retry = worker.next_retry_at ? " Next retry: " + worker.next_retry_at + "." : "";
  const next = worker.next_action ? " Next: " + worker.next_action : "";
  statusNoteEl.innerHTML = '<div class="why-note"><strong>Why</strong>' +
    escapeText((worker.status_reason || "Waiting for next reconciliation cycle.") + retry + next) +
    (links.length ? '<span class="links">' + links.join("") + '</span>' : "") + '</div>' +
    renderWorkerActions(worker.actions || []);
  statusNoteEl.classList.add("visible");
}

async function loadState() {
  try {
    const response = await fetch("/api/v1/state", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
	state.workers = data.all || [];
	state.supervisor = data.supervisor || null;
	state.outcome = data.outcome || null;
	state.readOnly = data.read_only === true;
	renderStats(data.summary || {}, state.workers.length, data.max_parallel || 0, data.read_only);
	renderSupervisor(state.supervisor);
	renderOutcome(state.outcome);
    if (!state.selected && state.workers.length) state.selected = sortWorkers(state.workers)[0].slot;
    if (state.selected && !state.workers.some(worker => worker.slot === state.selected)) {
      state.selected = state.workers.length ? sortWorkers(state.workers)[0].slot : "";
      state.lastLog = null;
    }
    renderWorkers();
    renderSelectedDetails();
  } catch (err) {
    statsEl.innerHTML = '<span class="error">' + escapeText(err.message) + '</span>';
  }
}

async function loadLog() {
  if (!state.selected) {
    logTitleEl.innerHTML = 'Log <span></span>';
    logMetaEl.textContent = "";
    logEl.textContent = "Select a worker.";
    renderSelectedDetails();
    return;
  }
  const worker = state.workers.find(item => item.slot === state.selected);
  logTitleEl.innerHTML = 'Log <span>' + escapeText(state.selected) + (worker ? " #" + escapeText(worker.issue_number) : "") + '</span>';
  try {
    const response = await fetch("/api/v1/logs/" + encodeURIComponent(state.selected) + "?lines=260", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    const text = data.text || "";
    logMetaEl.textContent = (data.truncated ? "tail " : "") + (data.updated_at || "");
    if (text !== state.lastLog) {
      state.lastLog = text;
      logEl.textContent = text || emptyLogText(worker);
      logEl.scrollTop = logEl.scrollHeight;
    }
  } catch (err) {
    logEl.textContent = "Log error: " + err.message;
  }
}

loadState().then(loadLog);
setInterval(loadState, 3000);
setInterval(loadLog, 2000);
