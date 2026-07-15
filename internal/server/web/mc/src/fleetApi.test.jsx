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

  test("token budget outcome is stuck even if stale state still says running", () => {
    const taxonomy = workerStatusTaxonomy({
      status: "running",
      alive: false,
      display_status: "token_budget_exceeded",
      worker_outcome: "token_budget_exceeded",
      needs_attention: true,
    });

    expect(taxonomy).toEqual({
      label: "token budget exceeded",
      tone: "stuck",
      section: "stuck",
    });
  });
});

describe("Provider route mapping", () => {
  test("preserves provider lanes, flattened route, and selection reason", () => {
    const fleet = mapFleetResponse({
      projects: [{
        name: "maestro",
        effective_config: {
          model_policy: {
            default: "claude",
            provider_lanes: [
              { provider: "anthropic", default: "claude" },
              { provider: "openai", default: "sol", fallback_backends: ["gpt55"] },
            ],
            resolved_route: ["claude", "sol", "gpt55"],
            selection_reason: "provider_lanes",
          },
        },
      }],
    }, now);

    expect(fleet.projects[0].effectiveConfig.modelPolicy).toMatchObject({
      default: "claude",
      resolvedRoute: ["claude", "sol", "gpt55"],
      selectionReason: "provider_lanes",
      providerLanes: [
        { provider: "anthropic", default: "claude", fallbackBackends: [] },
        { provider: "openai", default: "sol", fallbackBackends: ["gpt55"] },
      ],
    });
  });
});
