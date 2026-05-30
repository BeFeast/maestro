/* global React, ReactDOM */
// ============================================================
// Maestro Mission Control — App entry
// ============================================================

const { useState, useEffect, useMemo, useCallback, useRef } = React;

// ---- Hash router ----
function parseRoute(hash) {
  const h = (hash || "").replace(/^#\/?/, "");
  if (!h) return { screen: "fleet" };
  if (h === "cmdk") return { screen: "cmdk" };
  if (h.startsWith("project/")) return { screen: "project", slug: h.slice("project/".length) };
  if (h.startsWith("workers")) {
    const q = h.includes("?") ? new URLSearchParams(h.split("?")[1]) : null;
    return { screen: "workers", slot: q?.get("slot"), project: q?.get("project") };
  }
  if (h === "approvals") return { screen: "approvals" };
  if (h === "settings") return { screen: "settings" };
  return { screen: "fleet" };
}

function App() {
  // Tweaks (persisted)
  const TWEAK_DEFAULTS = /*EDITMODE-BEGIN*/{
    "scenario": "healthy",
    "theme": "light",
    "layout": "tape"
  }/*EDITMODE-END*/;
  const [tweaks, setTweak] = useTweaks(TWEAK_DEFAULTS);

  // Apply theme to <html>
  useEffect(() => {
    document.documentElement.setAttribute("data-theme", tweaks.theme);
  }, [tweaks.theme]);

  // Expose setters for the command palette
  useEffect(() => {
    window.__mc_setScenario = (s) => setTweak("scenario", s);
    window.__mc_toggleTheme = () => setTweak("theme", tweaks.theme === "dark" ? "light" : "dark");
  }, [setTweak, tweaks.theme]);

  // ---- Hash routing ----
  const [route, setRoute] = useState(() => parseRoute(window.location.hash));
  useEffect(() => {
    const onHash = () => setRoute(parseRoute(window.location.hash));
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const navigate = useCallback((path) => {
    if (path === "cmdk") {
      setCmdkOpen(true);
      return;
    }
    window.location.hash = "#/" + path;
  }, []);

  // ---- Cmd-K palette ----
  const [cmdkOpen, setCmdkOpen] = useState(false);
  useEffect(() => {
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault(); setCmdkOpen(true);
      } else if (e.key === "Escape") {
        setCmdkOpen(false); setDrawer(null);
      } else if (e.key === "/" && document.activeElement?.tagName !== "INPUT") {
        e.preventDefault(); setCmdkOpen(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // ---- Drawer ----
  const [drawer, setDrawer] = useState(null);
  const openDrawer = useCallback((worker) => setDrawer(worker), []);

  // ---- Live ticking clock — drives all "X ago" displays ----
  const baseRef = useRef(Date.now());
  const [now, setNow] = useState(baseRef.current);
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1500);
    return () => clearInterval(id);
  }, []);

  // ---- Per-project state (passed to sidebar so dots stay live) ----
  const projectStates = useMemo(() => {
    const out = {};
    PROJECT_ORDER.forEach(s => { out[s] = projectState(s, tweaks.scenario); });
    return out;
  }, [tweaks.scenario]);

  // ---- Render screen ----
  let screen;
  if (route.screen === "fleet") {
    screen = <FleetScreen
      scenarioKey={tweaks.scenario}
      layout={tweaks.layout}
      setLayout={(v) => setTweak("layout", v)}
      now={now}
      navigate={navigate}
    />;
  } else if (route.screen === "project") {
    screen = <ProjectScreen
      slug={route.slug}
      scenarioKey={tweaks.scenario}
      now={now}
      navigate={navigate}
      openDrawer={openDrawer}
    />;
  } else if (route.screen === "workers") {
    screen = <WorkersScreen
      scenarioKey={tweaks.scenario}
      now={now}
      navigate={navigate}
      openDrawer={openDrawer}
      selectedSlot={route.slot}
      filterProject={route.project}
    />;
  } else if (route.screen === "approvals") {
    screen = <ApprovalsScreen scenarioKey={tweaks.scenario} now={now} navigate={navigate} />;
  } else if (route.screen === "settings") {
    screen = <SettingsScreen scenarioKey={tweaks.scenario} now={now} />;
  } else {
    screen = <FleetScreen scenarioKey={tweaks.scenario} layout={tweaks.layout} setLayout={(v) => setTweak("layout", v)} now={now} navigate={navigate} />;
  }

  return (
    <>
      <div className="app">
        <SidebarV2 route={route} navigate={navigate} scenarioKey={tweaks.scenario} projectStates={projectStates} />
        <div className="main">
          <Topbar route={route} scenarioKey={tweaks.scenario} navigate={navigate}
            theme={tweaks.theme} toggleTheme={() => setTweak("theme", tweaks.theme === "dark" ? "light" : "dark")} />
          <div className="page">
            <div className="page-inner">{screen}</div>
          </div>
        </div>
      </div>

      {drawer && <WorkerDrawer worker={drawer} onClose={() => setDrawer(null)} now={now} />}
      {cmdkOpen && <CommandPalette onClose={() => setCmdkOpen(false)} navigate={navigate} />}

      {/* Tweaks panel */}
      <TweaksPanel title="Tweaks" defaultOpen={false}>
        <TweakSection title="Scenario">
          <TweakSelect
            label="Fleet state"
            value={tweaks.scenario}
            onChange={v => setTweak("scenario", v)}
            options={[
              { value: "healthy", label: "Healthy — all quiet" },
              { value: "busy", label: "Busy — workers in flight" },
              { value: "attention", label: "Attention — PR stuck" },
              { value: "broken", label: "Daemon offline" },
            ]}
          />
        </TweakSection>
        <TweakSection title="Fleet layout">
          <TweakRadio
            value={tweaks.layout}
            onChange={v => setTweak("layout", v)}
            options={[
              { value: "tape", label: "Tape" },
              { value: "rail", label: "Rail" },
              { value: "cards", label: "Cards" },
            ]}
          />
        </TweakSection>
        <TweakSection title="Theme">
          <TweakRadio
            value={tweaks.theme}
            onChange={v => setTweak("theme", v)}
            options={[
              { value: "dark",  label: "Dark" },
              { value: "light", label: "Light" },
            ]}
          />
        </TweakSection>
        <TweakSection title="Jump to">
          <div style={{ display: "grid", gap: 4 }}>
            {[
              ["fleet", "Fleet overview"],
              ["project/finance-tracker", "Project · finance-tracker"],
              ["project/maestro", "Project · maestro"],
              ["workers", "Workers"],
              ["approvals", "Approvals"],
              ["settings", "Settings"],
            ].map(([p, l]) => (
              <TweakButton key={p} onClick={() => navigate(p)}>{l}</TweakButton>
            ))}
          </div>
        </TweakSection>
      </TweaksPanel>
    </>
  );
}

ReactDOM.createRoot(document.getElementById("root")).render(<App />);
