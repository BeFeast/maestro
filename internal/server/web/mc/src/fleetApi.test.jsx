import { describe, expect, test } from "bun:test";
import {
  aggregateBackendQuota,
  aggregateProviderModelHealth,
  formatBackendQuotaSentence,
  formatProviderModelHealthSentence,
  mapFleetResponse,
  mapTmpfsHygiene,
  supervisorDecisionsFromProject,
  workerSessionsFromFleet,
  workerNextAction,
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

  test("closed terminal session stays done despite historical failure tokens", () => {
    const taxonomy = workerStatusTaxonomy({
      status: "done",
      issue_closed_at: "2026-07-15T11:30:00Z",
      display_status: "token_budget_exceeded",
      worker_outcome: "token_budget_exceeded",
      needs_attention: true,
    });

    expect(taxonomy).toEqual({
      label: "done",
      tone: "ok",
      section: "done",
    });
  });

  test("ordinary done session keeps a historical token-budget failure visible", () => {
    const taxonomy = workerStatusTaxonomy({
      status: "done",
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

  test("issue-guard retry hold is visible recent work, not a stuck worker", () => {
    const taxonomy = workerStatusTaxonomy({
      status: "dead",
      display_status: "waiting_for_issue_guard",
      live: true,
      needs_attention: false,
    });

    expect(taxonomy).toEqual({
      label: "waiting for issue guard",
      tone: "policy",
      section: "recent",
    });

    expect(workerNextAction({
      status: "dead",
      display_status: "waiting_for_issue_guard",
      issue_url: "https://github.com/BeFeast/ok-player/issues/406",
      next_action: "Maestro will resume the same canonical session.",
    })).toEqual({
      text: "Maestro will resume the same canonical session.",
      buttons: [{ label: "Open issue →", href: "https://github.com/BeFeast/ok-player/issues/406" }],
    });
  });

  test("model overload is distinct from model access and credential cooldown", () => {
    expect(workerStatusTaxonomy({
      status: "dead",
      display_status: "backend_model_overloaded",
      needs_attention: true,
    })).toEqual({
      label: "model overloaded",
      tone: "watch",
      section: "stuck",
    });

    expect(workerNextAction({
      status: "dead",
      display_status: "backend_model_overloaded",
      provider_limit_provider: "claude",
      provider_limit_model: "claude-fable-5",
    })).toEqual({
      text: "claude/claude-fable-5 is temporarily overloaded. Maestro kept the retry budget and can use another model on the same provider.",
      buttons: [{ label: "Open backend health →", action: "openBackendHealth" }],
    });
  });
});

describe("tmpfs hygiene pressure", () => {
  test("maps the exact pressure attention code and counts host attention", () => {
    const raw = {
      summary: {},
      tmpfs_hygiene: {
        timestamp: "2026-07-21T18:00:00Z",
        mode: "apply",
        tmpfs: true,
        use_pct: 87,
        pressure: true,
        attention_code: "tmpfs_pressure",
        freed_bytes: 8192,
      },
    };
    const mapped = mapTmpfsHygiene(raw.tmpfs_hygiene);
    expect(mapped.attentionCode).toBe("tmpfs_pressure");
    expect(mapped.usePct).toBe(87);
    expect(mapFleetResponse(raw, now).attentionCount).toBe(1);
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

describe("supervisor recommendation episodes", () => {
  test("keeps latest recommendation first/last/count/disposition metadata", () => {
    const decisions = supervisorDecisionsFromProject({
      needsAttention: 0,
      operatorState: {},
      supervisor: {
        latest: {
          created_at: "2026-07-15T10:00:00Z",
          first_seen_at: "2026-07-15T10:00:00Z",
          last_seen_at: "2026-07-15T12:00:00Z",
          seen_count: 25,
          recommendation_id: "supervisor:held",
          recommended_action: "monitor_open_pr",
          summary: "Waiting on the current dispatch guard.",
          disposition: { status: "dropped", reason: "ttl_expired_unconsumed" },
        },
      },
    }, now);

    expect(decisions).toHaveLength(1);
    expect(decisions[0]).toMatchObject({
      t: now,
      firstSeen: Date.parse("2026-07-15T10:00:00Z"),
      lastSeen: now,
      seenCount: 25,
      recommendationId: "supervisor:held",
      disposition: { status: "dropped", reason: "ttl_expired_unconsumed" },
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
