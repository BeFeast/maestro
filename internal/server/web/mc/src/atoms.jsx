import React from "react";
import { ecgPath } from "./utils.js";

export const Icon = {
  Fleet: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.25" strokeLinecap="round" strokeLinejoin="round"><path d="M1.5 3.5H4L5 2L6.5 5L8 3.5H14.5" /><path d="M1.5 8H5L6 6.5L7.5 9.5L9 8H14.5" /><path d="M1.5 12.5H6L7 11L8.5 13.5L10 12.5H14.5" /></svg>),
  Project: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M2 4.5L8 1.5L14 4.5L8 7.5L2 4.5Z" /><path d="M2 4.5V11.5L8 14.5V7.5" /><path d="M14 4.5V11.5L8 14.5" /></svg>),
  Workers: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><circle cx="5" cy="5.5" r="2.2" /><circle cx="11" cy="5.5" r="2.2" /><path d="M1.5 13C1.5 11 3 9.5 5 9.5C7 9.5 8.5 11 8.5 13" strokeLinecap="round" /><path d="M7.5 13C7.5 11 9 9.5 11 9.5C13 9.5 14.5 11 14.5 13" strokeLinecap="round" /></svg>),
  Approval: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M3 2.5H13L13 13.5L8 11.5L3 13.5L3 2.5Z" strokeLinejoin="round" /><path d="M5.5 7L7 8.5L10.5 5" strokeLinecap="round" strokeLinejoin="round" /></svg>),
  Settings: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><circle cx="8" cy="8" r="2.2" /><path d="M8 1.5V3M8 13V14.5M3.4 3.4L4.5 4.5M11.5 11.5L12.6 12.6M1.5 8H3M13 8H14.5M3.4 12.6L4.5 11.5M11.5 4.5L12.6 3.4" strokeLinecap="round" /></svg>),
  GitPR: () => (<svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><circle cx="4" cy="4" r="1.6" /><circle cx="4" cy="12" r="1.6" /><circle cx="12" cy="12" r="1.6" /><path d="M4 5.5V10.5" /><path d="M12 10.5V7C12 5.5 11 4.5 9.5 4.5H7M9 3L7 4.5L9 6" strokeLinecap="round" strokeLinejoin="round" /></svg>),
  Github: () => (<svg width="11" height="11" viewBox="0 0 16 16" fill="currentColor"><path d="M8 .5C3.6.5 0 4.1 0 8.5c0 3.5 2.3 6.5 5.5 7.6.4.1.5-.2.5-.4v-1.4c-2.2.5-2.7-1-2.7-1-.4-.9-.9-1.2-.9-1.2-.7-.5.1-.5.1-.5.8.1 1.2.8 1.2.8.7 1.2 1.9.9 2.4.7.1-.5.3-.9.5-1.1-1.8-.2-3.6-.9-3.6-3.9 0-.9.3-1.6.8-2.1-.1-.2-.4-1 .1-2.1 0 0 .7-.2 2.2.8.6-.2 1.3-.3 2-.3s1.4.1 2 .3c1.5-1 2.2-.8 2.2-.8.4 1.1.2 1.9.1 2.1.5.5.8 1.2.8 2.1 0 3-1.8 3.7-3.6 3.9.3.3.5.7.5 1.5v2.2c0 .2.1.5.5.4C13.7 15 16 12 16 8.5 16 4.1 12.4.5 8 .5z" /></svg>),
  Refresh: ({ s = 14, spin = false }) => (<svg width={s} height={s} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" style={{ animation: spin ? "spin 800ms linear" : undefined }}><path d="M14 8A6 6 0 1 1 12.5 4M14 2V5H11" strokeLinecap="round" strokeLinejoin="round" /></svg>),
  Search: ({ s = 14 }) => (<svg width={s} height={s} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><circle cx="7" cy="7" r="4.5" /><path d="M10.5 10.5L14 14" strokeLinecap="round" /></svg>),
  Chevron: ({ s = 10, dir = "right" }) => { const r = { right: 0, down: 90, up: -90, left: 180 }[dir]; return (<svg width={s} height={s} viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.5" style={{ transform: `rotate(${r}deg)` }}><path d="M4.5 2.5L8 6L4.5 9.5" strokeLinecap="round" strokeLinejoin="round" /></svg>); },
  Bolt: () => (<svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor"><path d="M9 1L3 9H8L7 15L13 7H8L9 1Z" /></svg>),
  X: ({ s = 14 }) => (<svg width={s} height={s} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M3 3L13 13M13 3L3 13" strokeLinecap="round" /></svg>),
  Sun: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><circle cx="8" cy="8" r="2.5" /><path d="M8 1V2.5M8 13.5V15M2.5 2.5L3.5 3.5M12.5 12.5L13.5 13.5M1 8H2.5M13.5 8H15M2.5 13.5L3.5 12.5M12.5 3.5L13.5 2.5" strokeLinecap="round" /></svg>),
  Moon: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M13.5 9.5C12.7 11.6 10.5 13 8 13C4.7 13 2 10.3 2 7C2 4.5 3.4 2.3 5.5 1.5C5.2 2.3 5 3.1 5 4C5 7 7.5 9.5 10.5 9.5C11.4 9.5 12.2 9.3 13 9C13 9.2 13.2 9.3 13.5 9.5Z" strokeLinejoin="round" /></svg>),
  Tape: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><rect x="1.5" y="4" width="13" height="8" rx="1.2" /><circle cx="5" cy="8" r="1.4" /><circle cx="11" cy="8" r="1.4" /></svg>),
  Grid: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><rect x="2" y="2" width="5" height="5" rx="0.6" /><rect x="9" y="2" width="5" height="5" rx="0.6" /><rect x="2" y="9" width="5" height="5" rx="0.6" /><rect x="9" y="9" width="5" height="5" rx="0.6" /></svg>),
  List: () => (<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><line x1="2" y1="4" x2="14" y2="4" strokeLinecap="round" /><line x1="2" y1="8" x2="14" y2="8" strokeLinecap="round" /><line x1="2" y1="12" x2="14" y2="12" strokeLinecap="round" /></svg>),
  Cmd: () => (<svg width="11" height="11" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"><rect x="3" y="3" width="10" height="10" rx="2.2" /></svg>),
  Play: () => (<svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor"><path d="M4 2.5L13 8L4 13.5V2.5Z" /></svg>),
  Pause: () => (<svg width="10" height="10" viewBox="0 0 16 16" fill="currentColor"><rect x="3" y="3" width="3.5" height="10" /><rect x="9.5" y="3" width="3.5" height="10" /></svg>),
};

export function Heartbeat({ tone = "ok", bpm = 18, daemonAlive = true, width = 540, height = 120 }) {
  const [t, setT] = React.useState(0);
  React.useEffect(() => {
    let raf;
    let last = performance.now();
    const loop = (now) => {
      const dt = (now - last) / 1000;
      last = now;
      setT(prev => prev + dt);
      raf = requestAnimationFrame(loop);
    };
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, []);

  const color = tone === "ok" ? "var(--ok)" : tone === "watch" ? "var(--watch)" : tone === "stuck" ? "var(--stuck)" : "var(--fg-3)";
  const path = ecgPath(daemonAlive ? bpm : 0, t, width, height);

  return (
    <div className="ecg">
      <div className={`ecg-corner ${tone}`}>
        <span className="led" />
        {daemonAlive ? "SUPERVISOR" : "DAEMON OFFLINE"}
      </div>
      <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none">
        <path d={path} stroke={color} strokeWidth="1" fill="none" opacity="0.18" />
        <path d={path} stroke={color} strokeWidth="1.8" fill="none" strokeLinejoin="round" strokeLinecap="round" style={{ filter: `drop-shadow(0 0 4px ${color})` }} />
      </svg>
      <div className="ecg-rate">
        <strong className="mono">{daemonAlive ? bpm : "—"}</strong> <span>{daemonAlive ? "decisions/h" : "no signal"}</span>
      </div>
    </div>
  );
}

export function Sparkline({ data, warm = false }) {
  const max = Math.max(...data, 1);
  return (
    <div className="spark">
      {data.map((v, i) => (
        <div key={i} className={`spark-bar ${warm ? "warm" : ""}`} style={{ height: `${(v / max) * 100}%` }} />
      ))}
    </div>
  );
}

export function Stat({ label, value, of, sub, tone = "info", live, sparkline, sparkWarm, onClick }) {
  return (
    <div className={`stat ${tone}`} onClick={onClick} style={{ cursor: onClick ? "pointer" : "default" }}>
      <div className="stat-label">
        {label}
        {live && <span className="stat-live" />}
      </div>
      <div className="stat-value">
        {value}
        {of && <span className="of">{of}</span>}
      </div>
      {sub && <div className="stat-sub">{sub}</div>}
      {sparkline && <Sparkline data={sparkline} warm={sparkWarm} />}
    </div>
  );
}

export function Pill({ tone = "idle", children, noDot, title }) {
  return <span className={`pill ${tone} ${noDot ? "no-dot" : ""}`} title={title}>{children}</span>;
}

export function Segmented({ value, onChange, options }) {
  return (
    <div className="seg">
      {options.map(o => (
        <button
          key={o.value}
          className={value === o.value ? "active" : ""}
          onClick={() => onChange(o.value)}
        >
          {o.icon}
          {o.label}
          {o.count != null && <span className="count">{o.count}</span>}
        </button>
      ))}
    </div>
  );
}

export function Panel({ title, sub, right, children, padded = false }) {
  return (
    <div className="panel">
      {(title || right) && (
        <div className="panel-head">
          {title && <div className="panel-title">{title}</div>}
          {sub && <div className="panel-sub">{sub}</div>}
          {right && <div className="right">{right}</div>}
        </div>
      )}
      <div style={padded ? { padding: "var(--s-4) var(--s-5)" } : undefined}>{children}</div>
    </div>
  );
}

export function QueueBar({ ready = 0, held = 0, blocked = 0, height = 6 }) {
  const total = Math.max(ready + held + blocked, 1);
  return (
    <div style={{ display: "flex", height, borderRadius: height / 2, overflow: "hidden", background: "var(--bg-3)" }}>
      {ready > 0 && <span style={{ flex: ready / total, background: "var(--ok)" }} title={`${ready} ready`} />}
      {held > 0 && <span style={{ flex: held / total, background: "var(--policy)" }} title={`${held} held`} />}
      {blocked > 0 && <span style={{ flex: blocked / total, background: "var(--stuck)" }} title={`${blocked} blocked`} />}
    </div>
  );
}

// ConfirmDialog is a minimal, dependency-free <dialog> wrapper used by
// approve/reject flows. open=true mounts and shows the dialog; onClose
// fires for both confirm and cancel paths (the caller distinguishes via
// onConfirm). Body content is passed as children.
export function ConfirmDialog({ open, title, children, confirmLabel = "Confirm", cancelLabel = "Cancel", danger = false, busy = false, onConfirm, onClose }) {
  const ref = React.useRef(null);
  React.useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) {
      try { el.showModal(); } catch (_) { el.show(); }
    }
    if (!open && el.open) {
      el.close();
    }
  }, [open]);
  if (!open) return null;
  return (
    <dialog ref={ref} className="mc-confirm-dialog" onClose={onClose}>
      <form method="dialog" onSubmit={e => { e.preventDefault(); }}>
        <h3 style={{ margin: 0, fontSize: 14, fontWeight: 600 }}>{title}</h3>
        <div style={{ marginTop: 12, fontSize: 13, lineHeight: 1.4 }}>{children}</div>
        <div style={{ marginTop: 16, display: "flex", gap: 8, justifyContent: "flex-end" }}>
          <button type="button" className="tb-btn" disabled={busy} onClick={onClose}>{cancelLabel}</button>
          <button type="button" className={"tb-btn " + (danger ? "danger" : "primary")} disabled={busy} onClick={onConfirm}>{busy ? "…" : confirmLabel}</button>
        </div>
      </form>
    </dialog>
  );
}
