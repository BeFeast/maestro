/* global React */
// ============================================================
// Maestro MC — FLEET screen with 3 layout variants
//   tape  — bold timeline-per-project (the metaphor pick)
//   rail  — dense data table (Datadog-style)
//   cards — 3-up dense cards
// ============================================================

function FleetScreen({ scenarioKey, layout, now, navigate, setLayout }) {
  const sc = SCENARIOS[scenarioKey];
  const projects = PROJECT_ORDER.map(s => ({ slug: s, ...PROJECTS[s], state: projectState(s, scenarioKey) }));

  const liveCount = projects.filter(p => p.state.state === "live").length;
  const attCount = projects.filter(p => p.state.state === "stuck").length;
  const idleCount = projects.length - liveCount - attCount;

  return (
    <div>
      {/* HEARTBEAT — verdict hero */}
      <div className={`hb tone-${sc.tone}`}>
        <div className="hb-left">
          <div className={`hb-line ${sc.tone}`}>
            <span className="pulse-dot" />
            <span>System status</span>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <strong>{sc.daemonAlive ? "supervisor online" : "supervisor offline"}</strong>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <span>last decision <strong>{sc.daemonAlive ? "12s ago" : "3m ago"}</strong></span>
          </div>
          <h1 className={`hb-verdict tone-${sc.tone}`}>
            <em>{sc.verdict[0]}</em>{sc.verdict[1]}
          </h1>
          <div className="hb-meta">
            <div><span>Wave policy</span><strong>dynamic</strong></div>
            <div><span>Backend</span><strong>claude</strong></div>
            <div><span>Max parallel</span><strong>5</strong></div>
            <div><span>Auto-merge</span><strong>sequential</strong></div>
          </div>
          <div className="hb-actions">
            {sc.tone === "watch" && (
              <button className="tb-btn primary" onClick={() => navigate("project/maestro")}>Open maestro →</button>
            )}
            {sc.tone === "stuck" && (
              <button className="tb-btn danger" onClick={() => navigate("workers")}>Tail daemon log →</button>
            )}
            {sc.tone === "ok" && scenarioKey === "busy" && (
              <button className="tb-btn" onClick={() => navigate("workers")}>Watch live workers →</button>
            )}
            {sc.tone === "ok" && scenarioKey === "healthy" && (
              <button className="tb-btn ghost" onClick={() => navigate("workers")}>View recent activity →</button>
            )}
            <button className="tb-btn ghost">View runbook →</button>
          </div>
        </div>
        <Heartbeat tone={sc.tone} bpm={sc.heartbeatBpm} daemonAlive={sc.daemonAlive} />
      </div>

      {/* STAT STRIP */}
      <div className="stats">
        <Stat
          label="Workers"
          value={sc.workerCount}
          of={` / 5`}
          tone={sc.workerCount > 0 ? "ok" : "info"}
          sub={sc.workerCount > 0 ? "running now" : "max parallel"}
          live={sc.workerCount > 0}
          onClick={() => navigate("workers")}
        />
        <Stat
          label="PRs in flight"
          value={sc.prCount}
          tone={scenarioKey === "attention" ? "watch" : "info"}
          sub={scenarioKey === "attention" ? "1 stalled · 3 healthy" : sc.prCount > 0 ? "all in healthy review" : "none open"}
        />
        <Stat
          label="Attention"
          value={sc.attentionCount}
          tone={sc.attentionCount > 0 ? (scenarioKey === "broken" ? "stuck" : "watch") : "info"}
          sub={sc.attentionCount > 0 ? "items need you" : "nothing waiting"}
        />
        <Stat
          label="Approvals"
          value={sc.activeApprovals}
          tone={sc.activeApprovals > 0 ? "watch" : "info"}
          sub={sc.activeApprovals > 0 ? "pending review" : "queue empty"}
          onClick={() => navigate("approvals")}
        />
        <Stat
          label="Throughput"
          value="49"
          tone="ok"
          sub="merged PRs · 7d"
          sparkline={genSpark(scenarioKey, 1)}
          sparkWarm={scenarioKey === "attention"}
          tooltip="Merged PRs across all projects in the last 7 days (UTC). Bars show per-day counts, oldest on the left."
        />
      </div>

      {/* LAYOUT HEADER + variant switcher */}
      <div className="layout-head">
        <div>
          <h2>Projects</h2>
          <div className="hint mono mt-2" style={{ fontSize: 11.5 }}>
            {projects.length} total · <span style={{ color: "var(--ok)" }}>{liveCount} live</span>
            {" · "}<span style={{ color: scenarioKey === "attention" ? "var(--watch)" : "var(--fg-3)" }}>{attCount} attention</span>
            {" · "}<span style={{ color: "var(--fg-3)" }}>{idleCount} idle</span>
          </div>
        </div>
        <div className="row gap-2">
          <Segmented
            value={layout}
            onChange={setLayout}
            options={[
              { value: "tape",  label: "Tape",  icon: <Icon.Tape /> },
              { value: "rail",  label: "Rail",  icon: <Icon.List /> },
              { value: "cards", label: "Cards", icon: <Icon.Grid /> },
            ]}
          />
        </div>
      </div>

      {layout === "tape"  && <FleetTape  projects={projects} scenarioKey={scenarioKey} navigate={navigate} now={now} />}
      {layout === "rail"  && <FleetRail  projects={projects} scenarioKey={scenarioKey} navigate={navigate} />}
      {layout === "cards" && <FleetCards projects={projects} scenarioKey={scenarioKey} navigate={navigate} />}

      {/* SECONDARY: live workers preview + approvals preview */}
      <div className="dash-grid mt-6">
        <div>
          <Panel
            title="Live workers"
            sub={`${sc.workerCount} of 5`}
            right={<a onClick={() => navigate("workers")} style={{ fontSize: 11.5 }}>Open workers →</a>}
          >
            <LiveWorkersPreview scenarioKey={scenarioKey} now={now} navigate={navigate} />
          </Panel>
        </div>
        <div>
          <Panel
            title="Approvals"
            sub={`${sc.activeApprovals} pending`}
            right={<a onClick={() => navigate("approvals")} style={{ fontSize: 11.5 }}>Open inbox →</a>}
          >
            <ApprovalsPreview scenarioKey={scenarioKey} now={now} />
          </Panel>
        </div>
      </div>
    </div>
  );
}

