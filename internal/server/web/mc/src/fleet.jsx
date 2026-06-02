import React from "react";
import { Heartbeat, Icon, Panel, Pill, Stat } from "./atoms.jsx";
import { useFleet } from "./fleetContext.jsx";
import { parseTimestamp, relTime } from "./utils.js";
import {
  actionLabel,
  approvalSlotLabel,
  backendHealthTone,
  formatAbsoluteTimestamp,
  formatBackendHealthSentence,
  formatCountdown,
  formatTokens,
  formatUSD,
  nextDecisionCountdown,
  pulseFreshnessTone,
  relTimePrecise,
} from "./fleetApi.js";

export function FleetScreen({ navigate }) {
  const { fleet, now } = useFleet();
  if (!fleet) {
    return (
      <div style={{ padding: "var(--s-12)", textAlign: "center", color: "var(--fg-2)" }}>
        Loading fleet data…
      </div>
    );
  }

  const projects = fleet.projects || [];
  const tone = fleet.verdictTone || "ok";
  const liveCount = projects.filter(p => p.state?.state === "live").length;
  const attCount = projects.filter(p => p.state?.state === "stuck" || p.state?.state === "watch").length;
  const idleCount = projects.length - liveCount - attCount;
  const pulse = fleet.supervisorPulse || null;
  const lastDecisionMs = pulse?.lastRunOnceMs != null
    ? pulse.lastRunOnceMs
    : (fleet.lastDecisionAge || (fleet.refreshedAt ? parseTimestamp(fleet.refreshedAt) : null));
  const lastDecision = lastDecisionMs != null ? relTimePrecise(lastDecisionMs, now) : "—";
  const lastDecisionTitle = pulse?.lastRunOnceAt
    ? formatAbsoluteTimestamp(pulse.lastRunOnceAt)
    : (fleet.refreshedAt ? formatAbsoluteTimestamp(fleet.refreshedAt) : "");
  const countdownSec = nextDecisionCountdown(pulse, now);
  const pulseTone = pulseFreshnessTone(pulse, now);
  const headerTone = pulseTone !== "idle" ? pulseTone : tone;

  const modeLabel = fleet.readOnly
    ? "read-only"
    : (pulse?.mode ? pulse.mode.replace(/_/g, " ") : "—");

  const workerSub = fleet.workerCount > 0 ? "running now" : "max parallel";
  const prSub = fleet.prCount > 0
    ? `${fleet.summary?.monitoring_pr || 0} monitored`
    : "none open";
  // #598: when the only "attention" items are convergence-bound PRs the
  // operator has nothing to do, so the stat sub-line reads calm.
  const selfResolvingCount = Number(fleet.selfResolvingCount || 0);
  let attSub;
  if (fleet.attentionCount > 0) {
    attSub = "items need you";
  } else if (selfResolvingCount > 0) {
    attSub = selfResolvingCount === 1
      ? "1 auto-merging"
      : `${selfResolvingCount} auto-merging`;
  } else {
    attSub = "nothing waiting";
  }
  const approvalSub = fleet.activeApprovals > 0 ? "pending review" : "queue empty";

  const ctaLabel = fleet.nextAction?.cta_label || "";
  const ctaTone = ctaToneForKind(fleet.nextAction?.kind);
  const onCTA = ctaHandlerForNextAction(fleet.nextAction, navigate);

  return (
    <div>
      <div className={`hb tone-${tone}`}>
        <div className="hb-left">
          <div className={`hb-line ${headerTone}`}>
            <span className="pulse-dot" />
            <strong>{fleet.daemonAlive ? "supervisor alive" : "supervisor offline"}</strong>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <span title={lastDecisionTitle || undefined}>last decision <strong>{lastDecision}</strong></span>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <span>next in <strong>{formatCountdown(countdownSec)}</strong></span>
          </div>
          <h1 className={`hb-verdict tone-${tone}`}>
            <em>{fleet.verdict[0]}</em>{fleet.verdict[1]}
          </h1>
          <div className="hb-meta">
            <div><span>Projects</span><strong>{fleet.summary?.projects || projects.length}</strong></div>
            <div><span>Workers running</span><strong>{fleet.workerCount}</strong></div>
            <div><span>Approvals pending</span><strong>{fleet.activeApprovals}</strong></div>
            <div><span>PRs open</span><strong>{fleet.prCount}</strong></div>
            <div><span>Mode</span><strong>{modeLabel}</strong></div>
          </div>
          <DecisionSparkline actions={pulse?.recentActions || []} />
          <div className="hb-actions">
            {ctaLabel && (
              <button className={`tb-btn ${ctaTone}`} onClick={onCTA}>
                {ctaLabel} →
              </button>
            )}
            {!ctaLabel && fleet.nextAction?.project && (
              <button className="tb-btn primary" onClick={() => navigate(`project/${fleet.nextAction.project}`)}>
                Open {fleet.nextAction.project} →
              </button>
            )}
            {fleet.workerCount > 0 && (
              <button className="tb-btn" onClick={() => navigate("workers")}>Watch live workers →</button>
            )}
            {fleet.attentionCount > 0 && (
              <button className="tb-btn danger" onClick={() => navigate("workers")}>Review attention →</button>
            )}
            <a className="tb-btn ghost" href="https://github.com/BeFeast/maestro/blob/main/docs/fleet-mission-control-runbook.md" target="_blank" rel="noreferrer">View runbook →</a>
          </div>
        </div>
        <Heartbeat tone={tone} bpm={fleet.heartbeatBpm} daemonAlive={fleet.daemonAlive} />
      </div>

      <BackendHealthRow entries={fleet.backendHealth || []} now={now} />

      <div className="stats">
        <Stat
          label="Workers"
          value={fleet.workerCount}
          of={projects.length ? ` / ${projects.reduce((n, p) => n + (p.maxParallel || 0), 0) || "—"}` : undefined}
          tone={fleet.workerCount > 0 ? "ok" : "info"}
          sub={workerSub}
          live={fleet.workerCount > 0}
          onClick={() => navigate("workers")}
        />
        <Stat
          label="PRs in flight"
          value={fleet.prCount}
          tone={fleet.prCount > 0 ? "watch" : "info"}
          sub={prSub}
        />
        <Stat
          label="Attention"
          value={fleet.attentionCount}
          tone={fleet.attentionCount > 0 ? (fleet.daemonAlive ? "watch" : "stuck") : "info"}
          sub={attSub}
        />
        <Stat
          label="Approvals"
          value={fleet.activeApprovals}
          tone={fleet.activeApprovals > 0 ? "watch" : "info"}
          sub={approvalSub}
          onClick={() => navigate("approvals")}
        />
        <Stat
          label="Throughput"
          value={String(fleet.throughputMerged7d)}
          tone="ok"
          sub="merged PRs · 7d"
          sparkline={fleet.throughputDaily7d.length ? fleet.throughputDaily7d : [0, 0, 0, 0, 0, 0, 0]}
          sparkWarm={tone === "watch"}
          tooltip="Merged PRs across all projects in the last 7 days (UTC). Bars show per-day counts, oldest on the left."
        />
      </div>

      <div className="layout-head">
        <div>
          <h2>Projects</h2>
          <div className="hint mono mt-2" style={{ fontSize: 11.5 }}>
            {projects.length} total · <span style={{ color: "var(--ok)" }}>{liveCount} live</span>
            {" · "}<span style={{ color: tone === "watch" ? "var(--watch)" : "var(--fg-3)" }}>{attCount} attention</span>
            {" · "}<span style={{ color: "var(--fg-3)" }}>{idleCount} idle</span>
          </div>
        </div>
      </div>

      <FleetTape projects={projects} navigate={navigate} now={now} />

      <div className="dash-grid mt-6">
        <div>
          <Panel
            title="Workers (last 24h)"
            right={<a onClick={() => navigate("workers")} style={{ fontSize: 11.5 }}>Open workers →</a>}
          >
            <LiveWorkersPreview navigate={navigate} />
          </Panel>
        </div>
        <div>
          <Panel
            title="Approvals"
            sub={`${fleet.activeApprovals} pending`}
            right={<a onClick={() => navigate("approvals")} style={{ fontSize: 11.5 }}>Open inbox →</a>}
          >
            <ApprovalsPreview />
          </Panel>
        </div>
      </div>

      <CostObservabilityPanel cost={fleet.costObservability} />
    </div>
  );
}

