/* global React */
// ============================================================
// Maestro MC — App shell: sidebar, topbar, router, cmd-k
// ============================================================

function Sidebar({ route, navigate, scenarioKey }) {
  const sc = SCENARIOS[scenarioKey];
  const isFleet = route.screen === "fleet";
  const isProject = route.screen === "project";
  const isWorkers = route.screen === "workers";
  const isApprovals = route.screen === "approvals";
  const isSettings = route.screen === "settings";

  return (
    <aside className="sb">
      <div className="sb-brand">
        <BrandMark />
        <div>
          <div className="sb-brand-name">Maestro</div>
          <div className="sb-brand-sub">mission control · v1.4</div>
        </div>
      </div>
    </aside>
  );
}

// The Maestro brand mark — "44" with a baton/heartbeat line connecting them
function BrandMark({ size = 32 }) {
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

// Cleaner re-implementation
function SidebarV2({ route, navigate, scenarioKey, projectStates }) {
  const screen = route.screen;
  const sc = SCENARIOS[scenarioKey];
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
        <span className="sb-link-count">{PROJECT_ORDER.length}</span>
      </div>
      <div className={`sb-link ${screen === "workers" ? "active" : ""}`} onClick={() => navigate("workers")}>
        <span className="sb-link-icon"><Icon.Workers /></span>
        <span>Workers</span>
        <span className="sb-link-count">{sc.workerCount}</span>
      </div>
      <div className={`sb-link ${screen === "approvals" ? "active" : ""}`} onClick={() => navigate("approvals")}>
        <span className="sb-link-icon"><Icon.Approval /></span>
        <span>Approvals</span>
        {sc.activeApprovals > 0 ? (
          <span className={`sb-link-count ${sc.tone === "stuck" || scenarioKey === "attention" ? "dot" : ""}`}>{sc.activeApprovals}</span>
        ) : null}
      </div>

      <div className="sb-sec">Projects</div>
      <div className="sb-projects">
        {PROJECT_ORDER.map(slug => {
          const st = projectStates[slug];
          const dotColor = st.state === "live" ? "var(--ok)"
            : st.state === "stuck" ? "var(--stuck)"
            : st.state === "policy" ? "var(--policy)"
            : st.state === "unknown" ? "var(--stuck)"
            : "var(--idle)";
          const active = screen === "project" && route.slug === slug;
          return (
            <div key={slug} className={`sb-proj ${active ? "active" : ""}`} onClick={() => navigate(`project/${slug}`)}>
              <span className="sb-proj-dot" style={{ background: dotColor, boxShadow: st.state === "live" ? "var(--glow-ok)" : st.state === "stuck" ? "var(--glow-stuck)" : "none" }} />
              <span className="sb-proj-name">{slug}</span>
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
        <span>v1.4.2 · {sc.daemonAlive ? "online" : "offline"}</span>
      </div>
    </aside>
  );
}

// ============================================================
// Topbar
// ============================================================
function Topbar({ route, scenarioKey, navigate, theme, toggleTheme }) {
  const sc = SCENARIOS[scenarioKey];
  const tone = sc.daemonAlive ? (sc.tone === "stuck" ? "stuck" : sc.tone === "watch" ? "watch" : "ok") : "stuck";
  const [spinning, setSpinning] = React.useState(false);
  const refresh = () => {
    setSpinning(true);
    setTimeout(() => setSpinning(false), 700);
  };

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
          {sc.daemonAlive ? <>supervisor · {sc.heartbeatBpm}/h</> : "supervisor offline"}
        </div>
        <button className="tb-btn ghost" onClick={refresh} title="Refresh">
          <Icon.Refresh spin={spinning} /> 12s
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

// ============================================================
// Command palette
// ============================================================
function CommandPalette({ onClose, navigate }) {
  const [q, setQ] = React.useState("");
  const [sel, setSel] = React.useState(0);
  const items = React.useMemo(() => {
    const all = [
      { kind: "page", title: "Fleet overview", to: "fleet", k: "G F" },
      { kind: "page", title: "Workers", to: "workers", k: "G W" },
      { kind: "page", title: "Approvals", to: "approvals", k: "G A" },
      { kind: "page", title: "Settings", to: "settings", k: "G ," },
      ...PROJECT_ORDER.map(slug => ({ kind: "project", title: slug, to: `project/${slug}`, k: "" })),
      { kind: "scenario", title: "Scenario: healthy", to: "scenario:healthy", k: "" },
      { kind: "scenario", title: "Scenario: busy", to: "scenario:busy", k: "" },
      { kind: "scenario", title: "Scenario: attention required", to: "scenario:attention", k: "" },
      { kind: "scenario", title: "Scenario: daemon down", to: "scenario:broken", k: "" },
      { kind: "action", title: "Toggle theme", to: "theme", k: "T" },
    ];
    if (!q) return all;
    return all.filter(x => x.title.toLowerCase().includes(q.toLowerCase()));
  }, [q]);

  const inputRef = React.useRef();
  React.useEffect(() => { inputRef.current?.focus(); }, []);

  const select = (i) => {
    const item = items[i];
    if (!item) return;
    if (item.to.startsWith("scenario:")) {
      const sc = item.to.split(":")[1];
      window.__mc_setScenario?.(sc);
    } else if (item.to === "theme") {
      window.__mc_toggleTheme?.();
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

  return (
    <div className="cmdk-scrim" onClick={onClose}>
      <div className="cmdk" onClick={e => e.stopPropagation()}>
        <input ref={inputRef} placeholder="Type a command or search…" value={q} onChange={e => { setQ(e.target.value); setSel(0); }} onKeyDown={onKey} />
        <div className="cmdk-list">
          {items.length === 0 && <div className="dim mono" style={{ padding: "var(--s-3)", fontSize: 11 }}>No results</div>}
          {items.map((it, i) => (
            <div key={it.to} className={`cmdk-item ${i === sel ? "sel" : ""}`} onClick={() => select(i)} onMouseEnter={() => setSel(i)}>
              <span style={{ width: 16, color: "var(--fg-3)" }}>
                {it.kind === "page" && <Icon.Fleet />}
                {it.kind === "project" && <Icon.Project />}
                {it.kind === "scenario" && <Icon.Bolt />}
                {it.kind === "action" && <Icon.Settings />}
              </span>
              {it.title}
              <span className="dim mono" style={{ fontSize: 10, marginLeft: 6, textTransform: "uppercase" }}>{it.kind}</span>
              {it.k && <span className="k">{it.k}</span>}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

Object.assign(window, { Sidebar, SidebarV2, Topbar, CommandPalette });
