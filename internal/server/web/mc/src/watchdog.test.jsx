import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  mapFleetResponse,
  nextDecisionCountdown,
  pulseFreshnessTone,
} from "./fleetApi.js";
import { WatchdogPanel } from "./screens.jsx";

const now = Date.parse("2026-07-13T10:00:00Z");

function watchdogFleet() {
  return {
    refreshed_at: "2026-07-13T10:00:00Z",
    verdict: { tone: "healthy" },
    summary: {},
    projects: [{
      name: "tx10-clock",
      repo: "BeFeast/tx10-clock",
      supervisor_pulse: {
        last_run_once_at: "2026-07-13T10:00:00Z",
        poll_interval_seconds: 120,
        orchestrator_interval_seconds: 120,
        supervisor_interval_seconds: 300,
        watchdog_eval_interval_seconds: 60,
        stalled_progress_watchdog: {
          enabled: true,
          mode: "multi-signal-progress-v1",
          contract_pending: true,
          evaluation_interval_seconds: 60,
          silence_budget_seconds: 1200,
          active_target_count: 1,
          next_deadline_at: "2026-07-13T10:10:00Z",
          last_recommendation: {
            action: "surface_gate_repair",
            phase: "pr_gate",
            at: "2026-07-13T09:59:00Z",
            reason: "PR gate overdue",
          },
        },
      },
    }],
  };
}

describe("stalled-progress Fleet contract", () => {
  test("uses actual supervisor cadence and preserves all three clocks", () => {
    const fleet = mapFleetResponse(watchdogFleet(), now);
    expect(fleet.supervisorPulse.orchestratorIntervalSeconds).toBe(120);
    expect(fleet.supervisorPulse.supervisorIntervalSeconds).toBe(300);
    expect(fleet.supervisorPulse.watchdogEvalIntervalSeconds).toBe(60);
    expect(nextDecisionCountdown(fleet.supervisorPulse, now)).toBe(300);
    expect(pulseFreshnessTone(fleet.supervisorPulse, now + 130_000)).toBe("ok");

    const project = fleet.projects[0];
    expect(project.cadences).toEqual({
      orchestratorSeconds: 120,
      supervisorSeconds: 300,
      watchdogSeconds: 60,
    });
  });

  test("renders pending contract, deadline, and recommendation without inventing recovery", () => {
    const project = mapFleetResponse(watchdogFleet(), now).projects[0];
    expect(project.stalledProgressWatchdog.contract).toBe("");
    expect(project.stalledProgressWatchdog.contractPending).toBe(true);
    expect(project.stalledProgressWatchdog.lastRecommendation.action).toBe("surface_gate_repair");
    expect(project.stalledProgressWatchdog.lastRecovery).toBe(null);

    const html = renderToStaticMarkup(
      <WatchdogPanel
        watchdog={project.stalledProgressWatchdog}
        cadences={project.cadences}
        now={now}
      />,
    );
    expect(html).toContain("contract pending");
    expect(html).toContain("pending actuator/live-canary proof");
    expect(html).toContain("Orchestrator cadence");
    expect(html).toContain("Supervisor cadence");
    expect(html).toContain("Watchdog cadence");
    expect(html).toContain("surface gate repair");
    expect(html).toContain("none recorded");
    expect(html).toContain("in 10m");
  });

  test("renders incomplete evidence as recovery-suppressed", () => {
	const raw = watchdogFleet();
	const watchdog = raw.projects[0].supervisor_pulse.stalled_progress_watchdog;
	watchdog.observation_incomplete = true;
	watchdog.unavailable_signals = ["terminal_checkpoint", "worktree_git"];
	watchdog.last_decision = {
	  action: "evidence_unavailable",
	  observation_incomplete: true,
	  unavailable_signals: watchdog.unavailable_signals,
	};
	const project = mapFleetResponse(raw, now).projects[0];
	const html = renderToStaticMarkup(
	  <WatchdogPanel watchdog={project.stalledProgressWatchdog} cadences={project.cadences} now={now} />,
	);
	expect(project.stalledProgressWatchdog.observationIncomplete).toBe(true);
	expect(project.stalledProgressWatchdog.lastDecision.action).toBe("evidence_unavailable");
	expect(html).toContain("incomplete (terminal_checkpoint, worktree_git) · recovery suppressed");
  });
});
