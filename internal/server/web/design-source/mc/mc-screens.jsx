/* global React */
// ============================================================
// Maestro MC — Project / Workers / Approvals / Settings screens
// ============================================================

// ============================================================
// PROJECT DASHBOARD
// ============================================================
function ProjectScreen({ slug, scenarioKey, now, navigate, openDrawer }) {
  const p = PROJECTS[slug];
  if (!p) return (
    <div style={{ padding: "var(--s-12)", textAlign: "center", color: "var(--fg-2)" }}>
      Project not found. <a onClick={() => navigate("fleet")}>← back to fleet</a>
    </div>
  );

  const state = projectState(slug, scenarioKey);
  const ws = workerSessions(scenarioKey, now);
  const projectLive = ws.live.filter(w => w.project === slug);
  const decisions = supervisorDecisions(scenarioKey, now);

  // Per-project verdict
  let verdict, vtone;
  if (slug === "ok-gobot") {
    verdict = ["Unconfigured.", " This project has no outcome brief or runner. Set up or remove."];
    vtone = "idle";
  } else if (scenarioKey === "broken") {
    verdict = ["No supervisor signal.", " Last decision 3m ago. Maestro is not making decisions for this project."];
    vtone = "stuck";
  } else if (slug === "maestro" && scenarioKey === "attention") {
    verdict = ["PR #331 has been waiting 47 minutes.", " Past 30m SLA. Review or override."];
    vtone = "stuck";
  } else if (slug === "maestro") {
    verdict = ["Idle by policy.", " 11 of 14 open issues held as epic/meta. 3 blocked by deps. Nothing to start."];
    vtone = "idle";
  } else if (slug === "finance-tracker" && scenarioKey === "busy") {
    verdict = ["2 workers in flight.", " CI green on both. PR #42 in healthy review."];
    vtone = "ok";
  } else if (slug === "finance-tracker" && scenarioKey === "attention") {
    verdict = ["1 worker active.", " Monitoring PR #42 (review pending 12m, healthy)."];
    vtone = "ok";
  } else {
    verdict = ["Idle, healthy.", " Monitoring PR #42. No eligible issues to start this tick."];
    vtone = "ok";
  }

  return (
    <div>
      {/* Project hero */}
      <div className={`hb tone-${vtone}`} style={{ gridTemplateColumns: "1fr 320px" }}>
        <div className="hb-left">
          <div className={`hb-line ${vtone}`}>
            <span className="pulse-dot" />
            <span>Project</span>
            <strong>{slug}</strong>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <span><Icon.Github /> {p.repo}</span>
            <span style={{ color: "var(--fg-4)" }}>·</span>
            <span>backend <strong>{p.backend}</strong></span>
          </div>
          <h1 className={`hb-verdict tone-${vtone}`}>
            <em>{verdict[0]}</em>{verdict[1]}
          </h1>
          {p.goal ? (
            <div style={{ fontSize: 14, color: "var(--fg-2)", maxWidth: "60ch", marginTop: -4 }}>
              <strong style={{ color: "var(--fg-1)" }}>Goal:</strong> {p.goal}
            </div>
          ) : (
            <div style={{ fontSize: 14, color: "var(--watch)" }}>
              No outcome brief configured. <a>Define one →</a>
            </div>
          )}
          <div className="hb-actions">
            <button className="tb-btn" onClick={() => navigate(`workers?project=${slug}`)}>Open workers →</button>
            <button className="tb-btn">Open in GitHub →</button>
            <button className="tb-btn">Pause project</button>
          </div>
        </div>
        <ProjectMiniHeartbeat tone={vtone} slug={slug} scenarioKey={scenarioKey} />
      </div>

      {/* Grid: workers + outcome + queue + decisions */}
      <div className="dash-grid mt-6">
        <Panel
          title="Workers"
          sub={projectLive.length > 0 ? `${projectLive.length} live` : "none running"}
          right={<a onClick={() => navigate(`workers?project=${slug}`)} style={{ fontSize: 11.5 }}>Open workers →</a>}
        >
          {projectLive.length > 0 ? (
            <div style={{ padding: "var(--s-2)" }}>
              {projectLive.map(w => (
                <div key={w.slot} className="dec" style={{ gridTemplateColumns: "80px 1fr auto" }} onClick={() => openDrawer(w)}>
                  <div className="dec-t mono">{w.slot}</div>
                  <div className="dec-body">
                    <div style={{ color: "var(--fg-0)", fontSize: 13 }}>#{w.issue.num} {w.issue.title}</div>
                    <div className="dim" style={{ fontSize: 11.5, marginTop: 2 }}>{w.summary}</div>
                  </div>
                  <div style={{ textAlign: "right" }}>
                    <Pill tone={w.tone} noDot>{w.status}</Pill>
                    <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>{relTime(w.age, now)}</div>
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div style={{ padding: "var(--s-5)", textAlign: "center" }}>
              <div style={{ color: "var(--fg-1)", fontSize: 13 }}>No workers running on {slug}.</div>
              <div className="mono dim mt-2" style={{ fontSize: 11 }}>{p.sessionsToday} sessions completed today · {p.sessions7d} this week</div>
              <a style={{ fontSize: 12, marginTop: 8, display: "inline-block" }} onClick={() => navigate(`workers?project=${slug}`)}>View history →</a>
            </div>
          )}
        </Panel>

        <Panel title="Outcome" sub={p.goal ? "passing" : "not configured"}>
          <div style={{ padding: "var(--s-4) var(--s-5)" }}>
            {p.goal ? (
              <>
                <div className="kv"><span>Health</span><strong style={{ color: "var(--ok)" }}>passing</strong></div>
                <div className="kv"><span>Runtime</span><a className="mono">{p.runtime.replace("http://", "")}</a></div>
                <div className="kv"><span>Last check</span><span className="mono">3m ago — 200 OK · 87ms</span></div>
                <div className="kv"><span>Sessions / 7d</span><strong className="mono">{p.sessions7d}</strong></div>
                <div className="kv"><span>Throughput</span><strong className="mono">{p.sessions7d} merges</strong></div>
              </>
            ) : (
              <>
                <div style={{ color: "var(--fg-1)", marginBottom: 12 }}>
                  Maestro will judge progress by issue throughput only.
                </div>
                <button className="tb-btn primary">Define outcome brief</button>
              </>
            )}
          </div>
        </Panel>

        <Panel title="Queue" sub={`${p.open} open · ${p.eligible} eligible`}>
          <div style={{ padding: "var(--s-4) var(--s-5)" }}>
            {p.open > 0 ? (
              <>
                <QueueBar ready={p.eligible} held={p.held} blocked={p.blocked} height={8} />
                <div className="kv mt-4"><span style={{ color: "var(--ok)" }}>Ready</span><strong className="mono">{p.eligible}</strong></div>
                <div className="kv"><span style={{ color: "var(--policy)" }}>Held (epic/meta)</span><strong className="mono">{p.held}</strong></div>
                <div className="kv"><span style={{ color: "var(--stuck)" }}>Blocked (deps)</span><strong className="mono">{p.blocked}</strong></div>
                <div className="mono dim mt-4" style={{ fontSize: 11 }}>Wave policy: dynamic · max parallel: 5</div>
              </>
            ) : (
              <div className="dim" style={{ textAlign: "center", padding: "var(--s-4)" }}>
                No open issues with matching label.
              </div>
            )}
          </div>
        </Panel>

        <div className="span-2">
          <Panel
            title="Recent supervisor decisions"
            sub={`${decisions.length} in last 60 min`}
            right={<a style={{ fontSize: 11.5 }}>Full history →</a>}
          >
            <div className="decisions-list">
              {decisions.map((d, i) => (
                <div key={i} className="dec">
                  <div className="dec-t">{relTime(d.t, now)}</div>
                  <div className="dec-body">
                    <div className="dec-verb">
                      {d.verb}
                      {d.warn && <span style={{ color: "var(--watch)", marginLeft: 6 }}>· past SLA</span>}
                    </div>
                    <div className="dec-note">{d.note}</div>
                  </div>
                  <div className="dec-conf">conf {(d.conf * 100).toFixed(0)}%</div>
                </div>
              ))}
            </div>
          </Panel>
        </div>
      </div>
    </div>
  );
}

// Mini ECG just for project view
function ProjectMiniHeartbeat({ tone, slug, scenarioKey }) {
  const events = tapeEvents(slug, scenarioKey);
  return (
    <div className="ecg" style={{ height: 132 }}>
      <div className={`ecg-corner ${tone}`}>
        <span className="led" /> ACTIVITY · 60M
      </div>
      <div style={{
        position: "absolute", inset: "32px 12px 28px 12px",
        background:
          "repeating-linear-gradient(90deg, transparent 0, transparent calc(100% / 12 - 1px), var(--grid-line) calc(100% / 12 - 1px), var(--grid-line) calc(100% / 12))",
        borderRadius: 3,
      }}>
        {events.map((e, i) => {
          const style = {
            position: "absolute",
            left: `${e.x * 100}%`,
            width: e.w ? `${e.w * 100}%` : "3px",
            borderRadius: 2,
            transition: "all 200ms ease",
          };
          if (e.kind === "run") return <div key={i} style={{ ...style, top: "50%", transform: "translateY(-50%)", height: 22, background: "var(--ok)", opacity: e.live ? 1 : 0.8, animation: e.live ? "blink 1.4s ease-in-out infinite" : undefined, boxShadow: e.live ? "var(--glow-ok)" : undefined }} />;
          if (e.kind === "pr") return <div key={i} style={{ ...style, top: "12%", height: 8, background: "var(--info)" }} />;
          if (e.kind === "merge") return <div key={i} style={{ ...style, top: "50%", transform: "translateY(-50%)", width: 3, height: 28, background: "var(--policy)" }} />;
          if (e.kind === "held") return <div key={i} style={{ ...style, top: "50%", transform: "translateY(-50%)", height: 22, background: "var(--policy)", opacity: 0.5 }} />;
          if (e.kind === "stuck") return <div key={i} style={{ ...style, top: "50%", transform: "translateY(-50%)", height: 22, background: "var(--stuck)", boxShadow: "var(--glow-stuck)", animation: "blink 0.8s ease-in-out infinite" }} />;
          if (e.kind === "fail") return <div key={i} style={{ ...style, top: "70%", height: 6, background: "var(--stuck)" }} />;
          return null;
        })}
        <div style={{ position: "absolute", top: 0, bottom: 0, left: "100%", width: 1, background: "var(--accent)", boxShadow: "0 0 6px var(--accent)" }} />
      </div>
      <div className="ecg-rate">
        <strong>{events.length}</strong> <span>events</span>
      </div>
    </div>
  );
}

// ============================================================
// WORKERS SCREEN
// ============================================================
function WorkersScreen({ scenarioKey, now, navigate, openDrawer, selectedSlot, filterProject }) {
  const [scope, setScope] = React.useState("live");
  const [showOlder, setShowOlder] = React.useState(false);
  const ws = workerSessions(scenarioKey, now);
  const allLive = filterProject ? ws.live.filter(w => w.project === filterProject) : ws.live;
  const allToday = filterProject ? ws.today.filter(w => w.project === filterProject) : ws.today;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "end", justifyContent: "space-between", marginBottom: "var(--s-4)" }}>
        <div>
          <h1 style={{ fontSize: 28, fontWeight: 600, letterSpacing: "-0.02em", color: "var(--fg-0)", margin: 0 }}>Workers</h1>
          <div className="mono dim mt-2" style={{ fontSize: 12 }}>
            {filterProject ? <>filtered by <strong style={{ color: "var(--fg-1)" }}>{filterProject}</strong> · </> : null}
            {allLive.length} live · {ws.todayCount} today · {ws.olderCount} older
            {filterProject && <a onClick={() => navigate("workers")} style={{ marginLeft: 8 }}>clear</a>}
          </div>
        </div>
        <div className="row gap-2">
          <div className="sb-search" style={{ margin: 0, width: 280 }}>
            <Icon.Search s={12} />
            <input placeholder="project:fin status:pr_open …" />
            <kbd>/</kbd>
          </div>
          <Segmented
            value={scope}
            onChange={setScope}
            options={[
              { value: "live",  label: "Live",  count: allLive.length },
              { value: "today", label: "Today", count: ws.todayCount },
              { value: "7d",    label: "7d",    count: ws.olderCount + ws.todayCount },
              { value: "all",   label: "All" },
            ]}
          />
        </div>
      </div>

      <div className="wt">
        <div className="wt-head">
          <div>SLOT</div>
          <div>ISSUE · PROJECT</div>
          <div>STATUS</div>
          <div>BRANCH / PR</div>
          <div>NEXT</div>
          <div>AGE</div>
        </div>

        {/* LIVE */}
        {(scope === "live" || scope === "today" || scope === "all") && (
          <>
            <div className="wt-group">
              <span className="pill live no-dot" style={{ background: "transparent", border: "none", padding: 0, color: "var(--ok)" }}>● live</span>
              <strong>{allLive.length} workers in flight</strong>
              <span className="dim mono" style={{ fontSize: 10.5, marginLeft: "auto" }}>refresh 5s · auto</span>
            </div>
            {allLive.length === 0 ? (
              <div style={{ padding: "var(--s-8) var(--s-4)", textAlign: "center", color: "var(--fg-2)", background: "var(--bg-1)", borderBottom: "1px solid var(--border-1)" }}>
                <div style={{ fontSize: 13 }}>No workers running.</div>
                <div className="mono dim mt-2" style={{ fontSize: 11 }}>{scenarioKey === "broken" ? "Daemon offline." : "Supervisor checking every 10 minutes."}</div>
              </div>
            ) : allLive.map(w => (
              <div key={w.slot} className={`wt-row ${selectedSlot === w.slot ? "selected" : ""}`} onClick={() => openDrawer(w)}>
                <div className="wt-slot">{w.slot}</div>
                <div>
                  <div className="wt-issue">#{w.issue.num} {w.issue.title}</div>
                  <div className="wt-project">{w.project}</div>
                </div>
                <div><Pill tone={w.tone} noDot>{w.status}</Pill></div>
                <div className="mono" style={{ fontSize: 11, color: "var(--fg-1)" }}>
                  {w.pr ? <>PR #{w.pr}</> : <span className="dim">draft</span>}
                  <div className="dim" style={{ fontSize: 10, marginTop: 2 }}>{w.branch}</div>
                </div>
                <div className="wt-next">{w.summary}</div>
                <div className="wt-age">{relTime(w.age, now)}</div>
              </div>
            ))}
          </>
        )}

        {/* TODAY (completed) */}
        {(scope === "today" || scope === "all") && (
          <>
            <div className="wt-group">
              <span className="pill ok no-dot" style={{ background: "transparent", border: "none", padding: 0, color: "var(--ok)" }}>● done</span>
              <strong>{allToday.length} merged today</strong>
              <span className="dim mono" style={{ fontSize: 10.5, marginLeft: "auto" }}>avg cycle 14m</span>
            </div>
            {allToday.map(w => (
              <div key={w.slot} className="wt-row" onClick={() => openDrawer(w)}>
                <div className="wt-slot dim">{w.slot}</div>
                <div>
                  <div className="wt-issue dim2">#{w.issue.num} {w.issue.title}</div>
                  <div className="wt-project">{w.project}</div>
                </div>
                <div><Pill tone="ok" noDot>merged</Pill></div>
                <div className="mono dim" style={{ fontSize: 11 }}>merged</div>
                <div className="dim" style={{ fontSize: 11.5 }}>{w.summary}</div>
                <div className="wt-age">{relTime(w.age, now)}</div>
              </div>
            ))}
          </>
        )}

        {/* OLDER (collapsed) */}
        {(scope === "7d" || scope === "all") && (
          <>
            {!showOlder ? (
              <div className="wt-group-collapsed" onClick={() => setShowOlder(true)}>
                <Icon.Chevron dir="right" />
                <strong>{ws.olderCount} older sessions</strong>
                <span className="dim mono" style={{ fontSize: 11 }}>last 7 days · audit history</span>
                <div className="age-bar" style={{ marginLeft: 8 }}><span style={{ width: "82%" }} /></div>
                <span className="dim mono" style={{ fontSize: 11 }}>82% merged · 14% terminated · 4% failed</span>
              </div>
            ) : (
              <div className="wt-group">
                <Icon.Chevron dir="down" />
                <strong>Older — 32 sessions · last 7d</strong>
                <a onClick={() => setShowOlder(false)} style={{ marginLeft: "auto", fontSize: 11 }}>collapse</a>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

// ============================================================
// WORKER DRAWER
// ============================================================
function WorkerDrawer({ worker, onClose, now }) {
  const [tab, setTab] = React.useState("log");
  if (!worker) return null;
  return (
    <>
      <div className="drawer-scrim" onClick={onClose} />
      <div className="drawer">
        <div className="drawer-head">
          <div>
            <div className="mono" style={{ fontSize: 11, color: "var(--fg-3)" }}>SLOT</div>
            <div style={{ fontSize: 16, fontWeight: 600, color: "var(--fg-0)" }} className="mono">{worker.slot}</div>
          </div>
          <Pill tone={worker.tone} noDot>{worker.status}</Pill>
          <button className="x" onClick={onClose}><Icon.X /></button>
        </div>
        <div className="drawer-body">
          <div className="drawer-sec">
            <div className="drawer-sec-title">Issue</div>
            <a style={{ fontSize: 14, fontWeight: 600 }}>#{worker.issue.num} {worker.issue.title}</a>
            <div className="mono dim mt-2" style={{ fontSize: 11 }}>{worker.project} · {worker.branch}</div>
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title">Supervisor reasoning</div>
            <div style={{ background: "var(--bg-2)", borderRadius: "var(--r-2)", padding: "var(--s-3)", fontSize: 12.5, color: "var(--fg-1)", borderLeft: "2px solid var(--accent)" }}>
              <strong style={{ color: "var(--accent)" }}>monitor_open_pr</strong>
              <div className="mt-2">{worker.summary}</div>
              <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>confidence 0.92 · risk safe</div>
            </div>
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title">Gates</div>
            <div className="kv"><span>CI</span><Pill tone="ok" noDot>passing</Pill></div>
            <div className="kv"><span>Greptile review</span><Pill tone={worker.stuckReason ? "stuck" : "watch"} noDot>{worker.stuckReason ? "stale 47m" : "in progress"}</Pill></div>
            <div className="kv"><span>Mergeable</span><Pill tone="ok" noDot>yes</Pill></div>
            <div className="kv"><span>Human review</span><Pill tone={worker.stuckReason ? "stuck" : "watch"} noDot>{worker.stuckReason ? "past SLA" : "waiting"}</Pill></div>
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title row" style={{ justifyContent: "space-between" }}>
              <span>Live log</span>
              <span className="dim">tailing · 5s</span>
            </div>
            <div className="log">
              {SAMPLE_LOG.map(([tone, msg], i) => (
                <div key={i} className={`ln ${tone}`}>
                  <span className="t">11:32:{(i*3).toString().padStart(2,"0")}</span>
                  <span className="m">{msg}</span>
                </div>
              ))}
              <div className="ln info">
                <span className="t">11:35:18</span>
                <span className="m">claude: monitoring PR for review feedback<span className="cursor"></span></span>
              </div>
            </div>
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title">Actions</div>
            <div className="row gap-2">
              <button className="tb-btn">Open PR in GitHub →</button>
              <button className="tb-btn ghost">Restart worker</button>
              <button className="tb-btn ghost" style={{ color: "var(--stuck)" }}>Stop worker</button>
            </div>
            <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>Write actions disabled in read-only mode.</div>
          </div>
        </div>
      </div>
    </>
  );
}

// ============================================================
// APPROVALS
// ============================================================
function ApprovalsScreen({ scenarioKey, now, navigate }) {
  const [showAudit, setShowAudit] = React.useState(false);
  const apps = approvalsList(scenarioKey, now);
  const stuck = apps.filter(a => a.state === "stuck");
  const watch = apps.filter(a => a.state === "watch");

  return (
    <div>
      <div style={{ marginBottom: "var(--s-4)" }}>
        <h1 style={{ fontSize: 28, fontWeight: 600, letterSpacing: "-0.02em", color: "var(--fg-0)", margin: 0 }}>Approvals</h1>
        <div className="mono dim mt-2" style={{ fontSize: 12 }}>
          {apps.length} active · {APPROVALS_AUDIT.length} in audit history
        </div>
      </div>

      {/* Past SLA */}
      {stuck.length > 0 && (
        <>
          <div className="layout-head" style={{ marginTop: 0 }}>
            <h2 style={{ color: "var(--stuck)" }}>Past SLA · act now</h2>
            <div className="hint">{stuck.length} stuck</div>
          </div>
          <div className="appv">
            {stuck.map((a, i) => <ApprovalRow key={i} a={a} now={now} />)}
          </div>
        </>
      )}

      {/* In progress */}
      {watch.length > 0 && (
        <>
          <div className="layout-head"><h2>Within SLA · watching</h2><div className="hint">{watch.length} pending</div></div>
          <div className="appv">
            {watch.map((a, i) => <ApprovalRow key={i} a={a} now={now} />)}
          </div>
        </>
      )}

      {apps.length === 0 && (
        <div style={{ padding: "var(--s-12)", textAlign: "center", color: "var(--fg-2)", background: "var(--bg-1)", borderRadius: "var(--r-4)", border: "1px solid var(--border-1)" }}>
          <div style={{ fontSize: 15, color: "var(--fg-1)" }}>Inbox zero.</div>
          <div className="mono dim mt-2" style={{ fontSize: 11 }}>No approvals pending. Maestro is operating autonomously.</div>
        </div>
      )}

      {/* Audit */}
      <div className="layout-head"><h2>Audit history</h2><div className="hint">{APPROVALS_AUDIT.length} prior</div></div>
      {!showAudit ? (
        <div className="audit-fold" onClick={() => setShowAudit(true)}>
          <Icon.Chevron dir="right" />
          <div>
            <strong>{APPROVALS_AUDIT.length} past approvals</strong>
            <div className="dim" style={{ fontSize: 11.5, marginTop: 2 }}>Approved · rejected · superseded. Click to expand.</div>
          </div>
        </div>
      ) : (
        <div className="appv">
          {APPROVALS_AUDIT.map((a, i) => (
            <div key={i} className="app-row stale">
              <div className="app-row-stage">
                <small>#{a.pr}</small>
                <strong>—</strong>
              </div>
              <div className="app-row-body">
                <h4 style={{ textDecoration: "line-through", color: "var(--fg-2)" }}>{a.title}</h4>
                <div className="meta">{a.project} · {a.state}</div>
              </div>
              <div className="app-row-actions">
                <span className="age">{Math.floor(a.ageMin / 60)}h ago</span>
              </div>
            </div>
          ))}
          <a onClick={() => setShowAudit(false)} className="mono" style={{ fontSize: 11 }}>collapse</a>
        </div>
      )}
    </div>
  );
}

function ApprovalRow({ a, now }) {
  const overdue = a.ageMin > a.sla;
  return (
    <div className={`app-row ${a.state}`}>
      <div className="app-row-stage">
        <strong>#{a.pr}</strong>
        <small>{a.stage}</small>
      </div>
      <div className="app-row-body">
        <h4>{a.title}</h4>
        <div className="meta">
          <a>{a.project}</a> · author <strong className="mono" style={{ color: "var(--fg-1)" }}>{a.author}</strong> · reviewer <strong className="mono" style={{ color: "var(--fg-1)" }}>{a.reviewer}</strong>
        </div>
        <p>{a.body}</p>
      </div>
      <div className="app-row-actions">
        <span className={`age ${overdue ? "bad" : ""}`}>{a.ageMin}m {overdue && `· SLA ${a.sla}m`}</span>
        <Pill tone={a.state} noDot>{overdue ? "past SLA" : "waiting"}</Pill>
        <button className="tb-btn primary" style={{ fontSize: 11 }}>Open PR →</button>
      </div>
    </div>
  );
}

// ============================================================
// SETTINGS
// ============================================================
function SettingsScreen({ scenarioKey, now }) {
  const [section, setSection] = React.useState("general");
  const [toggles, setToggles] = React.useState({ readOnly: true, autoMerge: true, autoRebase: true, telegram: true, retryReview: false });
  const t = (k) => setToggles({ ...toggles, [k]: !toggles[k] });

  return (
    <div>
      <h1 style={{ fontSize: 28, fontWeight: 600, letterSpacing: "-0.02em", color: "var(--fg-0)", margin: 0 }}>Settings</h1>
      <div className="mono dim mt-2" style={{ fontSize: 12 }}>~/.maestro/maestro.yaml · loaded 11:35:13</div>

      <div className="settings-grid">
        <div className="settings-nav">
          {[
            ["general", "General"],
            ["projects", "Projects"],
            ["backends", "AI backends"],
            ["policies", "Wave policies"],
            ["notifications", "Notifications"],
            ["security", "Auth & audit"],
          ].map(([k, lbl]) => (
            <a key={k} className={section === k ? "active" : ""} onClick={() => setSection(k)}>{lbl}</a>
          ))}
        </div>

        <div>
          {section === "general" && (
            <>
              <div className="setting-card">
                <h4>Daemon</h4>
                <div className="setting-row">
                  <label>Read-only mode</label>
                  <span className="desc">Block mutating HTTP endpoints. V1 default.</span>
                  <div className={`switch ${toggles.readOnly ? "on" : ""}`} onClick={() => t("readOnly")} />
                </div>
                <div className="setting-row">
                  <label>Max parallel workers</label>
                  <span className="desc">How many sessions can run simultaneously.</span>
                  <input className="setting-input" defaultValue="5" style={{ width: 80 }} />
                </div>
                <div className="setting-row">
                  <label>Tick interval</label>
                  <span className="desc">Supervisor decision frequency.</span>
                  <input className="setting-input" defaultValue="10m" style={{ width: 80 }} />
                </div>
              </div>

              <div className="setting-card">
                <h4>Auto-merge</h4>
                <div className="setting-row">
                  <label>Auto-merge on green CI</label>
                  <span className="desc">Merge PRs when all gates pass.</span>
                  <div className={`switch ${toggles.autoMerge ? "on" : ""}`} onClick={() => t("autoMerge")} />
                </div>
                <div className="setting-row">
                  <label>Merge strategy</label>
                  <span className="desc">Sequential (safe) or parallel (faster).</span>
                  <select className="setting-input">
                    <option>sequential</option><option>parallel</option>
                  </select>
                </div>
                <div className="setting-row">
                  <label>Auto-rebase conflicts</label>
                  <span className="desc">Rebase PRs that fall behind main.</span>
                  <div className={`switch ${toggles.autoRebase ? "on" : ""}`} onClick={() => t("autoRebase")} />
                </div>
              </div>

              <div className="setting-card">
                <h4>Review gate</h4>
                <div className="setting-row">
                  <label>Code review backend</label>
                  <span className="desc">Required check before merge.</span>
                  <select className="setting-input"><option>greptile</option><option>none</option></select>
                </div>
                <div className="setting-row">
                  <label>Retry on review feedback</label>
                  <span className="desc">Auto-spawn a follow-up worker if reviewer leaves actionable comments.</span>
                  <div className={`switch ${toggles.retryReview ? "on" : ""}`} onClick={() => t("retryReview")} />
                </div>
              </div>
            </>
          )}

          {section === "projects" && (
            <div className="setting-card">
              <h4>Projects in fleet</h4>
              {PROJECT_ORDER.map(slug => {
                const p = PROJECTS[slug];
                return (
                  <div key={slug} className="setting-row" style={{ gridTemplateColumns: "1fr auto auto", gap: 12 }}>
                    <div>
                      <div style={{ color: "var(--fg-0)", fontWeight: 500 }}>{slug}</div>
                      <div className="dim mono" style={{ fontSize: 11 }}>{p.repo} · max {p.eligible + p.held + p.blocked} parallel</div>
                    </div>
                    <Pill tone={p.goal ? "ok" : "idle"} noDot>{p.goal ? "configured" : "unconfigured"}</Pill>
                    <button className="tb-btn ghost">Edit</button>
                  </div>
                );
              })}
              <button className="tb-btn primary mt-4">+ Add project</button>
            </div>
          )}

          {section === "backends" && (
            <div className="setting-card">
              <h4>AI backends</h4>
              {[
                ["claude", "Anthropic Claude Code", "default", true],
                ["codex", "OpenAI Codex", "@openai/codex", true],
                ["gemini", "Google Gemini", "@google/gemini-cli", false],
                ["cline", "Cline (any provider)", "bun add -g cline", false],
              ].map(([id, name, hint, enabled]) => (
                <div key={id} className="setting-row" style={{ gridTemplateColumns: "1fr 1fr auto" }}>
                  <div>
                    <div style={{ color: "var(--fg-0)", fontWeight: 500 }}>{name}</div>
                    <div className="dim mono" style={{ fontSize: 11 }}>{hint}</div>
                  </div>
                  <div className="mono dim" style={{ fontSize: 11 }}>{id === "claude" ? "DEFAULT" : ""}</div>
                  <Pill tone={enabled ? "ok" : "idle"} noDot>{enabled ? "ready" : "not installed"}</Pill>
                </div>
              ))}
            </div>
          )}

          {section === "policies" && (
            <div className="setting-card">
              <h4>Wave policies</h4>
              <div className="setting-row">
                <label>Active policy</label>
                <span className="desc">Which issues become eligible each tick.</span>
                <select className="setting-input"><option>dynamic</option><option>fifo</option><option>priority-labeled</option></select>
              </div>
              <div className="setting-row">
                <label>Exclude labels</label>
                <span className="desc">Skip issues with these labels.</span>
                <input className="setting-input mono" defaultValue="epic, meta, blocked, wontfix" style={{ width: 280 }} />
              </div>
              <div className="setting-row">
                <label>Include labels</label>
                <span className="desc">OR semantics. Empty = all open.</span>
                <input className="setting-input mono" defaultValue="enhancement" style={{ width: 280 }} />
              </div>
            </div>
          )}

          {section === "notifications" && (
            <div className="setting-card">
              <h4>Telegram</h4>
              <div className="setting-row">
                <label>Enabled</label>
                <span className="desc">Send events through OpenClaw gateway.</span>
                <div className={`switch ${toggles.telegram ? "on" : ""}`} onClick={() => t("telegram")} />
              </div>
              <div className="setting-row">
                <label>Gateway</label>
                <span className="desc">Local HTTP gateway URL.</span>
                <input className="setting-input mono" defaultValue="http://localhost:18789" style={{ width: 240 }} />
              </div>
              <div className="setting-row">
                <label>Quiet hours</label>
                <span className="desc">Suppress non-critical notifications.</span>
                <input className="setting-input mono" defaultValue="22:00 – 08:00 CEST" style={{ width: 240 }} />
              </div>
            </div>
          )}

          {section === "security" && (
            <div className="setting-card">
              <h4>Auth &amp; audit (V2 preview)</h4>
              <div className="dim" style={{ marginBottom: 12 }}>Write operations land in V2 behind a confirm-and-audit flow. Below is the preview.</div>
              <div className="setting-row">
                <label>Operator session timeout</label>
                <span className="desc">Re-auth required for write actions.</span>
                <input className="setting-input" defaultValue="30m" style={{ width: 80 }} />
              </div>
              <div className="setting-row">
                <label>Require reason on stop</label>
                <span className="desc">Operator must enter ≥10 chars to stop a worker.</span>
                <div className="switch on" />
              </div>
              <div className="setting-row">
                <label>Audit log</label>
                <span className="desc">Persist all write actions.</span>
                <Pill tone="ok" noDot>~/.maestro/audit.jsonl</Pill>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

Object.assign(window, { ProjectScreen, WorkersScreen, WorkerDrawer, ApprovalsScreen, SettingsScreen });
