const projectsEl = document.getElementById("projects");
const projectSummaryEl = document.getElementById("project-summary");
const projectRailBodyEl = document.getElementById("project-rail-body");
const projectRailSummaryEl = document.getElementById("project-rail-summary");
const projectFilterEl = document.getElementById("project-filter");
const statsEl = document.getElementById("stats");
const subtitleEl = document.getElementById("subtitle");
const fleetVerdictEl = document.getElementById("fleet-verdict");
const approvalListEl = document.getElementById("approval-list");
const approvalSummaryEl = document.getElementById("approval-summary");
const attentionListEl = document.getElementById("attention-list");
const attentionSummaryEl = document.getElementById("attention-summary");
const fleetWorkersEl = document.getElementById("fleet-workers-body");
const workerSummaryEl = document.getElementById("worker-summary");
const workerProjectResetEl = document.getElementById("worker-project-reset");
const workerDetailSummaryEl = document.getElementById("worker-detail-summary");
const workerDetailBodyEl = document.getElementById("worker-detail-body");
const workerFilterEl = document.getElementById("worker-filter");
const scopeFilterEl = document.getElementById("scope-filter");
const statusFilterEl = document.getElementById("status-filter");
const backendFilterEl = document.getElementById("backend-filter");
const prFilterEl = document.getElementById("pr-filter");
const workerSortEl = document.getElementById("worker-sort");
const sortDirectionEl = document.getElementById("sort-direction");
const initialStateEl = document.getElementById("fleet-initial-state");

const defaultSortDirections = { status: "asc", project: "asc", issue: "asc", runtime: "desc", pr: "asc" };
const validSortKeys = new Set(["status", "project", "issue", "runtime", "pr"]);
const validSortDirs = new Set(["asc", "desc"]);
const statusOrder = new Map([
  ["review_retry_running", 0],
  ["running", 0],
  ["review_retry_recheck", 1],
  ["pr_open", 1],
  ["review_retry_pending", 2],
  ["review_retry_backoff", 2],
  ["queued", 2],
  ["dead", 3],
  ["failed", 4],
  ["conflict_failed", 5],
  ["retry_exhausted", 6],
  ["done", 7]
]);

const fleetState = {
  selectedProject: "all",
  projectQuery: "",
  readOnly: true,
  selectedWorkerKey: "",
  filters: {
    query: "",
    scope: "operator",
    status: "all",
    backend: "all",
    pr: "all"
  },
  sortKey: "status",
  sortDir: "asc",
  projects: [],
  approvals: [],
  attention: [],
  workers: [],
  detail: null,
  verdict: null,
  refreshedAt: ""
};

loadStateFromQuery();

