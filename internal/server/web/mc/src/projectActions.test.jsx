import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { mapFleetResponse } from "./fleetApi.js";
import { ProjectActionsPanel } from "./screens.jsx";

describe("project actions", () => {
  test("maps and renders stale backend batch restart controls", () => {
    const fleet = mapFleetResponse({
      refreshed_at: "2026-07-15T12:00:00Z",
      verdict: { tone: "healthy" },
      summary: {},
      projects: [{
        name: "Maestro",
        repo: "BeFeast/maestro",
        actions: [{
          id: "restart_stale_backend_workers",
          label: "Restart stale backend workers",
          description: "Preview and enqueue restart_worker approvals for PR-less workers running stale backend settings; open-PR workers are skipped.",
          scope: "project",
          target: "Maestro",
          mutating: true,
          requires_approval: true,
          workers: [{ slot: "sup-46", issue_number: 900, reason: "running xhigh, effective high" }],
          skipped_workers: [{ slot: "sup-47", issue_number: 901, pr_number: 904, reason: "open PR workers require in-place repair" }],
          method: "POST",
          endpoint: "/api/v1/fleet/actions",
        }],
      }],
    }, Date.parse("2026-07-15T12:00:00Z"));

    const project = fleet.projects[0];
    expect(project.actions).toHaveLength(1);
    expect(project.actions[0].id).toBe("restart_stale_backend_workers");

    const html = renderToStaticMarkup(<ProjectActionsPanel project={project} refresh={() => {}} />);
    expect(html).toContain("Project controls");
    expect(html).toContain("Restart stale backend workers");
    expect(html).toContain("1 restartable");
    expect(html).toContain("1 skipped");
  });
});
