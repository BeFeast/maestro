import React from "react";
import { Icon } from "./atoms.jsx";
import { formatRefreshAge, searchFleetItems } from "./fleetApi.js";
import { useFleet } from "./fleetContext.jsx";

export function BrandMark({ size = 32 }) {
  return (
    <svg className="sb-brand-mark-svg" viewBox="0 0 96 96" width={size} height={size} aria-label="Maestro" style={{ flexShrink: 0 }}>
      <defs>
        <linearGradient id="bm-g" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" stopColor="var(--accent)" />
          <stop offset="100%" stopColor="var(--accent-2)" />
        </linearGradient>
      </defs>
      <rect x="14" y="14" width="68" height="64" rx="12" fill="none" stroke="url(#bm-g)" strokeWidth="2.5" />
      <text x="26" y="42" fontFamily="JetBrains Mono, monospace" fontWeight="600" fontSize="22" fill="url(#bm-g)">4</text>
      <text x="56" y="70" fontFamily="JetBrains Mono, monospace" fontWeight="600" fontSize="22" fill="url(#bm-g)">4</text>
      <line x1="38" y1="64" x2="58" y2="34" stroke="url(#bm-g)" strokeWidth="4" strokeLinecap="round" />
      <circle cx="38" cy="64" r="3" fill="url(#bm-g)" />
      <circle cx="58" cy="34" r="2.5" fill="var(--bg-1)" />
    </svg>
  );
}

export function SidebarV2({ route, navigate, projectStates, fleet }) {
  const screen = route.screen;
  const projects = fleet?.projects || [];

  return (
    <aside className="sb">
      <div className="sb-brand">
        <BrandMark />
        <div>
          <div className="sb-brand-name">Maestro</div>
          <div className="sb-brand-sub">mission control · v1.4</div>
        </div>
      </div>

      <div className="sb-search" onClick={() => navigate("cmdk")}>
        <Icon.Search s={12} />
        <input placeholder="Quick switch…" readOnly />
        <kbd>⌘K</kbd>
      </div>

      <div className="sb-sec">Fleet</div>
      <div className={`sb-link ${screen === "fleet" ? "active" : ""}`} onClick={() => navigate("fleet")}>
        <span className="sb-link-icon"><Icon.Fleet /></span>
        <span>Overview</span>
        <span className="sb-link-count">{projects.length}</span>
      </div>
      <div className={`sb-link ${screen === "workers" ? "active" : ""}`} onClick={() => navigate("workers")}>
        <span className="sb-link-icon"><Icon.Workers /></span>
        <span>Workers</span>
        <span className="sb-link-count">{fleet?.workerCount || 0}</span>
      </div>
      <div className={`sb-link ${screen === "approvals" ? "active" : ""}`} onClick={() => navigate("approvals")}>
        <span className="sb-link-icon"><Icon.Approval /></span>
        <span>Approvals</span>
        {(fleet?.activeApprovals || 0) > 0 ? (
          <span className={`sb-link-count ${fleet?.verdictTone === "stuck" || fleet?.verdictTone === "watch" ? "dot" : ""}`}>
            {fleet.activeApprovals}
          </span>
        ) : null}
      </div>

      <div className="sb-sec">Projects</div>
      <div className="sb-projects">
        {projects.map(project => {
          const st = projectStates[project.slug] || project.state || { state: "idle", label: "" };
          const dotColor = st.state === "live" ? "var(--ok)"
            : st.state === "stuck" ? "var(--stuck)"
            : st.state === "policy" ? "var(--policy)"
            : st.state === "unknown" ? "var(--stuck)"
            : "var(--idle)";
          const active = screen === "project" && route.slug === project.slug;
          return (
            <div key={project.slug} className={`sb-proj ${active ? "active" : ""}`} onClick={() => navigate(`project/${project.slug}`)}>
              <span className="sb-proj-dot" style={{ background: dotColor, boxShadow: st.state === "live" ? "var(--glow-ok)" : st.state === "stuck" ? "var(--glow-stuck)" : "none" }} />
              <span className="sb-proj-name">{project.slug}</span>
              <span className="sb-proj-state">{st.label || ""}</span>
            </div>
          );
        })}
      </div>

      <div className="sb-sec">System</div>
      <div className={`sb-link ${screen === "settings" ? "active" : ""}`} onClick={() => navigate("settings")}>
        <span className="sb-link-icon"><Icon.Settings /></span>
        <span>Settings</span>
      </div>

      <div className="sb-foot">
        <span className="mono">●</span>
        <span>v1.4.2 · {fleet?.daemonAlive ? "online" : "offline"}</span>
      </div>
    </aside>
  );
}

