package main

import (
	"fmt"
	"io"
	"os"
)

// The interactive `maestro init` wizard is retired (#871). It used to scaffold a
// per-project maestro.yaml plus a systemd/launchd unit running `maestro run`,
// which is incompatible with the current single-daemon/config-store topology:
// the fleet is one long-lived `maestro daemon --watch-store` reading a shared
// SQLite config store, not one service per project. Silently generating the old
// per-project topology would create units that fight the daemon, so `init` now
// writes nothing and redirects operators to the `project plan`/`project apply`
// genesis flow.

func initCmd(args []string) {
	runInitRedirect(os.Stdout)
	// Exit non-zero so no bootstrap script mistakes this no-op for a completed
	// project setup — init used to create files, and it no longer does.
	os.Exit(1)
}

// runInitRedirect prints the deprecation notice and the exact commands that
// replace the retired wizard. It writes no files and creates no services.
func runInitRedirect(w io.Writer) {
	fmt.Fprintln(w, "maestro init is retired.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "It used to scaffold a per-project maestro.yaml and a systemd/launchd service")
	fmt.Fprintln(w, "that ran the orchestrator per project. That per-project topology is gone: the")
	fmt.Fprintln(w, "fleet is now a single long-lived `maestro daemon --watch-store` reading one")
	fmt.Fprintln(w, "SQLite config store, and each project is one row in that store — no per-project")
	fmt.Fprintln(w, "service.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Set up a project with the genesis flow instead (write a portable project YAML")
	fmt.Fprintln(w, "with repo, local_path, worktree_base, and a stable project_id, then):")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  # 1. Preview the effect on the store (zero writes):")
	fmt.Fprintln(w, "  maestro project plan  --file <portable-project.yaml> --db <store> --json")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  # 2. Apply it (idempotent; --confirm is the exact project_id from the plan):")
	fmt.Fprintln(w, "  maestro project apply --file <portable-project.yaml> --db <store> --confirm <project-id> --json")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The single `maestro daemon --watch-store` then observes the new row and starts")
	fmt.Fprintln(w, "one flow automatically — no per-project service and no separate loop to launch.")
	fmt.Fprintln(w, "See docs/project-setup-runbook.md for the full config-store topology.")
}
