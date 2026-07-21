import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { mapFleetResponse } from "./fleetApi.js";
import { DispatchBlockersPanel } from "./screens.jsx";

describe("dispatch blockers", () => {
  test("renders the top-level hold and exact per-issue guards", () => {
    const now = Date.parse("2026-07-21T18:00:00Z");
    const fleet = mapFleetResponse({
      summary: {},
      projects: [{
        name: "Maestro",
        repo: "BeFeast/maestro",
        dispatch_hold: {
          active: true,
          reason_class: "blocking_outcome_check",
          detail: "blocking outcome check source-main-ci is fail",
          since: "2026-07-21T17:58:00Z",
        },
        queue_snapshot: {
          open: 2,
          eligible: 1,
          selected_candidate: {
            number: 1023,
            title: "ready dispatch visibility",
            priority_label: "P1",
          },
          eligible_ranked: [{
            number: 1023,
            title: "ready dispatch visibility",
            priority_label: "P1",
          }],
          skipped_candidates: [{
            number: 1002,
            title: "merged change awaiting runtime proof",
            priority_label: "P0",
            category: "other",
            reason: "already in progress (code_landed, awaiting verification)",
          }],
        },
      }],
    }, now);

    const project = fleet.projects[0];
    expect(project.dispatchHold).toEqual({
      active: true,
      reasonClass: "blocking_outcome_check",
      detail: "blocking outcome check source-main-ci is fail",
      since: "2026-07-21T17:58:00Z",
    });

    const html = renderToStaticMarkup(<DispatchBlockersPanel project={project} now={now} />);
    expect(html).toContain("Dispatch blockers");
    expect(html).toContain("dispatch held");
    expect(html).toContain("blocking outcome check source-main-ci is fail");
    expect(html).toContain("#1023");
    expect(html).toContain("held");
    expect(html).toContain("#1002");
    expect(html).toContain("already in progress (code_landed, awaiting verification)");
  });
});
