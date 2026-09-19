import React from "react";
import { ConfirmDialog } from "./atoms.jsx";
import { useFleet } from "./fleetContext.jsx";
import { postFleetEmergency } from "./fleetApi.js";

// EMERGENCY STOP UI (#840): a fleet-wide big red button in the topbar and a
// persistent banner on every page while a stop is active. Activation is gated by
// a confirmation modal but never by an approval queue — the button writes the
// same switch the CLI does, and the daemon picks it up within one cycle.

const LEVEL_LABEL = {
  llm_stopped: "LLM calls stopped",
  all_stopped: "whole fleet stopped",
};

function describeLevel(level) {
  return LEVEL_LABEL[level] || level || "active";
}

// EmergencyBanner is the always-visible red banner shown while a stop is active.
export function EmergencyBanner({ emergency }) {
  if (!emergency || !emergency.active) return null;
  const parts = [];
  if (emergency.since) parts.push(`since ${emergency.since}`);
  if (emergency.actor) parts.push(`by ${emergency.actor}`);
  if (emergency.reason) parts.push(`reason: ${emergency.reason}`);
  return (
    <div className="emergency-banner" role="alert">
      <span className="emergency-banner-dot" aria-hidden="true">🛑</span>
      <strong>EMERGENCY STOP active</strong>
      <span className="emergency-banner-detail">
        {describeLevel(emergency.level)}
        {parts.length ? " — " + parts.join(", ") : ""}
      </span>
    </div>
  );
}

// EmergencyControls is the topbar button. When no stop is active it offers the
// red "Emergency stop" button (confirm → stop-llm); while active it offers
// "Resume".
export function EmergencyControls() {
  const { fleet, refresh } = useFleet();
  const emergency = fleet?.emergency || { active: false, level: "none" };
  const [dialog, setDialog] = React.useState(null); // "stop" | "resume" | null
  const [reason, setReason] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState("");

  const close = () => {
    if (busy) return;
    setDialog(null);
    setReason("");
    setErr("");
  };

  const run = async (level) => {
    setBusy(true);
    setErr("");
    try {
      await postFleetEmergency({ level, reason: reason.trim() || undefined });
      await refresh();
      setDialog(null);
      setReason("");
    } catch (e) {
      setErr(e?.message || String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      {emergency.active ? (
        <button
          className="tb-btn danger emergency-btn-active"
          onClick={() => setDialog("resume")}
          title="Resume normal operation"
        >
          ⏵ Resume
        </button>
      ) : (
        <button
          className="tb-btn danger"
          onClick={() => setDialog("stop")}
          title="Emergency stop — kill workers + verify/build children and halt all LLM spend"
        >
          ⏹ Emergency stop
        </button>
      )}

      <ConfirmDialog
        open={dialog === "stop"}
        title="Emergency stop — kill workers, verify/build & halt LLM spend"
        confirmLabel="Kill all & stop LLM"
        danger
        busy={busy}
        onConfirm={() => run("llm_stopped")}
        onClose={close}
      >
        <p style={{ margin: "0 0 10px" }}>
          Halts <strong>all LLM work fleet-wide immediately</strong>: in-flight
          workers are killed, and maestro-attached verify/build children
          (java/gradle, gh, go test/build, android tools) under the daemon
          cgroup are stopped too. The supervisor drops to deterministic-only, no
          new workers spawn, and router/LLM calls stop. The daemon itself keeps
          running (dashboards/state) so you can watch while nothing spends. The
          flag persists across a restart until you resume.
        </p>
        <label className="emergency-reason-label">
          Reason (optional)
          <input
            className="emergency-reason-input"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder="e.g. runaway supervise burn on metered backend"
            disabled={busy}
          />
        </label>
        {err && <div className="emergency-dialog-err">{err}</div>}
      </ConfirmDialog>

      <ConfirmDialog
        open={dialog === "resume"}
        title="Resume normal operation"
        confirmLabel="Resume"
        busy={busy}
        onConfirm={() => run("none")}
        onClose={close}
      >
        <p style={{ margin: 0 }}>
          Clear the emergency stop ({describeLevel(emergency.level)}). LLM calls
          and worker spawns resume on the next cycle.
        </p>
        {err && <div className="emergency-dialog-err">{err}</div>}
      </ConfirmDialog>
    </>
  );
}
