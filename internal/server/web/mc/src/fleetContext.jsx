import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import {
  fetchFleetRaw,
  loadStoredTheme,
  mapFleetResponse,
  refreshFleetTapeEvents,
  storeTheme,
} from "./fleetApi.js";

const FleetContext = createContext(null);

export function FleetProvider({ children }) {
  const [theme, setTheme] = useState(loadStoredTheme);
  const [fleet, setFleet] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [refreshing, setRefreshing] = useState(false);
  const [now, setNow] = useState(() => Date.now());

  const refresh = useCallback(async () => {
    setRefreshing(true);
    try {
      const raw = await fetchFleetRaw();
      setFleet(mapFleetResponse(raw, Date.now()));
      setError(null);
    } catch (err) {
      setError(err.message || String(err));
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    refresh();
    const poll = setInterval(refresh, 12000);
    return () => clearInterval(poll);
  }, [refresh]);

  useEffect(() => {
    const tick = setInterval(() => setNow(Date.now()), 1500);
    return () => clearInterval(tick);
  }, []);

  useEffect(() => {
    document.documentElement.setAttribute("data-theme", theme);
    storeTheme(theme);
  }, [theme]);

  const toggleTheme = useCallback(() => {
    setTheme(current => (current === "dark" ? "light" : "dark"));
  }, []);

  const fleetWithTape = useMemo(
    () => refreshFleetTapeEvents(fleet, now),
    [fleet, now]
  );

  const value = useMemo(
    () => ({
      fleet: fleetWithTape,
      loading,
      error,
      refreshing,
      refresh,
      theme,
      toggleTheme,
      now,
    }),
    [fleetWithTape, loading, error, refreshing, refresh, theme, toggleTheme, now]
  );

  return <FleetContext.Provider value={value}>{children}</FleetContext.Provider>;
}

export function useFleet() {
  const ctx = useContext(FleetContext);
  if (!ctx) throw new Error("useFleet must be used within FleetProvider");
  return ctx;
}
