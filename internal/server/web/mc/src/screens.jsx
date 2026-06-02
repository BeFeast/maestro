import React from "react";
import { Icon, Panel, PathValue, Pill, QueueBar, Segmented, ConfirmDialog, UrlValue } from "./atoms.jsx";
import { useFleet } from "./fleetContext.jsx";
import {
  actionLabel,
  approvalCTA,
  approvalRejectLabel,
  approvalReasonPlaceholder,
  approvalSlotLabel,
  attributionSegmentDuration,
  fetchWorkerDetail,
  formatAttributionSegment,
  formatAttributionTimeline,
  isApprovalActionCloseIssue,
  isApprovalActionMergePR,
  isExecutionSkippedApproval,
  manualFollowupForApproval,
  postFleetAction,
  postFleetApproval,
  postProjectApproval,
  projectBoardIssueURL,
  projectBySlug,
  supervisorDecisionsFromProject,
  workerNextAction,
  workerSessionsFromFleet,
} from "./fleetApi.js";
import { parseTimestamp, relTime, truncateBranchName } from "./utils.js";

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
              <a className="tb-btn" href={`https://github.com/${p.repo}`} target="_blank" rel="noreferrer">Open in GitHub →</a>
            )}
            {p.projectBoard?.url && (
              <a
                className="tb-btn"
                href={p.projectBoard.url}
                target="_blank"
                rel="noreferrer"
                title={p.projectBoard.error ? `Board fetch error: ${p.projectBoard.error}` : "Open GitHub Project board"}
              >
                Open GH Project board{p.projectBoard.number ? ` #${p.projectBoard.number}` : ""} →
              </a>
            )}
          </div>
        </div>
        <ProjectMiniHeartbeat tone={vtone} events={p.tapeEvents || []} />
      </div>

      {p.operatorState?.kind === "attention" && (p.operatorState.summary || p.operatorState.next_action) && (
        <Panel title="Needs attention" sub={p.operatorState.session || undefined}>
          <div style={{ padding: "var(--s-4) var(--s-5)" }}>
            <div style={{ fontSize: 14, color: "var(--fg-0)" }}>
              {p.operatorState.issue_number ? (
                <a href={p.operatorState.issue_url} target="_blank" rel="noreferrer">#{p.operatorState.issue_number}</a>
              ) : null}
              {p.operatorState.session ? (
                <span className="mono dim">{p.operatorState.issue_number ? " · " : ""}{p.operatorState.session}</span>
              ) : null}
              {(p.operatorState.issue_number || p.operatorState.session) ? " — " : ""}
              {p.operatorState.summary}
            </div>
            {p.operatorState.next_action && (
              <div style={{ marginTop: 8, fontSize: 13, color: "var(--watch)" }}>
                <strong style={{ color: "var(--fg-1)" }}>Next:</strong> {p.operatorState.next_action}
              </div>
            )}
            <div className="hb-actions" style={{ marginTop: 12 }}>
              {p.operatorState.pr_number ? (
                <a
                  className="tb-btn primary"
                  href={p.operatorState.pr_url || `https://github.com/${p.repo}/pull/${p.operatorState.pr_number}`}
                  target="_blank"
                  rel="noreferrer"
                >
                  Review PR #{p.operatorState.pr_number} →
                </a>
              ) : p.operatorState.session ? (
                <button
                  className="tb-btn primary"
                  onClick={() => navigate(`workers?project=${encodeURIComponent(p.slug)}&slot=${encodeURIComponent(p.operatorState.session)}`)}
                >
                  Open worker log {p.operatorState.session} →
                </button>
              ) : null}
              {p.operatorState.issue_url && (
                <a className="tb-btn ghost" href={p.operatorState.issue_url} target="_blank" rel="noreferrer">
                  Open issue{p.operatorState.issue_number ? ` #${p.operatorState.issue_number}` : ""} →
                </a>
              )}
            </div>
          </div>
        </Panel>
      )}

      {/* #598: convergence-bound PR — read as calm "Auto-merging — no action
          needed" instead of an alarm. Mirrors the fleet verdict logic so the
          per-project view never asks the operator to click for a PR Maestro
          will merge on its own. */}
      {p.operatorState?.kind === "auto_merging" && (
        <Panel title="Auto-merging" sub={p.operatorState.session || undefined}>
          <div style={{ padding: "var(--s-4) var(--s-5)" }}>
            <div style={{ fontSize: 14, color: "var(--fg-0)" }}>
              {p.operatorState.pr_number ? (
                <a href={p.operatorState.pr_url || `https://github.com/${p.repo}/pull/${p.operatorState.pr_number}`} target="_blank" rel="noreferrer">
                  PR #{p.operatorState.pr_number}
                </a>
              ) : null}
              {p.operatorState.issue_number ? (
                <span>{p.operatorState.pr_number ? " · " : ""}
                  <a href={p.operatorState.issue_url} target="_blank" rel="noreferrer">#{p.operatorState.issue_number}</a>
                </span>
              ) : null}
              {(p.operatorState.pr_number || p.operatorState.issue_number) ? " — " : ""}
              {p.operatorState.summary}
            </div>
            <div style={{ marginTop: 8, fontSize: 13, color: "var(--ok)" }}>
              <strong style={{ color: "var(--fg-1)" }}>Status:</strong> Auto-merging — no action needed.
            </div>
            <div className="hb-actions" style={{ marginTop: 12 }}>
              {p.operatorState.pr_number && (
                <a
                  className="tb-btn ghost"
                  href={p.operatorState.pr_url || `https://github.com/${p.repo}/pull/${p.operatorState.pr_number}`}
                  target="_blank"
                  rel="noreferrer"
                >
                  Open PR #{p.operatorState.pr_number} →
                </a>
              )}
            </div>
          </div>
        </Panel>
      )}

      <div className="dash-grid mt-6">
        <Panel
          title="Workers"
          sub={projectLive.length > 0 ? `${projectLive.length} live` : "none running"}
          right={<a onClick={() => navigate(`workers?project=${encodeURIComponent(p.slug)}`)} style={{ fontSize: 11.5 }}>Open workers →</a>}
        >
          {projectLive.length > 0 ? (
            <div style={{ padding: "var(--s-2)" }}>
              {projectLive.map(w => {
                const cardURL = projectBoardIssueURL(p.projectBoard, w.issue.num);
                return (
                  <div key={w.slot} className="dec" style={{ gridTemplateColumns: "80px 1fr auto" }} onClick={() => openDrawer(w)}>
                    <div className="dec-t mono">{w.slot}</div>
                    <div className="dec-body">
                      <div style={{ color: "var(--fg-0)", fontSize: 13 }}>
                        #{w.issue.num} {w.issue.title}
                        {cardURL && (
                          <a
                            href={cardURL}
                            target="_blank"
                            rel="noreferrer"
                            onClick={e => e.stopPropagation()}
                            title="Open this issue's card on the GitHub Project board"
                            style={{ marginLeft: 6, fontSize: 11 }}
                          >
                            ↗ board
                          </a>
                        )}
                      </div>
                      <div className="dim" style={{ fontSize: 11.5, marginTop: 2 }}>{w.summary}</div>
                      <AttributionInline worker={w} now={now} />
                    </div>
                    <div style={{ textAlign: "right" }}>
                      <Pill tone={w.tone} noDot>{w.status}</Pill>
                      <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>{relTime(w.age, now)}</div>
                    </div>
                  </div>
                );
              })}
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
                <div className="kv"><span>Runtime</span><UrlValue url={p.outcome?.runtime_target || p.runtime || ""} /></div>
                {p.outcome?.healthcheck_url && (
                  <div className="kv"><span>Healthcheck</span><UrlValue url={p.outcome.healthcheck_url} /></div>
                )}
                {p.dashboardUrl && (
                  <div className="kv"><span>Dashboard</span><UrlValue url={p.dashboardUrl} /></div>
                )}
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

        {p.projectBoard && <ProjectBoardPanel board={p.projectBoard} />}

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

function ProjectBoardPanel({ board }) {
  const columns = board?.columns || [];
  const total = Number(board?.totalItems || 0);
  const subtitle = board?.error
    ? "fetch error"
    : columns.length === 0
      ? "no columns"
      : `${total} item${total === 1 ? "" : "s"}`;
  return (
    <Panel
      title={`Project board #${board.number || ""}`.trim()}
      sub={subtitle}
      right={board?.url
        ? <a href={board.url} target="_blank" rel="noreferrer" style={{ fontSize: 11.5 }}>Open board →</a>
        : null}
    >
      <div style={{ padding: "var(--s-4) var(--s-5)" }}>
        {board?.error && (
          <div className="mono dim" style={{ fontSize: 11, marginBottom: 8 }}>
            {board.error}
          </div>
        )}
        {columns.length === 0 ? (
          <div className="dim" style={{ textAlign: "center", padding: "var(--s-4)" }}>
            No board data yet. Refresher runs every couple of minutes.
          </div>
        ) : (
          columns.map(c => (
            <div key={c.optionId || c.name} className="kv">
              <span>{c.name}</span>
              <strong className="mono">{c.count}</strong>
            </div>
          ))
        )}
        {board?.fetchedAt && (
          <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>
            fetched {board.fetchedAt.replace("T", " ").replace("Z", "Z")}
          </div>
        )}
      </div>
    </Panel>
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
  const matchProject = w => !filterProject || w.project === filterProject || w.project_name === filterProject;
  const allRunning = ws.running.filter(matchProject);
  const allRecent = ws.recent.filter(matchProject);
  const allStuck = ws.stuck.filter(matchProject);
  const allToday = ws.today.filter(matchProject);
  const stuckTodayCount = allStuck.filter(w => {
    const finished = parseTimestamp(w.finished_at) || w.age;
    if (!finished) return false;
    const start = new Date(now); start.setHours(0, 0, 0, 0);
    return finished >= start.getTime();
  }).length;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "end", justifyContent: "space-between", marginBottom: "var(--s-4)" }}>
        <div>
          <h1 style={{ fontSize: 28, fontWeight: 600, letterSpacing: "-0.02em", color: "var(--fg-0)", margin: 0 }}>Workers</h1>
          <div className="mono dim mt-2" style={{ fontSize: 12 }}>
            {filterProject ? <>filtered by <strong style={{ color: "var(--fg-1)" }}>{filterProject}</strong> · </> : null}
            <strong style={{ color: "var(--fg-0)" }}>{allRunning.length} running</strong>
            {" · "}{allRecent.length} recent 24h
            {" · "}<strong style={{ color: allStuck.length > 0 ? "var(--stuck)" : "var(--fg-3)" }}>{stuckTodayCount} stuck today</strong>
            {" · "}{allToday.length} done today
            {" · "}{ws.olderCount} older
            {filterProject && <a onClick={() => navigate("workers")} style={{ marginLeft: 8 }}>clear</a>}
          </div>
        </div>
        <div className="row gap-2">
          <Segmented
            value={scope}
            onChange={setScope}
            options={[
              { value: "live", label: "Recent", count: allRecent.length },
              { value: "today", label: "Today", count: allToday.length },
              { value: "7d", label: "7d", count: ws.olderCount + allToday.length },
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
                <div className="wt-issue-cell">
                  <div className="wt-issue">#{w.issue.num} {w.issue.title}</div>
                  <div className="wt-project">{w.project}</div>
                  <AttributionInline worker={w} now={now} />
                </div>
                <div className="wt-status"><Pill tone={w.tone} noDot title={w.status}>{w.status}</Pill></div>
                <div className="wt-branch mono">
                  {w.pr ? <>PR #{w.pr}</> : <span className="dim">draft</span>}
                  {w.branch && (
                    <div className="wt-branch-name dim" title={w.branch}>{truncateBranchName(w.branch)}</div>
                  )}
                </div>
                <div className="wt-next">{w.summary}</div>
                <div className="wt-age">{relTime(w.age, now)}</div>
              </div>
            ))}
          </>
        )}

        {(scope === "live" || scope === "today" || scope === "all") && allStuck.length > 0 && (
          <>
            <div className="wt-group">
              <span className="pill stuck no-dot" style={{ background: "transparent", border: "none", padding: 0, color: "var(--stuck)" }}>● stuck</span>
              <strong>{allStuck.length} needs attention</strong>
              {stuckTodayCount > 0 && <span className="dim" style={{ fontSize: 11, marginLeft: 8 }}>· {stuckTodayCount} stuck today</span>}
            </div>
            {allStuck.map(w => (
              <div key={`${w.slot}-stuck`} className={`wt-row ${selectedSlot === w.slot ? "selected" : ""}`} onClick={() => openDrawer(w)}>
                <div className="wt-slot">{w.slot}</div>
                <div className="wt-issue-cell">
                  <div className="wt-issue">#{w.issue.num} {w.issue.title}</div>
                  <div className="wt-project">{w.project}</div>
                  <AttributionInline worker={w} now={now} />
                </div>
                <div className="wt-status"><Pill tone={w.tone} noDot title={w.status}>{w.status}</Pill></div>
                <div className="wt-branch mono">
                  {w.pr ? <>PR #{w.pr}</> : <span className="dim">no PR</span>}
                  {w.branch && (
                    <div className="wt-branch-name dim" title={w.branch}>{truncateBranchName(w.branch)}</div>
                  )}
                </div>
                <div className="wt-next">{workerNextAction(w).text || w.summary}</div>
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
                <div className="wt-issue-cell">
                  <div className="wt-issue dim2">#{w.issue.num} {w.issue.title}</div>
                  <div className="wt-project">{w.project}</div>
                </div>
                <div className="wt-status"><Pill tone="ok" noDot>done</Pill></div>
                <div className="wt-branch mono dim">{w.pr ? `PR #${w.pr}` : "—"}</div>
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

// AttributionInline renders the short single-line attribution text on a
// session card: «claude opus-4.8 xhigh (12m) → codex gpt-5.5 medium
// (4m, fallover)». Renders nothing when the worker has no attribution
// timeline (older sessions before #518 / backends without metadata).
export function AttributionInline({ worker, now }) {
  const attribution = worker?.attribution || [];
  if (!attribution.length) return null;
  const text = formatAttributionTimeline(attribution, now);
  if (!text) return null;
  return (
    <div
      className="mono dim mt-2"
      style={{ fontSize: 10.5, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}
      title={text}
    >
      {text}
    </div>
  );
}

// AttributionTimeline renders the full per-segment list for the worker
// drawer, including the EndReason between segments. Each segment becomes
// a row with backend/model/duration; closed segments display a small
// tag like "ended: provider_limit" so the cause of fallover is explicit.
function AttributionTimeline({ attribution, now }) {
  const segments = Array.isArray(attribution) ? attribution : [];
  if (!segments.length) return null;
  return (
    <div className="drawer-sec">
      <div className="drawer-sec-title">Backend timeline</div>
      <div style={{ background: "var(--bg-2)", borderRadius: "var(--r-2)", padding: "var(--s-3)" }}>
        {segments.map((seg, i) => {
          const summary = formatAttributionSegment(seg, now);
          const duration = attributionSegmentDuration(seg, now);
          const open = !seg.ended_at;
          return (
            <div key={i} style={{ display: "flex", flexDirection: "column", marginBottom: i < segments.length - 1 ? 8 : 0 }}>
              <div className="row gap-2" style={{ alignItems: "center" }}>
                <Pill tone={open ? "info" : "idle"} noDot>{seg.backend || "—"}</Pill>
                <span className="mono" style={{ fontSize: 12, color: "var(--fg-1)" }}>
                  {summary || seg.backend || "—"}
                </span>
                {duration && <span className="mono dim" style={{ fontSize: 10.5 }}>· {duration}</span>}
                {open && <span className="mono" style={{ fontSize: 10.5, color: "var(--ok)" }}>· active</span>}
              </div>
              <div className="mono dim" style={{ fontSize: 10.5, marginLeft: 4 }}>
                {seg.reason ? `start: ${seg.reason}` : ""}
                {seg.ended_at && seg.end_reason ? `${seg.reason ? " · " : ""}end: ${seg.end_reason}` : ""}
              </div>
              {i < segments.length - 1 && (
                <div className="mono dim" style={{ fontSize: 11, marginLeft: 4, marginTop: 4 }}>
                  ↓ {seg.end_reason || "rolled over"}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

export function WorkerDrawer({ worker, onClose, now }) {
  const { fleet, refresh } = useFleet();
  const [detail, setDetail] = React.useState(null);
  const [error, setError] = React.useState(null);
  const logRef = React.useRef(null);

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

  const next = workerNextAction(worker);
  const handleAction = (action) => {
    if (action === "openLog" && logRef.current) {
      logRef.current.scrollIntoView({ behavior: "smooth", block: "start" });
    }
    // retry / resetBudget / markBlocked / openBackendHealth are
    // intentionally not wired here yet: backend handlers are tracked
    // separately (see issue #540 "out of scope" — buttons render with
    // an action key so future endpoints can attach without UI churn).
  };

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
              <UrlValue
                url={worker.issue_url}
                label={`#${worker.issue.num} ${worker.issue.title}`}
                monospace={false}
                scopeBadge={false}
                className="drawer-issue-link"
              />
            ) : (
              <span style={{ fontSize: 14, fontWeight: 600 }}>#{worker.issue.num} {worker.issue.title}</span>
            )}
            <div className="mono dim mt-2" style={{ fontSize: 11 }}>{worker.project} · {worker.branch || "—"}</div>
            {worker.worktree && (
              <div className="kv mt-2"><span>Worktree</span><PathValue path={worker.worktree} /></div>
            )}
          </div>

          <div className="drawer-sec">
            <div className="drawer-sec-title">Next action</div>
            <div style={{ background: "var(--bg-2)", borderRadius: "var(--r-2)", padding: "var(--s-3)", fontSize: 12.5, color: "var(--fg-1)", borderLeft: "2px solid var(--accent)" }}>
              <strong style={{ color: "var(--accent)" }}>{worker.status || "monitor"}</strong>
              <div className="mt-2">{next.text || worker.status_reason || worker.summary}</div>
              {next.buttons.length > 0 && (
                <div className="row gap-2 mt-2">
                  {next.buttons.map((b, i) => b.href ? (
                    <a key={i} className="tb-btn" href={b.href} target="_blank" rel="noreferrer">{b.label}</a>
                  ) : (
                    <button key={i} className="tb-btn" onClick={() => handleAction(b.action)}>{b.label}</button>
                  ))}
                </div>
              )}
            </div>
            {worker.status_reason && worker.status_reason !== next.text && (
              <div className="mono dim mt-2" style={{ fontSize: 11 }}>{worker.status_reason}</div>
            )}
          </div>

          <AttributionTimeline
            attribution={detail?.worker?.attribution || worker.attribution}
            now={now}
          />

          <div className="drawer-sec" ref={logRef}>
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

          <WorkerActionsPanel worker={worker} readOnly={fleet?.readOnly} refresh={refresh} />

          <div className="drawer-sec">
            <div className="drawer-sec-title">Links</div>
            <div className="row gap-2">
              {worker.pr_url && <a className="tb-btn" href={worker.pr_url} target="_blank" rel="noreferrer">Open PR in GitHub →</a>}
              {worker.issue_url && <a className="tb-btn ghost" href={worker.issue_url} target="_blank" rel="noreferrer">Open issue →</a>}
              {(() => {
                const board = (fleet?.projects || []).find(
                  pr => pr.name === (worker.project_name || worker.project) || pr.slug === (worker.project_name || worker.project)
                )?.projectBoard;
                const href = projectBoardIssueURL(board, worker.issue_number || worker.issue?.num);
                if (!href) return null;
                return (
                  <a className="tb-btn ghost" href={href} target="_blank" rel="noreferrer">
                    Open on project board →
                  </a>
                );
              })()}
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

// WorkerActionsPanel renders the per-worker action buttons surfaced in
// the fleet snapshot (#567). Safe verbs (mark_issue_ready /
// mark_issue_blocked) execute synchronously; cautious-gate verbs
// (restart_worker / stop_worker / approve_merge) enqueue a pending
// Approval the operator completes from the Approvals screen. Buttons
// the server reports as `disabled` keep their disabled state and
// expose the server's reason as a tooltip.
//
// #476 (dashboard write-path, frontend): every mutating click is gated
// behind a ConfirmDialog. Approval-required verbs make the
// approval-gate semantics explicit in the body so the operator knows
// the click enqueues a pending Approval rather than executing
// immediately.
function WorkerActionsPanel({ worker, readOnly, refresh }) {
  const actions = Array.isArray(worker?.actions) ? worker.actions : [];
  const [busyId, setBusyId] = React.useState("");
  const [message, setMessage] = React.useState(null);
  const [pending, setPending] = React.useState(null);
  const [pendingReason, setPendingReason] = React.useState("");

  if (!actions.length) return null;

  const project = worker.project_name || worker.project || "";
  const slot = worker.slot || "";
  const issueNumber = worker.issue_number || worker.issue?.num || 0;
  const prNumber = worker.pr_number || 0;

  const closeDialog = () => {
    setPending(null);
    setPendingReason("");
  };

  const send = async () => {
    if (!pending) return;
    const action = pending;
    setBusyId(action.id);
    setMessage(null);
    try {
      const resp = await postFleetAction({
        actionId: action.id,
        project,
        slot,
        issueNumber,
        prNumber,
        reason: pendingReason.trim(),
      });
      const tag = resp?.approval_id
        ? `approval ${resp.approval_id} queued`
        : (resp?.action_id ? `executed ${resp.action_id}` : "ok");
      setMessage({ tone: "ok", text: `${action.label || action.id}: ${tag}` });
      closeDialog();
      if (typeof refresh === "function") {
        try { await refresh(); } catch (_) { /* swallow; message already surfaced */ }
      }
    } catch (err) {
      setMessage({ tone: "stuck", text: `${action.label || action.id}: ${err.message || String(err)}` });
    } finally {
      setBusyId("");
    }
  };

  const openConfirm = (action) => {
    if (action.disabled || busyId) return;
    setMessage(null);
    setPendingReason("");
    setPending(action);
  };

  const targetSummary = [
    slot && `slot ${slot}`,
    issueNumber > 0 && `issue #${issueNumber}`,
    prNumber > 0 && `PR #${prNumber}`,
    project && `project ${project}`,
  ].filter(Boolean).join(" · ");

  const isDangerVerb = (id) => id === "stop_worker";
  const isRecoveryVerb = (id) => id === "restart_worker";

  return (
    <div className="drawer-sec">
      <div className="drawer-sec-title">Controls</div>
      <div className="row gap-2" style={{ flexWrap: "wrap" }}>
        {actions.map(action => {
          const danger = isDangerVerb(action.id);
          const recovery = isRecoveryVerb(action.id);
          const cls = "tb-btn"
            + (danger ? " danger" : "")
            + (recovery ? " recovery" : "")
            + (action.disabled ? " ghost" : "");
          const title = action.disabled
            ? (action.disabled_reason || "Unavailable")
            : (action.description || action.label || action.id);
          const approvalCls = "mono " + (danger ? "approval-on-danger" : "dim");
          return (
            <button
              key={action.id}
              className={cls}
              disabled={action.disabled || busyId === action.id}
              title={title}
              onClick={() => openConfirm(action)}
            >
              {busyId === action.id ? "…" : (action.label || action.id)}
              {action.requires_approval && !action.disabled && (
                <span className={approvalCls} style={{ fontSize: 10, marginLeft: 4 }}>(approval)</span>
              )}
            </button>
          );
        })}
      </div>
      {message && (
        <div className={`mono mt-2`} style={{ fontSize: 11, color: message.tone === "stuck" ? "var(--stuck)" : "var(--ok)" }}>
          {message.text}
        </div>
      )}
      {readOnly && (
        <div className="mono dim mt-2" style={{ fontSize: 10.5 }}>
          Controls are disabled while the fleet runs in read-only mode.
        </div>
      )}
      <ConfirmDialog
        open={pending !== null}
        title={pending ? `${pending.label || pending.id}?` : ""}
        danger={pending ? isDangerVerb(pending.id) : false}
        confirmLabel={pending ? (pending.label || pending.id) : "Confirm"}
        busy={!!busyId}
        onClose={closeDialog}
        onConfirm={send}
      >
        {pending && (
          <>
            <div className="mono dim" style={{ fontSize: 11, marginBottom: 8 }}>
              action: {pending.label || pending.id}
              {targetSummary ? ` · ${targetSummary}` : ""}
            </div>
            <div style={{ marginBottom: 12, fontSize: 12, color: "var(--fg-2)" }}>
              {pending.requires_approval ? (
                <>
                  This <strong>enqueues a pending Approval</strong> on the cautious gate;
                  the action does <strong>not</strong> execute until the approval is
                  resolved from the Approvals screen.
                </>
              ) : (
                <>
                  This will <strong>execute immediately</strong> against GitHub.
                </>
              )}
            </div>
            {pending.description && pending.description !== pending.label && (
              <div className="dim" style={{ fontSize: 11.5, marginBottom: 12 }}>{pending.description}</div>
            )}
            <label htmlFor={`reason-action-${slot || "p"}-${pending.id}`} style={{ display: "block", fontSize: 11, color: "var(--fg-2)", marginBottom: 4 }}>
              Reason <span className="dim">(optional, recorded in the audit log)</span>
            </label>
            <textarea
              id={`reason-action-${slot || "p"}-${pending.id}`}
              value={pendingReason}
              onChange={e => setPendingReason(e.target.value)}
              placeholder={pending.requires_approval ? "why this approval is being enqueued" : "why this safe action is being executed"}
              autoFocus
              rows={3}
              disabled={!!busyId}
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
                  if (!busyId) send();
                }
              }}
            />
            <div className="mono dim" style={{ fontSize: 10, marginTop: 4 }}>
              ⌘/Ctrl+Enter to confirm, Esc to cancel.
            </div>
          </>
        )}
      </ConfirmDialog>
    </div>
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
          {audit.map((a, i) => <AuditApprovalRow key={a.id || i} a={a} />)}
          <a onClick={() => setShowAudit(false)} className="mono" style={{ fontSize: 11 }}>collapse</a>
        </div>
      )}
    </div>
  );
}

// AuditApprovalRow renders one row of the historical-approval list. Approved
// and rejected entries render dimmed; `execution_skipped` entries break out of
// that pattern (premortem #8): the executor returned a non-failure status but
// no side effect ran, so showing it as a dim "executed" row misleads the
// operator into thinking the change took effect. Instead we surface an amber
// warning row with the executor summary inline and, for change_global_config,
// a non-dismissible follow-up command the operator still has to run.
function AuditApprovalRow({ a }) {
  const skipped = isExecutionSkippedApproval(a);
  const followup = manualFollowupForApproval(a);
  const summary = a.summary || a.body || "";
  const rowClass = skipped ? "app-row skipped" : "app-row stale";
  const statusLabel = skipped ? "execution skipped · no side effect ran" : a.status;
  const stageLabel = skipped ? "skipped" : a.stage;
  return (
    <div className={rowClass}>
      <div className="app-row-stage">
        <small>{approvalSlotLabel(a)}</small>
        <strong>{actionLabel(a.action)}</strong>
      </div>
      <div className="app-row-body">
        <h4 style={skipped ? undefined : { color: "var(--fg-2)" }}>
          {skipped && <SkippedIcon />} {a.title}
        </h4>
        <div className="meta">
          <span>{a.project}</span>
          <span>· {stageLabel}</span>
          {skipped
            ? <Pill tone="watch" noDot>{statusLabel}</Pill>
            : <span>· {statusLabel}</span>}
        </div>
        {skipped && summary && summary !== a.title && (
          <p className="app-row-skipped-summary">{summary}</p>
        )}
        {followup && <ManualFollowupBanner followup={followup} />}
      </div>
      <div className="app-row-actions">
        <span className="age">{a.updated_age || a.created_age || "—"}</span>
      </div>
    </div>
  );
}

// SkippedIcon is the distinct glyph used on `execution_skipped` audit rows.
// A triangle-with-bang reads as "warning, not done" without colliding with
// the green check used for completed approvals.
function SkippedIcon() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      style={{ verticalAlign: "-2px", color: "var(--watch)" }}
    >
      <path d="M8 1.5L15 13.5H1L8 1.5Z" />
      <path d="M8 6V9.5" />
      <circle cx="8" cy="11.6" r="0.6" fill="currentColor" stroke="none" />
    </svg>
  );
}

// ManualFollowupBanner renders the non-dismissible callout shown inside an
// execution_skipped audit row when the verb still needs an operator command
// (today: change_global_config). The command is a one-line copy-friendly
// shell snippet so the operator can paste it into a terminal without
// constructing the path/unit name by hand.
function ManualFollowupBanner({ followup }) {
  const [copied, setCopied] = React.useState(false);
  const onCopy = React.useCallback(async () => {
    try {
      if (navigator?.clipboard?.writeText) {
        await navigator.clipboard.writeText(followup.command);
      } else {
        const ta = document.createElement("textarea");
        ta.value = followup.command;
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
      }
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch (_) {
      // Clipboard rejection (e.g. permission denied) is non-fatal; the
      // command is still selectable from the mono text node.
    }
  }, [followup.command]);
  return (
    <div className="app-row-followup" role="note" aria-label={followup.headline}>
      <div className="app-row-followup-head">
        <SkippedIcon />
        <strong>{followup.headline}</strong>
      </div>
      <div className="app-row-followup-detail">{followup.detail}</div>
      <div className="app-row-followup-cmd">
        <code className="mono">{followup.command}</code>
        <button
          type="button"
          className="tb-btn"
          style={{ fontSize: 11 }}
          onClick={onCopy}
          title={copied ? "Copied" : "Copy command"}
        >
          {copied ? "Copied ✓" : "Copy"}
        </button>
      </div>
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
  const isMergePR = isApprovalActionMergePR(a.action);
  const isCloseIssue = isApprovalActionCloseIssue(a.action);
  const primaryCTA = approvalCTA(a.action, a.pr, a.issue_number);
  const rejectCTA = approvalRejectLabel(a.action);
  // For merge_pr the supervisor's planner rule only emits the approval once
  // CI is CLEAN and mergeable is true (#514), so a pending merge_pr approval
  // implicitly means those gates already passed — surface them as badges so
  // the operator sees what was checked, not just "approve a generic action".
  const gateBadges = isMergePR
    ? [
      { tone: "ok", label: "CI green" },
      { tone: "ok", label: "mergeable" },
      { tone: "ok", label: "review ok" },
    ]
    : [];
  return (
    <div className={`app-row ${a.state}`}>
      <div className="app-row-stage">
        <strong>{approvalSlotLabel(a)}</strong>
        <small>{a.stage}</small>
      </div>
      <div className="app-row-body">
        <h4 style={isMergePR ? { fontSize: 16 } : undefined}>{a.title}</h4>
        <div className="meta" style={{ display: "flex", flexWrap: "wrap", gap: 6, alignItems: "center" }}>
          <span>{a.project}</span>
          <span>· action <strong className="mono" style={{ color: "var(--fg-1)" }}>{actionLabel(a.action)}</strong></span>
          {gateBadges.map(b => (
            <Pill key={b.label} tone={b.tone} noDot>{b.label}</Pill>
          ))}
        </div>
        {a.body && a.body !== a.title && <p>{a.body}</p>}
      </div>
      <div className="app-row-actions">
        <span className={`age ${overdue ? "bad" : ""}`}>{a.ageMin}m {overdue && `· SLA ${a.sla}m`}</span>
        <Pill tone={a.state} noDot>{overdue ? "past SLA" : "waiting"}</Pill>
        {a.pr_url && <a className="tb-btn" style={{ fontSize: 11 }} href={a.pr_url} target="_blank" rel="noreferrer">Open PR →</a>}
        {canMutate && (
          <>
            <button className="tb-btn primary" disabled={busy} style={{ fontSize: 11 }} onClick={() => setPendingVerb("approve")} title={`${primaryCTA} (approve)`}>{primaryCTA}</button>
            <button className="tb-btn danger" disabled={busy} style={{ fontSize: 11 }} onClick={() => setPendingVerb("reject")} title={`${rejectCTA} (reject)`}>{rejectCTA}</button>
            <ConfirmDialog
              open={pendingVerb !== null}
              title={pendingVerb === "approve" ? `${primaryCTA}?` : `${rejectCTA}?`}
              danger={pendingVerb === "reject" || (pendingVerb === "approve" && (isMergePR || isCloseIssue))}
              confirmLabel={pendingVerb === "approve" ? primaryCTA : rejectCTA}
              busy={busy}
              onClose={closeDialog}
              onConfirm={() => send(pendingVerb)}
            >
              <div className="mono dim" style={{ fontSize: 11, marginBottom: 8 }}>
                action: {actionLabel(a.action)} · project: {a.project || "—"}
                {a.pr ? ` · PR #${a.pr}` : ""}
                {!a.pr && a.issue_number ? ` · issue #${a.issue_number}` : ""}
              </div>
              <div style={{ marginBottom: 12, fontSize: 12, color: "var(--fg-2)" }}>
                {pendingVerb === "approve" ? (
                  <>The approval moves to <strong>approved</strong> immediately, and the maestro supervisor (or the CLI) will <strong>{actionLabel(a.action).toLowerCase()}</strong>.</>
                ) : (
                  <>The approval moves to <strong>rejected</strong> immediately; the supervisor will <strong>not</strong> {actionLabel(a.action).toLowerCase()}.</>
                )}
              </div>
              <label htmlFor={`reason-${a.id}`} style={{ display: "block", fontSize: 11, color: "var(--fg-2)", marginBottom: 4 }}>
                Reason <span className="dim">(optional, recorded in the approval audit)</span>
              </label>
              <textarea
                id={`reason-${a.id}`}
                value={pendingReason}
                onChange={e => setPendingReason(e.target.value)}
                placeholder={approvalReasonPlaceholder(a.action, pendingVerb)}
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
                ⌘/Ctrl+Enter to confirm, Esc to cancel.
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
                  <button className="tb-btn ghost" disabled>Edit</button>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
