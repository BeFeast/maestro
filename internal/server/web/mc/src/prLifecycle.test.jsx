import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { PRGateFacts } from "./screens.jsx";

describe("Mission Control PR gate facts", () => {
  test("renders the real Greptile score with merge request lifecycle", () => {
    const html = renderToStaticMarkup(<PRGateFacts gates={{
      prNumber: 26,
      ci: "success",
      reviewSummary: "Greptile 4/5 · passed",
      reviewStreams: [{ name: "greptile", passed: true, score: 4, scoreMax: 5, verdict: "ok_to_merge", summary: "Greptile 4/5 · passed" }],
      mergeAction: { approvalId: "approval-26", status: "pending", label: "Merge requested", actionRequired: true },
      summary: "Merge requested for PR #26. Greptile 4/5 · passed.",
    }} />);

    expect(html).toContain("Greptile 4/5 · passed");
    expect(html).toContain("Merge requested");
    expect(html).toContain("approval-26");
    expect(html).toContain("CI success");
  });

  test("renders merged as terminal without repair or merge CTA copy", () => {
    const html = renderToStaticMarkup(<PRGateFacts gates={{
      prNumber: 26,
      ci: "success",
      reviewSummary: "Greptile 4/5 · passed",
      reviewStreams: [{ name: "greptile", passed: true, score: 4, scoreMax: 5, verdict: "ok_to_merge", summary: "Greptile 4/5 · passed" }],
      merged: true,
      summary: "Greptile 4/5 · passed · PR merged",
    }} />);

    expect(html).toContain("Greptile 4/5 · passed");
    expect(html).toContain("PR merged");
    expect(html).not.toContain("Address Greptile feedback");
    expect(html).not.toContain("Approve merge");
  });
});
