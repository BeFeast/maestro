const projectsEl = document.getElementById("projects");
const projectSummaryEl = document.getElementById("project-summary");
const projectRailBodyEl = document.getElementById("project-rail-body");
const projectRailSummaryEl = document.getElementById("project-rail-summary");
const projectFilterEl = document.getElementById("project-filter");
const projectCountAllEl = document.getElementById("project-count-all");
const projectCountRunningEl = document.getElementById("project-count-running");
const projectCountAttentionEl = document.getElementById("project-count-attention");
const projectCountIdleEl = document.getElementById("project-count-idle");
const statsEl = document.getElementById("stats");
const subtitleEl = document.getElementById("subtitle");
const fleetVerdictEl = document.getElementById("fleet-verdict");
const approvalListEl = document.getElementById("approval-list");
const approvalSummaryEl = document.getElementById("approval-summary");
const approvalAuditLinkEl = document.getElementById("approval-audit-link");
const attentionListEl = document.getElementById("attention-list");
const attentionSummaryEl = document.getElementById("attention-summary");
const needsYouRailEl = document.getElementById("needs-you-rail");
const needsYouListEl = document.getElementById("needs-you-list");
const needsYouSummaryEl = document.getElementById("needs-you-summary");
const needsYouAuditLinkEl = document.getElementById("needs-you-audit-link");
const fleetWorkersEl = document.getElementById("fleet-workers-body");
const fleetWorkersShellEl = document.getElementById("fleet-workers-shell");
const workerShellSummaryEl = document.getElementById("worker-shell-summary");
const workerSummaryEl = document.getElementById("worker-summary");
const workerProjectResetEl = document.getElementById("worker-project-reset");
const workerControlsEl = document.getElementById("worker-controls");
const workerDetailEl = document.getElementById("worker-detail");
const workerDetailCloseEl = document.getElementById("worker-detail-close");
const workerDetailBackdropEl = document.getElementById("worker-detail-backdrop");
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
const fleetRefreshEl = document.getElementById("fleet-refresh");
const searchTriggerEl = document.getElementById("fleet-search-trigger");
const searchDialogEl = document.getElementById("fleet-search-dialog");
const searchBackdropEl = document.getElementById("fleet-search-backdrop");
const searchInputEl = document.getElementById("fleet-search-input");
const searchResultsEl = document.getElementById("fleet-search-results");
const searchSummaryEl = document.getElementById("fleet-search-summary");
const searchCloseEl = document.getElementById("fleet-search-close");
const projectDiagnosticsEl = document.getElementById("project-diagnostics");
const expandedProjectStorageKey = "maestro.fleet.expandedProject";

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
  expandedProject: readStoredExpandedProject(),
  requestedWorker: "",
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
  summary: {},
  detail: null,
  operatorBrief: null,
  verdict: null,
  refreshedAt: "",
  search: {
    open: false,
    query: "",
    activeIndex: 0,
    results: []
  }
};

loadStateFromQuery();

function readStoredExpandedProject() {
  try {
    return window.localStorage.getItem(expandedProjectStorageKey) || "";
  } catch (_) {
    return "";
  }
}

function writeStoredExpandedProject(projectName) {
  try {
    if (projectName) window.localStorage.setItem(expandedProjectStorageKey, projectName);
    else window.localStorage.removeItem(expandedProjectStorageKey);
  } catch (_) {}
}

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
  return active + " " + pluralize(historicalCount, "historical approval") + " moved to audit history.";
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

function supervisorRiskLabel(risk) {
  switch (String(risk || "").trim()) {
  case "safe": return "Safe recommendation";
  case "mutating": return "Mutating action";
  case "approval_gated": return "Approval required";
  default:
    return String(risk || "-").replace(/_/g, " ");
  }
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
      '<div class="approval-risk"><span>' + escapeText(supervisorRiskLabel(approval.risk || "-")) + '</span></div>' +
      '<div class="approval-summary">' + escapeText(approval.summary || "No summary recorded.") + '</div></div>' +
  '</article>';
}

