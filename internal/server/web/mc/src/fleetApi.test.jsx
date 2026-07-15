import { describe, expect, test } from "bun:test";
import {
  aggregateBackendQuota,
  aggregateProviderModelHealth,
  formatBackendQuotaSentence,
  formatProviderModelHealthSentence,
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

describe("provider/model credential health", () => {
  test("surfaces aggregate rotation counts without credential identifiers", () => {
    const projects = [{
      provider_model_health: {
        claude: {
          "claude-fable-5": {
            state: "cooldown",
            reason: "model_cooldown",
            credential_candidates: 2,
            credential_candidates_known: true,
            credential_usable: 0,
            credential_usable_known: true,
            aggregate_reason: "all_model_credentials_cooling_down",
            retry_after: "2026-07-15T12:05:00Z",
          },
        },
      },
    }];

    const rows = aggregateProviderModelHealth(projects, now);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      provider: "claude",
      model: "claude-fable-5",
      credentialCandidates: 2,
      credentialUsable: 0,
    });
    expect(formatProviderModelHealthSentence(rows[0], now)).toContain("0/2 credentials usable");
    expect(JSON.stringify(rows)).not.toContain("credential_file");
  });

  test("quota sentence explains a low-usage credential pool can still reject Fable", () => {
    const modelHealth = aggregateProviderModelHealth([{
      provider_model_health: {
        claude: {
          "claude-fable-5": {
            state: "cooldown",
            reason: "model_cooldown",
            credential_candidates: 2,
            credential_candidates_known: true,
            credential_usable: 0,
            credential_usable_known: true,
          },
        },
      },
    }], now);
    const quota = aggregateBackendQuota([{
      backend_quota: [{
        backend: "claude",
        window_cap_tokens: 1000,
        window_used_tokens: 0,
        window_percent: 0,
        dispatch_threshold: 0.85,
      }],
    }], modelHealth);

    expect(formatBackendQuotaSentence(quota[0], now)).toContain(
      "claude-fable-5 unavailable: 0/2 credentials usable",
    );
  });
});
