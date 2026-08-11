package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/mirrorstore"
)

// newReadSource builds a mirror-first read source for one flow (#826), or
// returns nil when there is no mirror to read from (webhook ingestion disabled).
// A nil return tells the caller to keep reading GitHub directly, exactly as
// before this phase.
//
// The GitHub client is built from repo (a restart-required field, stable for the
// flow's lifetime). The escape hatch is wired to getCfg so a live config-store
// edit flips the whole flow back to API-direct without a redeploy: apiDirect is
// re-evaluated on every read, and MirrorFirst() is false unless the flow's
// CURRENT config sets github_mirror.source: mirror-first (#826 AC 3/8).
func (d *Daemon) newReadSource(repo string, getCfg func() *config.Config) *mirrorstore.Source {
	if d.mirror == nil {
		return nil
	}
	horizon := d.mirrorHorizon
	var fc config.ForgeConfig
	if cfg := getCfg(); cfg != nil {
		horizon = cfg.GitHubMirror.StaleHorizon()
		fc = cfg.Forge
	}
	// The mirror store is fed by GITHUB webhooks and keyed by bare owner/name —
	// the same key a mirrored repo has on BOTH forges. A forgejo row must never
	// read it: the mirror would silently serve the GitHub mirror's issue/PR
	// state instead of the Forgejo original (#1172 M2). Config validation
	// already rejects mirror-first on forgejo rows; this belt covers the
	// api-direct-but-mirror-open case and returns nil so the flow reads its
	// own forge directly.
	if fc.IsForgejo() {
		return nil
	}
	return mirrorstore.NewSource(github.New(repo, fc), d.mirror, repo, mirrorstore.SourceOptions{
		Horizon: horizon,
		APIDirect: func() bool {
			cfg := getCfg()
			return cfg == nil || !cfg.GitHubMirror.MirrorFirst()
		},
	})
}

// runMirrorReconcile is the per-flow reconciliation loop (#827, epic #811 phase
// E): a low-frequency snapshot of the repo's authoritative GitHub open-issue /
// open-PR sets that repairs mirror drift a missed webhook left behind. It is a
// SAFETY NET over webhook ingestion, so it runs whenever the daemon has an open
// mirror — even with mirror-first reads still off — keeping the read model correct
// and warm for the moment an operator flips the escape hatch.
//
// The cadence is read live from getCfg (the flow's config holder, swapped by the
// reload pump), so a config-store edit to github_mirror.reconcile_seconds retimes
// the loop without a restart. Each pass is cheap on an unchanged repo: the snapshot
// reads flow through the gh wrapper's conditional (ETag) layer and answer 304, and
// the reconciler writes only when it detects a divergence (#827 AC 2).
//
// The loop skips a pass while the shared primary rate-limit gate is armed — the
// snapshot reads would only fail against an empty core budget, and a skipped pass
// is harmless because the next one converges (#812).
func (d *Daemon) runMirrorReconcile(ctx context.Context, name, repo string, getCfg func() *config.Config) {
	if d.mirror == nil {
		return
	}
	var fc config.ForgeConfig
	if cfg := getCfg(); cfg != nil {
		fc = cfg.Forge
	}
	// Never reconcile a forgejo row into the mirror store: the store is
	// GitHub-webhook-defined, and a forgejo-mode client's ported reads would
	// interleave Forgejo-reconciled rows with GitHub webhook rows under the
	// same owner/name key (#1172 M2). Forgejo rows simply have no mirror.
	if fc.IsForgejo() {
		log.Printf("[%s] mirror reconcile skipped — repo=%s is a forgejo row; the mirror store is GitHub-webhook-fed", name, repo)
		return
	}
	rec := mirrorstore.NewReconciler(d.mirror, github.New(repo, fc), repo)
	interval := reconcileInterval(getCfg)
	log.Printf("[%s] mirror reconcile loop started — repo=%s cadence=%s", name, repo, interval)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if paused, _ := github.PrimaryRateLimitPaused(); paused {
			timer.Reset(reconcileInterval(getCfg))
			continue
		}
		res, err := rec.Reconcile(ctx)
		if err != nil {
			log.Printf("[%s] mirror reconcile failed: %v", name, err)
		} else if res.Repaired() > 0 {
			log.Printf("[%s] mirror reconcile repaired %d row(s) (issues=%d prs=%d) — drift healed without operator action",
				name, res.Repaired(), res.IssuesRepaired, res.PRsRepaired)
		}
		timer.Reset(reconcileInterval(getCfg))
	}
}

// reconcileInterval reads the flow's current reconciliation cadence, falling back
// to the package default when the config is momentarily unavailable.
func reconcileInterval(getCfg func() *config.Config) time.Duration {
	if cfg := getCfg(); cfg != nil {
		return cfg.GitHubMirror.ReconcileInterval()
	}
	return config.DefaultMirrorReconcileInterval
}

// mirrorReadDigestLine is the fragment SetMirrorReadDigest appends to the gh
// wrapper's hourly REST-usage journal line so an operator reading journalctl
// sees, alongside API consumption, how many supervisor/orchestrator reads the
// mirror served versus how many fell back to the API (#826 AC 5), plus the
// reconciliation loop's pass and drift-repair totals (#827). Empty until the
// mirror has served/turned away a read or run a reconcile pass, so the pre-#826
// line is unchanged on a fleet that has enabled neither.
func mirrorReadDigestLine() string {
	var parts []string
	if st := mirrorstore.ReadStatsSnapshot(); st.Total() > 0 {
		parts = append(parts, fmt.Sprintf("mirror reads: %d served locally, %d fell back to API (%.0f%% hit)",
			st.MirrorHits, st.APIFallbacks, st.HitRate()*100))
	}
	if rt := mirrorstore.ReconcileTotalsSnapshot(); rt.Runs > 0 {
		last := "never"
		if !rt.LastSuccessAt.IsZero() {
			last = rt.LastSuccessAt.UTC().Format(time.RFC3339)
		}
		parts = append(parts, fmt.Sprintf("mirror reconcile: %d pass(es), %d drift repair(s), last %s",
			rt.Runs, rt.Repairs, last))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}
