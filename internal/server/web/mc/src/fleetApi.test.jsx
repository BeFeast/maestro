import { describe, expect, test } from "bun:test";
import {
  mapFleetResponse,
  workerSessionsFromFleet,
  workerStatusTaxonomy,
} from "./fleetApi.js";

const now = Date.parse("2026-07-15T12:00:00Z");

function worker(slot, fields = {}) {
  return {
    project_name: "maestro",
    slot,
    issue_number: 901,
    issue_title: slot,
    status: "done",
    started_at: "2026-07-15T11:00:00Z",
    ...fields,
  };
}

describe("Fleet workers ordering contract", () => {
  test("preserves API order and counts only alive running workers as in flight", () => {
    const raw = {
      summary: { running: 1 },
      workers: [
        worker("run-1", {
          status: "running",
          alive: true,
          live: true,
          started_at: "2026-07-15T11:59:00Z",
        }),
        worker("review-recheck", {
          status: "pr_open",
          display_status: "review_retry_recheck",
          live: true,
          pr_number: 12,
        }),
        worker("stale-running", {
          status: "running",
          alive: false,
          live: true,
          needs_attention: true,
          status_reason: "State says running, but the worker PID is not alive.",
        }),
        worker("code-landed", {
          status: "code_landed",
          live: true,
          needs_attention: true,
          pr_number: 13,
        }),
      ],
    };

    const fleet = mapFleetResponse(raw, now);
    expect(fleet.workers.map(w => w.slot)).toEqual([
      "run-1",
      "review-recheck",
      "stale-running",
      "code-landed",
    ]);

    const groups = workerSessionsFromFleet(fleet, now);
    expect(groups.running.map(w => w.slot)).toEqual(["run-1"]);
    expect(groups.runningCount).toBe(fleet.workerCount);
    expect(groups.recent.map(w => w.slot)).toEqual(["review-recheck"]);
    expect(groups.stuck.map(w => w.slot)).toEqual(["stale-running", "code-landed"]);
  });

  test("running with alive=false is needs-attention, not running", () => {
    const taxonomy = workerStatusTaxonomy({
      status: "running",
      alive: false,
      needs_attention: true,
    });

    expect(taxonomy).toEqual({
      label: "running",
      tone: "stuck",
      section: "stuck",
    });
  });
});
