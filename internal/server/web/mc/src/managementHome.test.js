import { test, expect, describe } from "bun:test";
import { obsidianUri, managementHomeView, copyText } from "./managementHome.js";

describe("obsidianUri", () => {
  test("encodes vault and vault-relative path independently", () => {
    expect(obsidianUri("god", "Dev/Areas/maestro")).toBe(
      "obsidian://open?vault=god&file=Dev%2FAreas%2Fmaestro",
    );
  });

  test("encodes spaces and non-ASCII characters", () => {
    expect(obsidianUri("My Vault", "Dev/Áreas/café notes")).toBe(
      "obsidian://open?vault=My%20Vault&file=Dev%2F%C3%81reas%2Fcaf%C3%A9%20notes",
    );
  });

  test("returns empty string when vault or path is missing", () => {
    expect(obsidianUri("", "Dev/Areas/maestro")).toBe("");
    expect(obsidianUri("god", "")).toBe("");
    expect(obsidianUri(null, null)).toBe("");
  });

  test("does not derive the URI from the absolute path", () => {
    // A different absolute path must not change the URI: it comes from the
    // structured fields, not from slicing /home/.../Dev/Areas/maestro.
    const uri = obsidianUri("god", "Dev/Areas/maestro");
    expect(uri.includes("/home/")).toBe(false);
  });
});

describe("managementHomeView", () => {
  test("primary label is the vault-relative path, not the absolute path", () => {
    const view = managementHomeView({
      kind: "obsidian",
      path: "/home/god/vault/Dev/Areas/maestro",
      vault: "god",
      vault_path: "Dev/Areas/maestro",
    });
    expect(view.label).toBe("Dev/Areas/maestro");
    expect(view.path).toBe("/home/god/vault/Dev/Areas/maestro");
    expect(view.uri).toBe("obsidian://open?vault=god&file=Dev%2FAreas%2Fmaestro");
  });

  test("returns null for absent metadata (no dead button)", () => {
    expect(managementHomeView(null)).toBe(null);
    expect(managementHomeView(undefined)).toBe(null);
    expect(managementHomeView({})).toBe(null);
    expect(managementHomeView({ kind: "", path: "", vault: "", vault_path: "" })).toBe(null);
  });

  test("omits the URI when vault/vault_path are incomplete but keeps the path", () => {
    const view = managementHomeView({ kind: "obsidian", path: "/home/god/notes" });
    expect(view).not.toBe(null);
    expect(view.uri).toBe("");
    expect(view.label).toBe("/home/god/notes");
  });
});

describe("copyText", () => {
  test("resolves ok on a working clipboard", async () => {
    let written = "";
    const clip = { writeText: async (t) => { written = t; } };
    const res = await copyText("/home/god/vault/Dev/Areas/maestro", clip);
    expect(res.ok).toBe(true);
    expect(written).toBe("/home/god/vault/Dev/Areas/maestro");
  });

  test("reports unavailable when clipboard is missing (degrades honestly)", async () => {
    const res = await copyText("anything", null);
    expect(res.ok).toBe(false);
    expect(res.reason).toBe("unavailable");
  });

  test("reports error when writeText rejects", async () => {
    const clip = { writeText: async () => { throw new Error("denied"); } };
    const res = await copyText("anything", clip);
    expect(res.ok).toBe(false);
    expect(res.reason).toBe("error");
  });
});
