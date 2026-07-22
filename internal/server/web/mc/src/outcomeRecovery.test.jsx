import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { OutcomeCheckReceipt, OutcomeRecoveryReceipt } from "./screens.jsx";

describe("outcome recovery receipts", () => {
  test("renders the exact safe check deadline", () => {
    const html = renderToStaticMarkup(<OutcomeCheckReceipt check={{
      name: "linux-candidate-delivery",
      blocking: true,
      status: "fail",
      deadline_at: "2026-07-18T13:35:00Z",
    }} />);

    expect(html).toContain("fail · deadline 2026-07-18 13:35:00Z");
  });

  test("renders safe recovery status, attempt times, and exit code", () => {
    const now = Date.parse("2026-07-18T14:00:00Z");
    const html = renderToStaticMarkup(<OutcomeRecoveryReceipt now={now} recovery={{
      status: "failed",
      summary: "outcome recovery command failed; retry held until cooldown",
      started_at: "2026-07-18T13:55:00Z",
      next_eligible_at: "2026-07-18T14:15:00Z",
      exit_code: 7,
    }} />);

    expect(html).toContain("Recovery");
    expect(html).toContain("failed");
    expect(html).toContain("Last attempt");
    expect(html).toContain("5m ago");
    expect(html).toContain("Retry eligible");
    expect(html).toContain("future");
    expect(html).toContain("Exit code");
    expect(html).toContain(">7<");
    expect(html).not.toContain("recovery_command");
  });
});
