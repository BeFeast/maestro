import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { mapAdvisor, mapFleetResponse } from "./fleetApi.js";
import { AdvisorReviewSection } from "./screens.jsx";

describe("Advisor plan gate", () => {
  test("maps durable Advisor state and exact findings", () => {
    const raw = {
      phase: "advisor",
      plan_version: 2,
      advisor_review_round: 2,
      advisor_max_review_rounds: 2,
      advisor_backend: "advisor",
      advisor_model: "review-model",
      advisor_verdict: "PLAN_REVISE",
      advisor_unresolved_findings: "Missing timeout coverage.\nValidation is ambiguous.",
      advisor_terminal_reason: "review_rounds_exhausted",
      advisor_reviews: [{
        plan_version: 2,
        review_round: 2,
        backend: "advisor",
        model: "review-model",
        verdict: "PLAN_REVISE",
        findings: "Missing timeout coverage.\nValidation is ambiguous.",
        terminal_reason: "review_rounds_exhausted",
        reviewed_at: "2026-07-21T12:00:00Z",
      }],
    };
    const advisor = mapAdvisor(raw);
    expect(advisor.planVersion).toBe(2);
    expect(advisor.reviewRound).toBe(2);
    expect(advisor.unresolvedFindings).toContain("Validation is ambiguous.");
    expect(advisor.reviews).toHaveLength(1);

    const html = renderToStaticMarkup(<AdvisorReviewSection advisor={advisor} />);
    expect(html).toContain("Advisor plan gate");
    expect(html).toContain("PLAN_REVISE");
    expect(html).toContain("review_rounds_exhausted");
    expect(html).toContain("Missing timeout coverage.");
    expect(html).toContain("failed closed");
  });

  test("maps effective Advisor rollout controls", () => {
    const fleet = mapFleetResponse({
      summary: {},
      projects: [{
        name: "maestro",
        effective_config: {
          pipeline: {
            advisor: { enabled: true, backend: "advisor", effort: "high" },
            advisor_review_rounds: 4,
            advisor_best_effort: true,
          },
        },
      }],
    });
    expect(fleet.projects[0].effectiveConfig.pipeline.advisor.enabled).toBe(true);
    expect(fleet.projects[0].effectiveConfig.pipeline.advisorReviewRounds).toBe(4);
    expect(fleet.projects[0].effectiveConfig.pipeline.advisorBestEffort).toBe(true);
  });
});
