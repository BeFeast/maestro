# Install Testing Report

Testing the maestro installation and onboarding flow (Issue #85).

## Test Environment

Fresh machine (not .14 or .22), testing the full install -> init -> run flow.

## install.sh

### What works
- OS/arch detection (linux/darwin, amd64/arm64) works correctly
- Downloads correct tarball from GitHub Releases
- Binary naming matches release workflow (`maestro-{os}-{arch}.tar.gz` containing `maestro-{os}-{arch}`)
- `sudo` is only used when `INSTALL_DIR` is not writable
- Custom `INSTALL_DIR` works via environment variable
- Cleanup via `trap` on exit

### Friction points found
1. **No checksum verification** — `checksums.txt` is generated in the release workflow but `install.sh` never downloads or verifies it. A compromised release asset would be installed without detection.
2. **No post-install verification** — Fixed: now runs `maestro version` after install.
3. **No next-steps guidance** — Fixed: now prints next steps (init, run).
4. **Silent failure on missing `curl`/`tar`** — These are assumed present. Rare issue in practice.

## maestro init

### What works
- Interactive prompts with sensible defaults
- Repo format validation (owner/repo)
- Generates valid `maestro.yaml`
- Creates `~/.maestro/` state directory
- Generates platform-appropriate service file (systemd on Linux, launchd on macOS)
- Invalid `max_parallel` falls back to default (3)

### Friction points found
1. **Missing "cline" in backend prompt** — Fixed: prompt now shows `(claude/codex/gemini/cline)`.
2. **No prerequisite checks** — Fixed: init now warns if `git`, `gh`, `tmux`, or AI CLIs are missing.
3. **No model backend validation** — Accepts any string (e.g., "foo"). Should validate against known backends.
4. **Generated config is minimal** — Missing useful keys like `max_runtime_minutes`, `worker_prompt`, `merge_strategy`, `auto_rebase`. New users don't discover these until they read the full README.
5. **No `.gitignore` warning** — `maestro.yaml` is created in the repo dir but contains local paths. Easy to accidentally commit.
6. **systemd unit doesn't set PATH** — AI CLIs installed via npm/bun (in `~/.local/bin` or `~/.bun/bin`) won't be found when the service runs. Needs `Environment=PATH=...` in the unit file.
7. **No worker prompt setup** — Init doesn't ask about or create a worker prompt template. Without one, workers use a bare-bones built-in prompt.

## README

### What works
- Clear structure: prerequisites, installation, quickstart, configuration, troubleshooting
- Good troubleshooting section covering common issues
- Multi-project setup is well documented
- Service file instructions for both Linux and macOS

### Friction points found
1. **No `maestro version` in quickstart** — Fixed: added verification step after install.
2. **No `.gitignore` warning in quickstart** — Fixed: added step to add `maestro.yaml` to `.gitignore`.
3. **`loginctl enable-linger` buried in troubleshooting** — This is required for systemd services to persist across logouts but isn't mentioned in the quickstart or service setup section.
4. **No end-to-end walkthrough** — A new user has to piece together prerequisites, install, init, and service setup from different sections.

## Sub-issues opened

- #103: install.sh — add checksum verification
- #104: init — validate model backend and generate richer config
- #105: init — set PATH in generated systemd unit for AI CLI discovery
