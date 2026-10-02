# Model catalog and configured backend definitions

Settings separates a project's referenced backend definitions from retained
definitions with no references in that project's config. The latter stay in a
collapsed group: other projects, issue `model:*` labels and historical sessions
can still refer to them. Selection enabled/disabled is shown independently.

Rows show the configured command's executable basename, explicit model argument
when present, and provider metadata. A command/model metadata disagreement is
visible rather than relabeled as a different model. No raw command arguments,
credentials, or host paths are returned by this display projection.

The optional AI Hub catalog is read from
`$XDG_CONFIG_HOME/ai-hub-ops/catalog.yml` (normally
`~/.config/ai-hub-ops/catalog.yml`); `MAESTRO_MODEL_CATALOG` can select another
file. The reader accepts a bounded regular file up to 1 MiB, version 1, and
projects only model IDs, upstream providers, tiers and file modification time.
Explicit `direct/*` routes are omitted from the proxy catalog view. Missing,
malformed or unreadable files produce **Catalog unverified**, not an obsolete
or unavailable-model assertion.

Catalog membership does not prove gateway availability, account access, or
request admission. Reading the catalog does not query a provider, change any
backend definition, substitute a model, or modify the normalized execution
policy digest. Existing default, fallback and pilot model routes remain exact.

## Updating an existing backend

A catalog display refresh and a backend migration are separate operations.
Before changing a shared definition, inspect every project reference and open
issue `model:<backend>` label, preserve historical names, and compare both its
command model and model metadata. Retiring an unused row is optional; lack of
config references alone does not authorize deletion.

Every project currently loads all shared backend definitions. Changing even an
unused definition changes `AIExecutionConfigDigest` for strict native projects.
Apply a backend migration only with the corresponding manifests/acceptance
revision and worker-continuity plan. Do not silently switch a pinned pilot or
interpret catalog membership as authorization for a paid fallback.