function renderApprovalInbox() {
  if (approvalListEl && approvalListEl.closest("section")) {
    approvalListEl.closest("section").hidden = true;
  }
  const approvals = fleetState.approvals || [];
  const counts = approvals.reduce((acc, approval) => {
    const status = approval.status || "unknown";
    acc[status] = (acc[status] || 0) + 1;
    return acc;
  }, {});
  const pending = approvals.filter(isPendingApproval);
  const historical = approvals.filter(approval => !isPendingApproval(approval));
  approvalListEl.classList.toggle("approval-list-compact", pending.length === 0);
  approvalSummaryEl.textContent = approvalInboxSummaryText(pending.length, historical.length);
  if (approvalAuditLinkEl) {
    approvalAuditLinkEl.hidden = historical.length === 0;
  }

  const activeHTML = pending.length
    ? '<div class="approval-active-list">' + pending.map(approvalCardHTML).join("") + '</div>'
    : '<div class="empty approval-empty approval-active-empty">No pending approvals need review.</div>';
  const historyHTML = historical.length
    ? '<div class="approval-history-link-card"><strong>Audit history</strong><span>' + escapeText(approvalHistoryCountText(counts, historical.length)) + '</span><a href="/approvals/audit">Open full approval audit</a></div>'
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

function workerQueryValue(worker) {
  if (!worker) return "";
  return (worker.project_name || "") + "/" + (worker.slot || "");
}

function selectedWorkerQueryValue() {
  const worker = selectedWorker();
  return worker ? workerQueryValue(worker) : "";
}

function resolveWorkerQuery(value) {
  const raw = String(value || "").trim();
  if (!raw) return null;
  const parts = raw.includes("\u001f") ? raw.split("\u001f") : raw.split("/");
  if (parts.length >= 2) {
    const projectName = parts.shift();
    const slot = parts.join("/");
    const byProjectAndSlot = (fleetState.workers || []).find(worker =>
      (worker.project_name || "") === projectName && (worker.slot || "") === slot
    );
    if (byProjectAndSlot) return byProjectAndSlot;
  }
  return (fleetState.workers || []).find(worker => (worker.slot || "") === raw) || null;
}

function setWorkerDrawerOpen(open) {
  document.body.classList.toggle("worker-drawer-open", open);
  workerDetailEl.setAttribute("aria-hidden", open ? "false" : "true");
  workerDetailBackdropEl.hidden = !open;
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
  fleetState.requestedWorker = normalizeParam(params.get("worker"), "");
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
  setQueryParam(params, "worker", selectedWorkerQueryValue(), "");
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

function compactSearchText(value) {
  return normalizedSearchText(value).replace(/[^a-z0-9]+/g, "");
}

function searchTerms(value) {
  return normalizedSearchText(value).split(/\s+/).filter(Boolean);
}

function searchTokens(value) {
  return normalizedSearchText(value).split(/[^a-z0-9#]+/).map(compactSearchText).filter(Boolean);
}

function searchNumberAliases(label, number) {
  const value = Number(number || 0);
  if (!Number.isFinite(value) || value <= 0) return [];
  const text = String(value);
  const prefix = String(label || "").trim();
  return [text, "#" + text, prefix + " " + text, prefix + " #" + text, prefix + text];
}

function searchMetaText(parts) {
  return parts.map(value => String(value ?? "").trim()).filter(Boolean).join(" · ");
}

function makeFleetSearchResult(input) {
  const terms = input.terms || [];
  const text = [input.kind, input.title, input.meta, input.project, input.slot, input.url].concat(terms).map(normalizedSearchText).join(" ");
  return {
    id: input.id,
    kind: input.kind,
    title: input.title,
    meta: input.meta || "",
    action: input.action || "url",
    url: input.url || "",
    project: input.project || "",
    slot: input.slot || "",
    rank: Number(input.rank || 0),
    searchText: text,
    searchCompact: compactSearchText(text),
    tokens: new Set(searchTokens(text))
  };
}

function addFleetSearchResult(results, seen, input) {
  if (!input || !input.id || seen.has(input.id)) return;
  seen.add(input.id);
  results.push(makeFleetSearchResult(input));
}

function searchProjectURL(project) {
  return (project && project.dashboard_url) || githubRepoURL(project && project.repo) || "";
}

function buildFleetSearchIndex() {
  const results = [];
  const seen = new Set();
  for (const project of fleetState.projects || []) {
    const name = project.name || "project";
    const url = searchProjectURL(project);
    addFleetSearchResult(results, seen, {
      id: "project:" + name,
      kind: "Project",
      title: name,
      meta: searchMetaText([project.repo, projectStateLabel(project), project.operator_state && project.operator_state.summary]),
      action: url ? "url" : "project",
      url,
      project: name,
      rank: 40,
      terms: ["project", "slug", project.config_path, projectSearchText(project)]
    });
    if (project.dashboard_url) {
      addFleetSearchResult(results, seen, {
        id: "dashboard:" + name,
        kind: "Dashboard",
        title: name + " dashboard",
        meta: searchMetaText([project.repo, project.dashboard_url]),
        action: "url",
        url: project.dashboard_url,
        project: name,
        rank: 35,
        terms: ["dashboard", "project dashboard", name, project.repo]
      });
    }
  }

  for (const worker of fleetState.workers || []) {
    const project = worker.project_name || "";
    const slot = worker.slot || "";
    const issueAliases = searchNumberAliases("issue", worker.issue_number);
    const prAliases = searchNumberAliases("pr", worker.pr_number);
    const workerRank = (worker.needs_attention ? 70 : 0) + (worker.live ? 55 : 20);
    addFleetSearchResult(results, seen, {
      id: "session:" + project + ":" + slot,
      kind: "Session",
      title: searchMetaText([project, slot]) || "Worker session",
      meta: searchMetaText([
        worker.issue_number ? "Issue #" + worker.issue_number : "No issue",
        worker.pr_number ? "PR #" + worker.pr_number : "No PR",
        statusLabel(worker)
      ]),
      action: "worker",
      project,
      slot,
      rank: workerRank,
      terms: ["worker", "session", workerSearchText(worker)].concat(issueAliases, prAliases)
    });
    if (worker.issue_number) {
      addFleetSearchResult(results, seen, {
        id: "issue:" + project + ":" + worker.issue_number + ":" + slot,
        kind: "Issue",
        title: "Issue #" + worker.issue_number,
        meta: searchMetaText([project, slot, worker.issue_title]),
        action: worker.issue_url ? "url" : "worker",
        url: worker.issue_url || "",
        project,
        slot,
        rank: workerRank - 5,
        terms: [worker.issue_title, workerSearchText(worker)].concat(issueAliases)
      });
    }
    if (worker.pr_number) {
      addFleetSearchResult(results, seen, {
        id: "pr:" + project + ":" + worker.pr_number + ":" + slot,
        kind: "PR",
        title: "PR #" + worker.pr_number,
        meta: searchMetaText([project, slot, worker.issue_number ? "Issue #" + worker.issue_number : ""]),
        action: worker.pr_url ? "url" : "worker",
        url: worker.pr_url || "",
        project,
        slot,
        rank: workerRank - 10,
        terms: [worker.issue_title, workerSearchText(worker)].concat(prAliases)
      });
    }
  }

  for (const approval of fleetState.approvals || []) {
    const targets = [];
    if (approval.issue_number) targets.push("Issue #" + approval.issue_number);
    if (approval.pr_number) targets.push("PR #" + approval.pr_number);
    if (approval.session) targets.push("Session " + approval.session);
    const project = approval.project_name || "";
    addFleetSearchResult(results, seen, {
      id: "approval:" + (approval.id || targets.join(":")),
      kind: "Approval",
      title: targets.length ? "Approval " + targets.join(" / ") : "Approval " + (approval.id || "target"),
      meta: searchMetaText([project, actionLabel(approval.action), approval.status, approval.summary]),
      action: approval.pr_url || approval.issue_url ? "url" : (approval.session ? "worker" : "project"),
      url: approval.pr_url || approval.issue_url || approval.dashboard_url || "",
      project,
      slot: approval.session || "",
      rank: approval.status === "pending" ? 65 : 15,
      terms: [approval.id, approval.decision_id, approval.summary, approval.action]
        .concat(searchNumberAliases("issue", approval.issue_number), searchNumberAliases("pr", approval.pr_number))
    });
  }
  return results;
}

function fuzzySearchMatch(haystack, needle) {
  if (!needle) return true;
  let index = 0;
  for (const ch of haystack) {
    if (ch === needle[index]) index++;
    if (index === needle.length) return true;
  }
  return false;
}

function scoreFleetSearchResult(result, query) {
  const terms = searchTerms(query);
  if (!terms.length) return result.rank;
  let score = result.rank;
  for (const term of terms) {
    const normalized = normalizedSearchText(term);
    const compact = compactSearchText(term);
    if (!compact) continue;
    if (result.tokens.has(compact)) {
      score += 100;
    } else if (result.searchText.includes(normalized)) {
      score += 75;
    } else if (result.searchCompact.includes(compact)) {
      score += 55;
    } else if (fuzzySearchMatch(result.searchCompact, compact)) {
      score += 20;
    } else {
      return -1;
    }
  }
  return score;
}

function searchFleetResults(query) {
  const index = buildFleetSearchIndex();
  const limit = searchTerms(query).length ? 12 : 10;
  return index.map(result => ({ result, score: scoreFleetSearchResult(result, query) }))
    .filter(entry => entry.score >= 0)
    .sort((left, right) => {
      if (left.score !== right.score) return right.score - left.score;
      return compareText(left.result.title, right.result.title);
    })
    .slice(0, limit)
    .map(entry => entry.result);
}

function searchResultID(index) {
  return "fleet-search-result-" + index;
}

function searchResultActionText(result) {
  if (result.action === "worker") return "Opens worker detail";
  if (result.action === "project") return "Scopes worker table";
  return "Opens link";
}

function searchResultHTML(result, index) {
  const active = index === fleetState.search.activeIndex;
  const cls = "fleet-search-result" + (active ? " is-active" : "");
  return '<button type="button" id="' + searchResultID(index) + '" class="' + cls + '" role="option" aria-selected="' + (active ? "true" : "false") + '" data-search-result="' + index + '">' +
    '<span class="fleet-search-kind">' + escapeText(result.kind) + '</span>' +
    '<span class="fleet-search-copy"><strong>' + escapeText(result.title) + '</strong>' +
      '<span>' + escapeText(result.meta || searchResultActionText(result)) + '</span></span>' +
    '<span class="fleet-search-target">' + escapeText(searchResultActionText(result)) + '</span>' +
  '</button>';
}

function renderSearchPalette() {
  if (!searchDialogEl || !searchBackdropEl || !searchResultsEl || !searchSummaryEl) return;
  const search = fleetState.search;
  searchDialogEl.hidden = !search.open;
  searchBackdropEl.hidden = !search.open;
  document.body.classList.toggle("fleet-search-open", search.open);
  if (!search.open) return;

  search.results = searchFleetResults(search.query);
  if (search.activeIndex >= search.results.length) {
    search.activeIndex = Math.max(0, search.results.length - 1);
  }
  if (searchInputEl && searchInputEl.value.trim() !== search.query) {
    searchInputEl.value = search.query;
  }
  const hasQuery = searchTerms(search.query).length > 0;
  searchSummaryEl.textContent = search.results.length
    ? (hasQuery ? search.results.length + " result" + (search.results.length === 1 ? "" : "s") : "Top loaded fleet results")
    : (hasQuery ? "No matching loaded fleet data" : "Type to search projects, sessions, issues, PRs, and dashboards");
  searchResultsEl.innerHTML = search.results.length
    ? search.results.map(searchResultHTML).join("")
    : '<div class="fleet-search-empty" role="status">No matching projects, sessions, issues, or PRs are loaded in this fleet snapshot.</div>';

  if (searchInputEl) {
    if (search.results.length) searchInputEl.setAttribute("aria-activedescendant", searchResultID(search.activeIndex));
    else searchInputEl.removeAttribute("aria-activedescendant");
  }
  searchResultsEl.querySelectorAll("button[data-search-result]").forEach(button => {
    button.addEventListener("click", () => {
      const result = search.results[Number(button.dataset.searchResult || 0)];
      selectFleetSearchResult(result);
    });
  });
}

function openSearchPalette() {
  if (!searchDialogEl) return;
  fleetState.search.open = true;
  fleetState.search.activeIndex = 0;
  renderSearchPalette();
  window.setTimeout(() => {
    if (searchInputEl) {
      searchInputEl.focus();
      searchInputEl.select();
    }
  }, 0);
}

function closeSearchPalette(returnFocus) {
  fleetState.search.open = false;
  renderSearchPalette();
  if (returnFocus !== false && searchTriggerEl) searchTriggerEl.focus();
}

function isSearchShortcut(event) {
  return !event.defaultPrevented && (event.metaKey || event.ctrlKey) && !event.altKey && String(event.key || "").toLowerCase() === "k";
}

function scrollSearchActiveResultIntoView() {
  const active = document.getElementById(searchResultID(fleetState.search.activeIndex));
  if (active) active.scrollIntoView({ block: "nearest" });
}

function moveSearchActive(delta) {
  const count = fleetState.search.results.length;
  if (!count) return;
  fleetState.search.activeIndex = (fleetState.search.activeIndex + delta + count) % count;
  renderSearchPalette();
  scrollSearchActiveResultIntoView();
}

function openSearchURL(url) {
  const target = String(url || "").trim();
  if (!target) return false;
  window.open(target, "_blank", "noopener,noreferrer");
  return true;
}

function scopeSearchProject(projectName) {
  if (!projectName) return false;
  fleetState.selectedProject = projectName;
  updateQueryState();
  renderFleetWorkers();
  document.querySelector(".fleet-workers")?.scrollIntoView({ block: "start", behavior: "smooth" });
  return true;
}

function selectFleetSearchResult(result) {
  if (!result) return;
  closeSearchPalette(false);
  if (result.action === "worker" && result.project && result.slot) {
    selectWorker(result.project, result.slot);
    return;
  }
  if (result.action === "project" && scopeSearchProject(result.project)) return;
  if (openSearchURL(result.url)) return;
  if (result.project && result.slot) {
    selectWorker(result.project, result.slot);
    return;
  }
  scopeSearchProject(result.project);
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
  const displayed = displayStatus(worker);
  const terminal = new Set(["done", "failed", "dead", "conflict_failed", "retry_exhausted"]);
  if (terminal.has(displayed) || terminal.has(worker.status || "")) return false;
  if (worker.live === true) return true;
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
  if (attentionListEl && attentionListEl.closest("section")) {
    attentionListEl.closest("section").hidden = true;
  }
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

function needsYouItemHTML(item) {
  if (item.kind === "approval") {
    const approval = item.approval;
    return '<article class="needs-you-item needs-you-approval">' +
      '<div class="needs-you-kicker">' + escapeText(approval.project_name || "-") + ' · approval</div>' +
      '<div class="needs-you-headline">' + escapeText(actionLabel(approval.action || "-")) + '</div>' +
      '<div class="needs-you-copy">' + escapeText(approval.summary || "Supervisor approval needs review.") + '</div>' +
    '</article>';
  }
  const worker = item.worker;
  return '<article class="needs-you-item needs-you-worker" data-project="' + escapeText(worker.project_name || "") + '" data-slot="' + escapeText(worker.slot || "") + '" tabindex="0">' +
    '<div class="needs-you-kicker">' + escapeText(worker.project_name || "-") + ' · slot ' + escapeText(worker.slot || "-") + '</div>' +
    '<div class="needs-you-headline">' + issueSummaryHTML(worker) + '</div>' +
    '<div class="needs-you-copy"><strong>Why:</strong> ' + escapeText(attentionReasonText(worker)) + '</div>' +
  '</article>';
}

function renderNeedsYouRail() {
  const pendingApprovals = (fleetState.approvals || []).filter(isPendingApproval);
  const attentionWorkers = (fleetState.attention || []).filter(worker => worker.needs_attention);
  const items = pendingApprovals.map(approval => ({ kind: "approval", approval }))
    .concat(attentionWorkers.map(worker => ({ kind: "worker", worker })));

  if (!needsYouRailEl || !needsYouListEl || !needsYouSummaryEl) return;
  if (!items.length) {
    needsYouRailEl.hidden = true;
    return;
  }

  needsYouRailEl.hidden = false;
  if (needsYouAuditLinkEl) needsYouAuditLinkEl.hidden = pendingApprovals.length === 0;
  needsYouSummaryEl.textContent = items.length === 1
    ? "1 operator item is waiting."
    : items.length + " operator items are waiting.";
  needsYouListEl.innerHTML = items.map(needsYouItemHTML).join("");
  needsYouListEl.querySelectorAll(".needs-you-worker[data-slot]").forEach(card => {
    card.addEventListener("click", () => selectWorker(card.dataset.project || "", card.dataset.slot || ""));
    card.addEventListener("keydown", event => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        selectWorker(card.dataset.project || "", card.dataset.slot || "");
      }
    });
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

function fleetBriefToneClass(tone) {
  switch (String(tone || "").trim()) {
  case "healthy":
  case "busy":
  case "attention":
  case "daemon-down":
    return tone;
  case "error":
    return "daemon-down";
  default:
    return "attention";
  }
}

function renderFleetVerdict(brief, verdict) {
  const source = brief && brief.sentence ? brief : verdict;
  const tone = fleetBriefToneClass(source && source.tone);
  const summary = fleetState.summary || {};
  const running = Number(summary.running || 0);
  const attention = Number(summary.needs_attention || 0);
  const approvals = Number(summary.approvals_pending || 0);
  let headline = "Supervisor status unavailable.";
  if (attention > 0 || approvals > 0) {
    const parts = [];
    if (attention > 0) parts.push(pluralize(attention, "item") + " need attention");
    if (approvals > 0) parts.push(pluralize(approvals, "approval") + " wait for review");
    headline = parts.join(" · ") + ".";
  } else if (running > 0) {
    headline = pluralize(running, "worker") + " " + (running === 1 ? "is" : "are") + " running. No operator action is pending.";
  } else {
    headline = "All quiet. No operator action is pending.";
  }
  const metaParts = [];
  if (fleetState.refreshedAt) metaParts.push("Last sync " + formatTimestamp(fleetState.refreshedAt));
  if (attention > 0 || approvals > 0) {
    metaParts.push("Review Needs You below");
  } else if (running > 0) {
    metaParts.push("Workers are in flight");
  } else {
    metaParts.push("Supervisor heartbeat is current");
  }
  if (summary.projects) metaParts.push(pluralize(Number(summary.projects || 0), "project") + " monitored");
  if (brief && brief.project) metaParts.push("Focus: " + brief.project);
  fleetVerdictEl.className = "fleet-verdict verdict-" + tone;
  fleetVerdictEl.innerHTML = '<div class="fleet-verdict-copy">' +
    '<div class="fleet-verdict-headline">' + escapeText(headline) + '</div>' +
    '<div class="fleet-verdict-meta">' + escapeText(metaParts.join(" · ")) + '</div>' +
  '</div>';
}

function renderStats(summary) {
  const projects = Number(summary.projects || 0);
  const running = Number(summary.running || 0);
  const prOpen = Number(summary.pr_open || 0);
  const monitoring = Number(summary.monitoring_pr || 0);
  const failed = Number(summary.failed || 0);
  const attention = Number(summary.needs_attention || 0) + Number(summary.approvals_pending || 0) + Number(summary.errors || 0) +
    Number(summary.stale || 0) + Number(summary.dispatch_failures || 0) + Number(summary.outcome_drift || 0) + Number(summary.no_eligible_issues || 0);
  const throughputMerged = Number(summary.throughput_merged_7d || 0);
  const throughputDaily = Array.isArray(summary.throughput_daily_7d)
    ? summary.throughput_daily_7d.map(value => Number(value || 0))
    : [];
  const items = [
    { label: "Running", value: running, suffix: projects ? "of " + projects : "", note: projects ? "worker slots" : "No configured projects" },
    { label: "PRs in flight", value: prOpen, note: monitoring ? monitoring + " monitored" : (prOpen ? pluralize(prOpen, "open PR") : "none") },
    { label: "Failed", value: failed, note: failed ? "needs review" : "clear" },
    { label: "Attention", value: attention, note: attention ? "needs you" : "quiet" },
    { label: "Issue throughput", value: throughputMerged, note: "merged · last 7d", sparkline: throughputDaily }
  ];
  statsEl.innerHTML = items.map(item =>
    '<article class="stat"><span class="stat-label">' + escapeText(item.label) + '</span>' +
      '<div class="stat-value"><strong>' + escapeText(item.value) + '</strong>' +
      (item.suffix ? '<span class="stat-suffix">' + escapeText(item.suffix) + '</span>' : '') + '</div>' +
      (item.sparkline ? renderStatSparkline(item.sparkline) : '') +
      '<span class="stat-note">' + escapeText(item.note) + '</span></article>'
  ).join("");
}

function renderStatSparkline(values) {
  const bars = Array.isArray(values) ? values.map(value => Number(value || 0)) : [];
  if (!bars.length) return "";
  const max = Math.max(...bars, 1);
  const empty = bars.every(value => value === 0);
  const columns = bars.map((value, index) => {
    const height = Math.max(6, Math.round((value / max) * 28));
    return '<span class="stat-sparkline-bar" style="height:' + height + 'px" title="' +
      escapeText(throughputDayLabel(index, bars.length) + ': ' + pluralize(value, 'merged PR')) + '"></span>';
  }).join("");
  return '<div class="stat-sparkline' + (empty ? ' stat-sparkline-empty' : '') + '" role="img" aria-label="Merged PR throughput for the last 7 days">' + columns + '</div>';
}

function throughputDayLabel(index, total) {
  const daysAgo = Math.max(0, Number(total || 0) - Number(index || 0) - 1);
  if (daysAgo === 0) return "today";
  if (daysAgo === 1) return "yesterday";
  return String(daysAgo) + " days ago";
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

function setProjectCount(el, value) {
  if (el) el.textContent = String(value);
}

function updateProjectSegmentCounts(projects) {
  const items = projects || [];
  const activeKinds = new Set(["working", "monitoring_pr", "pending_dispatch"]);
  const running = items.filter(project => activeKinds.has(projectStateKey(project))).length;
  const attention = items.reduce((sum, project) => sum + Number(project.needs_attention || 0), 0);
  const idle = items.filter(project => !activeKinds.has(projectStateKey(project)) && Number(project.needs_attention || 0) === 0).length;
  setProjectCount(projectCountAllEl, items.length);
  setProjectCount(projectCountRunningEl, running);
  setProjectCount(projectCountAttentionEl, attention);
  setProjectCount(projectCountIdleEl, idle);
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
    return '<span class="pill rail-state-unconfigured">setup</span>' +
      '<div class="rail-subline" title="No outcome brief configured">No outcome brief configured</div>';
  }

  const key = projectStateKey(project);
  const operator = project.operator_state || {};
  const summary = String(operator.summary || ((project.running || 0) + '/' + (project.max_parallel || 0) + ' worker process(es) running.'));
  return '<span class="pill rail-state-' + cssToken(key) + '">' + escapeText(projectStateLabel(project)) + '</span>' +
    '<div class="rail-subline" title="' + escapeText(summary) + '">' + escapeText(summary) + '</div>';
}

function projectQueueRailHTML(project) {
  const q = project.queue_snapshot;
  if (!q) return '<span class="empty">No queue snapshot</span>';
  const ready = Number(q.eligible || 0);
  const held = Number(q.held || q.held_issues || 0);
  const open = Number(q.open || 0);
  const selected = q.selected_candidate && q.selected_candidate.number
    ? "selected #" + q.selected_candidate.number
    : "";
  return '<div class="rail-mainline">' + escapeText(ready ? ready + " ready" : "0 ready") + '</div>' +
    '<div class="rail-subline" title="' + escapeText(String(q.idle_reason || selected || "")) + '">' +
      escapeText(held ? held + " held · " + open + " open" : (selected || open + " open")) +
    '</div>';
}

function projectPRRailHTML(project) {
  if ((project.pr_open || 0) === 0) return '<span class="empty">—</span>';
  const workers = (fleetState.workers || []).filter(worker => worker.project_name === project.name && worker.pr_number);
  const seen = new Set();
  const links = [];
  for (const worker of workers) {
    if (!worker.live || displayStatus(worker) === "done" || worker.status === "done") continue;
    if (seen.has(worker.pr_number)) continue;
    seen.add(worker.pr_number);
    links.push(linkHTML(worker.pr_url || (project.repo ? 'https://github.com/' + project.repo + '/pull/' + worker.pr_number : ''), 'PR #' + worker.pr_number));
    if (links.length >= 3) break;
  }
  const fallback = !links.length && githubPullsURL(project.repo) ? [linkHTML(githubPullsURL(project.repo), 'Open PRs')] : [];
  return '<div class="rail-mainline">' + escapeText(project.pr_open || 0) + ' open</div>' +
    '<div class="rail-links">' + links.concat(fallback).join(' ') + '</div>';
}

function projectOutcomeRailHTML(project) {
  if (projectIsUnconfigured(project)) {
    return '<div class="rail-subline rail-setup-copy" title="No outcome brief configured">No outcome brief configured</div>' +
      '<div class="rail-note rail-setup-link">Set up &rarr;</div>';
  }

  const outcome = project.outcome || {};
  const health = outcome.health_state || "unknown";
  const goal = outcome.configured === true && outcome.goal ? outcome.goal : "No outcome brief configured";
  return '<span class="pill outcome-' + cssToken(health) + '">' + escapeText(health.replace(/_/g, ' ')) + '</span>' +
    '<div class="rail-subline" title="' + escapeText(goal) + '">' + escapeText(goal) + '</div>';
}

function projectFreshnessRailHTML(project) {
  const freshness = project.freshness || {};
  const age = freshness.snapshot_age ? "Snapshot " + freshness.snapshot_age + " ago" : "No snapshot yet";
  return '<div class="rail-mainline" title="' + escapeText(freshness.reason || age) + '">' + escapeText(age) + '</div>';
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

function projectOpenRailHTML(project) {
  const url = project.dashboard_url || githubRepoURL(project.repo);
  const label = projectIsUnconfigured(project) ? "Set up" : "Open";
  return '<div class="rail-open-link">' + linkHTML(url, label + " →") + '</div>';
}

function projectRailRowHTML(project) {
  const key = projectStateKey(project);
  const modifier = projectIsUnconfigured(project) ? ' project-row--unconfigured' : '';
  return '<tr class="project-rail-row project-row-' + cssToken(key) + modifier + '" data-project="' + escapeText(project.name || "") + '" data-url="' + escapeText(project.dashboard_url || githubRepoURL(project.repo) || "") + '" tabindex="0">' +
    '<td class="project-rail-project"><div class="project-rail-project-wrap"><div class="project-rail-project-copy">' + projectIdentityRailHTML(project) + '</div></div></td>' +
    '<td class="project-rail-state-cell">' + projectStateRailHTML(project) + '</td>' +
    '<td class="project-rail-queue-cell">' + projectQueueRailHTML(project) + '</td>' +
    '<td class="project-rail-pr-cell">' + projectPRRailHTML(project) + '</td>' +
    '<td class="project-rail-outcome-cell">' + projectOutcomeRailHTML(project) + '</td>' +
    '<td class="project-rail-freshness-cell">' + projectFreshnessRailHTML(project) + '</td>' +
    '<td class="project-rail-links-cell">' + projectOpenRailHTML(project) + '</td>' +
  '</tr>';
}

function renderProjectRail() {
  ensureSelectedProject();
  const allProjects = fleetState.projects || [];
  const total = allProjects.length;
  const projects = visibleProjects();
  updateProjectSegmentCounts(allProjects);
  projectRailSummaryEl.textContent = projectRailSummaryText(projects, total);
  if (!projects.length) {
    const empty = total ? "No configured projects match the project search." : "No configured projects are available in this fleet.";
    projectRailBodyEl.innerHTML = '<tr class="project-rail-empty"><td colspan="7" class="empty">' + escapeText(empty) + '</td></tr>';
    return;
  }

  projectRailBodyEl.innerHTML = projects.map(projectRailRowHTML).join("");
  projectRailBodyEl.querySelectorAll(".project-rail-row[data-project]").forEach(row => {
    row.addEventListener("click", event => {
      if (event.target.closest("a, button")) return;
      const url = row.dataset.url || "";
      if (url) window.open(url, "_blank", "noopener,noreferrer");
    });
    row.addEventListener("keydown", event => {
      if ((event.key === "Enter" || event.key === " ") && !event.target.closest("a, button")) {
        event.preventDefault();
        const url = row.dataset.url || "";
        if (url) window.open(url, "_blank", "noopener,noreferrer");
      }
    });
  });
  projectRailBodyEl.querySelectorAll(".project-rail-row[data-project] a, .project-rail-row[data-project] button").forEach(control => {
    control.addEventListener("click", event => event.stopPropagation());
    control.addEventListener("keydown", event => event.stopPropagation());
  });
  projectRailBodyEl.querySelectorAll(".project-workers-link[data-project]").forEach(button => {
    button.addEventListener("click", event => {
      event.preventDefault();
      fleetState.selectedProject = button.dataset.project || "all";
      updateQueryState();
      renderFleetWorkers();
      if (fleetWorkersShellEl) fleetWorkersShellEl.open = true;
      fleetWorkersShellEl?.scrollIntoView({ block: "start", behavior: "smooth" });
    });
  });
}

function renderFleetWorkers() {
  const base = selectedProjectWorkers();
  const visible = sortWorkers(filteredWorkers(true));
  const showingDefaultScope = fleetState.filters.scope === "operator" && !hasWorkerDrilldownFilters();
  const hiddenHistory = showingDefaultScope ? base.filter(worker => !defaultWorkerVisible(worker)) : [];
  const liveVisibleCount = base.filter(defaultWorkerVisible).length;
  const rowCount = visible.length + (hiddenHistory.length ? 1 : 0);
  const totalWorkers = base.length;
  const table = fleetWorkersEl.closest("table");
  if (table) table.classList.toggle("worker-table-empty", rowCount === 0);
  if (workerControlsEl) {
    const shouldShowControls = liveVisibleCount > 10 || hasWorkerFilters() || fleetState.filters.scope !== "operator";
    workerControlsEl.hidden = !shouldShowControls;
  }
  const projectLabel = fleetState.selectedProject === "all" ? "all projects" : fleetState.selectedProject;
  const projectScoped = fleetState.selectedProject !== "all";
  workerProjectResetEl.hidden = !projectScoped;
  workerProjectResetEl.setAttribute("aria-label", projectScoped ? "Clear " + projectLabel + " worker scope and show all projects" : "Workers are showing all projects");
  const scopeLabel = scopeLabelText(fleetState.filters.scope);
  const filterText = hasWorkerFilters() ? " · " + visible.length + " shown from " + totalWorkers + " total" : " · " + totalWorkers + " total";
  const attentionCount = visible.filter(worker => worker.needs_attention).length;
  const shellSummary = liveVisibleCount + " live/operator · " + hiddenHistory.length + " history collapsed" +
    (attentionCount ? " · " + attentionCount + " attention" : "");
  if (workerShellSummaryEl) workerShellSummaryEl.textContent = shellSummary;
  if (fleetWorkersShellEl && (hasWorkerFilters() || fleetState.filters.scope !== "operator" || projectScoped || fleetState.selectedWorkerKey)) {
    fleetWorkersShellEl.open = true;
  }
  workerSummaryEl.textContent = visible.length + " worker" + (visible.length === 1 ? "" : "s") + " shown in " + projectLabel +
    filterText + (hiddenHistory.length ? " · " + hiddenHistory.length + " history collapsed" : "") +
    (attentionCount ? " · " + attentionCount + " need attention" : "") +
    (workerControlsEl && workerControlsEl.hidden ? " · filters hidden until the live queue is larger" : " · " + scopeLabel);

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
  fleetWorkersEl.querySelectorAll("tr[data-slot] a, tr[data-slot] button").forEach(control => {
    control.addEventListener("click", event => event.stopPropagation());
    control.addEventListener("keydown", event => event.stopPropagation());
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
	if (fleetWorkersShellEl) fleetWorkersShellEl.open = true;
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
  fleetState.requestedWorker = selectedWorkerQueryValue();
  updateQueryState();
  fleetState.detail = null;
  renderAttentionInbox();
  renderFleetWorkers();
  renderWorkerDetailLoading(projectName, slot);
  loadWorkerDetail();
}

function closeWorkerDetail() {
  fleetState.selectedWorkerKey = "";
  fleetState.requestedWorker = "";
  fleetState.detail = null;
  updateQueryState();
  renderAttentionInbox();
  renderFleetWorkers();
  renderWorkerDetail(null);
}

function renderWorkerDetailLoading(projectName, slot) {
  setWorkerDrawerOpen(true);
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
    setWorkerDrawerOpen(false);
    workerDetailSummaryEl.textContent = "No worker selected";
    workerDetailBodyEl.innerHTML = '<div class="empty">Select a fleet worker to show metadata and log output.</div>';
    return;
  }
  if (!data || !data.worker) {
    const worker = selectedWorker();
    if (!worker) {
      workerDetailSummaryEl.textContent = "Worker unavailable";
      setWorkerDrawerOpen(false);
      workerDetailBodyEl.innerHTML = '<div class="empty">Selected worker is no longer visible in the fleet snapshot.</div>';
      return;
    }
    data = { worker: worker, log: { available: false, reason: "Worker detail has not loaded yet." } };
  }

  const worker = data.worker;
  setWorkerDrawerOpen(true);
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
  const meta = [
    supervisorDecisionMetaText(latest),
    latest.queue && queueText(latest.queue)
  ].filter(Boolean).map(escapeText).join(' · ');
  return '<div class="supervisor">' +
    '<div class="label">Supervisor</div>' +
    '<div class="decision"><strong title="Raw action: ' + escapeText(rawAction) + '">' + escapeText(operatorSentence) + '</strong>' + summary +
    (meta ? '<br><small title="Raw action: ' + escapeText(rawAction) + '">' + meta + '</small>' : '') + '</div>' +
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
    ? '<div class="project-diagnostics-note">This is the raw inspector layer. The primary fleet view above stays compact on purpose.</div>'
    : '<div class="empty">No project diagnostics match the project search.</div>';
  if (projectDiagnosticsEl) {
    projectDiagnosticsEl.open = false;
  }
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
if (fleetRefreshEl) fleetRefreshEl.addEventListener("click", loadFleet);
if (searchTriggerEl) searchTriggerEl.addEventListener("click", openSearchPalette);
if (searchCloseEl) searchCloseEl.addEventListener("click", () => closeSearchPalette());
if (searchBackdropEl) searchBackdropEl.addEventListener("click", () => closeSearchPalette());
if (searchInputEl) {
  searchInputEl.addEventListener("input", () => {
    fleetState.search.query = searchInputEl.value.trim();
    fleetState.search.activeIndex = 0;
    renderSearchPalette();
  });
  searchInputEl.addEventListener("keydown", event => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      moveSearchActive(1);
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      moveSearchActive(-1);
    } else if (event.key === "Enter") {
      event.preventDefault();
      selectFleetSearchResult(fleetState.search.results[fleetState.search.activeIndex]);
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      closeSearchPalette();
    }
  });
}

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

workerDetailCloseEl.addEventListener("click", closeWorkerDetail);
workerDetailBackdropEl.addEventListener("click", closeWorkerDetail);
document.addEventListener("keydown", event => {
  if (isSearchShortcut(event)) {
    event.preventDefault();
    openSearchPalette();
    return;
  }
  if (event.key === "Escape" && fleetState.search.open) {
    event.preventDefault();
    closeSearchPalette();
    return;
  }
  if (event.key === "Escape" && fleetState.selectedWorkerKey) {
    closeWorkerDetail();
  }
});

function applyFleetData(data) {
  fleetState.readOnly = data.read_only !== false;
  fleetState.refreshedAt = data.refreshed_at || "";
  fleetState.summary = data.summary || {};
  fleetState.projects = data.projects || [];
  fleetState.workers = fleetWorkersFromData(data);
  fleetState.approvals = approvalsFromData(data);
  fleetState.attention = attentionFromData(data);
  fleetState.operatorBrief = data.operator_brief || null;
  fleetState.verdict = data.verdict || null;
  if (!fleetState.selectedWorkerKey && fleetState.requestedWorker) {
    const requested = resolveWorkerQuery(fleetState.requestedWorker);
    if (requested) fleetState.selectedWorkerKey = workerKey(requested);
    fleetState.requestedWorker = "";
  }

  if (fleetState.selectedWorkerKey && !selectedWorker()) {
    fleetState.requestedWorker = "";
    fleetState.selectedWorkerKey = "";
    fleetState.detail = null;
  }
  const controlMode = fleetState.readOnly ? "read-only controls disabled" : "controls require approval configuration";
  const summary = fleetState.summary || {};
  const alerts = [];
  if (summary.stale) alerts.push(summary.stale + " stale");
  if (summary.errors) alerts.push(summary.errors + " error" + (summary.errors === 1 ? "" : "s"));
  subtitleEl.textContent = "Last refresh " + formatTimestamp(fleetState.refreshedAt) + " · " +
    fleetState.projects.length + " configured project" + (fleetState.projects.length === 1 ? "" : "s") + " · " + controlMode +
    (alerts.length ? " · " + alerts.join(" · ") : "");
  renderFilterOptions();
  syncFilterControls();
  renderFleetVerdict(fleetState.operatorBrief, fleetState.verdict);
  renderStats(summary);
  renderProjectRail();
  renderProjectOverview();
  renderApprovalInbox();
  renderAttentionInbox();
  renderNeedsYouRail();
  renderFleetWorkers();
  if (fleetState.search.open) renderSearchPalette();
  const needsDetailLoad = fleetState.selectedWorkerKey && (!fleetState.detail || !fleetState.detail.worker || workerKey(fleetState.detail.worker) !== fleetState.selectedWorkerKey);
  if (needsDetailLoad) {
    const worker = selectedWorker();
    renderWorkerDetailLoading(worker && worker.project_name, worker && worker.slot);
    loadWorkerDetail();
  } else {
    renderWorkerDetail(fleetState.detail);
  }
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
