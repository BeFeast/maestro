import { expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { mapFleetResponse } from "./fleetApi.js";
import { ModelBackendCatalog } from "./screens.jsx";

test("enabled backends stay visible and disabled definitions stay collapsed even when referenced", () => {
  const fleet = mapFleetResponse({ projects: [{ name: "pilot", effective_config: { model_policy: {
    default: "pilot",
    backends: [
      { name: "pilot", harness: "claude", model: "claude-opus-5", command_model: "claude-opus-5", enabled: true, references: ["model.default"], catalog_status: "listed" },
      { name: "codex", harness: "codex", model: "gpt-6-astra", command_model: "kimi-k2.7-code", enabled: true, references: [], catalog_status: "not_listed" },
      { name: "claude", harness: "claude", model: "claude-opus-4-8", enabled: false, references: ["routing.router_model"], catalog_status: "not_listed" },
      { name: "old-worker", harness: "claude", model: "claude-opus-4-8", enabled: false, references: [], catalog_status: "not_listed" },
    ],
    catalog: { status: "available", models: [{ id: "gpt-6-astra", provider: "openai", tier: "primary" }] },
  } } }] });
  const policy = fleet.projects[0].effectiveConfig.modelPolicy;
  expect(policy.backends[1].commandModel).toBe("kimi-k2.7-code");
  const html = renderToStaticMarkup(<ModelBackendCatalog policy={policy} />);
  expect(html).toContain("Enabled backends (2)");
  expect(html).toContain("Disabled backends (2)");
  expect(html).toContain("<details");
  expect(html).not.toContain("<details open");
  expect(html.indexOf("claude-opus-5")).toBeLessThan(html.indexOf("<details"));
  expect(html.indexOf("kimi-k2.7-code")).toBeLessThan(html.indexOf("<details"));
  expect(html.indexOf("claude-opus-4-8")).toBeGreaterThan(html.indexOf("Disabled backends (2)"));
  expect(html.indexOf("routing.router_model")).toBeGreaterThan(html.indexOf("Disabled backends (2)"));
  expect(html).toContain("Selection disabled");
  expect(html).toContain("Configured model metadata differs: gpt-6-astra");
  expect(html).toContain("Outside current catalog");
  expect(html).not.toContain("unavailable model");
});

test("missing reference and catalog metadata stay unknown", () => {
  const html = renderToStaticMarkup(<ModelBackendCatalog policy={{ backends: [{ name: "legacy", enabled: true }] }} />);
  expect(html).toContain("Enabled backends (1)");
  expect(html).toContain("Project reference information unavailable");
  expect(html).toContain("Current model catalog unavailable");
  expect(html).not.toContain("Disabled backends");
  expect(html).not.toContain("Outside current catalog");
});
