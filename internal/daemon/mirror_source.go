package daemon

import (
	"fmt"

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
	if cfg := getCfg(); cfg != nil {
		horizon = cfg.GitHubMirror.StaleHorizon()
	}
	return mirrorstore.NewSource(github.New(repo), d.mirror, repo, mirrorstore.SourceOptions{
		Horizon: horizon,
		APIDirect: func() bool {
			cfg := getCfg()
			return cfg == nil || !cfg.GitHubMirror.MirrorFirst()
		},
	})
}

// mirrorReadDigestLine is the fragment SetMirrorReadDigest appends to the gh
// wrapper's hourly REST-usage journal line so an operator reading journalctl
// sees, alongside API consumption, how many supervisor/orchestrator reads the
// mirror served versus how many fell back to the API (#826 AC 5). Empty until
// the mirror has served or turned away a read, so the pre-#826 line is unchanged
// on a fleet that has not enabled mirror-first.
func mirrorReadDigestLine() string {
	st := mirrorstore.ReadStatsSnapshot()
	if st.Total() == 0 {
		return ""
	}
	return fmt.Sprintf("; mirror reads: %d served locally, %d fell back to API (%.0f%% hit)",
		st.MirrorHits, st.APIFallbacks, st.HitRate()*100)
}
