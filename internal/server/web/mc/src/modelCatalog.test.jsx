import { expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { mapFleetResponse } from "./fleetApi.js";
import { ModelBackendCatalog } from "./screens.jsx";

test("retained definitions are collapsed without changing pinned project routes", () => {
  const fleet = mapFleetResponse({ projects: [{ name: "pilot", effective_config: { model_policy: {
    default: "pilot",
    backends: [
      { name: "pilot", harness: "claude", model: "claude-opus-4-8", command_model: "claude-opus-4-8", enabled: true, references: ["model.default"], catalog_status: "not_listed" },
      { name: "codex", harness: "codex", model: "gpt-6-astra", command_model: "kimi-k2.7-code", enabled: true, references: [], catalog_status: "not_listed" },
    ],
    catalog: { status: "available", models: [{ id: "gpt-6-astra", provider: "openai", tier: "primary" }] },
  } } }] });
  const policy = fleet.projects[0].effectiveConfig.modelPolicy;
  expect(policy.backends[1].commandModel).toBe("kimi-k2.7-code");
  const html = renderToStaticMarkup(<ModelBackendCatalog policy={policy} />);
  expect(html).toContain("Referenced by this project (1)");
  expect(html).toContain("<details");
  expect(html).not.toContain("<details open");
  expect(html.indexOf("claude-opus-4-8")).toBeLessThan(html.indexOf("<details"));
  expect(html.indexOf("kimi-k2.7-code")).toBeGreaterThan(html.indexOf("Not referenced by this project (1)"));
  expect(html).toContain("Configured model metadata differs: gpt-6-astra");
  expect(html).toContain("Outside current catalog");
  expect(html).not.toContain("unavailable model");
});

test("missing reference and catalog metadata stay unknown", () => {
  const html = renderToStaticMarkup(<ModelBackendCatalog policy={{ backends: [{ name: "legacy", enabled: true }] }} />);
  expect(html).toContain("Reference information unavailable (1)");
  expect(html).toContain("Current model catalog unavailable");
  expect(html).not.toContain("Not referenced by this project");
  expect(html).not.toContain("Outside current catalog");
});
