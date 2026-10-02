import { expect, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { mapFleetResponse } from "./fleetApi.js";
import { DispatchBlockersPanel } from "./screens.jsx";
import { projectRepoURL, projectIssueURL, projectPRURL } from "./forgeLinks.js";

for (const forge of ["github", "forgejo"]) {
  test(`${forge} links survive API mapping and queue rendering`, () => {
    const project = mapFleetResponse({ projects: [{
      name: "hedroom", repo: "BeFeast/hedroom", forge,
      forge_base_url: forge === "forgejo" ? "https://git.oklabs.uk/" : "",
      queue_snapshot: { eligible: 1, selected_candidate: { number: 382, title: "Delivery" },
        eligible_ranked: [{ number: 382, title: "Delivery" }] },
    }] }).projects[0];
    const base = forge === "forgejo" ? "https://git.oklabs.uk/BeFeast/hedroom" : "https://github.com/BeFeast/hedroom";
    expect(projectRepoURL(project)).toBe(base);
    expect(projectIssueURL(project, 382)).toBe(`${base}/issues/382`);
    expect(projectPRURL(project, 400)).toBe(`${base}/${forge === "forgejo" ? "pulls" : "pull"}/400`);
    const html = renderToStaticMarkup(<DispatchBlockersPanel project={project} />);
    expect(html).toContain(`href="${base}/issues/382"`);
    if (forge === "forgejo") expect(html).not.toContain("github.com");
  });
}

test("invalid and missing project targets do not create links", () => {
  expect(projectIssueURL({repo:"BeFeast/hedroom"}, 0)).toBe("");
  expect(projectPRURL({repo:"BeFeast/hedroom"}, -1)).toBe("");
  expect(projectRepoURL({repo:"invalid"})).toBe("");
  expect(projectRepoURL({repo:"BeFeast/hedroom",forge:"forgejo"})).toBe("");
});
