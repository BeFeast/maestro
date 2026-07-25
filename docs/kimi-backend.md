# Kimi Code CLI backend

Maestro treats `kimi` as a first-class worker backend. A backend named `kimi`,
one declared with `provider: moonshot` (or `provider: kimi`), or one whose
command basename is `kimi` resolves to the Kimi command builder.

```yaml
model:
  default: moonshot-primary
  backends:
    moonshot-primary:
      provider: moonshot
      cmd: kimi
```

Add the normal backend `pricing` block when USD estimates are required; without
rates, Fleet reports tokens only.

## Worker contract

The worker runs Kimi in non-interactive print mode with JSONL output:

```text
kimi --print --output-format=stream-json
```

Maestro supplies the prompt on stdin. Kimi print mode enables its unattended
approval behavior, so no separate approval flag is required. An explicit
operator `--output-format` in `cmd` or `extra_args` wins; choosing text output
disables the JSONL side channel and structured usage accounting.

When Kimi emits its native usage object, Maestro records uncached input,
output, cache-read, and cache-creation tokens, advances the respawn-safe usage
watermark, and applies configured split pricing in history and Fleet cost
observability. Current Kimi releases do not guarantee a usage object on every
print-mode response. Missing usage therefore remains unavailable rather than
being treated as zero-token progress.

`worker_max_tokens` is not supported for the Kimi kind. Maestro rejects a
positive live budget at worker startup because Kimi stream-json does not
guarantee response-by-response usage, so it cannot safely stop the process at
the configured ceiling.

## Authentication and pooled proxy routing

Two authentication arrangements are supported:

- Native Moonshot/Kimi: run `kimi login` once as the same operating-system user
  that launches Maestro workers. Kimi keeps its authentication in its own
  user-level configuration; do not copy that material into project YAML.
- CLIProxyAPI: configure a custom Kimi provider/model in Kimi's user-level
  config, using its `openai_legacy` provider type and the proxy
  endpoint. Keep the provider credential in Kimi's user config or Maestro's
  private worker credential boundary, never in the repository. Routing Kimi
  through the shared proxy keeps requests on the pooled credential/quota path.

Verify the selected setup without dumping configuration or environment values:

```sh
kimi --version
printf 'Reply with OK.\n' | kimi --print --output-format=stream-json
```
