import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { PRGateFacts, WorkerActionsPanel } from "./screens.jsx";

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

  test("renders explicit audited merge hold and release controls", () => {
    const html = renderToStaticMarkup(<WorkerActionsPanel worker={{
      project_name: "Maestro",
      slot: "sup-1",
      issue_number: 885,
      pr_number: 900,
      actions: [
        { id: "hold_merge", label: "Hold merge", description: "Audited hold", requires_approval: false },
        { id: "release_merge", label: "Release merge", description: "Audited release", requires_approval: false, disabled: true, disabled_reason: "No hold is active." },
      ],
    }} readOnly={false} refresh={() => {}} />);

    expect(html).toContain("Hold merge");
    expect(html).toContain("Release merge");
    expect(html).toContain("No hold is active");
  });
});