// ============================================================
// VARIANT A — TAPE: each project = horizontal timeline
// ============================================================
function FleetTape({ projects, scenarioKey, navigate, now }) {
  return (
    <div className="tape">
      <div className="tape-ruler">
        <div>PROJECT · STATE</div>
        <div className="tape-ruler-ticks">
          {["60m","50m","40m","30m","20m","10m","NOW"].map(t => <span key={t}>{t}</span>)}
        </div>
        <div style={{ textAlign: "right" }}>LAST</div>
      </div>
      {projects.map(p => {
        const events = tapeEvents(p.slug, scenarioKey);
        return (
          <div key={p.slug} className="tape-row" onClick={() => navigate(`project/${p.slug}`)}>
            <div className="tape-id">
              <div className="tape-id-top">
                <ProjectStatePill state={p.state} />
              </div>
              <div className="tape-name mt-2">{p.slug}</div>
              <div className="tape-summary">{projectSummaryLine(p, scenarioKey)}</div>
            </div>
            <div className="tape-canvas">
              {events.map((e, i) => {
                const style = {
                  left: `${e.x * 100}%`,
                  width: e.w ? `${e.w * 100}%` : "3px",
                };
                return <div key={i} className={`tape-event ${e.kind} ${e.live ? "live" : ""}`} style={style} />;
              })}
              <div className="tape-now" style={{ left: "100%" }} />
            </div>
            <div className="tape-meta">
              <div className="tape-meta-state mono" style={{ fontSize: 10, color: "var(--fg-3)" }}>{p.state.label || "idle"}</div>
              <div className="tape-meta-age">{p.slug === "ok-gobot" ? "2d ago" : scenarioKey === "broken" ? "stale" : "12s ago"}</div>
            </div>
          </div>
        );
      })}
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
  const tone = { live: "live", stuck: "stuck", ok: "ok", policy: "policy", idle: "idle", unknown: "stuck" }[state.state] || "idle";
  return <Pill tone={tone}>{state.label || state.state}</Pill>;
}

function projectSummaryLine(p, scenarioKey) {
  if (p.slug === "ok-gobot") return "No outcome configured · 0 sessions";
  if (scenarioKey === "broken") return "No data since daemon offline";
  if (p.slug === "finance-tracker") {
    if (scenarioKey === "busy") return "2 workers active · CI green";
    if (scenarioKey === "attention") return "1 worker · monitoring CI";
    return "Idle · monitoring PR #42";
  }
  if (p.slug === "maestro") {
    if (scenarioKey === "attention") return "PR #331 stalled on review · 47m";
    return "11 of 14 open issues held by epic/meta label";
  }
  return "";
}

// ============================================================
// VARIANT B — RAIL: dense data table
// ============================================================
function FleetRail({ projects, scenarioKey, navigate }) {
  return (
    <div className="rail">
      <div className="rail-head">
        <div>PROJECT</div>
        <div>STATE</div>
        <div>QUEUE</div>
        <div>PR</div>
        <div>OUTCOME</div>
        <div>LAST</div>
        <div></div>
      </div>
      {projects.map(p => <RailRow key={p.slug} p={p} scenarioKey={scenarioKey} navigate={navigate} />)}
    </div>
  );
}

function RailRow({ p, scenarioKey, navigate }) {
  const pr = scenarioKey === "attention" && p.slug === "maestro" ? { num: 331, tone: "stuck", label: "stuck 47m" }
    : p.slug === "finance-tracker" ? { num: 42, tone: "watch", label: "in review" }
    : null;
  return (
    <div className="rail-row" onClick={() => navigate(`project/${p.slug}`)}>
      <div>
        <div style={{ fontWeight: 600, color: "var(--fg-0)" }}>{p.slug}</div>
        <div className="mono" style={{ fontSize: 10.5, color: "var(--fg-3)", marginTop: 1 }}>
          <Icon.Github /> &nbsp;{p.repo}
        </div>
      </div>
      <div><ProjectStatePill state={p.state} /></div>
      <div>
        {p.slug === "ok-gobot" ? (
          <span className="dim mono" style={{ fontSize: 11 }}>—</span>
        ) : (
          <div className="rail-q">
            <QueueBar ready={p.eligible} held={p.held} blocked={p.blocked} />
            <span className="rail-q-num">{p.eligible}/{p.open}</span>
          </div>
        )}
      </div>
      <div>
        {pr ? (
          <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontFamily: "var(--font-mono)", fontSize: 11 }}>
            <Icon.GitPR /> #{pr.num}
            <Pill tone={pr.tone} noDot>{pr.label}</Pill>
          </span>
        ) : <span className="dim mono" style={{ fontSize: 11 }}>—</span>}
      </div>
      <div>
        {p.goal ? (
          <div style={{ fontSize: 12, color: "var(--fg-1)", maxWidth: "32ch", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
            {p.goal}
          </div>
        ) : (
          <span className="dim mono" style={{ fontSize: 11 }}>not configured</span>
        )}
      </div>
      <div className="mono" style={{ fontSize: 11, color: "var(--fg-3)" }}>
        {p.slug === "ok-gobot" ? "2d" : scenarioKey === "broken" ? "stale" : "12s"}
      </div>
      <div style={{ textAlign: "right" }}>
        <span className="rail-cta">Open <Icon.Chevron /></span>
      </div>
    </div>
  );
}

// ============================================================
// VARIANT C — CARDS
// ============================================================
function FleetCards({ projects, scenarioKey, navigate }) {
  return (
    <div className="cards">
      {projects.map(p => {
        const stateClass = p.state.state === "live" ? "live" : p.state.state === "stuck" ? "stuck"
          : p.state.state === "policy" ? "policy" : p.state.state === "ok" ? "live" : "idle";
        return (
          <div key={p.slug} className={`card ${stateClass}`} onClick={() => navigate(`project/${p.slug}`)}>
            <div className="card-head">
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8 }}>
                <div className="card-name">{p.slug}</div>
                <ProjectStatePill state={p.state} />
              </div>
              <div className="card-repo"><Icon.Github /> &nbsp;{p.repo}</div>
            </div>

            <div className="card-line summary">
              <span className="lab">Status</span>
              <span style={{ color: "var(--fg-1)" }}>{projectSummaryLine(p, scenarioKey)}</span>
            </div>

            <div className="card-line">
              <span className="lab">Queue</span>
              <span className="mono" style={{ color: "var(--fg-1)" }}>
                {p.slug === "ok-gobot" ? "—" : `${p.eligible} ready · ${p.held} held · ${p.blocked} blocked`}
              </span>
            </div>
            {p.slug !== "ok-gobot" && (
              <div className="card-q">
                <QueueBar ready={p.eligible} held={p.held} blocked={p.blocked} height={6} />
              </div>
            )}

            <div className="card-line">
              <span className="lab">Outcome</span>
              {p.goal ? (
                <span style={{ color: "var(--fg-1)", fontSize: 12 }}>{p.goal}</span>
              ) : (
                <span className="dim mono" style={{ fontSize: 11 }}>not configured</span>
              )}
            </div>

            <div className="card-foot">
              <span>{p.sessions7d} sessions · 7d</span>
              <a onClick={(e) => { e.stopPropagation(); navigate(`project/${p.slug}`); }}>Open →</a>
            </div>
          </div>
        );
      })}
    </div>
  );
}

