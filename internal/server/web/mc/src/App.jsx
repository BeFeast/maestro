import React, { useCallback, useEffect, useMemo, useState } from "react";
import { FleetScreen } from "./fleet.jsx";
import { FleetProvider, useFleet } from "./fleetContext.jsx";
import { CommandPalette, SidebarV2, Topbar } from "./shell.jsx";
import {
  ApprovalsScreen,
  ProjectScreen,
  SettingsScreen,
  WorkerDrawer,
  WorkersScreen,
} from "./screens.jsx";

function parseRoute(pathname, search) {
  const path = (pathname || "/").replace(/\/+$/, "") || "/";
  if (path === "/" || path === "/fleet") return { screen: "fleet" };
  if (path === "/workers") {
    const q = new URLSearchParams(search || "");
    return { screen: "workers", slot: q.get("slot"), project: q.get("project") };
  }
  if (path === "/approvals") return { screen: "approvals" };
  if (path === "/settings") return { screen: "settings" };
  const projectMatch = path.match(/^\/project\/(.+)$/);
  if (projectMatch) {
    let slug;
    try {
      slug = decodeURIComponent(projectMatch[1]);
    } catch (e) {
      // Malformed percent-encoding (e.g. /project/%E0%A4%A) — degrade
      // to the fleet screen instead of white-screening the dashboard.
      // See issue #473.
      return { screen: "fleet" };
    }
    return { screen: "project", slug };
  }
  return { screen: "fleet" };
}

function routeToPath(route) {
  switch (route.screen) {
  case "fleet":
    return "/fleet";
  case "workers": {
    const params = new URLSearchParams();
    if (route.project) params.set("project", route.project);
    if (route.slot) params.set("slot", route.slot);
    const qs = params.toString();
    return qs ? `/workers?${qs}` : "/workers";
  }
  case "approvals":
    return "/approvals";
  case "settings":
    return "/settings";
  case "project":
    return `/project/${encodeURIComponent(route.slug || "")}`;
  default:
    return "/fleet";
  }
}

function AppShell() {
  const { fleet, theme, toggleTheme, now, error } = useFleet();
  const [route, setRoute] = useState(() =>
    parseRoute(window.location.pathname, window.location.search)
  );
  const [cmdkOpen, setCmdkOpen] = useState(false);
  const [drawer, setDrawer] = useState(null);

  const navigate = useCallback((path) => {
    if (path === "cmdk") {
      setCmdkOpen(true);
      return;
    }
    const normalized = path.startsWith("/") ? path : `/${path}`;
    const url = new URL(normalized, window.location.origin);
    window.history.pushState(null, "", url.pathname + url.search);
    setRoute(parseRoute(url.pathname, url.search));
  }, []);

  useEffect(() => {
    const onPop = () => setRoute(parseRoute(window.location.pathname, window.location.search));
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  useEffect(() => {
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setCmdkOpen(true);
      } else if (e.key === "Escape") {
        setCmdkOpen(false);
        setDrawer(null);
      } else if (e.key === "/" && document.activeElement?.tagName !== "INPUT") {
        e.preventDefault();
        setCmdkOpen(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const projectStates = useMemo(() => {
    const out = {};
    (fleet?.projects || []).forEach(p => { out[p.slug] = p.state; });
    return out;
  }, [fleet]);

  let screen;
  if (route.screen === "fleet") {
    screen = <FleetScreen navigate={navigate} />;
  } else if (route.screen === "project") {
    screen = (
      <ProjectScreen
        slug={route.slug}
        now={now}
        navigate={navigate}
        openDrawer={setDrawer}
      />
    );
  } else if (route.screen === "workers") {
    screen = (
      <WorkersScreen
        now={now}
        navigate={navigate}
        openDrawer={setDrawer}
        selectedSlot={route.slot}
        filterProject={route.project}
      />
    );
  } else if (route.screen === "approvals") {
    screen = <ApprovalsScreen navigate={navigate} />;
  } else if (route.screen === "settings") {
    screen = <SettingsScreen />;
  } else {
    screen = <FleetScreen navigate={navigate} />;
  }

  return (
    <>
      <div className="app">
        <SidebarV2 route={route} navigate={navigate} projectStates={projectStates} fleet={fleet} />
        <div className="main">
          <Topbar route={route} navigate={navigate} theme={theme} toggleTheme={toggleTheme} />
          <div className="page">
            <div className="page-inner">
              {error && !fleet && (
                <div className="error" style={{ marginBottom: "var(--s-4)" }}>
                  Fleet API error: {error}
                </div>
              )}
              {screen}
            </div>
          </div>
        </div>
      </div>

      {drawer && <WorkerDrawer worker={drawer} onClose={() => setDrawer(null)} now={now} />}
      {cmdkOpen && (
        <CommandPalette
          onClose={() => setCmdkOpen(false)}
          navigate={navigate}
          toggleTheme={toggleTheme}
          fleet={fleet}
        />
      )}
    </>
  );
}

export default function App() {
  return (
    <FleetProvider>
      <AppShell />
    </FleetProvider>
  );
}

export { parseRoute, routeToPath };