export function Topbar({ route, navigate, theme, toggleTheme }) {
  const { fleet, refresh, refreshing, now } = useFleet();
  const tone = fleet?.daemonAlive
    ? (fleet.verdictTone === "stuck" ? "stuck" : fleet.verdictTone === "watch" ? "watch" : "ok")
    : "stuck";

  let crumbContent;
  if (route.screen === "fleet") crumbContent = <span className="here">Fleet</span>;
  else if (route.screen === "workers") crumbContent = <><a onClick={() => navigate("fleet")}>Fleet</a><span className="crumb-sep">/</span><span className="here">Workers</span></>;
  else if (route.screen === "approvals") crumbContent = <><a onClick={() => navigate("fleet")}>Fleet</a><span className="crumb-sep">/</span><span className="here">Approvals</span></>;
  else if (route.screen === "settings") crumbContent = <span className="here">Settings</span>;
  else if (route.screen === "project") crumbContent = <><a onClick={() => navigate("fleet")}>Fleet</a><span className="crumb-sep">/</span><span className="here">{route.slug}</span></>;
  else crumbContent = <span className="here">{route.screen}</span>;

  return (
    <div className="topbar">
      <div className="crumb">{crumbContent}</div>

      <div className="topbar-right">
        <div className={`tb-status ${tone}`}>
          <span className="dot" />
          {fleet?.daemonAlive ? <>supervisor · {fleet.heartbeatBpm}/h</> : "supervisor offline"}
        </div>
        <button className="tb-btn ghost" onClick={refresh} title="Refresh">
          <Icon.Refresh spin={refreshing} /> {formatRefreshAge(fleet?.refreshedAt, now)}
        </button>
        <div className="tb-divider" />
        <button className="tb-btn ghost" onClick={toggleTheme} title="Toggle theme">
          {theme === "dark" ? <Icon.Sun /> : <Icon.Moon />}
        </button>
        <button className="tb-jump" onClick={() => navigate("cmdk")} title="Open command palette">
          <Icon.Search s={12} />
          <span>Jump to…</span>
          <kbd>⌘K</kbd>
        </button>
      </div>
    </div>
  );
}

// CommandPalette is the Cmd-K / Ctrl-K search palette (issue #345 / M-010).
// It is *read-only*: every entry resolves to either a navigation path, an
// external URL opened in a new tab, or the local theme toggle. No write
// actions (approve/reject, retry, dispatch, etc.) are exposed through the
// palette in V1 — the supervisor's existing approval surfaces stay the only
// way to take state-changing actions.
export function CommandPalette({ onClose, navigate, toggleTheme, fleet }) {
  const [q, setQ] = React.useState("");
  const [sel, setSel] = React.useState(0);

  const items = React.useMemo(() => searchFleetItems(fleet, q, 12), [fleet, q]);

  const inputRef = React.useRef();
  React.useEffect(() => { inputRef.current?.focus(); }, []);
  React.useEffect(() => { setSel(0); }, [q]);

  const select = (i) => {
    const item = items[i];
    if (!item) return;
    if (item.to === "theme") {
      toggleTheme();
    } else if (item.external) {
      try {
        window.open(item.to, "_blank", "noopener,noreferrer");
      } catch (_) {
        // Some hosts (jsdom in tests, locked-down embeds) block window.open;
        // we silently degrade — read-only navigation must never throw.
      }
    } else {
      navigate(item.to);
    }
    onClose();
  };

  const onKey = (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); setSel(s => Math.min(s + 1, items.length - 1)); }
    else if (e.key === "ArrowUp") { e.preventDefault(); setSel(s => Math.max(s - 1, 0)); }
    else if (e.key === "Enter") { e.preventDefault(); select(sel); }
    else if (e.key === "Escape") { e.preventDefault(); onClose(); }
  };

  const iconForKind = (kind) => {
    switch (kind) {
    case "Project": return <Icon.Project />;
    case "Dashboard": return <Icon.Fleet />;
    case "Session": return <Icon.Workers />;
    case "Issue": return <Icon.Github />;
    case "PR": return <Icon.Github />;
    case "Approval": return <Icon.Approval />;
    case "Action": return <Icon.Settings />;
    case "Page":
    default: return <Icon.Fleet />;
    }
  };

  return (
    <div className="cmdk-scrim" onClick={onClose} role="dialog" aria-label="Command palette">
      <div className="cmdk" onClick={e => e.stopPropagation()}>
        <input
          ref={inputRef}
          placeholder="Type a command or search projects, slots, issues, PRs…"
          value={q}
          onChange={e => setQ(e.target.value)}
          onKeyDown={onKey}
          aria-label="Search"
          aria-autocomplete="list"
        />
        <div className="cmdk-list" role="listbox">
          {items.length === 0 && <div className="dim mono" style={{ padding: "var(--s-3)", fontSize: 11 }}>No results</div>}
          {items.map((it, i) => (
            <div
              key={it.id}
              className={`cmdk-item ${i === sel ? "sel" : ""}`}
              role="option"
              aria-selected={i === sel}
              onClick={() => select(i)}
              onMouseEnter={() => setSel(i)}
            >
              <span style={{ width: 16, color: "var(--fg-3)" }}>{iconForKind(it.kind)}</span>
              <span style={{ flex: "1 1 auto", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                <strong>{it.title}</strong>
                {it.meta ? <span className="dim" style={{ marginLeft: 8, fontSize: 11 }}>{it.meta}</span> : null}
              </span>
              <span className="dim mono" style={{ fontSize: 10, marginLeft: 6, textTransform: "uppercase" }}>{it.kind}</span>
              {it.external && <span className="k" title="Opens externally">↗</span>}
            </div>
          ))}
        </div>
        <div className="cmdk-foot dim mono" style={{ padding: "var(--s-2) var(--s-3)", fontSize: 10, borderTop: "1px solid var(--border-1)" }}>
          ↑↓ navigate · Enter open · Esc close · read-only
        </div>
      </div>
    </div>
  );
}
