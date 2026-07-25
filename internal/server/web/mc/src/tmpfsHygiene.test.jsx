import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { attentionTargetURL, TmpfsHygienePanel } from "./fleet.jsx";
import { mapTmpfsHygiene } from "./fleetApi.js";

describe("Fleet tmpfs pressure details", () => {
  test("renders actionable sweep details and routes host-only attention to them", () => {
    const hygiene = mapTmpfsHygiene({
      timestamp: "2026-07-21T18:00:00Z",
      mode: "apply",
      tmpfs: true,
      use_pct: 87,
      pressure: true,
      attention_code: "tmpfs_pressure",
      freed_bytes: 8192,
      protected_entries: 4,
      deleted_entries: 3,
      partial_entries: 1,
    });

    const html = renderToStaticMarkup(
      <TmpfsHygienePanel hygiene={hygiene} now={Date.parse("2026-07-21T18:05:00Z")} />,
    );
    expect(html).toContain("Host tmpfs pressure");
    expect(html).toContain("87% used");
    expect(html).toContain("8.0 KiB");
    expect(html).toContain("maestro tmpfs-hygiene --dry-run");
    expect(html).toContain("tmpfs hygiene runbook");
    expect(attentionTargetURL({ tmpfsHygiene: hygiene, projects: [] })).toBe("#tmpfs-pressure");
  });
});
