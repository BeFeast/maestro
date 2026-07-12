// Management Home helpers (#870).
//
// Pure, dependency-free logic for the Mission Control Management Home surface so
// it can be unit-tested with `bun test` without a DOM or React. The React panel
// in screens.jsx renders whatever `managementHomeView` returns and never derives
// the Obsidian URI by slicing the absolute path.
//
// Safety contract: the absolute execution-host `path` is display/copy only. It
// is never emitted to GitHub — no automated GitHub-facing output is produced
// from this module.

// obsidianUri builds the `obsidian://open?vault=<encoded>&file=<encoded>` URI
// from the structured vault name and the vault-relative path — NOT from string
// slicing the absolute path. Each field is percent-encoded independently so
// spaces and non-ASCII characters round-trip. Returns "" when either field is
// missing so the caller can hide the "Open in Obsidian" action rather than emit
// a broken link.
export function obsidianUri(vault, vaultPath) {
  const v = String(vault == null ? "" : vault).trim();
  const f = String(vaultPath == null ? "" : vaultPath).trim();
  if (!v || !f) return "";
  return `obsidian://open?vault=${encodeURIComponent(v)}&file=${encodeURIComponent(f)}`;
}

// managementHomeView normalizes the raw `management_home` object from the Fleet
// API into the shape the panel renders, or null when no home is configured
// (absent metadata → the panel renders nothing, no dead button). The primary
// label is the vault-relative path (e.g. Dev/Areas/<slug>), never the absolute
// path.
export function managementHomeView(raw) {
  if (!raw || typeof raw !== "object") return null;
  const kind = String(raw.kind || "").trim();
  const path = String(raw.path || "").trim();
  const vault = String(raw.vault || "").trim();
  const vaultPath = String(raw.vault_path || "").trim();
  // Treat an all-empty block as absent so legacy projects render no panel.
  if (!kind && !path && !vault && !vaultPath) return null;
  return {
    kind,
    path, // absolute execution-host path — copy/select only, never sent to GitHub
    vault,
    vaultPath,
    label: vaultPath || path, // vault-relative label is primary
    uri: obsidianUri(vault, vaultPath),
  };
}

// copyText copies `text` to the clipboard and reports success or the specific
// failure so the UI can degrade honestly (the path stays selectable either way).
// The clipboard is injected for testability and defaults to navigator.clipboard.
// Returns { ok: true } or { ok: false, reason: "unavailable" | "error" }.
export async function copyText(text, clipboard) {
  const clip =
    clipboard !== undefined
      ? clipboard
      : typeof navigator !== "undefined"
        ? navigator.clipboard
        : null;
  if (!clip || typeof clip.writeText !== "function") {
    return { ok: false, reason: "unavailable" };
  }
  try {
    await clip.writeText(String(text == null ? "" : text));
    return { ok: true };
  } catch (err) {
    return { ok: false, reason: "error", error: err };
  }
}
