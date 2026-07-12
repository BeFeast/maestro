import { describe, expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { managementHomeView } from "./managementHome.js";
import { ManagementHomePanel } from "./screens.jsx";

describe("ManagementHomePanel", () => {
  test("renders the vault-relative label, exact selectable path, encoded action, and honest fallback", () => {
    const home = managementHomeView({
      kind: "obsidian",
      path: "/home/god/My Vault/Dev/Áreas/café notes",
      vault: "My Vault",
      vault_path: "Dev/Áreas/café notes",
    });

    const html = renderToStaticMarkup(
      <ManagementHomePanel home={home} projectId="3f2504e0-4f89-41d3-9a0c-0305e82c3301" />,
    );

    expect(html).toContain("Dev/Áreas/café notes");
    expect(html).toContain("/home/god/My Vault/Dev/Áreas/café notes");
    expect(html).toContain("Copy Path");
    expect(html).toContain("vault=My%20Vault&amp;file=Dev%2F%C3%81reas%2Fcaf%C3%A9%20notes");
    expect(html).toContain("Requires Obsidian with its local protocol handler");
    expect(html).toContain("selectable path above remains the fallback");
  });

  test("renders nothing when Management Home metadata is absent", () => {
    expect(renderToStaticMarkup(<ManagementHomePanel home={null} projectId="" />)).toBe("");
  });
});
