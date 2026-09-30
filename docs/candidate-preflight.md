# Offline candidate resource preflight

`maestro candidate-preflight` compares **supplied declarations** for stable and
candidate runtimes. It never loads a live config store, starts a daemon, binds a
port, contacts a forge/provider, reads a usage queue, or runs cleanup. It uses
only inline configuration and filesystem metadata (`stat`/`readlink`). Database
files are never opened as SQLite, initialized, or migrated.

```sh
maestro candidate-preflight --input snapshots.json
# Or supply the same JSON on stdin:
maestro candidate-preflight < snapshots.json
```

The command emits JSON. Exit 0 means `no_declared_overlap`, exit 1 means
`overlap` or `unresolved`, and exit 2 means invalid input/usage. Every report has
`activation_authorized: false`, including exit 0. No detected overlap is only
evidence about the supplied declarations on the current host, at this instant.
It is not proof of active-owner fencing or permission to run a candidate.

## Input v1

The input requires `version: 1`, `stable`, and `candidate`. Each snapshot has an
`options` object and an explicit `projects` array (which can be empty). Project
entries have a unique `name` and an inline `config_yaml` string containing the
project config snapshot. Config parsing is strict; sidecar policy files are not
loaded. Resource values are read from these documents, never from runtime rows.
Do not include credentials in a snapshot. Unknown keys and unsupported identity
forms fail closed. Input is limited to 4 MiB.

Example with separate declared resources:

```json
{
  "version": 1,
  "stable": {
    "options": {
      "store": "/srv/example/stable/maestro.db",
      "approvals_db": "/srv/example/stable/maestro.db",
      "state_db": "/srv/example/stable/maestro.db",
      "webhook_db": "/srv/example/stable/maestro.db",
      "emergency_db": "/srv/example/stable/maestro.db",
      "host": "127.0.0.1",
      "port": 8786
    },
    "projects": [{
      "name": "product",
      "config_yaml": "repo: example/product\nlocal_path: /srv/example/stable/checkout\nstate_dir: /srv/example/stable/state\nworktree_base: /srv/example/stable/worktrees\nworker_runtime:\n  mode: isolated\n  scratch_root: /srv/example/stable/scratch\nsupervisor:\n  temp_dir: /srv/example/stable/supervisor-tmp\nself_deploy:\n  promotion_policy: explicit\n"
    }]
  },
  "candidate": {
    "options": {
      "store": "/srv/example/candidate/maestro.db",
      "approvals_db": "/srv/example/candidate/maestro.db",
      "state_db": "/srv/example/candidate/maestro.db",
      "webhook_db": "/srv/example/candidate/maestro.db",
      "emergency_db": "/srv/example/candidate/maestro.db",
      "host": "127.0.0.1",
      "port": 8787
    },
    "projects": [{
      "name": "fixture",
      "config_yaml": "repo: example/candidate-fixture\nlocal_path: /srv/example/candidate/checkout\nstate_dir: /srv/example/candidate/state\nworktree_base: /srv/example/candidate/worktrees\nworker_runtime:\n  mode: isolated\n  scratch_root: /srv/example/candidate/scratch\nsupervisor:\n  temp_dir: /srv/example/candidate/supervisor-tmp\nself_deploy:\n  promotion_policy: explicit\n"
    }]
  }
}
```

`options` uses the resource flags of `maestro daemon`, with underscores instead
of hyphens. `store` is mandatory: the CLI's default selection includes legacy
database discovery, which this offline command deliberately does not perform.
All other defaults come from the same helper used by the daemon CLI:

| Option | Omitted value |
| --- | --- |
| `host`, `port` | `127.0.0.1`, `8786`; port 0 declares no HTTP endpoint |
| `approvals_store`, `state_store` | `json` |
| `approvals_db`, `state_db`, `webhook_db`, `emergency_db` | Each store package's current default, currently `$HOME/.maestro/maestro.db` |
| `webhook_secret_file` | Empty; webhook DB marked inactive |

Changing `store` does **not** change these independent auxiliary DB defaults.
Approvals DB is marked active even in JSON mode because delivery uses it. State
DB is marked active for SQLite mode. Every configured DB role is reported and
compared, including inactive roles, to expose latent sharing. Self-deploy marker
directory is derived exactly as the daemon does: `<dirname(store)>/self-deploy`.
Sibling stores therefore share it by default.

Project resources include StateDir, checkout, worktree base, isolated worker
scratch, supervisor temp directory, forge repository queue, and promotion policy.
Config defaults are reused for state and scratch paths; a legacy worker runtime
has no isolated scratch declaration and produces `unresolved`. Remote runner
resources are also `unresolved`; this command cannot inspect the remote host.
Empty local/worktree paths require an explicit declaration instead of guessing.

## Comparison and limits

- Plain filesystem paths are resolved against the command's current working
  directory. Existing symlink ancestors and hardlinked files are recognized,
  including declarations whose final path does not exist yet. Missing resources
  are never created. Dangling symlinks, incompatible file types, SQLite URI/query
  forms, and unresolved identities produce `unresolved`.
- Exact path matches and ancestor/descendant mutable directory overlaps are
  reported across stable and candidate, including databases beneath a directory.
- Queues use forge kind, instance scheme/host/effective port/case-sensitive base
  path, and owner/repo. Repository names are compared case-insensitively. Different
  labels, UUIDs and copied databases do not partition the same repository queue.
  DNS/IP aliases are not inferred; use an approved canonical forge identity.
- Literal HTTP IPs and localhost are supported. Wildcard and loopback collisions
  are treated conservatively; other hostnames are unresolved without DNS access.
- Shared process/cgroup/tmux namespaces, host-wide emergency/cleanup effects,
  actual queue ownership, permissions, credentials and side effects of hooks are
  outside this declaration comparison. A copied DB and a distinct port cannot
  establish those boundaries. Offline fixtures must remain offline until active
  ownership and process isolation are implemented and separately verified.

Refs #1195. Automatic promotion fencing is described in
[the self-deploy runbook](self-deploy-runbook.md).
