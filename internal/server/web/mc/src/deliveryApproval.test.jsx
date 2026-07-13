import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  actionLabel,
  approvalCTA,
  approvalReasonPlaceholder,
  approvalRejectLabel,
  mapDeliveryApproval,
} from "./fleetApi.js";
import { DeliveryApprovalDetails } from "./screens.jsx";

const mergedSHA = "0123456789abcdef0123456789abcdef01234567";

function delivery(overrides = {}) {
  return {
    merged_sha: mergedSHA,
    merged_at: "2026-07-13T08:59:00Z",
    approval_generation: 0,
    target_label: "production kiosk",
    verification_label: "service healthy",
    rollback_label: "previous release",
    timeout_minutes: 10,
    expires_at: "2026-07-14T09:00:00Z",
    config_digest: "sha256:config",
    ...overrides,
  };
}

describe("deploy_project approval labels", () => {
  test("name the delivery effect and refusal explicitly", () => {
    expect(actionLabel("deploy_project")).toBe("Deploy project");
    expect(approvalCTA("deploy_project")).toBe("Deploy pinned revision");
    expect(approvalRejectLabel("deploy_project")).toBe("Don't deploy");
    expect(approvalReasonPlaceholder("deploy_project", "approve")).toContain("pinned revision");
    expect(approvalReasonPlaceholder("deploy_project", "reject")).toContain("hold this revision");
  });
});

describe("mapDeliveryApproval", () => {
  test("allow-lists structured fields and drops all raw text surfaces", () => {
    const mapped = mapDeliveryApproval({
      ...delivery(),
      command: "RAW_SECRET_COMMAND",
      raw_command: "ANOTHER_RAW_SECRET",
      command_preview: "SECRET_PREVIEW",
      output: "SECRET_OUTPUT",
      exit_error: "SECRET_ERROR",
      target: "SECRET_TARGET",
      rollback: "SECRET_ROLLBACK",
      unknown: "not-for-the-ui",
    });

    expect(mapped.merged_sha).toBe(mergedSHA);
    expect(mapped.target_label).toBe("production kiosk");
    expect(Object.hasOwn(mapped, "command")).toBe(false);
    expect(Object.hasOwn(mapped, "raw_command")).toBe(false);
    expect(Object.hasOwn(mapped, "command_preview")).toBe(false);
    expect(Object.hasOwn(mapped, "output")).toBe(false);
    expect(Object.hasOwn(mapped, "exit_error")).toBe(false);
    expect(Object.hasOwn(mapped, "target")).toBe(false);
    expect(Object.hasOwn(mapped, "rollback")).toBe(false);
    expect(Object.hasOwn(mapped, "unknown")).toBe(false);
  });
});

describe("DeliveryApprovalDetails", () => {
  test("renders immutable approval context and explicit safe labels", () => {
    const html = renderToStaticMarkup(
      <DeliveryApprovalDetails approval={{
        action: "deploy_project",
        status: "pending",
        delivery: {
          ...delivery(),
          command: "RAW_SECRET_COMMAND",
          raw_command: "ANOTHER_RAW_SECRET",
        },
      }} />,
    );

    for (const expected of [
      "Merged revision", mergedSHA, "Merged at", "2026-07-13T08:59:00Z", "Approval expires", "2026-07-14T09:00:00Z",
      "Target label", "production kiosk", "Verification label", "service healthy",
      "Rollback label", "previous release", "Approved config digest", "sha256:config",
    ]) {
      expect(html).toContain(expected);
    }
    expect(html).not.toContain("RAW_SECRET_COMMAND");
    expect(html).not.toContain("ANOTHER_RAW_SECRET");
  });

  test("shows a non-replay recovery alert for an interrupted execution", () => {
    const html = renderToStaticMarkup(
      <DeliveryApprovalDetails approval={{
        action: "deploy_project",
        status: "executing",
        delivery: delivery({
          started_at: "2026-07-13T09:01:00Z",
          executed_revision: mergedSHA,
          failure_stage: "deploy",
          deploy_exit_code: 7,
        }),
      }} />,
    );

    expect(html).toContain('role="alert"');
    expect(html).toContain("Recovery required");
    expect(html).toContain("will not replay it automatically");
    expect(html).toContain("Executed revision");
    expect(html).toContain("Failure stage");
    expect(html).toContain("Deploy exit code");
  });

  test("renders terminal verification metadata without output", () => {
    const html = renderToStaticMarkup(
      <DeliveryApprovalDetails approval={{
        action: "deploy_project",
        status: "executed",
        delivery: delivery({
          started_at: "2026-07-13T09:01:00Z",
          finished_at: "2026-07-13T09:02:00Z",
          executed_revision: mergedSHA,
          verified: true,
          deploy_exit_code: 0,
          verify_exit_code: 0,
        }),
      }} />,
    );

    for (const expected of [
      "Execution result", "verified", "Executed revision", "Started", "Finished",
      "Deploy exit code", "Verifier exit code",
    ]) {
      expect(html).toContain(expected);
    }
  });
});
