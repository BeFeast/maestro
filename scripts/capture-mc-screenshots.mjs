// Capture Mission Control (MC) dashboard screenshots for the README.
//
// Run against a LIVE MC instance (same host the fleet runs on). Playwright +
// a cached chromium build are expected to be available; on an unprivileged LXC
// with userns enabled, headless chromium launches WITHOUT --no-sandbox.
//
// Usage:
//   bun scripts/capture-mc-screenshots.mjs
//
// Env overrides:
//   MC_BASE_URL  base URL of the running MC (default http://127.0.0.1:8786)
//   MC_PROJECT   project slug for the project view (default: first sidebar project)
//   MC_OUT_DIR   output directory (default docs/images/mc)
//
// Output: docs/images/mc/<view>.png — committed to the repo and referenced
// from README.md with repo-relative paths.

import { mkdir } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const { chromium } = await import("playwright");

const BASE_URL = process.env.MC_BASE_URL ?? "http://127.0.0.1:8786";
const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const OUT_DIR = resolve(repoRoot, process.env.MC_OUT_DIR ?? "docs/images/mc");

const VIEWPORT = { width: 1440, height: 900 };
const DEVICE_SCALE_FACTOR = 2;

await mkdir(OUT_DIR, { recursive: true });

const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({
  viewport: VIEWPORT,
  deviceScaleFactor: DEVICE_SCALE_FACTOR,
});

async function open(path) {
  await page.goto(`${BASE_URL}${path}`, {
    waitUntil: "networkidle",
    timeout: 30000,
  });
  // The MC SPA renders into #root after first paint; wait for content and let
  // charts / sparklines finish animating before we capture.
  await page.waitForFunction(() => document.querySelector("#root")?.children.length > 0, {
    timeout: 15000,
  });
  await page.waitForTimeout(1200);
}

async function shot(name, { fullPage = true } = {}) {
  const path = resolve(OUT_DIR, `${name}.png`);
  await page.screenshot({ path, fullPage });
  console.log(`saved ${path}`);
}

// Discover the project slug for the project view (first sidebar project unless
// overridden) so the script stays reusable across fleets.
async function firstProjectSlug() {
  if (process.env.MC_PROJECT) return process.env.MC_PROJECT;
  await open("/");
  const name = await page.$eval(".sb-proj-name", (el) => el.textContent.trim()).catch(() => null);
  return name;
}

const project = await firstProjectSlug();

// 1. Fleet overview — project health grid + hero next-action / operator brief.
await open("/");
await shot("overview");

// 2. Approvals — pending-approval list + approve/reject (cautious-gate write path).
await open("/approvals");
await shot("approvals");

// 3. A project view — drill-down for a single project.
if (project) {
  await open(`/project/${encodeURIComponent(project)}`);
  await shot("project");
} else {
  console.warn("no project slug found; skipping project view");
}

// 4. Workers — live worker / throughput detail (optional ops panel).
await open("/workers");
await shot("workers");

await browser.close();
console.log("done");
