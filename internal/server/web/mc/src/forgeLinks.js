export function projectRepoURL(project) {
  const repo = String(project?.repo || "").trim();
  if (!/^[^\s/]+\/[^\s/]+$/.test(repo)) return "";
  const base = project?.forge === "forgejo" ? String(project.forgeBaseURL || "").replace(/\/+$/, "") : "https://github.com";
  return base ? `${base}/${repo}` : "";
}

export function projectIssueURL(project, number) {
  const base = projectRepoURL(project);
  return base && Number.isInteger(number) && number > 0 ? `${base}/issues/${number}` : "";
}

export function projectPRURL(project, number) {
  const base = projectRepoURL(project);
  return base && Number.isInteger(number) && number > 0 ? `${base}/${project?.forge === "forgejo" ? "pulls" : "pull"}/${number}` : "";
}
