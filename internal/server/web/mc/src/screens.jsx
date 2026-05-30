import React from "react";
import { Icon, Panel, Pill, QueueBar, Segmented, ConfirmDialog } from "./atoms.jsx";
import { useFleet } from "./fleetContext.jsx";
import {
  actionLabel,
  fetchWorkerDetail,
  postFleetApproval,
  postProjectApproval,
  projectBySlug,
  supervisorDecisionsFromProject,
  workerSessionsFromFleet,
} from "./fleetApi.js";
import { parseTimestamp, relTime } from "./utils.js";

export function ProjectScreen({ slug, navigate, openDrawer }) {
  const { fleet, now } = useFleet();
  const p = projectBySlug(fleet, slug);

  if (!p) {
    return (
      <div style={{ padding: "var(--s-12)", textAlign: "center", color: "var(--fg-2)" }}>
        Project not found. <a onClick={() => navigate("fleet")}>← back to fleet</a>
      </div>
    );
  }

  const ws = workerSessionsFromFleet(fleet, now);
  const projectLive = ws.live.filter(w => w.project === p.slug || w.project_name === p.name);
  const decisions = supervisorDecisionsFromProject(p, now);
  const vtone = p.state?.state === "stuck" ? "stuck"
    : p.state?.state === "watch" ? "watch"
    : p.state?.state === "live" ? "ok"
    : p.state?.state === "idle" && !p.goal ? "idle"
    : "ok";
  const verdict = [
    p.operatorState?.summary || p.state?.label || "Project status",
    p.operatorState?.next_action ? ` ${p.operatorState.next_action}` : "",
  ];

  return (
    <div>
      <div className={`hb tone-${vtone}`} style={{ gridTemplateColumns: "1fr 320px" }}>
        <div className="hb-left">
          <div className={`hb-line ${vtone}`}>
            <span className="pulse-dot" />
            <span>Project</span>
            <strong>{p.slug}</strong>
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
              No outcome brief configured.
            </div>
          )}
          <div className="hb-actions">
            <button className="tb-btn" onClick={() => navigate(`workers?project=${encodeURIComponent(p.slug)}`)}>Open workers →</button>
            {p.repo && (
              <a className="tb-btn ghost" href={`https://github.com/${p.repo}`} target="_blank" rel="noreferrer">Open in GitHub →</a>
            )}
            {p.dashboardUrl && (
              <a className="tb-btn ghost" href={p.dashboardUrl} target="_blank" rel="noreferrer">Project dashboard →</a>
            )}
          </div>
        </div>
        <ProjectMiniHeartbeat tone={vtone} events={p.tapeEvents || []} />
      </div>

      <div className="dash-grid mt-6">
        <Panel
          title="Workers"
          sub={projectLive.length > 0 ? `${projectLive.length} live` : "none running"}
          right={<a onClick={() => navigate(`workers?project=${encodeURIComponent(p.slug)}`)} style={{ fontSize: 11.5 }}>Open workers →</a>}
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
              <div style={{ color: "var(--fg-1)", fontSize: 13 }}>No workers running on {p.slug}.</div>
              <div className="mono dim mt-2" style={{ fontSize: 11 }}>{p.sessions} sessions recorded · {p.running} running now</div>
              <a style={{ fontSize: 12, marginTop: 8, display: "inline-block" }} onClick={() => navigate(`workers?project=${encodeURIComponent(p.slug)}`)}>View history →</a>
            </div>
          )}
        </Panel>

        <Panel title="Outcome" sub={p.goal ? (p.outcome?.health_state || "configured") : "not configured"}>
          <div style={{ padding: "var(--s-4) var(--s-5)" }}>
            {p.goal ? (
              <>
                <div className="kv"><span>Health</span><strong style={{ color: p.outcome?.health_state === "healthy" ? "var(--ok)" : "var(--watch)" }}>{p.outcome?.health_state || "unknown"}</strong></div>
                <div className="kv"><span>Runtime</span><span className="mono">{p.outcome?.runtime_target || p.runtime || "—"}</span></div>
                <div className="kv"><span>Last check</span><span className="mono">{p.outcome?.health_checked_at ? relTime(parseTimestamp(p.outcome.health_checked_at), now) : "—"}</span></div>
                <div className="kv"><span>Sessions</span><strong className="mono">{p.sessions}</strong></div>
              </>
            ) : (
              <div style={{ color: "var(--fg-1)", marginBottom: 12 }}>
                Maestro will judge progress by issue throughput only.
              </div>
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
                <div className="mono dim mt-4" style={{ fontSize: 11 }}>
                  {p.queueSnapshot?.policy_rule ? `Wave policy: ${p.queueSnapshot.policy_rule}` : ""}
                  {p.maxParallel ? ` · max parallel: ${p.maxParallel}` : ""}
                </div>
              </>
            ) : (
              <div className="dim" style={{ textAlign: "center", padding: "var(--s-4)" }}>
                {p.queueSnapshot?.idle_reason || "No open issues with matching label."}
              </div>
            )}
          </div>
        </Panel>

        <div className="span-2">
          <Panel
            title="Recent supervisor decisions"
            sub={`${decisions.length} latest`}
          >
            <div className="decisions-list">
              {decisions.length === 0 ? (
                <div className="dim" style={{ padding: "var(--s-4)" }}>No supervisor decisions recorded yet.</div>
              ) : decisions.map((d, i) => (
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

function ProjectMiniHeartbeat({ tone, events }) {
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
        {events.length === 0 ? (
          <div className="dim mono" style={{ position: "absolute", inset: 0, display: "flex", alignItems: "center", justifyContent: "center", fontSize: 10 }}>
            No events in last 60m
          </div>
        ) : events.map((e, i) => {
          const style = {
            position: "absolute",
            left: `${e.x * 100}%`,
            width: e.w ? `${e.w * 100}%` : "3px",
            borderRadius: 2,
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

export function WorkersScreen({ navigate, openDrawer, selectedSlot, filterProject }) {
  const { fleet, now } = useFleet();
  const [scope, setScope] = React.useState("live");
  const [showOlder, setShowOlder] = React.useState(false);
  const ws = workerSessionsFromFleet(fleet || { workers: [] }, now);
  const allRunning = filterProject
    ? ws.running.filter(w => w.project === filterProject || w.project_name === filterProject)
    : ws.running;
  const allRecent = filterProject
    ? ws.recent.filter(w => w.project === filterProject || w.project_name === filterProject)
    : ws.recent;
  // backward-compat alias for code below that still reads allLive
  const allLive = allRecent;
  const allToday = filterProject
    ? ws.today.filter(w => w.project === filterProject || w.project_name === filterProject)
    : ws.today;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "end", justifyContent: "space-between", marginBottom: "var(--s-4)" }}>
        <div>
          <h1 style={{ fontSize: 28, fontWeight: 600, letterSpacing: "-0.02em", color: "var(--fg-0)", margin: 0 }}>Workers</h1>
          <div className="mono dim mt-2" style={{ fontSize: 12 }}>
            {filterProject ? <>filtered by <strong style={{ color: "var(--fg-1)" }}>{filterProject}</strong> · </> : null}
            <strong style={{ color: "var(--fg-0)" }}>{allRunning.length} running</strong>
            {" · "}{allRecent.length} recent 24h · {ws.todayCount} today · {ws.olderCount} older
            {filterProject && <a onClick={() => navigate("workers")} style={{ marginLeft: 8 }}>clear</a>}
          </div>
        </div>
        <div className="row gap-2">
          <Segmented
            value={scope}
            onChange={setScope}
            options={[
              { value: "live", label: "Recent", count: allRecent.length },
              { value: "today", label: "Today", count: ws.todayCount },
              { value: "7d", label: "7d", count: ws.olderCount + ws.todayCount },
              { value: "all", label: "All" },
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

        {(scope === "live" || scope === "today" || scope === "all") && (
          <>
            <div className="wt-group">
              <span className="pill live no-dot" style={{ background: "transparent", border: "none", padding: 0, color: "var(--ok)" }}>● running</span>
              <strong>{allRunning.length} {allRunning.length === 1 ? "worker" : "workers"} in flight</strong>
              <span className="dim" style={{ fontSize: 11, marginLeft: 8 }}>· {allRecent.length} active in last 24 h</span>
              <span className="dim mono" style={{ fontSize: 10.5, marginLeft: "auto" }}>refresh 12s · auto</span>
            </div>
            {allRecent.length === 0 ? (
              <div style={{ padding: "var(--s-8) var(--s-4)", textAlign: "center", color: "var(--fg-2)", background: "var(--bg-1)", borderBottom: "1px solid var(--border-1)" }}>
                <div style={{ fontSize: 13 }}>No workers running.</div>
                <div className="mono dim mt-2" style={{ fontSize: 11 }}>{fleet?.daemonAlive ? "Supervisor checking for eligible issues." : "Daemon offline."}</div>
              </div>
            ) : allRecent.map(w => (
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

        {(scope === "today" || scope === "all") && allToday.length > 0 && (
          <>
            <div className="wt-group">
              <span className="pill ok no-dot" style={{ background: "transparent", border: "none", padding: 0, color: "var(--ok)" }}>● done</span>
              <strong>{allToday.length} completed today</strong>
            </div>
            {allToday.map(w => (
              <div key={`${w.slot}-done`} className="wt-row" onClick={() => openDrawer(w)}>
                <div className="wt-slot dim">{w.slot}</div>
                <div>
                  <div className="wt-issue dim2">#{w.issue.num} {w.issue.title}</div>
                  <div className="wt-project">{w.project}</div>
                </div>
                <div><Pill tone="ok" noDot>{w.status || "done"}</Pill></div>
                <div className="mono dim" style={{ fontSize: 11 }}>{w.pr ? `PR #${w.pr}` : "—"}</div>
                <div className="dim" style={{ fontSize: 11.5 }}>{w.summary}</div>
                <div className="wt-age">{relTime(w.age, now)}</div>
              </div>
            ))}
          </>
        )}

        {(scope === "7d" || scope === "all") && ws.olderCount > 0 && (
          !showOlder ? (
            <div className="wt-group-collapsed" onClick={() => setShowOlder(true)}>
              <Icon.Chevron dir="right" />
              <strong>{ws.olderCount} older sessions</strong>
              <span className="dim mono" style={{ fontSize: 11 }}>last 7 days · audit history</span>
            </div>
          ) : (
            <div className="wt-group">
              <Icon.Chevron dir="down" />
              <strong>Older sessions</strong>
              <a onClick={() => setShowOlder(false)} style={{ marginLeft: "auto", fontSize: 11 }}>collapse</a>
            </div>
          )
        )}
      </div>
    </div>
  );
}

export function WorkerDrawer({ worker, onClose, now }) {
  const { fleet } = useFleet();
  const [detail, setDetail] = React.useState(null);
  const [error, setError] = React.useState(null);

  React.useEffect(() => {
    if (!worker) {
      setDetail(null);
      setError(null);
      return;
    }
    let cancelled = false;
    const load = async () => {
      try {
        const data = await fetchWorkerDetail(worker.project_name || worker.project, worker.slot);
        if (!cancelled) {
          setDetail(data);
          setError(null);
        }
      } catch (err) {
        if (!cancelled) {
          setError(err.message || String(err));
          setDetail(null);
        }
      }
    };
    load();
    const poll = setInterval(load, 5000);
    return () => {
      cancelled = true;
      clearInterval(poll);
    };
  }, [worker]);

  if (!worker) return null;

  const logLines = detail?.log?.text
    ? detail.log.text.split("\n").filter(Boolean)
    : [];

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
            {worker.issue_url ? (
              <a href={worker.issue_url} target="_blank" rel="noreferrer" style={{ fontSize: 14, fontWeight: 600 }}>#{worker.issue.num} {worker.issue.title}</a>
            ) : (
              <span style={{ fontSize: 14, fontWeight: 600 }}>#{worker.issue.num} {worker.issue.title}</span>
            )}
            <div className="mono dim mt-2" style={{ fontSize: 11 }}>{worker.project} · {worker.branch || "—"}</div>
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title">Supervisor reasoning</div>
            <div style={{ background: "var(--bg-2)", borderRadius: "var(--r-2)", padding: "var(--s-3)", fontSize: 12.5, color: "var(--fg-1)", borderLeft: "2px solid var(--accent)" }}>
              <strong style={{ color: "var(--accent)" }}>{worker.next_action || worker.status || "monitor"}</strong>
              <div className="mt-2">{worker.status_reason || worker.summary}</div>
            </div>
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title row" style={{ justifyContent: "space-between" }}>
              <span>Live log</span>
              <span className="dim">{detail?.log?.available ? "tailing · 5s" : "unavailable"}</span>
            </div>
            {error && <div className="error" style={{ marginBottom: 8 }}>{error}</div>}
            <div className="log">
              {logLines.length === 0 ? (
                <div className="ln info"><span className="m dim">No log lines available.</span></div>
              ) : logLines.map((line, i) => (
                <div key={i} className="ln info">
                  <span className="m">{line}</span>
                </div>
              ))}
            </div>
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title">Actions</div>
            <div className="row gap-2">
              {worker.pr_url && <a className="tb-btn" href={worker.pr_url} target="_blank" rel="noreferrer">Open PR in GitHub →</a>}
            </div>
            {fleet?.readOnly && (
              <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>Write actions disabled in read-only mode.</div>
            )}
          </div>
        </div>
      </div>
    </>
  );
}

export function ApprovalsScreen({ navigate }) {
  const { fleet } = useFleet();
  const [showAudit, setShowAudit] = React.useState(false);
  const apps = fleet?.pendingApprovals || [];
  const audit = fleet?.historicalApprovals || [];
  const stuck = apps.filter(a => a.state === "stuck");
  const watch = apps.filter(a => a.state === "watch");

  return (
    <div>
      <div style={{ marginBottom: "var(--s-4)" }}>
        <h1 style={{ fontSize: 28, fontWeight: 600, letterSpacing: "-0.02em", color: "var(--fg-0)", margin: 0 }}>Approvals</h1>
        <div className="mono dim mt-2" style={{ fontSize: 12 }}>
          {apps.length} active · {audit.length} in audit history
        </div>
      </div>

      {stuck.length > 0 && (
        <>
          <div className="layout-head" style={{ marginTop: 0 }}>
            <h2 style={{ color: "var(--stuck)" }}>Past SLA · act now</h2>
            <div className="hint">{stuck.length} stuck</div>
          </div>
          <div className="appv">
            {stuck.map((a, i) => <ApprovalRow key={a.id || i} a={a} />)}
          </div>
        </>
      )}

      {watch.length > 0 && (
        <>
          <div className="layout-head"><h2>Within SLA · watching</h2><div className="hint">{watch.length} pending</div></div>
          <div className="appv">
            {watch.map((a, i) => <ApprovalRow key={a.id || i} a={a} />)}
          </div>
        </>
      )}

      {apps.length === 0 && (
        <div style={{ padding: "var(--s-12)", textAlign: "center", color: "var(--fg-2)", background: "var(--bg-1)", borderRadius: "var(--r-4)", border: "1px solid var(--border-1)" }}>
          <div style={{ fontSize: 15, color: "var(--fg-1)" }}>Inbox zero.</div>
          <div className="mono dim mt-2" style={{ fontSize: 11 }}>No approvals pending. Maestro is operating autonomously.</div>
        </div>
      )}

      <div className="layout-head">
        <h2>Audit history</h2>
        <div className="hint">{audit.length} prior · <a href="/approvals/audit">full audit page →</a></div>
      </div>
      {!showAudit ? (
        <div className="audit-fold" onClick={() => setShowAudit(true)}>
          <Icon.Chevron dir="right" />
          <div>
            <strong>{audit.length} past approvals</strong>
            <div className="dim" style={{ fontSize: 11.5, marginTop: 2 }}>Approved · rejected · superseded. Click to expand.</div>
          </div>
        </div>
      ) : (
        <div className="appv">
          {audit.map((a, i) => (
            <div key={a.id || i} className="app-row stale">
              <div className="app-row-stage">
                <small>#{a.pr || "—"}</small>
                <strong>{actionLabel(a.action)}</strong>
              </div>
              <div className="app-row-body">
                <h4 style={{ color: "var(--fg-2)" }}>{a.title}</h4>
                <div className="meta">{a.project} · {a.status}</div>
              </div>
              <div className="app-row-actions">
                <span className="age">{a.updated_age || a.created_age || "—"}</span>
              </div>
            </div>
          ))}
          <a onClick={() => setShowAudit(false)} className="mono" style={{ fontSize: 11 }}>collapse</a>
        </div>
      )}
    </div>
  );
}

function ApprovalRow({ a }) {
  const overdue = a.past_sla || a.ageMin > a.sla;
  const { fleet, refresh } = useFleet();
  const canMutate = !!a.id && fleet && fleet.readOnly === false;
  const [busy, setBusy] = React.useState(false);
  const [pendingVerb, setPendingVerb] = React.useState(null); // "approve" | "reject" | null
  const [pendingReason, setPendingReason] = React.useState("");
  const [errMsg, setErrMsg] = React.useState(null);
  const closeDialog = React.useCallback(() => {
    setPendingVerb(null);
    setPendingReason("");
  }, []);
  const send = React.useCallback(async (verb) => {
    setBusy(true);
    setErrMsg(null);
    const reason = pendingReason.trim();
    try {
      if (a.project) {
        await postFleetApproval({ approvalId: a.id, project: a.project, verb, reason });
      } else {
        await postProjectApproval({ approvalId: a.id, verb, reason });
      }
      closeDialog();
      await refresh();
    } catch (err) {
      setErrMsg(err && err.message ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [a.id, a.project, pendingReason, refresh, closeDialog]);
  return (
    <div className={`app-row ${a.state}`}>
      <div className="app-row-stage">
        <strong>#{a.pr || "—"}</strong>
        <small>{a.stage}</small>
      </div>
      <div className="app-row-body">
        <h4>{a.title}</h4>
        <div className="meta">
          <span>{a.project}</span> · action <strong className="mono" style={{ color: "var(--fg-1)" }}>{actionLabel(a.action)}</strong>
        </div>
        <p>{a.body}</p>
      </div>
      <div className="app-row-actions">
        <span className={`age ${overdue ? "bad" : ""}`}>{a.ageMin}m {overdue && `· SLA ${a.sla}m`}</span>
        <Pill tone={a.state} noDot>{overdue ? "past SLA" : "waiting"}</Pill>
        {a.pr_url && <a className="tb-btn primary" style={{ fontSize: 11 }} href={a.pr_url} target="_blank" rel="noreferrer">Open PR →</a>}
        {canMutate && (
          <>
            <button className="tb-btn" disabled={busy} style={{ fontSize: 11 }} onClick={() => setPendingVerb("approve")} title="Approve this approval">Approve</button>
            <button className="tb-btn danger" disabled={busy} style={{ fontSize: 11 }} onClick={() => setPendingVerb("reject")} title="Reject this approval">Reject</button>
            <ConfirmDialog
              open={pendingVerb !== null}
              title={pendingVerb === "approve" ? `Approve ${a.id}?` : `Reject ${a.id}?`}
              danger={pendingVerb === "reject"}
              confirmLabel={pendingVerb === "approve" ? "Approve" : "Reject"}
              busy={busy}
              onClose={closeDialog}
              onConfirm={() => send(pendingVerb)}
            >
              <div className="mono dim" style={{ fontSize: 11, marginBottom: 8 }}>
                action: {actionLabel(a.action)} · project: {a.project || "—"}
                {a.pr ? ` · PR #${a.pr}` : ""}
              </div>
              <div style={{ marginBottom: 12, fontSize: 12, color: "var(--fg-2)" }}>
                The approval moves to <strong>{pendingVerb === "approve" ? "approved" : "rejected"}</strong> immediately; for approved <strong>{actionLabel(a.action)}</strong>, the maestro supervisor (or the CLI) executes the side effect.
              </div>
              <label htmlFor={`reason-${a.id}`} style={{ display: "block", fontSize: 11, color: "var(--fg-2)", marginBottom: 4 }}>
                Reason <span className="dim">(optional, recorded in the approval audit)</span>
              </label>
              <textarea
                id={`reason-${a.id}`}
                value={pendingReason}
                onChange={e => setPendingReason(e.target.value)}
                placeholder={pendingVerb === "approve" ? "e.g. CI green, manual smoke ok" : "e.g. failing on review item X"}
                autoFocus
                rows={3}
                disabled={busy}
                style={{
                  width: "100%",
                  fontFamily: "inherit",
                  fontSize: 13,
                  padding: 8,
                  border: "1px solid var(--border-1)",
                  borderRadius: "var(--r-2)",
                  background: "var(--bg-0)",
                  color: "var(--fg-0)",
                  resize: "vertical",
                  boxSizing: "border-box",
                }}
                onKeyDown={e => {
                  if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
                    e.preventDefault();
                    if (!busy) send(pendingVerb);
                  }
                }}
              />
              <div className="mono dim" style={{ fontSize: 10, marginTop: 4 }}>
                ⌘/Ctrl+Enter to {pendingVerb}, Esc to cancel.
              </div>
            </ConfirmDialog>
          </>
        )}
        {errMsg && (
          <div className="mono dim" style={{ color: "var(--bad, #c33)", fontSize: 11 }}>{errMsg}</div>
        )}
      </div>
    </div>
  );
}

export function SettingsScreen() {
  const { fleet } = useFleet();
  const [section, setSection] = React.useState("general");

  return (
    <div>
      <h1 style={{ fontSize: 28, fontWeight: 600, letterSpacing: "-0.02em", color: "var(--fg-0)", margin: 0 }}>Settings</h1>
      <div className="mono dim mt-2" style={{ fontSize: 12 }}>
        Fleet config · {fleet?.readOnly ? "read-only mode" : "controls enabled"} · refreshed {fleet?.refreshedAt || "—"}
      </div>

      <div className="settings-grid">
        <div className="settings-nav">
          {[
            ["general", "General"],
            ["projects", "Projects"],
          ].map(([k, lbl]) => (
            <a key={k} className={section === k ? "active" : ""} onClick={() => setSection(k)}>{lbl}</a>
          ))}
        </div>

        <div>
          {section === "general" && (
            <div className="setting-card">
              <h4>Fleet</h4>
              <div className="setting-row">
                <label>Read-only mode</label>
                <span className="desc">Block mutating HTTP endpoints. V1 default.</span>
                <div className={`switch ${fleet?.readOnly ? "on" : ""}`} style={{ opacity: 0.6, pointerEvents: "none" }} />
              </div>
              <div className="setting-row">
                <label>Configured projects</label>
                <span className="desc">Projects loaded from fleet YAML.</span>
                <span className="mono">{fleet?.projects?.length || 0}</span>
              </div>
            </div>
          )}

          {section === "projects" && (
            <div className="setting-card">
              <h4>Projects in fleet</h4>
              {(fleet?.projects || []).map(project => (
                <div key={project.slug} className="setting-row" style={{ gridTemplateColumns: "1fr auto auto", gap: 12 }}>
                  <div>
                    <div style={{ color: "var(--fg-0)", fontWeight: 500 }}>{project.slug}</div>
                    <div className="dim mono" style={{ fontSize: 11 }}>{project.repo} · max {project.maxParallel || "—"} parallel</div>
                  </div>
                  <Pill tone={project.goal ? "ok" : "idle"} noDot>{project.goal ? "configured" : "unconfigured"}</Pill>
                  {project.dashboardUrl ? (
                    <a className="tb-btn ghost" href={project.dashboardUrl} target="_blank" rel="noreferrer">Open</a>
                  ) : (
                    <button className="tb-btn ghost" disabled>Edit</button>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
