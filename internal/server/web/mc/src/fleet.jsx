import React from "react";
import { Heartbeat, Icon, Panel, Pill, Stat } from "./atoms.jsx";
import { useFleet } from "./fleetContext.jsx";
import { parseTimestamp, relTime } from "./utils.js";

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
  const lastDecision = fleet.lastDecisionAge
    ? relTime(fleet.lastDecisionAge, now)
    : fleet.refreshedAt
      ? relTime(parseTimestamp(fleet.refreshedAt) || now, now)
      : "—";

  const workerSub = fleet.workerCount > 0 ? "running now" : "max parallel";
  const prSub = fleet.prCount > 0
    ? `${fleet.summary?.monitoring_pr || 0} monitored`
    : "none open";
  const attSub = fleet.attentionCount > 0 ? "items need you" : "nothing waiting";
  const approvalSub = fleet.activeApprovals > 0 ? "pending review" : "queue empty";

  return (
    <div>
      <div className={`hb tone-${tone}`}>
        <div className="hb-left">
          <div className={`hb-line ${tone}`}>
            <span className="pulse-dot" />
            <span>System status</span>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <strong>{fleet.daemonAlive ? "supervisor online" : "supervisor offline"}</strong>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <span>last decision <strong>{lastDecision}</strong></span>
          </div>
          <h1 className={`hb-verdict tone-${tone}`}>
            <em>{fleet.verdict[0]}</em>{fleet.verdict[1]}
          </h1>
          <div className="hb-meta">
            <div><span>Projects</span><strong>{fleet.summary?.projects || projects.length}</strong></div>
            <div><span>Running</span><strong>{fleet.workerCount}</strong></div>
            <div><span>PRs open</span><strong>{fleet.prCount}</strong></div>
            <div><span>Mode</span><strong>{fleet.readOnly ? "read-only" : "controls"}</strong></div>
          </div>
          <div className="hb-actions">
            {fleet.nextAction?.project && (
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
            title="Live workers"
            sub={`${fleet.workerCount} running`}
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
        <div style={{ color: "var(--fg-1)", fontSize: 13 }}>No workers running.</div>
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
          <div className="dec-t mono">#{a.pr || "—"}</div>
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