// ============================================================
// Secondary panels on fleet page
// ============================================================
function LiveWorkersPreview({ scenarioKey, now, navigate }) {
  const ws = workerSessions(scenarioKey, now);
  if (ws.live.length === 0) {
    return (
      <div style={{ padding: "var(--s-5)", textAlign: "center" }}>
        <div style={{ color: "var(--fg-1)", fontSize: 13 }}>No workers running.</div>
        <div className="mono dim mt-2" style={{ fontSize: 11 }}>
          {scenarioKey === "broken" ? "Daemon offline. Restart to resume." : "Supervisor checking every 10m for eligible issues."}
        </div>
      </div>
    );
  }
  return (
    <div style={{ padding: "var(--s-2)" }}>
      {ws.live.map(w => (
        <div key={w.slot} className="dec" style={{ gridTemplateColumns: "80px 1fr auto" }} onClick={() => navigate(`workers?slot=${w.slot}`)}>
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

function ApprovalsPreview({ scenarioKey, now }) {
  const apps = approvalsList(scenarioKey, now);
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
        <div key={i} className="dec" style={{ gridTemplateColumns: "60px 1fr auto" }}>
          <div className="dec-t mono">#{a.pr}</div>
          <div className="dec-body">
            <div style={{ color: "var(--fg-0)", fontSize: 12.5 }}>{a.title}</div>
            <div className="mono dim" style={{ fontSize: 10.5, marginTop: 2 }}>{a.project} · {a.stage}</div>
          </div>
          <div style={{ textAlign: "right" }}>
            <Pill tone={a.state} noDot>{a.ageMin > a.sla ? "past SLA" : "waiting"}</Pill>
            <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>{a.ageMin}m</div>
          </div>
        </div>
      ))}
    </div>
  );
}

Object.assign(window, { FleetScreen });