function escapeText(value) {
  return String(value ?? "").replace(/[&<>"']/g, ch => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;"
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
  return String(action || "-").replace(/_/g, " ");
}

function cssToken(value) {
  return String(value || "unknown").toLowerCase().replace(/[^a-z0-9_-]+/g, "_");
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

function renderActions(actions, options) {
  const items = actions || [];
  if (!items.length) return '<span class="empty">No controls.</span>';
  const showDetails = !options || options.details !== false;
  return '<div class="actions">' + items.map(action =>
    '<div class="action-item"><button type="button" class="action-btn" disabled aria-disabled="true" title="' +
    escapeText(action.disabled_reason || "Write action unavailable") + '">' +
    escapeText(action.label || actionLabel(action.id)) + '</button>' +
    (showDetails ? actionDetailHTML(action) : "") + '</div>'
  ).join("") + '</div>' +
  '<div class="action-note">' + escapeText(actionDisabledReason(items)) + '</div>';
}

function approvalStatusRank(status) {
  switch (status) {
  case "pending": return 0;
  case "superseded": return 1;
  case "stale": return 2;
  case "approved": return 3;
  case "rejected": return 4;
  default: return 5;
  }
}

function approvalTimeMillis(approval) {
  const updated = Date.parse(approval.updated_at || "");
  if (Number.isFinite(updated)) return updated;
  const created = Date.parse(approval.created_at || "");
  return Number.isFinite(created) ? created : 0;
}

function sortApprovals(approvals) {
  return approvals.map((approval, index) => ({ approval, index }))
    .sort((left, right) => {
      const status = compareNumber(approvalStatusRank(left.approval.status), approvalStatusRank(right.approval.status));
      if (status !== 0) return status;
      const freshness = compareNumber(approvalTimeMillis(right.approval), approvalTimeMillis(left.approval));
      if (freshness !== 0) return freshness;
      const project = compareText(left.approval.project_name, right.approval.project_name);
      if (project !== 0) return project;
      const id = compareText(left.approval.id, right.approval.id);
      if (id !== 0) return id;
      return left.index - right.index;
    })
    .map(entry => entry.approval);
}

function approvalsFromData(data) {
  const approvals = Array.isArray(data.approvals)
    ? data.approvals.slice()
    : (data.projects || []).flatMap(project => (project.approvals || []).map(approval => ({
      ...approval,
      project_name: approval.project_name || project.name,
      project_repo: approval.project_repo || project.repo,
      dashboard_url: approval.dashboard_url || project.dashboard_url
    })));
  return sortApprovals(approvals);
}

function approvalStatusClass(approval) {
  return "pill a-" + cssToken(approval.status || "unknown");
}

function approvalCardClass(approval) {
  return "approval-card approval-" + cssToken(approval.status || "unknown");
}

function isPendingApproval(approval) {
  return (approval.status || "") === "pending";
}

function pluralize(count, singular, plural) {
  return count + " " + (count === 1 ? singular : (plural || singular + "s"));
}

function approvalInboxSummaryText(activeCount, historicalCount) {
  if (!activeCount && !historicalCount) return "No active or historical approvals.";
  const active = activeCount === 0
    ? "No active approvals need review."
    : pluralize(activeCount, "active pending approval") + " " + (activeCount === 1 ? "needs" : "need") + " review.";
  if (!historicalCount) return active + " No historical approvals are recorded.";
  return active + " " + pluralize(historicalCount, "historical approval") + " " + (historicalCount === 1 ? "is" : "are") + " collapsed below.";
}

function approvalHistoryCountText(counts, historicalCount) {
  const known = (counts.superseded || 0) + (counts.stale || 0) + (counts.approved || 0) + (counts.rejected || 0);
  const parts = [
    [counts.superseded || 0, "superseded"],
    [counts.stale || 0, "stale"],
    [counts.approved || 0, "approved"],
    [counts.rejected || 0, "rejected"],
    [Math.max(0, historicalCount - known), "other"]
  ].filter(([count]) => count > 0).map(([count, label]) => count + " " + label);
  return parts.length ? parts.join(" · ") : "No historical approvals";
}

function approvalSessionVisible(approval) {
  return (fleetState.workers || []).some(worker =>
    worker.project_name === approval.project_name && worker.slot === approval.session);
}

function approvalTargetHTML(approval) {
  const links = [];
  if (approval.issue_number) links.push(linkHTML(approval.issue_url, "Issue #" + approval.issue_number));
  if (approval.pr_number) links.push(linkHTML(approval.pr_url, "PR #" + approval.pr_number));
  if (approval.session) {
    if (approvalSessionVisible(approval)) {
      links.push('<button type="button" class="link-button approval-session-link" data-project="' +
        escapeText(approval.project_name || "") + '" data-slot="' + escapeText(approval.session || "") + '">Session ' +
        escapeText(approval.session) + '</button>');
    } else {
      links.push('<span>Session ' + escapeText(approval.session) + '</span>');
    }
  }
  return links.length ? links.join(" ") : '<span class="empty">No target</span>';
}

function approvalCardHTML(approval) {
  const project = approval.project_name || "-";
  const id = approval.id || "-";
  const action = actionLabel(approval.action || "-");
  const createdAge = approval.created_age || "-";
  const updatedAge = approval.updated_age || "-";
  const sessionStatus = approval.session_status ? "Status " + approval.session_status : "";
  return '<article class="' + approvalCardClass(approval) + '" title="' + escapeText(approval.summary || "") + '">' +
    '<div class="approval-project"><strong title="' + escapeText(project) + '">' + linkHTML(approval.dashboard_url, project) + '</strong>' +
      '<div class="approval-meta"><span title="' + escapeText(id) + '">' + escapeText(id) + '</span></div></div>' +
    '<div class="approval-action"><strong title="' + escapeText(action) + '">' + escapeText(action) + '</strong>' +
      '<div class="approval-meta"><span class="' + approvalStatusClass(approval) + '">' + escapeText(approval.status || "unknown") + '</span></div></div>' +
    '<div class="approval-target">' + approvalTargetHTML(approval) + (sessionStatus ? '<span>' + escapeText(sessionStatus) + '</span>' : "") + '</div>' +
    '<div class="approval-main"><div class="approval-age"><span>Created ' + escapeText(createdAge) + ' ago</span><span>Updated ' + escapeText(updatedAge) + ' ago</span></div>' +
      '<div class="approval-risk"><span>Risk ' + escapeText(approval.risk || "-") + '</span></div>' +
      '<div class="approval-summary">' + escapeText(approval.summary || "No summary recorded.") + '</div></div>' +
  '</article>';
}

function renderApprovalInbox() {
  const approvals = fleetState.approvals || [];
  const counts = approvals.reduce((acc, approval) => {
    const status = approval.status || "unknown";
    acc[status] = (acc[status] || 0) + 1;
    return acc;
  }, {});
  const pending = approvals.filter(isPendingApproval);
  const historical = approvals.filter(approval => !isPendingApproval(approval));
  const historyDetails = approvalListEl.querySelector(".approval-history");
  const historyWasOpen = historyDetails ? historyDetails.open : false;
  approvalListEl.classList.toggle("approval-list-compact", pending.length === 0);
  approvalSummaryEl.textContent = approvalInboxSummaryText(pending.length, historical.length);

  const activeHTML = pending.length
    ? '<div class="approval-active-list">' + pending.map(approvalCardHTML).join("") + '</div>'
    : '<div class="empty approval-empty approval-active-empty">No pending approvals need review.</div>';
  const historyHTML = historical.length
    ? '<details class="approval-history"' + (historyWasOpen ? ' open' : '') + '><summary><strong>Audit history</strong><span>' + escapeText(approvalHistoryCountText(counts, historical.length)) + '</span></summary>' +
      '<div class="approval-history-list">' + historical.map(approvalCardHTML).join("") + '</div></details>'
    : '';
  approvalListEl.innerHTML = activeHTML + historyHTML;

  approvalListEl.querySelectorAll(".approval-session-link[data-slot]").forEach(button => {
    button.addEventListener("click", () => selectWorker(button.dataset.project || "", button.dataset.slot || ""));
  });
}

function formatTimestamp(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return date.toLocaleString();
}

function formatDurationSeconds(value) {
  const seconds = Number(value || 0);
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  if (seconds < 60) return Math.round(seconds) + "s";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return minutes + "m";
  const hours = Math.round(minutes / 60);
  if (hours < 48) return hours + "h";
  return Math.round(hours / 24) + "d";
}

function workerKey(worker) {
  return (worker.project_name || "") + "\u001f" + (worker.slot || "");
}

function selectedWorker() {
  if (!fleetState.selectedWorkerKey) return null;
  return (fleetState.workers || []).find(worker => workerKey(worker) === fleetState.selectedWorkerKey) || null;
}

function normalizeParam(value, fallback) {
  const normalized = String(value ?? "").trim();
  return normalized === "" ? fallback : normalized;
}

function loadStateFromQuery() {
  const params = new URLSearchParams(window.location.search);
  fleetState.selectedProject = normalizeParam(params.get("project"), "all");
  fleetState.filters.query = normalizeParam(params.get("q"), "");
  const scope = normalizeParam(params.get("scope"), "operator");
  fleetState.filters.scope = ["operator", "attention", "live", "recent", "all"].includes(scope) ? scope : "operator";
  fleetState.filters.status = normalizeParam(params.get("status"), "all");
  fleetState.filters.backend = normalizeParam(params.get("backend"), "all");
  const prFilter = normalizeParam(params.get("pr"), "all");
  fleetState.filters.pr = ["all", "with", "without"].includes(prFilter) ? prFilter : "all";
  const sortKey = normalizeParam(params.get("sort"), "status");
  fleetState.sortKey = validSortKeys.has(sortKey) ? sortKey : "status";
  const sortDir = normalizeParam(params.get("dir"), defaultSortDirections[fleetState.sortKey] || "asc");
  fleetState.sortDir = validSortDirs.has(sortDir) ? sortDir : (defaultSortDirections[fleetState.sortKey] || "asc");
}

function updateQueryState() {
  const params = new URLSearchParams(window.location.search);
  setQueryParam(params, "project", fleetState.selectedProject, "all");
  setQueryParam(params, "q", fleetState.filters.query, "");
  setQueryParam(params, "scope", fleetState.filters.scope, "operator");
  setQueryParam(params, "status", fleetState.filters.status, "all");
  setQueryParam(params, "backend", fleetState.filters.backend, "all");
  setQueryParam(params, "pr", fleetState.filters.pr, "all");
  setQueryParam(params, "sort", fleetState.sortKey, "status");
  setQueryParam(params, "dir", fleetState.sortDir, defaultSortDirections[fleetState.sortKey] || "asc");
  const next = params.toString();
  const url = window.location.pathname + (next ? "?" + next : "");
  window.history.replaceState(null, "", url);
}

function setQueryParam(params, key, value, defaultValue) {
  if (value && value !== defaultValue) {
    params.set(key, value);
  } else {
    params.delete(key);
  }
}

function uniqueSorted(values) {
  return Array.from(new Set(values.map(value => String(value ?? "").trim()).filter(Boolean)))
    .sort((left, right) => left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" }));
}

function optionHTML(value, label, selectedValue) {
  const selected = value === selectedValue ? " selected" : "";
  return '<option value="' + escapeText(value) + '"' + selected + '>' + escapeText(label) + '</option>';
}

function selectOptionsHTML(allLabel, values, selectedValue) {
  const options = [optionHTML("all", allLabel, selectedValue)];
  if (selectedValue !== "all" && !values.includes(selectedValue)) {
    options.push(optionHTML(selectedValue, selectedValue + " (not present)", selectedValue));
  }
  for (const value of values) {
    options.push(optionHTML(value, value, selectedValue));
  }
  return options.join("");
}

function renderFilterOptions() {
  statusFilterEl.innerHTML = selectOptionsHTML("All statuses", uniqueSorted((fleetState.workers || []).map(displayStatus)), fleetState.filters.status);
  backendFilterEl.innerHTML = selectOptionsHTML("All backends", uniqueSorted((fleetState.workers || []).map(worker => worker.backend)), fleetState.filters.backend);
}

function syncFilterControls() {
  projectFilterEl.value = fleetState.projectQuery;
  workerFilterEl.value = fleetState.filters.query;
  scopeFilterEl.value = fleetState.filters.scope;
  statusFilterEl.value = fleetState.filters.status;
  backendFilterEl.value = fleetState.filters.backend;
  prFilterEl.value = fleetState.filters.pr;
  workerSortEl.value = fleetState.sortKey;
  sortDirectionEl.textContent = fleetState.sortDir === "desc" ? "Desc" : "Asc";
  sortDirectionEl.setAttribute("aria-label", "Sort " + fleetState.sortDir + "; activate to switch direction");
}

function normalizedSearchText(value) {
  return String(value ?? "").trim().toLowerCase();
}

function workerSearchText(worker) {
  const issueNumber = worker.issue_number ? String(worker.issue_number) : "";
  const prNumber = worker.pr_number ? String(worker.pr_number) : "";
  return [
    worker.project_name,
    worker.project_repo,
    worker.slot,
    issueNumber,
    issueNumber ? "#" + issueNumber : "",
    worker.issue_title,
    worker.status,
    displayStatus(worker),
    statusLabel(worker),
    worker.backend,
    prNumber,
    prNumber ? "#" + prNumber : "no pr",
    worker.runtime
  ].map(normalizedSearchText).join(" ");
}

function workerMatchesFilters(worker) {
  const drilldown = hasWorkerDrilldownFilters();
  if (!workerMatchesScope(worker) && !(fleetState.filters.scope === "operator" && drilldown)) return false;
  if (fleetState.filters.status !== "all" && displayStatus(worker) !== fleetState.filters.status) return false;
  if (fleetState.filters.backend !== "all" && (worker.backend || "") !== fleetState.filters.backend) return false;
  if (fleetState.filters.pr === "with" && !worker.pr_number) return false;
  if (fleetState.filters.pr === "without" && worker.pr_number) return false;
  const terms = normalizedSearchText(fleetState.filters.query).split(/\s+/).filter(Boolean);
  if (!terms.length) return true;
  const haystack = workerSearchText(worker);
  return terms.every(term => haystack.includes(term));
}

function isLiveWorker(worker) {
  if (worker.live === true) return true;
  const displayed = displayStatus(worker);
  return ["running", "pr_open", "queued", "review_retry_running", "review_retry_recheck", "review_retry_pending", "review_retry_backoff"].includes(displayed) ||
    ["running", "pr_open", "queued"].includes(worker.status || "");
}

function defaultWorkerVisible(worker) {
  return workerNeedsAttention(worker) || isLiveWorker(worker);
}

function workerMatchesScope(worker) {
  switch (fleetState.filters.scope) {
  case "attention":
    return workerNeedsAttention(worker);
  case "live":
    return isLiveWorker(worker);
  case "recent":
    return !isLiveWorker(worker) && !workerNeedsAttention(worker);
  case "all":
    return true;
  case "operator":
  default:
    return workerNeedsAttention(worker) || isLiveWorker(worker);
  }
}

function filteredWorkers(includeProjectFilter) {
  return (fleetState.workers || []).filter(worker => {
    if (includeProjectFilter && fleetState.selectedProject !== "all" && worker.project_name !== fleetState.selectedProject) return false;
    return workerMatchesFilters(worker);
  });
}

function selectedProjectWorkers() {
  if (fleetState.selectedProject === "all") return fleetState.workers || [];
  return (fleetState.workers || []).filter(worker => worker.project_name === fleetState.selectedProject);
}

function hasWorkerFilters() {
  return fleetState.filters.scope !== "operator" || hasWorkerDrilldownFilters();
}

function hasWorkerDrilldownFilters() {
  return fleetState.filters.query !== "" || fleetState.filters.status !== "all" || fleetState.filters.backend !== "all" || fleetState.filters.pr !== "all";
}

function workerNeedsAttention(worker) {
  return worker.needs_attention || (worker.status === "running" && worker.alive === false);
}

function statusRank(worker) {
  const attention = workerNeedsAttention(worker) ? 0 : 1;
  const displayed = displayStatus(worker);
  const rank = statusOrder.has(displayed) ? statusOrder.get(displayed) : 99;
  return attention * 100 + rank;
}

function compareText(left, right) {
  return String(left || "").localeCompare(String(right || ""), undefined, { numeric: true, sensitivity: "base" });
}

function compareNumber(left, right) {
  const leftNumber = Number(left);
  const rightNumber = Number(right);
  const leftValue = Number.isFinite(leftNumber) ? leftNumber : Number.MAX_SAFE_INTEGER;
  const rightValue = Number.isFinite(rightNumber) ? rightNumber : Number.MAX_SAFE_INTEGER;
  if (leftValue === rightValue) return 0;
  return leftValue < rightValue ? -1 : 1;
}

function runtimeSeconds(worker) {
  const value = Number(worker.runtime_seconds || 0);
  return Number.isFinite(value) ? value : 0;
}

function compareWorkers(left, right, key) {
  switch (key) {
  case "project":
    return compareText(left.project_name, right.project_name);
  case "issue":
    return compareNumber(left.issue_number || Number.MAX_SAFE_INTEGER, right.issue_number || Number.MAX_SAFE_INTEGER);
  case "runtime":
    return compareNumber(runtimeSeconds(left), runtimeSeconds(right));
  case "pr":
    return compareNumber(left.pr_number || Number.MAX_SAFE_INTEGER, right.pr_number || Number.MAX_SAFE_INTEGER);
  case "status":
  default:
    return compareNumber(statusRank(left), statusRank(right));
  }
}

function sortWorkers(workers) {
  const direction = fleetState.sortDir === "desc" ? -1 : 1;
  return workers.map((worker, index) => ({ worker, index }))
    .sort((left, right) => {
      const result = compareWorkers(left.worker, right.worker, fleetState.sortKey);
      if (result !== 0) return result * direction;
      return left.index - right.index;
    })
    .map(entry => entry.worker);
}

function displayStatus(worker) {
  return worker.display_status || worker.status || "-";
}

function statusLabel(worker) {
  if (worker.status === "running" && worker.alive === false) return "running stale";
  return displayStatus(worker);
}

function statusClass(worker) {
  let cls = "pill s-" + cssToken(displayStatus(worker) || "unknown");
  if (worker.needs_attention || (worker.status === "running" && worker.alive === false)) cls += " attention";
  return cls;
}

function rowClass(worker) {
  if (worker.needs_attention || (worker.status === "running" && worker.alive === false)) return "row-attention";
  const displayed = displayStatus(worker);
  if (worker.status === "running" || displayed === "review_retry_running") return "row-running";
  if (worker.status === "pr_open" || displayed === "review_retry_recheck") return "row-pr";
  return "";
}

function workerWhyText(worker) {
  const reason = (worker.status_reason || "").trim();
  const action = (worker.next_action || "").trim();
  if (!reason && !action) return "";
  if (!reason) return "Next: " + action;
  const sep = reason.endsWith(".") || reason.endsWith("!") || reason.endsWith("?") ? " " : ". ";
  return reason + (action ? sep + "Next: " + action : "");
}

function workerWhyHTML(worker) {
  if (!worker.needs_attention && displayStatus(worker) === "running") return "";
  const why = workerWhyText(worker);
  if (!why) return "";
  return '<div class="why-line"><strong>Why:</strong> ' + escapeText(why) + '</div>';
}

function attentionKey(worker) {
  return [worker.project_name || "", worker.slot || "", worker.issue_number || ""].join("\u001f");
}

function startedAtMillis(worker) {
  const startedAt = Date.parse(worker.started_at || "");
  return Number.isFinite(startedAt) ? startedAt : 0;
}

function attentionSeverityRank(worker) {
  const text = [displayStatus(worker), worker.status, worker.status_reason, worker.next_action].map(normalizedSearchText).join(" ");
  if (text.includes("blocked") || ["dead", "failed", "conflict_failed", "retry_exhausted"].includes(worker.status)) return 0;
  if (worker.status === "running") return 1;
  if (worker.status === "pr_open" || worker.status === "queued") return 2;
  return 3;
}

function sortAttentionWorkers(workers) {
  return workers.map((worker, index) => ({ worker, index }))
    .sort((left, right) => {
      const severity = compareNumber(attentionSeverityRank(left.worker), attentionSeverityRank(right.worker));
      if (severity !== 0) return severity;
      const freshness = compareNumber(startedAtMillis(right.worker), startedAtMillis(left.worker));
      if (freshness !== 0) return freshness;
      const project = compareText(left.worker.project_name, right.worker.project_name);
      if (project !== 0) return project;
      const slot = compareText(left.worker.slot, right.worker.slot);
      if (slot !== 0) return slot;
      return left.index - right.index;
    })
    .map(entry => entry.worker);
}

function attentionFromData(data) {
  const items = [];
  const seen = new Set();
  const add = worker => {
    if (!worker || !workerNeedsAttention(worker)) return;
    const key = attentionKey(worker);
    if (seen.has(key)) return;
    seen.add(key);
    items.push(worker);
  };

  if (Array.isArray(data.attention)) {
    data.attention.forEach(add);
  }
  if (!Array.isArray(data.attention) && Array.isArray(data.workers)) {
    data.workers.forEach(add);
  }
  for (const project of data.projects || []) {
    for (const worker of project.attention || []) {
      add({
        ...worker,
        project_name: worker.project_name || project.name,
        project_repo: worker.project_repo || project.repo,
        dashboard_url: worker.dashboard_url || project.dashboard_url
      });
    }
  }
  return sortAttentionWorkers(items);
}

function attentionReasonText(worker) {
  return (worker.status_reason || "").trim() || "Needs operator review.";
}

function attentionNextActionText(worker) {
  return (worker.next_action || "").trim() || "Open the worker detail and choose the next safe action.";
}

function renderAttentionInbox() {
  const attention = fleetState.attention || [];
  if (!attention.length) {
    attentionSummaryEl.textContent = "No projects need attention";
    attentionListEl.innerHTML = '<div class="empty attention-empty">No projects need attention right now. The fleet is waiting normally.</div>';
    return;
  }

  const severe = attention.filter(worker => attentionSeverityRank(worker) === 0).length;
  attentionSummaryEl.textContent = attention.length + " item" + (attention.length === 1 ? "" : "s") + " need attention" +
    (severe ? " · " + severe + " blocked/dead/retry" : "");
  attentionListEl.innerHTML = attention.map(worker => {
    const project = worker.project_name || "-";
    const slot = worker.slot || "-";
    const age = worker.runtime || "-";
    const pr = worker.pr_number ? linkHTML(worker.pr_url, "PR #" + worker.pr_number) : "No PR";
    const selected = workerKey(worker) === fleetState.selectedWorkerKey ? " selected" : "";
    return '<article class="attention-card' + selected + '" data-project="' + escapeText(worker.project_name || "") + '" data-slot="' + escapeText(worker.slot || "") + '" tabindex="0" title="' + escapeText(attentionReasonText(worker)) + '">' +
      '<div class="attention-context">' +
        '<span class="attention-project" title="' + escapeText(project) + '">' + linkHTML(worker.dashboard_url, project) + '</span>' +
        '<div class="attention-meta"><span>Slot ' + escapeText(slot) + '</span><span>Age ' + escapeText(age) + '</span></div>' +
      '</div>' +
      '<div class="attention-main">' +
        '<div class="attention-top">' +
          '<div class="attention-issue" title="' + escapeText(issueSummaryText(worker)) + '">' + issueSummaryHTML(worker) + '</div>' +
          '<span class="' + statusClass(worker) + '" title="' + escapeText(statusLabel(worker)) + '">' + escapeText(statusLabel(worker)) + '</span>' +
          '<span class="attention-pr">' + pr + '</span>' +
        '</div>' +
        '<div class="attention-lines"><div><strong>Why:</strong> ' + escapeText(attentionReasonText(worker)) + '</div>' +
        '<div><strong>Next:</strong> ' + escapeText(attentionNextActionText(worker)) + '</div></div>' +
      '</div>' +
    '</article>';
  }).join("");

  attentionListEl.querySelectorAll(".attention-card[data-slot]").forEach(card => {
    card.addEventListener("click", () => selectWorker(card.dataset.project || "", card.dataset.slot || ""));
    card.addEventListener("keydown", event => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        selectWorker(card.dataset.project || "", card.dataset.slot || "");
      }
    });
  });
  attentionListEl.querySelectorAll("a").forEach(link => {
    link.addEventListener("click", event => event.stopPropagation());
  });
}

function fleetWorkersFromData(data) {
  if (Array.isArray(data.workers)) return data.workers;
  return (data.projects || []).flatMap(project => (project.active || []).map(worker => ({
    ...worker,
    project_name: project.name,
    project_repo: project.repo,
    dashboard_url: project.dashboard_url
  })));
}

function countFailed(project) {
  return project.failed || 0;
}

function renderFleetVerdict(verdict) {
  const tones = new Set(["healthy", "busy", "attention", "daemon-down"]);
  const tone = verdict && tones.has(verdict.tone) ? verdict.tone : "attention";
  const sentence = verdict && verdict.sentence ? verdict.sentence : "Supervisor status unavailable. No worker state or attention state could be confirmed.";
  fleetVerdictEl.className = "fleet-verdict verdict-" + tone;
  fleetVerdictEl.textContent = sentence;
}

function renderStats(summary) {
  const items = [
    ["Projects", summary.projects || 0],
    ["Active", summary.active || 0],
    ["PR open", summary.pr_open || 0],
    ["Failed", summary.failed || 0],
    ["Attention", summary.needs_attention || 0]
  ];
  statsEl.innerHTML = items.map(([label, value]) =>
    '<div class="stat"><strong>' + escapeText(value) + '</strong><span>' + escapeText(label) + '</span></div>'
  ).join("");
}

function ensureSelectedProject() {
  const projectNames = new Set((fleetState.projects || []).map(project => project.name));
  if (fleetState.selectedProject !== "all" && !projectNames.has(fleetState.selectedProject)) {
    fleetState.selectedProject = "all";
    updateQueryState();
  }
}

function projectIsUnconfigured(project) {
  return !((project.outcome || {}).configured === true);
}

function projectStateKey(project) {
  if (projectIsUnconfigured(project)) return "unconfigured";
  const operator = project.operator_state || {};
  return operator.kind || "idle";
}

function projectStateLabel(project) {
  if (projectIsUnconfigured(project)) return "setup";
  const operator = project.operator_state || {};
  return operator.label || "Idle";
}

function projectSearchText(project) {
  const q = project.queue_snapshot || {};
  const o = project.outcome || {};
  return [
    project.name,
    project.repo,
    project.config_path,
    projectStateLabel(project),
    project.operator_state && project.operator_state.summary,
    project.operator_state && project.operator_state.next_action,
    project.error,
    q.idle_reason,
    q.top_skipped_reason,
    q.policy_rule,
    o.goal,
    o.health_state,
    o.next_action,
    project.freshness && project.freshness.reason
  ].map(normalizedSearchText).join(" ");
}

function projectMatchesSearch(project) {
  const terms = normalizedSearchText(fleetState.projectQuery).split(/\s+/).filter(Boolean);
  if (!terms.length) return true;
  const haystack = projectSearchText(project);
  return terms.every(term => haystack.includes(term));
}

function visibleProjects() {
  return (fleetState.projects || []).filter(projectMatchesSearch);
}

function projectRailSummaryText(projects, total) {
  if (!total) return "No configured projects.";
  const activeKinds = new Set(["working", "monitoring_pr", "pending_dispatch"]);
  const active = projects.filter(project => activeKinds.has(projectStateKey(project))).length;
  const attention = projects.reduce((sum, project) => sum + Number(project.needs_attention || 0), 0);
  const filtered = projects.length === total ? "" : " shown from " + total;
  return projects.length + " project" + (projects.length === 1 ? "" : "s") + filtered +
    " · " + active + " active · " + attention + " attention";
}

function githubRepoURL(repo) {
  const value = String(repo || "").trim();
  if (!/^[^\s/]+\/[^\s/]+$/.test(value)) return "";
  return "https://github.com/" + value;
}

function githubPullsURL(repo) {
  const url = githubRepoURL(repo);
  return url ? url + "/pulls?q=is%3Apr+is%3Aopen" : "";
}

function projectIdentityRailHTML(project) {
  const name = project.name || "project";
  const repo = project.repo || project.config_path || "";
  return '<div class="rail-project-name" title="' + escapeText(name) + '">' + linkHTML(project.dashboard_url, name) + '</div>' +
    '<div class="rail-project-repo" title="' + escapeText(repo) + '">' + escapeText(repo) + '</div>';
}

function projectStateRailHTML(project) {
  if (projectIsUnconfigured(project)) {
    const parts = [
      '<span class="pill rail-state-unconfigured">setup</span>',
      '<div class="rail-subline" title="No outcome brief configured">No outcome brief configured</div>',
      '<div class="rail-note rail-setup-link">Set up &rarr;</div>'
    ];
    if (project.error) parts.push('<div class="rail-alert" title="' + escapeText(project.error) + '">State error</div>');
    if (project.freshness && project.freshness.stale) parts.push('<div class="rail-warn">Stale snapshot</div>');
    return parts.join("");
  }

  const key = projectStateKey(project);
  const operator = project.operator_state || {};
  const summary = String(operator.summary || ((project.running || 0) + '/' + (project.max_parallel || 0) + ' worker process(es) running.'));
  const parts = [
    '<span class="pill rail-state-' + cssToken(key) + '">' + escapeText(projectStateLabel(project)) + '</span>',
    '<div class="rail-subline" title="' + escapeText(summary) + '">' + escapeText(summary) + '</div>'
  ];
  if (operator.next_action) parts.push('<div class="rail-note" title="' + escapeText(operator.next_action) + '">Next: ' + escapeText(operator.next_action) + '</div>');
  if (project.error) parts.push('<div class="rail-alert" title="' + escapeText(project.error) + '">State error</div>');
  if (project.freshness && project.freshness.stale && key !== "stale") parts.push('<div class="rail-warn">Stale snapshot</div>');
  return parts.join("");
}

function projectQueueRailHTML(project) {
  const q = project.queue_snapshot;
  if (!q) return '<span class="empty">No queue snapshot</span>';
  const parts = [
    'open=' + Number(q.open || 0),
    'eligible=' + Number(q.eligible || 0),
    'excluded=' + Number(q.excluded || 0),
    'held/meta=' + Number(q.held || q.held_issues || 0),
    'blocked-deps=' + Number(q.blocked_by_dependency || q.blocked_by_dependency_issues || 0)
  ];
  const lines = ['<div class="rail-mainline">' + escapeText(parts.join(' · ')) + '</div>'];
  if (q.selected_candidate && q.selected_candidate.number) {
    const selected = 'selected #' + q.selected_candidate.number + (q.selected_candidate.title ? ' ' + q.selected_candidate.title : '');
    lines.push('<div class="rail-subline" title="' + escapeText(selected) + '">' + escapeText(selected) + '</div>');
  }
  const idleReason = (project.running || 0) === 0 ? String(q.idle_reason || "").trim() : "";
  if (idleReason && projectStateKey(project) !== "queue_blocked") lines.push('<div class="rail-warn" title="' + escapeText(idleReason) + '">' + escapeText(idleReason) + '</div>');
  return lines.join("");
}

function projectPRRailHTML(project) {
  const workers = (fleetState.workers || []).filter(worker => worker.project_name === project.name && worker.pr_number);
  const seen = new Set();
  const links = [];
  for (const worker of workers) {
    if (seen.has(worker.pr_number)) continue;
    seen.add(worker.pr_number);
    links.push(linkHTML(worker.pr_url || (project.repo ? 'https://github.com/' + project.repo + '/pull/' + worker.pr_number : ''), 'PR #' + worker.pr_number));
    if (links.length >= 3) break;
  }
  if ((project.pr_open || 0) === 0 && links.length === 0) return '<span class="empty">No open PR</span>';
  const fallback = !links.length && githubPullsURL(project.repo) ? [linkHTML(githubPullsURL(project.repo), 'Open PRs')] : [];
  return '<div class="rail-mainline">' + escapeText(project.pr_open || 0) + ' open</div>' +
    '<div class="rail-links">' + links.concat(fallback).join(' ') + '</div>';
}

function projectOutcomeRailHTML(project) {
  if (projectIsUnconfigured(project)) {
    const next = (project.outcome && project.outcome.next_action) || "Add an outcome brief to this project's Maestro config.";
    return '<div class="rail-subline rail-setup-copy" title="No outcome brief configured">No outcome brief configured</div>' +
      '<div class="rail-note rail-setup-link" title="' + escapeText(next) + '">Set up &rarr;</div>';
  }

  const outcome = project.outcome || {};
  const health = outcome.health_state || "unknown";
  const goal = outcome.configured === true && outcome.goal ? outcome.goal : "No outcome brief configured";
  const next = outcome.next_action || "";
  return '<span class="pill outcome-' + cssToken(health) + '">' + escapeText(health.replace(/_/g, ' ')) + '</span>' +
    '<div class="rail-subline" title="' + escapeText(goal) + '">' + escapeText(goal) + '</div>' +
    (next ? '<div class="rail-note" title="' + escapeText(next) + '">' + escapeText(next) + '</div>' : '');
}

function projectFreshnessRailHTML(project) {
  const freshness = project.freshness || {};
  const age = freshness.snapshot_age ? "Snapshot " + freshness.snapshot_age + " ago" : "No snapshot yet";
  const details = [];
  if (freshness.state_updated_at) details.push("State " + formatTimestamp(freshness.state_updated_at));
  if (freshness.log_updated_at) details.push("Logs " + formatTimestamp(freshness.log_updated_at));
  if (freshness.reason) details.push(freshness.reason);
  return '<div class="rail-mainline" title="' + escapeText(details.join(' · ')) + '">' + escapeText(age) + '</div>';
}

function projectLinksRailHTML(project) {
  const links = [];
  const setupURL = project.dashboard_url || githubRepoURL(project.repo);
  if (projectIsUnconfigured(project) && setupURL) {
    links.push('<a class="setup-link" href="' + escapeText(setupURL) + '" target="_blank" rel="noreferrer">Set up &rarr;</a>');
  }
  if (project.dashboard_url) links.push(linkHTML(project.dashboard_url, "Dashboard"));
  if (githubRepoURL(project.repo)) links.push(linkHTML(githubRepoURL(project.repo), "GitHub"));
  links.push('<button type="button" class="link-button project-workers-link" data-project="' + escapeText(project.name || "") + '">Workers</button>');
  return '<div class="rail-links">' + links.join(' ') + '</div>';
}

function projectRailRowHTML(project) {
  const key = projectStateKey(project);
  const modifier = projectIsUnconfigured(project) ? ' project-row--unconfigured' : '';
  return '<tr class="project-rail-row project-row-' + cssToken(key) + modifier + '" data-project="' + escapeText(project.name || "") + '">' +
    '<td class="project-rail-project">' + projectIdentityRailHTML(project) + '</td>' +
    '<td class="project-rail-state-cell">' + projectStateRailHTML(project) + '</td>' +
    '<td class="project-rail-queue-cell">' + projectQueueRailHTML(project) + '</td>' +
    '<td class="project-rail-pr-cell">' + projectPRRailHTML(project) + '</td>' +
    '<td class="project-rail-outcome-cell">' + projectOutcomeRailHTML(project) + '</td>' +
    '<td class="project-rail-freshness-cell">' + projectFreshnessRailHTML(project) + '</td>' +
    '<td class="project-rail-links-cell">' + projectLinksRailHTML(project) + '</td>' +
  '</tr>';
}

function renderProjectRail() {
  ensureSelectedProject();
  const total = (fleetState.projects || []).length;
  const projects = visibleProjects();
  projectRailSummaryEl.textContent = projectRailSummaryText(projects, total);
  if (!projects.length) {
    const empty = total ? "No configured projects match the project search." : "No configured projects are available in this fleet.";
    projectRailBodyEl.innerHTML = '<tr class="project-rail-empty"><td colspan="7" class="empty">' + escapeText(empty) + '</td></tr>';
    return;
  }

  projectRailBodyEl.innerHTML = projects.map(projectRailRowHTML).join("");
  projectRailBodyEl.querySelectorAll(".project-workers-link[data-project]").forEach(button => {
    button.addEventListener("click", event => {
      event.preventDefault();
      fleetState.selectedProject = button.dataset.project || "all";
      updateQueryState();
      renderFleetWorkers();
      document.querySelector(".fleet-workers")?.scrollIntoView({ block: "start", behavior: "smooth" });
    });
  });
}

function renderFleetWorkers() {
  const base = selectedProjectWorkers();
  const visible = sortWorkers(filteredWorkers(true));
  const showingDefaultScope = fleetState.filters.scope === "operator" && !hasWorkerDrilldownFilters();
  const hiddenHistory = showingDefaultScope ? base.filter(worker => !defaultWorkerVisible(worker)) : [];
  const rowCount = visible.length + (hiddenHistory.length ? 1 : 0);
  const table = fleetWorkersEl.closest("table");
  if (table) table.classList.toggle("worker-table-empty", rowCount === 0);
  const projectLabel = fleetState.selectedProject === "all" ? "all projects" : fleetState.selectedProject;
  const projectScoped = fleetState.selectedProject !== "all";
  workerProjectResetEl.hidden = !projectScoped;
  workerProjectResetEl.setAttribute("aria-label", projectScoped ? "Clear " + projectLabel + " worker scope and show all projects" : "Workers are showing all projects");
  const scopeLabel = scopeLabelText(fleetState.filters.scope);
  const filterText = hasWorkerFilters() ? " · " + visible.length + " shown from " + base.length + " total" : " · " + base.length + " total";
  const attentionCount = visible.filter(worker => worker.needs_attention).length;
  workerSummaryEl.textContent = scopeLabel + " · " + visible.length + " worker" + (visible.length === 1 ? "" : "s") + " in " + projectLabel +
    filterText + (hiddenHistory.length ? " · " + hiddenHistory.length + " historical collapsed" : "") +
    (attentionCount ? " · " + attentionCount + " need attention" : "");

  if (rowCount === 0) {
    const empty = fleetEmptyText(projectLabel, base.length);
    fleetWorkersEl.innerHTML = '<tr><td colspan="9" class="empty">' + escapeText(empty) + '</td></tr>';
    return;
  }

  const rows = visible.map(worker => {
    const pr = worker.pr_number ? "#" + worker.pr_number : "-";
    const project = worker.project_name || "-";
    const issueText = issueSummaryText(worker);
    const selected = workerKey(worker) === fleetState.selectedWorkerKey ? " selected" : "";
    return '<tr class="' + rowClass(worker) + selected + '" data-project="' + escapeText(worker.project_name || "") + '" data-slot="' + escapeText(worker.slot || "") + '" tabindex="0">' +
      '<td class="project-col" title="' + escapeText(project) + '">' + linkHTML(worker.dashboard_url, project) + '</td>' +
      '<td class="slot-col" title="' + escapeText(worker.slot || "-") + '">' + escapeText(worker.slot || "-") + '</td>' +
      '<td class="issue-col" title="' + escapeText(issueText) + '">' + issueSummaryHTML(worker) + workerWhyHTML(worker) + '</td>' +
      '<td class="status-col" title="' + escapeText(statusLabel(worker)) + '"><span class="' + statusClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td class="backend-col" title="' + escapeText(worker.backend || "-") + '">' + escapeText(worker.backend || "-") + '</td>' +
      '<td class="pr-col" title="' + escapeText(pr) + '">' + linkHTML(worker.pr_url, pr) + '</td>' +
      '<td class="runtime-col" title="' + escapeText(worker.runtime || "-") + '">' + escapeText(worker.runtime || "-") + '</td>' +
      '<td class="tokens-col">' + compactNumber(worker.tokens_used_total) + '</td>' +
      '<td class="action-col">' + renderActions(worker.actions || [], { details: false }) + '</td>' +
    '</tr>';
  });
  if (hiddenHistory.length) {
    rows.push(historySummaryRowHTML(hiddenHistory));
  }
  fleetWorkersEl.innerHTML = rows.join("");

  fleetWorkersEl.querySelectorAll("tr[data-slot]").forEach(row => {
    row.addEventListener("click", () => selectWorker(row.dataset.project || "", row.dataset.slot || ""));
    row.addEventListener("keydown", event => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        selectWorker(row.dataset.project || "", row.dataset.slot || "");
      }
    });
  });
  fleetWorkersEl.querySelectorAll("button[data-history-scope]").forEach(button => {
    button.addEventListener("click", event => {
      event.stopPropagation();
      showHistoryScope(button.dataset.historyScope || "recent");
    });
  });
}