// CostObservabilityPanel renders the fleet-wide token + USD spend
// rollup the server precomputes for /api/v1/fleet (#619). It shows the
// today/7d totals up top, then a per-backend row table and a
// per-project row table. The SPA never blends pricing rates client-side
// — every $ figure comes straight from cost_observability — so the
// panel degrades gracefully to tokens-only when a backend has no
// pricing configured.
export function CostObservabilityPanel({ cost }) {
  if (!cost) return null;
  const today = cost.today || {};
  const week = cost.week || {};
  const lifetime = cost.lifetime || {};
  const hasAnyTokens =
    Number(today.tokens || 0) > 0 ||
    Number(week.tokens || 0) > 0 ||
    Number(lifetime.tokens || 0) > 0;
  if (!hasAnyTokens) {
    return (
      <div className="mt-6">
        <Panel title="Cost & usage" sub="no token activity recorded">
          <div style={{ padding: "var(--s-5)", textAlign: "center" }}>
            <div className="dim" style={{ fontSize: 13 }}>
              No worker has recorded token usage yet. Cost figures will appear here once the first session ticks.
            </div>
          </div>
        </Panel>
      </div>
    );
  }
  return (
    <div className="mt-6">
      <Panel title="Cost & usage" sub="today · 7d · lifetime · per backend · per project">
        <div style={{ padding: "var(--s-4) var(--s-5)" }}>
          <div className="row gap-4" style={{ flexWrap: "wrap", marginBottom: "var(--s-3)" }}>
            <CostWindowCard label="Today" window={today} />
            <CostWindowCard label="7 days" window={week} />
            <CostWindowCard label="Lifetime" window={lifetime} />
          </div>

          {cost.perBackend && cost.perBackend.length > 0 && (
            <div style={{ marginTop: "var(--s-4)" }}>
              <div className="mono dim" style={{ fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.06em", marginBottom: 6 }}>
                by backend
              </div>
              <CostBackendTable rows={cost.perBackend} />
            </div>
          )}

          {cost.perProject && cost.perProject.length > 0 && (
            <div style={{ marginTop: "var(--s-4)" }}>
              <div className="mono dim" style={{ fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.06em", marginBottom: 6 }}>
                by project
              </div>
              <CostProjectTable rows={cost.perProject} />
            </div>
          )}
        </div>
      </Panel>
    </div>
  );
}

function CostWindowCard({ label, window: w }) {
  const tokens = Number(w?.tokens || 0);
  const usd = Number(w?.usd || 0);
  const sessions = Number(w?.sessions || 0);
  const unpriced = Number(w?.unpricedTokens || 0);
  return (
    <div style={{ minWidth: 160, padding: "var(--s-3) var(--s-4)", border: "1px solid var(--grid-line)", borderRadius: 4 }}>
      <div className="mono dim" style={{ fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.06em" }}>
        {label}
      </div>
      <div style={{ fontSize: 20, fontWeight: 600, color: "var(--fg-0)", marginTop: 4 }}>
        {usd > 0 ? formatUSD(usd) : "—"}
      </div>
      <div className="mono dim" style={{ fontSize: 11, marginTop: 2 }}>
        {formatTokens(tokens)} tokens · {sessions} session{sessions === 1 ? "" : "s"}
      </div>
      {unpriced > 0 && usd > 0 && (
        <div className="mono" style={{ fontSize: 10.5, marginTop: 2, color: "var(--watch)" }}>
          {formatTokens(unpriced)} tokens unpriced
        </div>
      )}
    </div>
  );
}

function CostBackendTable({ rows }) {
  return (
    <div className="cost-table" role="table">
      <div className="cost-row cost-head mono dim" role="row" style={{ display: "grid", gridTemplateColumns: "1.2fr 0.6fr 1.2fr 1.2fr 1.2fr", fontSize: 10.5, padding: "4px 0", textTransform: "uppercase", letterSpacing: "0.06em" }}>
        <div role="columnheader">Backend</div>
        <div role="columnheader">Price</div>
        <div role="columnheader">Today</div>
        <div role="columnheader">7 days</div>
        <div role="columnheader">Lifetime</div>
      </div>
      {rows.map(row => (
        <div key={row.backend} role="row" style={{ display: "grid", gridTemplateColumns: "1.2fr 0.6fr 1.2fr 1.2fr 1.2fr", fontSize: 12, padding: "4px 0", borderTop: "1px solid var(--grid-line)" }}>
          <div role="cell" className="mono">
            <strong>{row.backend}</strong>
          </div>
          <div role="cell" className="mono">
            {row.priceConfigured ? (
              <span title={`input $${row.inputUSDPerMtok}/Mtok · output $${row.outputUSDPerMtok}/Mtok`}>
                set
              </span>
            ) : (
              <span className="dim" title="No pricing configured — tokens only">tokens only</span>
            )}
          </div>
          <CostCell w={row.today} priced={row.priceConfigured} />
          <CostCell w={row.week} priced={row.priceConfigured} />
          <CostCell w={row.lifetime} priced={row.priceConfigured} />
        </div>
      ))}
    </div>
  );
}

function CostProjectTable({ rows }) {
  return (
    <div className="cost-table" role="table">
      <div className="cost-row cost-head mono dim" role="row" style={{ display: "grid", gridTemplateColumns: "1.8fr 1.2fr 1.2fr 1.2fr", fontSize: 10.5, padding: "4px 0", textTransform: "uppercase", letterSpacing: "0.06em" }}>
        <div role="columnheader">Project</div>
        <div role="columnheader">Today</div>
        <div role="columnheader">7 days</div>
        <div role="columnheader">Lifetime</div>
      </div>
      {rows.map(row => (
        <div key={row.project} role="row" style={{ display: "grid", gridTemplateColumns: "1.8fr 1.2fr 1.2fr 1.2fr", fontSize: 12, padding: "4px 0", borderTop: "1px solid var(--grid-line)" }}>
          <div role="cell" className="mono">
            <strong>{row.project}</strong>
            {row.repo && <span className="dim" style={{ marginLeft: 6, fontSize: 10.5 }}>{row.repo}</span>}
          </div>
          <CostCell w={row.today} priced={true} />
          <CostCell w={row.week} priced={true} />
          <CostCell w={row.lifetime} priced={true} />
        </div>
      ))}
    </div>
  );
}

function CostCell({ w, priced }) {
  const tokens = Number(w?.tokens || 0);
  const usd = Number(w?.usd || 0);
  if (tokens <= 0) {
    return <div role="cell" className="mono dim">—</div>;
  }
  return (
    <div role="cell" className="mono">
      {priced && usd > 0 ? (
        <span>
          <strong>{formatUSD(usd)}</strong>
          <span className="dim" style={{ marginLeft: 6, fontSize: 10.5 }}>{formatTokens(tokens)}</span>
        </span>
      ) : (
        <span>{formatTokens(tokens)}</span>
      )}
    </div>
  );
}

function FleetTape({ projects, navigate, now }) {
  return (
    <div className="tape">
      <div className="tape-ruler">
        <div>PROJECT · STATE</div>
        <div className="tape-ruler-ticks">
          {["60m", "50m", "40m", "30m", "20m", "10m", "NOW"].map(t => <span key={t}>{t}</span>)}
        </div>
        <div style={{ textAlign: "right" }}>LAST</div>
      </div>
      {projects.map(p => (
        <div key={p.slug} className="tape-row" onClick={() => navigate(`project/${p.slug}`)}>
          <div className="tape-id">
            <div className="tape-id-top">
              <ProjectStatePill state={p.state} />
            </div>
            <div className="tape-name mt-2">{p.slug}</div>
            <div className="tape-summary">{p.summaryLine}</div>
          </div>
          <div className="tape-canvas">
            {(p.tapeEvents || []).length === 0 ? (
              <div className="dim mono" style={{ position: "absolute", inset: 0, display: "flex", alignItems: "center", justifyContent: "center", fontSize: 10.5 }}>
                No events in last 60m
              </div>
            ) : (
              p.tapeEvents.map((e, i) => {
                const style = {
                  left: `${e.x * 100}%`,
                  width: e.w ? `${e.w * 100}%` : "3px",
                };
                return <div key={i} className={`tape-event ${e.kind} ${e.live ? "live" : ""}`} style={style} />;
              })
            )}
            <div className="tape-now" style={{ left: "100%" }} />
          </div>
          <div className="tape-meta">
            <div className="tape-meta-state mono" style={{ fontSize: 10, color: "var(--fg-3)" }}>{p.state?.label || "idle"}</div>
            <div className="tape-meta-age">
              {p.freshness?.snapshot_age || (p.freshness?.stale ? "stale" : relTime(parseTimestamp(p.freshness?.snapshot_at) || now, now))}
            </div>
          </div>
        </div>
      ))}
      <div className="tape-legend">
        <span style={{ color: "var(--fg-2)", marginRight: 8 }}>LEGEND</span>
        <div className="tape-legend-item"><span className="tape-legend-swatch" style={{ background: "var(--ok)" }} /> worker run</div>
        <div className="tape-legend-item"><span className="tape-legend-swatch" style={{ background: "var(--info)" }} /> PR open</div>
        <div className="tape-legend-item"><span className="tape-legend-swatch" style={{ background: "var(--policy)", height: 12, width: 3 }} /> merge</div>
        <div className="tape-legend-item"><span className="tape-legend-swatch" style={{ background: "var(--policy)", opacity: 0.6 }} /> held by policy</div>
        <div className="tape-legend-item"><span className="tape-legend-swatch" style={{ background: "var(--stuck)" }} /> stuck / fail</div>
      </div>
    </div>
  );
}

function ProjectStatePill({ state }) {
  const tone = { live: "live", stuck: "stuck", ok: "ok", policy: "policy", idle: "idle", watch: "watch", unknown: "stuck" }[state?.state] || "idle";
  return <Pill tone={tone}>{state?.label || state?.state || "idle"}</Pill>;
}

function LiveWorkersPreview({ navigate }) {
  const { fleet, now } = useFleet();
  const live = (fleet?.workers || []).filter(w => w.live);

  if (live.length === 0) {
    return (
      <div style={{ padding: "var(--s-5)", textAlign: "center" }}>
        <div style={{ color: "var(--fg-1)", fontSize: 13 }}>No workers active in the last 24h.</div>
        <div className="mono dim mt-2" style={{ fontSize: 11 }}>
          {fleet?.daemonAlive ? "Supervisor checking for eligible issues." : "Daemon offline. Restart to resume."}
        </div>
      </div>
    );
  }

  return (
    <div style={{ padding: "var(--s-2)" }}>
      {live.slice(0, 5).map(w => (
        <div key={`${w.project}-${w.slot}`} className="dec" style={{ gridTemplateColumns: "80px 1fr auto" }} onClick={() => navigate(`workers?project=${encodeURIComponent(w.project)}&slot=${encodeURIComponent(w.slot)}`)}>
          <div className="dec-t mono">{w.slot}</div>
          <div className="dec-body">
            <div style={{ color: "var(--fg-0)", fontSize: 12.5 }}>{w.issue.title}</div>
            <div className="mono dim" style={{ fontSize: 10.5, marginTop: 2 }}>{w.project} #{w.issue.num}</div>
          </div>
          <div style={{ textAlign: "right" }}>
            <Pill tone={w.tone} noDot>{w.status}</Pill>
            <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>{relTime(w.age, now)}</div>
          </div>
        </div>
      ))}
    </div>
  );
}

function ApprovalsPreview() {
  const { fleet } = useFleet();
  const apps = fleet?.pendingApprovals || [];

  if (apps.length === 0) {
    return (
      <div style={{ padding: "var(--s-5)", textAlign: "center" }}>
        <div style={{ color: "var(--fg-1)", fontSize: 13 }}>Nothing waiting.</div>
        <div className="mono dim mt-2" style={{ fontSize: 11 }}>All approvals up to date.</div>
      </div>
    );
  }

  return (
    <div style={{ padding: "var(--s-2)" }}>
      {apps.slice(0, 3).map((a, i) => (
        <div key={a.id || i} className="dec" style={{ gridTemplateColumns: "60px 1fr auto" }}>
          <div className="dec-t mono">{approvalSlotLabel(a)}</div>
          <div className="dec-body">
            <div style={{ color: "var(--fg-0)", fontSize: 12.5 }}>{a.title}</div>
            <div className="mono dim" style={{ fontSize: 10.5, marginTop: 2 }}>{a.project} · {a.stage}</div>
          </div>
          <div style={{ textAlign: "right" }}>
            <Pill tone={a.state} noDot>{a.past_sla ? "past SLA" : "waiting"}</Pill>
            <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>{a.ageMin}m</div>
          </div>
        </div>
      ))}
    </div>
  );
}

// DecisionSparkline renders the last 10 supervisor `recommended_action`
// verbs as a compact chip strip so an operator can read "two monitor
// ticks, then a spawn, then a label" at a glance (#531 gap 15). Idle
// reads as a positive signal — a row of dim monitor chips — instead of
// the wedged "RUNNING 0 / PRS OPEN 0" the old hero produced.
function DecisionSparkline({ actions }) {
  if (!actions || actions.length === 0) {
    return (
      <div className="hb-spark dim mono" title="No supervisor decisions recorded yet.">
        <span style={{ color: "var(--fg-3)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.06em" }}>
          last 10 decisions
        </span>
        <span style={{ color: "var(--fg-3)", fontSize: 11 }}>— no cycles yet —</span>
      </div>
    );
  }
  return (
    <div className="hb-spark mono">
      <span style={{ color: "var(--fg-3)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.06em" }}>
        last {actions.length} decisions
      </span>
      <div className="hb-spark-chips">
        {actions.map((verb, i) => (
          <span key={i} className={`hb-spark-chip ${decisionChipTone(verb)}`} title={actionLabel(verb)}>
            {decisionChipGlyph(verb)}
          </span>
        ))}
      </div>
    </div>
  );
}

// BackendHealthRow renders one pill per configured backend so an
// operator can see at a glance that, e.g., claude is in cooldown until
// 21:00 UTC while codex / opencode remain available. Rendered between
// the hero card and the rollup stats — that is where the operator
// looks for "is the fleet flying right now". Hidden entirely when no
// project surfaces backend_health (older servers / unconfigured).
function BackendHealthRow({ entries, now }) {
  if (!entries || entries.length === 0) return null;
  return (
    <div className="backend-health row gap-2" style={{ flexWrap: "wrap", marginTop: "var(--s-3)", marginBottom: "var(--s-3)" }}>
      <span className="mono dim" style={{ fontSize: 10.5, textTransform: "uppercase", letterSpacing: "0.06em" }}>
        backends
      </span>
      {entries.map(entry => {
        const tone = backendHealthTone(entry.state);
        const sentence = formatBackendHealthSentence(entry, now);
        const title = entry.retryAfter
          ? `${entry.backend} ${entry.state}; retry after ${entry.retryAfter}`
          : `${entry.backend} ${entry.state || "unknown"}`;
        return (
          <Pill key={entry.backend} tone={tone} noDot title={title}>
            <strong style={{ fontSize: 11.5, marginRight: 6 }}>{entry.backend}</strong>
            <span style={{ fontSize: 10.5, opacity: 0.85 }}>
              {sentence || entry.state || "unknown"}
            </span>
          </Pill>
        );
      })}
    </div>
  );
}

function decisionChipTone(verb) {
  switch (String(verb || "").trim()) {
  case "merge_pr":
  case "approve_merge":
    return "ok";
  case "spawn_worker":
  case "label_issue_ready":
    return "info";
  case "monitor_open_pr":
  case "wait_for_review":
  case "wait_for_ci":
  case "wait_for_running_worker":
  case "wait_for_worker":
  case "wait_for_capacity":
  case "wait_for_ordered_queue":
    return "idle";
  case "check_outcome_health":
    return "watch";
  case "review_retry_exhausted":
  case "none":
  case "skip_wave":
    return "muted";
  default:
    return "idle";
  }
}

function decisionChipGlyph(verb) {
  switch (String(verb || "").trim()) {
  case "merge_pr": return "M";
  case "approve_merge": return "A";
  case "spawn_worker": return "S";
  case "label_issue_ready": return "L";
  case "monitor_open_pr": return "·";
  case "wait_for_review":
  case "wait_for_ci":
  case "wait_for_running_worker":
  case "wait_for_worker":
  case "wait_for_capacity":
  case "wait_for_ordered_queue":
    return "w";
  case "check_outcome_health": return "H";
  case "review_retry_exhausted": return "R";
  case "none":
  case "skip_wave":
    return "—";
  default:
    return String(verb || "·").charAt(0).toLowerCase();
  }
}

// ctaToneForKind picks the button class for the header CTA. Approvals
// read "primary" (the operator's main job is approving); stuck/error
// kinds read "danger" so the wedge is loud.
function ctaToneForKind(kind) {
  switch (String(kind || "").trim()) {
  case "approval_pending":
    return "primary";
  case "error":
  case "dispatch_failure":
  case "stale_worker":
  case "attention":
    return "danger";
  default:
    return "primary";
  }
}

// ctaHandlerForNextAction routes the header button to the right SPA
// screen for the chosen `next_action.kind`. Approvals jump to the inbox,
// stuck/dispatch failures jump to the workers screen, everything else
// zooms into the project.
function ctaHandlerForNextAction(action, navigate) {
  if (!action) return () => {};
  const kind = String(action.kind || "").trim();
  const project = action.project || "";
  return () => {
    if (kind === "approval_pending") {
      navigate("approvals");
      return;
    }
    if (kind === "stale_worker" || kind === "dispatch_failure" || kind === "attention") {
      navigate("workers");
      return;
    }
    if (project) {
      navigate(`project/${project}`);
      return;
    }
    navigate("approvals");
  };
}
