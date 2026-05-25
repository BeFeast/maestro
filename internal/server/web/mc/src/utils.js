export function ecgPath(bpm, t, width = 600, height = 120) {
  if (bpm <= 0) {
    return `M 0 ${height / 2} L ${width} ${height / 2}`;
  }
  const beatsVisible = Math.max(1, Math.round((bpm / 60) * 4));
  const segW = width / beatsVisible;
  const midY = height / 2;
  const points = [];
  const offset = (t * 90) % segW;
  for (let b = -1; b <= beatsVisible + 1; b++) {
    const x0 = b * segW - offset;
    points.push([x0 + segW * 0.0, midY]);
    points.push([x0 + segW * 0.30, midY]);
    points.push([x0 + segW * 0.35, midY - 5]);
    points.push([x0 + segW * 0.40, midY + 8]);
    points.push([x0 + segW * 0.43, midY - 40]);
    points.push([x0 + segW * 0.46, midY + 16]);
    points.push([x0 + segW * 0.50, midY - 4]);
    points.push([x0 + segW * 0.55, midY]);
    points.push([x0 + segW * 1.00, midY]);
  }
  return "M " + points.map(p => `${p[0].toFixed(1)} ${p[1].toFixed(1)}`).join(" L ");
}

export function relTime(date, now) {
  const ms = now - (date instanceof Date ? date.getTime() : date);
  const min = Math.floor(ms / 60000);
  const hr = Math.floor(min / 60);
  const day = Math.floor(hr / 24);
  if (ms < 0) return "future";
  if (min < 1) return "just now";
  if (min < 60) return `${min}m ago`;
  if (hr < 24) return `${hr}h ago`;
  return `${day}d ago`;
}

export function parseTimestamp(value) {
  if (!value) return null;
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : null;
}

export function slugifyProject(name) {
  return String(name || "").trim();
}