function historySummaryRowHTML(workers) {
  const count = workers.length;
  const sample = workers.slice(0, 3).map(worker => (worker.project_name || "-") + " / " + (worker.slot || "-")).join(", ");
  const note = "Done/stale sessions are collapsed by default." + (sample ? " Examples: " + sample + "." : "") + " Search or switch scope to inspect every session.";
  return '<tr class="history-row"><td colspan="9"><div class="history-row-content">' +
    '<div><strong>' + escapeText(count + " historical worker" + (count === 1 ? "" : "s")) + '</strong><span> ' + escapeText(note) + '</span></div>' +
    '<button type="button" class="history-row-action" data-history-scope="recent">Show history</button>' +
    '</div></td></tr>';
}

function showHistoryScope(scope) {
	fleetState.filters.scope = scope;
	syncFilterControls();
	updateQueryState();
	renderProjectRail();
	renderProjectOverview();
	renderFleetWorkers();
}

function scopeLabelText(scope) {
  switch (scope) {
  case "attention": return "Attention only";
  case "live": return "Live only";
  case "recent": return "Done/history";
  case "all": return "All workers";
  case "operator":
  default: return "Needs action/live";
  }
}

function fleetEmptyText(projectLabel, total) {
	const historyHint = total ? " Switch Scope to Done/history or All workers to inspect " + total + " historical worker" + (total === 1 ? "" : "s") + "." : "";
	if (fleetState.filters.scope === "operator") {
		return "No workers need operator action and no workers are live in " + projectLabel + "." + historyHint;
  }
  if (fleetState.filters.scope === "attention") {
    return "No workers currently need attention in " + projectLabel + "." + historyHint;
  }
	if (fleetState.filters.scope === "live") {
		return "No workers are currently running or waiting on open PRs in " + projectLabel + "." + historyHint;
	}
	if (fleetState.filters.scope === "recent" && !total) {
		return "No completed historical worker sessions are available for " + projectLabel + ".";
	}
	if (fleetState.filters.scope === "all" && !total) {
		return "No worker sessions are available for " + projectLabel + ".";
	}
	if (hasWorkerFilters()) {
		return "No workers match the current filters.";
	}
  return "No worker sessions are available for " + projectLabel + ".";
}

function selectWorker(projectName, slot) {
  fleetState.selectedWorkerKey = projectName + "\u001f" + slot;
  fleetState.detail = null;
  renderAttentionInbox();
  renderFleetWorkers();
  renderWorkerDetailLoading(projectName, slot);
  loadWorkerDetail();
}

function renderWorkerDetailLoading(projectName, slot) {
  workerDetailSummaryEl.textContent = projectName && slot ? projectName + " / " + slot : "Loading worker";
  workerDetailBodyEl.innerHTML = '<div class="empty">Loading worker detail...</div>';
}

function emptyLogText(worker) {
  if (!worker) return "No log output available.";
  if (worker.status === "running" && worker.backend === "claude") {
    return "Log file is available, but Claude print mode may stay quiet until it finishes.";
  }
  if (worker.status === "running") return "Log file is available, but no output has been written yet.";
  return "Log file is available, but no output was captured.";
}

function aliveText(worker) {
  if (!worker || worker.alive === undefined || worker.alive === null) return "-";
  return worker.alive ? "true" : "false";
}

function detailField(label, value) {
  return '<div class="detail-field"><span>' + escapeText(label) + '</span><strong title="' + escapeText(value || "-") + '">' + escapeText(value || "-") + '</strong></div>';
}

function renderWorkerDetail(data) {
  if (!fleetState.selectedWorkerKey) {
    workerDetailSummaryEl.textContent = "No worker selected";
    workerDetailBodyEl.innerHTML = '<div class="empty">Select a fleet worker to show metadata and log output.</div>';
    return;
  }
  if (!data || !data.worker) {
    const worker = selectedWorker();
    if (!worker) {
      workerDetailSummaryEl.textContent = "Worker unavailable";
      workerDetailBodyEl.innerHTML = '<div class="empty">Selected worker is no longer visible in the fleet snapshot.</div>';
      return;
    }
    data = { worker: worker, log: { available: false, reason: "Worker detail has not loaded yet." } };
  }

  const worker = data.worker;
  const log = data.log || {};
  const issue = worker.issue_number ? "#" + worker.issue_number : "-";
  const pr = worker.pr_number ? "#" + worker.pr_number : "-";
  const links = [];
  if (worker.issue_url) links.push(linkHTML(worker.issue_url, "Issue " + issue));
  if (worker.pr_url) links.push(linkHTML(worker.pr_url, "PR " + pr));
  workerDetailSummaryEl.textContent = (worker.project_name || "-") + " / " + (worker.slot || "-") + " / " + statusLabel(worker);

  const fields = [
    detailField("Project", worker.project_name || "-"),
    detailField("Slot", worker.slot || "-"),
    detailField("Issue", issue + (worker.issue_title ? " " + worker.issue_title : "")),
    detailField("PR", pr),
    detailField("Backend", worker.backend || "-"),
    detailField("Status", statusLabel(worker)),
    detailField("Alive", aliveText(worker)),
    detailField("Attention", worker.needs_attention ? "yes" : "no"),
    detailField("Worktree", worker.worktree || "-"),
    detailField("Branch", worker.branch || "-"),
    detailField("Started", formatTimestamp(worker.started_at)),
    detailField("Finished", formatTimestamp(worker.finished_at)),
    detailField("Runtime", worker.runtime || "-"),
    detailField("Next retry", formatTimestamp(worker.next_retry_at)),
    detailField("Retry count", worker.retry_count ? String(worker.retry_count) : "0"),
    detailField("Log", worker.has_log ? "recorded" : "not recorded")
  ].join("");

  const noteClass = worker.needs_attention || (worker.status === "running" && worker.alive === false) ? " detail-note attention" : "detail-note";
  const reason = workerWhyText(worker) || "Waiting for the next Maestro reconciliation cycle.";
  const logText = log.available ? (log.text || emptyLogText(worker)) : (log.reason || "Log output is unavailable for this session.");
  const logMeta = log.available
    ? (log.truncated ? "tail, " : "") + (log.updated_at || "")
    : "unavailable";

  workerDetailBodyEl.innerHTML = '<div class="detail-grid">' + fields + '</div>' +
    '<div class="' + noteClass + '"><strong>State</strong> ' + escapeText(reason) +
      (links.length ? '<div class="detail-links">' + links.join("") + '</div>' : "") +
    '</div>' +
    '<div class="project-actions"><div class="label">Approval-gated controls</div>' + renderActions(worker.actions || [], { details: false }) + '</div>' +
    '<div class="log-tail">' +
      '<div class="log-tail-head"><strong>Recent log tail</strong><span>' + escapeText(logMeta) + '</span></div>' +
      '<pre>' + escapeText(logText) + '</pre>' +
    '</div>';
}

async function loadWorkerDetail() {
  const worker = selectedWorker();
  if (!worker) {
    fleetState.detail = null;
    renderWorkerDetail(null);
    return;
  }
  const key = workerKey(worker);
  try {
    const url = "/api/v1/fleet/worker?project=" + encodeURIComponent(worker.project_name || "") + "&slot=" + encodeURIComponent(worker.slot || "") + "&lines=260";
    const response = await fetch(url, { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    if (key !== fleetState.selectedWorkerKey) return;
    fleetState.detail = await response.json();
    renderWorkerDetail(fleetState.detail);
  } catch (err) {
    if (key !== fleetState.selectedWorkerKey) return;
    workerDetailSummaryEl.textContent = "Worker detail error";
    workerDetailBodyEl.innerHTML = '<div class="error">Unable to load worker detail: ' + escapeText(err.message) + '</div>';
  }
}

function queueSnapshotHTML(project) {
  const q = project.queue_snapshot;
  if (!q) return "";
  const excluded = Number(q.excluded || 0);
  const held = Number(q.held || q.held_issues || 0);
  const blockedByDependency = Number(q.blocked_by_dependency || q.blocked_by_dependency_issues || 0);
  const nonRunnable = Number(q.non_runnable_project_status_count || 0);
  const parts = [
    "open=" + Number(q.open || 0),
    "eligible=" + Number(q.eligible || 0),
    "excluded=" + excluded,
    "held/meta=" + held,
    "blocked-deps=" + blockedByDependency,
    "non-runnable=" + nonRunnable
  ];
  const selected = q.selected_candidate && q.selected_candidate.number
    ? "selected #" + q.selected_candidate.number + (q.selected_candidate.title ? " " + q.selected_candidate.title : "")
    : "";
  if (selected) parts.push(selected);

  const lines = ['<div class="queue-line"><strong>Queue</strong><span>' + escapeText(parts.join(" · ")) + '</span></div>'];
  const skipped = [];
  if (excluded) skipped.push(excluded + " excluded by label/policy");
  if (held) skipped.push(held + " held parent/meta");
  if (blockedByDependency) skipped.push(blockedByDependency + " blocked by open dependencies");
  if (nonRunnable) skipped.push(nonRunnable + " in non-runnable project status");
  if (skipped.length) {
    lines.push('<div class="queue-line"><strong>Skipped</strong><span>' + escapeText(skipped.join(" · ")) + '</span></div>');
  }
  const isIdle = (project.running || 0) === 0;
  let idleReason = isIdle ? (q.idle_reason || "") : "";
  const topSkip = isIdle && q.eligible === 0 && q.top_skipped_reason && !(idleReason || "").includes(q.top_skipped_reason)
    ? q.top_skipped_reason
    : "";
  if (topSkip) {
    idleReason = idleReason ? idleReason + " Top skip: " + topSkip : "Top skip: " + topSkip;
  }
  if (idleReason) {
    lines.push('<div class="queue-line queue-idle"><strong>Idle</strong><span>' + escapeText(idleReason) + '</span></div>');
  }
  return '<div class="queue-snapshot"><div class="label">Queue Snapshot</div>' + lines.join("") + '</div>';
}

function outcomeHTML(project) {
  const o = project.outcome || {};
  const configured = o.configured === true;
  const goal = configured ? (o.goal || "Configured outcome") : "No outcome brief configured";
  const target = o.runtime_target || "-";
  const host = o.runtime_host ? " · " + o.runtime_host : "";
  const health = o.health_state || (configured ? "unknown" : "not_configured");
  const next = o.next_action || (configured ? "Verify runtime health." : "Add an outcome brief to config.");
  const checked = o.health_checked_at ? formatTimestamp(o.health_checked_at) : "-";
  const summary = o.health_summary || "";
  return '<div class="outcome-status"><div class="label">Outcome Status</div>' +
    '<div class="outcome-lines">' +
      '<div class="outcome-line"><strong>Goal</strong> ' + escapeText(goal) + '</div>' +
      '<div class="outcome-line"><strong>Runtime</strong> ' + escapeText(target + host) + '</div>' +
      '<div class="outcome-line"><strong>Health</strong> ' + escapeText(health.replace(/_/g, " ")) + '</div>' +
      '<div class="outcome-line"><strong>Checked</strong> ' + escapeText(checked) + '</div>' +
      (summary ? '<div class="outcome-line"><strong>Signal</strong> ' + escapeText(summary) + '</div>' : "") +
      '<div class="outcome-line"><strong>Next</strong> ' + escapeText(next) + '</div>' +
    '</div></div>';
}

function rawSupervisorAction(action) {
  const raw = String(action || "").trim();
  return raw || "none";
}

function supervisorOperatorSentence(item) {
  if (item && item.operator_sentence) return item.operator_sentence;
  const raw = rawSupervisorAction(item && (item.recommended_action || item.action));
  if (raw === "none") return "Skipped this tick; no safe action was selected.";
  return "Supervisor reported " + raw + "; inspect diagnostics for details.";
}

function renderSupervisor(project) {
  const sup = project.supervisor;
  if (!sup || !sup.has_run || !sup.latest) {
    return '<div class="supervisor"><div class="label">Supervisor</div><div class="empty">No supervisor decision yet.</div></div>';
  }
  const latest = sup.latest;
  const reasons = (latest.stuck_reasons && latest.stuck_reasons.length ? latest.stuck_reasons : latest.reasons || []).slice(0, 2);
  const rawAction = rawSupervisorAction(latest.recommended_action);
  const operatorSentence = supervisorOperatorSentence(latest);
  const summary = latest.summary ? ' · ' + escapeText(latest.summary) : '';
  return '<div class="supervisor">' +
    '<div class="label">Supervisor</div>' +
    '<div class="decision"><strong title="Raw action: ' + escapeText(rawAction) + '">' + escapeText(operatorSentence) + '</strong>' + summary +
    '<br><small><span class="raw-action">raw: ' + escapeText(rawAction) + '</span> · Risk ' + escapeText(latest.risk || "-") +
    (latest.confidence ? " · Confidence " + Number(latest.confidence).toFixed(2) : "") + '</small></div>' +
    (reasons.length ? '<div class="empty">' + reasons.map(escapeText).join("<br>") + '</div>' : "") +
  '</div>';
}

function supervisorWhyText(project) {
  const latest = project && project.supervisor && project.supervisor.latest;
  if (!latest) return "";
  if (latest.summary) return latest.summary;
  const reasons = latest.stuck_reasons && latest.stuck_reasons.length ? latest.stuck_reasons : latest.reasons || [];
  return reasons.length ? reasons[0] : "";
}

function renderProjectWhy(project) {
  const attention = project.attention || [];
  if (attention.length) {
    return '<div class="project-why"><div class="label">Why Attention</div>' +
      attention.map(worker => '<div class="why-item"><strong>' + escapeText(worker.slot || "-") + '</strong> ' +
        escapeText(workerWhyText(worker) || "Needs operator review.") + '</div>').join("") +
      '</div>';
  }
  if ((project.running || 0) === 0 && project.queue_snapshot && project.queue_snapshot.idle_reason) {
    return "";
  }
  const why = supervisorWhyText(project);
  if ((project.running || 0) === 0 && why) {
    return '<div class="project-why"><div class="label">Why Not Running</div>' +
      '<div class="why-item">' + escapeText(why) + '</div></div>';
  }
  return "";
}

function renderWorkers(project) {
  const workers = project.active || [];
  if (!workers.length) {
    return '<div class="workers"><div class="label">Latest Sessions</div><div class="empty">No worker sessions in this snapshot.</div></div>';
  }
  const visible = workers.slice(0, 5);
  const hidden = Math.max(0, workers.length - visible.length);
  return '<div class="workers"><div class="label">Latest Sessions</div><table>' +
    visible.map(worker => '<tr>' +
      '<td class="project-worker-slot">' + escapeText(worker.slot) + '</td>' +
      '<td class="project-worker-status"><span class="' + statusClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td class="project-worker-issue" title="' + escapeText(issueSummaryText(worker)) + '">' + issueSummaryHTML(worker) + workerWhyHTML(worker) + '</td>' +
      '<td class="project-worker-runtime">' + escapeText(worker.runtime || "-") + '</td>' +
    '</tr>').join("") +
  '</table>' + (hidden ? '<div class="more-row">+' + escapeText(hidden) + ' more in Fleet Workers history</div>' : '') + '</div>';
}

function renderProjectActions(project) {
  if (project.read_only === true) {
    return '<div class="project-actions"><div class="action-note">Write controls are disabled in read-only mode.</div></div>';
  }
  return '<div class="project-actions"><div class="label">Approval-gated controls</div>' +
    renderActions(project.actions || [], { details: false }) + '</div>';
}

function projectFreshnessHTML(project) {
  const freshness = project.freshness || {};
  const age = freshness.snapshot_age ? "Snapshot " + freshness.snapshot_age + " ago" : "No snapshot yet";
  const details = [];
  if (freshness.state_updated_at) details.push("State " + formatTimestamp(freshness.state_updated_at));
  if (freshness.log_updated_at) details.push("Logs " + formatTimestamp(freshness.log_updated_at));
  const title = freshness.reason || details.join(" · ") || age;
  return '<div class="freshness" title="' + escapeText(title) + '"><span>' + escapeText(age) + '</span></div>';
}

function projectBadgesHTML(project) {
  const badges = [];
  if (projectIsUnconfigured(project)) {
    badges.push('<span class="badge badge-setup">setup</span>');
  }
  if (project.error) {
    badges.push('<span class="badge badge-error">State error</span>');
  }
  if (project.freshness && project.freshness.stale) {
    const threshold = formatDurationSeconds(project.freshness.stale_after_seconds);
    badges.push('<span class="badge badge-stale">Stale' + (threshold ? ' &gt;' + escapeText(threshold) : '') + '</span>');
  }
  return badges.length ? '<div class="badges">' + badges.join("") + '</div>' : '';
}

function projectClass(project) {
  let cls = "project";
  if (projectIsUnconfigured(project)) cls += " project-unconfigured";
  if (project.error) cls += " project-error";
  if (project.freshness && project.freshness.stale) cls += " project-stale";
  return cls;
}

function projectHeaderHTML(project, rightHTML) {
  return '<div class="project-head"><div class="project-head-main"><h2>' + escapeText(project.name) + '</h2><div class="repo">' +
    escapeText(project.repo || project.config_path || "") + '</div>' + projectFreshnessHTML(project) + '</div>' +
    '<div class="project-head-side">' + (rightHTML || "") + projectBadgesHTML(project) + '</div></div>';
}

function renderProject(project) {
  if (project.error) {
    return '<article class="' + projectClass(project) + '">' + projectHeaderHTML(project, "") +
      '<div class="error">State error: ' + escapeText(project.error) + '</div></article>';
  }
  const failed = countFailed(project);
  const links = '<div class="links">' + linkHTML(project.dashboard_url, "Dashboard") + " " +
    linkHTML(project.repo ? "https://github.com/" + project.repo : "", "GitHub") + '</div>';
  return '<article class="' + projectClass(project) + '">' +
    projectHeaderHTML(project, links) +
    '<div class="metric-row">' +
      '<div class="metric"><strong>' + escapeText(project.running || 0) + " / " + escapeText(project.max_parallel || 0) + '</strong><span>Running</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.pr_open || 0) + '</strong><span>PR open</span></div>' +
      '<div class="metric"><strong>' + escapeText(failed) + '</strong><span>Failed</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.sessions || 0) + '</strong><span>Sessions</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.needs_attention || 0) + '</strong><span>Attention</span></div>' +
	'</div>' +
	outcomeHTML(project) +
	queueSnapshotHTML(project) +
    renderProjectWhy(project) +
    renderProjectActions(project) +
    renderSupervisor(project) +
    renderWorkers(project) +
  '</article>';
}

function renderProjectOverview() {
  const projects = visibleProjects();
  const total = (fleetState.projects || []).length;
  const attention = projects.reduce((sum, project) => sum + Number(project.needs_attention || 0), 0);
  const running = projects.reduce((sum, project) => sum + Number(project.running || 0), 0);
  const filtered = projects.length === total ? "" : " shown from " + total;
  projectSummaryEl.textContent = projects.length + " project" + (projects.length === 1 ? "" : "s") + filtered +
    " · " + running + " running · " + attention + " attention";
  projectsEl.innerHTML = projects.length
    ? projects.map(renderProject).join("")
    : '<div class="empty">No project diagnostics match the project search.</div>';
}

function refreshWorkersFromControls() {
  updateQueryState();
  renderFleetWorkers();
}

function clearWorkerProjectScope() {
  if (fleetState.selectedProject === "all") return;
  fleetState.selectedProject = "all";
  updateQueryState();
  renderFleetWorkers();
}

workerProjectResetEl.addEventListener("click", clearWorkerProjectScope);

projectFilterEl.addEventListener("input", () => {
  fleetState.projectQuery = projectFilterEl.value.trim();
  renderProjectRail();
  renderProjectOverview();
});

workerFilterEl.addEventListener("input", () => {
  fleetState.filters.query = workerFilterEl.value.trim();
  refreshWorkersFromControls();
});

scopeFilterEl.addEventListener("change", () => {
  fleetState.filters.scope = scopeFilterEl.value || "operator";
  refreshWorkersFromControls();
});

statusFilterEl.addEventListener("change", () => {
  fleetState.filters.status = statusFilterEl.value || "all";
  refreshWorkersFromControls();
});

backendFilterEl.addEventListener("change", () => {
  fleetState.filters.backend = backendFilterEl.value || "all";
  refreshWorkersFromControls();
});

prFilterEl.addEventListener("change", () => {
  fleetState.filters.pr = prFilterEl.value || "all";
  refreshWorkersFromControls();
});

workerSortEl.addEventListener("change", () => {
  const nextSort = validSortKeys.has(workerSortEl.value) ? workerSortEl.value : "status";
  if (nextSort !== fleetState.sortKey) {
    fleetState.sortKey = nextSort;
    fleetState.sortDir = defaultSortDirections[nextSort] || "asc";
  }
  syncFilterControls();
  updateQueryState();
  renderFleetWorkers();
});

sortDirectionEl.addEventListener("click", () => {
  fleetState.sortDir = fleetState.sortDir === "desc" ? "asc" : "desc";
  syncFilterControls();
  updateQueryState();
  renderFleetWorkers();
});

function applyFleetData(data) {
  fleetState.readOnly = data.read_only !== false;
  fleetState.refreshedAt = data.refreshed_at || "";
  fleetState.projects = data.projects || [];
  fleetState.workers = fleetWorkersFromData(data);
  fleetState.approvals = approvalsFromData(data);
  fleetState.attention = attentionFromData(data);
  fleetState.verdict = data.verdict || null;
  if (fleetState.selectedWorkerKey && !selectedWorker()) {
    fleetState.selectedWorkerKey = "";
    fleetState.detail = null;
  }
  const controlMode = fleetState.readOnly ? "read-only controls disabled" : "controls require approval configuration";
  const summary = data.summary || {};
  const alerts = [];
  if (summary.stale) alerts.push(summary.stale + " stale");
  if (summary.errors) alerts.push(summary.errors + " error" + (summary.errors === 1 ? "" : "s"));
  subtitleEl.textContent = "Last refresh " + formatTimestamp(fleetState.refreshedAt) + " · " +
    fleetState.projects.length + " configured project" + (fleetState.projects.length === 1 ? "" : "s") + " · " + controlMode +
    (alerts.length ? " · " + alerts.join(" · ") : "");
  renderFilterOptions();
  syncFilterControls();
  renderFleetVerdict(fleetState.verdict);
  renderStats(summary);
  renderProjectRail();
  renderProjectOverview();
  renderApprovalInbox();
  renderAttentionInbox();
  renderFleetWorkers();
  renderWorkerDetail(fleetState.detail);
}

async function loadFleet() {
  try {
    const response = await fetch("/api/v1/fleet", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    applyFleetData(await response.json());
  } catch (err) {
    subtitleEl.textContent = "Fleet API error" + (fleetState.refreshedAt ? " · Last successful refresh " + formatTimestamp(fleetState.refreshedAt) : "");
    renderFleetVerdict({ tone: "daemon-down", sentence: "Fleet API error. Supervisor heartbeat unavailable; worker state and attention state could not be confirmed." });
    approvalSummaryEl.textContent = "Fleet API error";
    approvalListEl.innerHTML = '<div class="error">Unable to load approval inbox.</div>';
    attentionSummaryEl.textContent = "Fleet API error";
    attentionListEl.innerHTML = '<div class="error">Unable to load attention inbox.</div>';
    workerSummaryEl.textContent = "Fleet API error";
    fleetWorkersEl.innerHTML = '<tr><td colspan="9" class="empty">Unable to load fleet workers.</td></tr>';
    projectSummaryEl.textContent = "Fleet API error";
    projectsEl.innerHTML = '<div class="error">' + escapeText(err.message) + '</div>';
  }
}

function parseInitialFleetData() {
  if (!initialStateEl || !initialStateEl.textContent.trim()) return null;
  try {
    return JSON.parse(initialStateEl.textContent);
  } catch (_) {
    return null;
  }
}

const initialFleetData = parseInitialFleetData();
if (initialFleetData) {
  applyFleetData(initialFleetData);
} else {
  renderFilterOptions();
  syncFilterControls();
}
loadFleet();
setInterval(loadFleet, 3000);
setInterval(loadWorkerDetail, 2000);
