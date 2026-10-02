package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

var (
	reconcileNativeConsultation = supervisor.ReconcileNativeConsultation
	reconcileAbandonedRoot      = supervisor.ReconcileAbandonedRoot
	// auxiliaryReconcileRetryBackoff is the single wait before the one retry of
	// a reconcile that lost the consultation store lock to a live cycle.
	auxiliaryReconcileRetryBackoff = 2 * time.Second
)

// AuxiliaryRootReport is the reconcile outcome for one auxiliary receipt root.
type AuxiliaryRootReport struct {
	// ProjectDir is the indexed project state dir Root belongs to.
	ProjectDir string
	Root       string
	Role       string
	// Result is "released" (an abandoned consultation was sealed and its
	// capacity released), "sealed" (an abandoned pre-launch receipt was closed;
	// it held no capacity), "clear" (no durable occupancy remains) or "held"
	// (occupancy remains; see Hold).
	Result string
	Hold   string
	// Occupants is the durable occupancy still recorded under Root.
	Occupants []supervisor.AuxiliaryOccupant
}

func (r AuxiliaryRootReport) String() string {
	s := fmt.Sprintf("root=%s role=%s result=%s", r.Root, r.Role, r.Result)
	if r.Hold != "" {
		s += " hold=" + r.Hold
	}
	if len(r.Occupants) > 0 {
		s += " occupancy=" + describeAuxiliaryOccupancy(r.Occupants, nil, time.Now())
	}
	return s
}

// auxiliaryRootsOf lists the receipt roots under one indexed project state
// dir that have ever run a consultation: the supervisor root and the native
// review scope. Roots without receipts are skipped so the store lock never
// creates receipt directories for them.
func auxiliaryRootsOf(projectDir string) []struct{ dir, role string } {
	var roots []struct{ dir, role string }
	for _, root := range []struct{ dir, role string }{{projectDir, "supervisor"}, {filepath.Join(projectDir, "native-reviews"), "reviewer"}} {
		if auxiliaryRootHasConsultations(root.dir) {
			roots = append(roots, root)
		}
	}
	return roots
}

func auxiliaryRootHasConsultations(dir string) bool {
	for _, name := range []string{"launch.json", "current.json"} {
		if _, err := os.Lstat(filepath.Join(dir, "supervisor-consultations", name)); err == nil {
			return true
		}
	}
	return false
}

// reconcileAuxiliaryRoot runs the supported replay path once over root. With a
// configured project (cfg != nil) it replays under that project's own config,
// as a supervise cycle would; otherwise the config is derived from the receipt
// (removed project, #1232). Nothing is launched: a fresh identity only replays
// the prior unresolved role. A reconcile that lost the store lock to a live
// cycle (consultation_in_progress) is retried once after a short backoff.
func reconcileAuxiliaryRoot(ctx context.Context, cfg *config.Config, projectDir, root, role string, authorities []config.NativeSessionRegistrationConfig, limiter aiexecution.AuxiliaryLimiter) AuxiliaryRootReport {
	report := AuxiliaryRootReport{ProjectDir: filepath.Clean(projectDir), Root: root, Role: role}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if cfg != nil {
			local := *cfg
			local.StateDir = root
			identity := supervisor.ConsultationIdentity{ID: uuid.NewString(), ProjectID: cfg.ProjectID, Role: role}
			_, err = reconcileNativeConsultation(&local, identity, "")
		} else {
			_, err = reconcileAbandonedRoot(root, authorities, limiter)
		}
		var hold *supervisor.ConsultationHold
		if !errors.As(err, &hold) || hold.Code != "consultation_in_progress" || attempt > 0 {
			break
		}
		select {
		case <-ctx.Done():
			attempt = 2
		case <-time.After(auxiliaryReconcileRetryBackoff):
		}
	}
	var hold *aiexecution.Hold
	if errors.As(err, &hold) && hold.Code == "native_prior_outcome_reconciled" {
		report.Result = "released"
		err = nil
	}
	if errors.As(err, &hold) && hold.Code == "native_prelaunch_receipt_sealed" {
		report.Result = "sealed"
		err = nil
	}
	occupants, occErr := supervisor.PendingAuxiliaryOccupancy(projectDir)
	if occErr != nil {
		report.Result, report.Hold = "held", occErr.Error()
		return report
	}
	for _, o := range occupants {
		if o.Root == filepath.Clean(root) {
			report.Occupants = append(report.Occupants, o)
		}
	}
	if report.Result == "released" || report.Result == "sealed" {
		return report
	}
	if len(report.Occupants) == 0 {
		report.Result = "clear"
		return report
	}
	report.Result = "held"
	switch {
	case err == nil:
		report.Hold = "occupancy_retained"
	case errors.As(err, &hold):
		report.Hold = hold.Code
	default:
		var chold *supervisor.ConsultationHold
		if errors.As(err, &chold) {
			report.Hold = chold.Code
		} else {
			report.Hold = err.Error()
		}
	}
	return report
}

// reconcileAbandonedAuxiliaryConsultations runs the replay path over the
// project's supervisor and native-review receipt roots once, when the flow is
// registered (daemon start and hot add), synchronously before the flow's first
// supervise cycle. A consultation abandoned by a previous daemon keeps its
// launch marker, and therefore fleet auxiliary capacity, until that path seals
// it; it is otherwise reachable only from a same-role supervise cycle, which a
// paused project or supervisor.enabled=false never runs (#1232).
func reconcileAbandonedAuxiliaryConsultations(ctx context.Context, cfg *config.Config, name string) {
	if cfg == nil || strings.TrimSpace(cfg.StateDir) == "" || cfg.Supervisor.NativeSessionRegistration == nil {
		return
	}
	for _, root := range auxiliaryRootsOf(cfg.StateDir) {
		report := reconcileAuxiliaryRoot(ctx, cfg, cfg.StateDir, root.dir, root.role, nil, nil)
		logAuxiliaryRootReport(name, report, "")
	}
}

func logAuxiliaryRootReport(name string, report AuxiliaryRootReport, releaseHint string) {
	switch report.Result {
	case "released":
		log.Printf("[%s] auxiliary reconcile: sealed abandoned %s consultation under %s; capacity released", name, report.Role, report.Root)
	case "sealed":
		log.Printf("[%s] auxiliary reconcile: closed abandoned pre-launch %s receipt under %s; nothing had launched and no capacity was held", name, report.Role, report.Root)
	case "clear":
		log.Printf("[%s] auxiliary reconcile: %s root %s has no unresolved native consultation", name, report.Role, report.Root)
	default:
		log.Printf("[%s] auxiliary reconcile: %s root %s still held (%s): %s%s", name, report.Role, report.Root, report.Hold, describeAuxiliaryOccupancy(report.Occupants, nil, time.Now()), releaseHint)
	}
}

// ReconcileIndexedAuxiliaryRoots walks every project state dir persisted in
// the auxiliary receipt index (including roots of removed projects) and runs
// the replay path over each root that has receipts. configured supplies the
// live project configs; a root without one is reconciled with a config derived
// from its own receipt. only restricts the walk to one indexed state dir;
// orphansOnly skips configured projects (their flows reconcile themselves).
// Nothing is launched; the reports name every occupant that remains.
func ReconcileIndexedAuxiliaryRoots(ctx context.Context, index AuxiliaryReceiptIndex, configured []*config.Config, limiter aiexecution.AuxiliaryLimiter, only string, orphansOnly bool) ([]AuxiliaryRootReport, error) {
	if index == nil {
		return nil, errors.New("auxiliary receipt index unavailable")
	}
	dirs, err := index.AuxiliaryStateDirs(ctx)
	if err != nil {
		return nil, fmt.Errorf("read auxiliary receipt index: %w", err)
	}
	sort.Strings(dirs)
	byDir := map[string]*config.Config{}
	var authorities []config.NativeSessionRegistrationConfig
	seen := map[string]bool{}
	for _, cfg := range configured {
		if cfg == nil || strings.TrimSpace(cfg.StateDir) == "" {
			continue
		}
		byDir[filepath.Clean(cfg.StateDir)] = cfg
		if r := cfg.Supervisor.NativeSessionRegistration; r != nil && r.AuthorityUID != nil {
			key := r.ControlSocket + "\x00" + fmt.Sprint(*r.AuthorityUID)
			if !seen[key] {
				seen[key] = true
				authorities = append(authorities, *r)
			}
		}
	}
	if only != "" {
		only = filepath.Clean(only)
		found := false
		for _, dir := range dirs {
			if filepath.Clean(dir) == only {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("state dir %s is not in the auxiliary receipt index", only)
		}
		dirs = []string{only}
	}
	var reports []AuxiliaryRootReport
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return reports, ctx.Err()
		}
		dir = filepath.Clean(dir)
		cfg := byDir[dir]
		if cfg != nil && orphansOnly {
			continue
		}
		if cfg != nil && cfg.Supervisor.NativeSessionRegistration == nil {
			// A configured project without native registration cannot seal; its
			// roots are still reported so the occupancy is visible.
			cfg = nil
		}
		for _, root := range auxiliaryRootsOf(dir) {
			reports = append(reports, reconcileAuxiliaryRoot(ctx, cfg, dir, root.dir, root.role, authorities, limiter))
		}
	}
	return reports, nil
}

// reconcileOrphanAuxiliaryRoots is the daemon-start pass over indexed roots
// that no started flow owns (removed projects). It returns once the pass has
// finished; the daemon waits on the channel before tearing flows down.
func (d *Daemon) reconcileOrphanAuxiliaryRoots(ctx context.Context, configured []*config.Config) <-chan struct{} {
	done := make(chan struct{})
	if d.spawnLimiter == nil || d.spawnLimiter.auxiliaryStore == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[daemon] auxiliary reconcile of indexed roots panicked (contained): %v", r)
			}
		}()
		reports, err := ReconcileIndexedAuxiliaryRoots(ctx, d.spawnLimiter.auxiliaryStore, configured, d.spawnLimiter, "", true)
		if err != nil {
			log.Printf("[daemon] auxiliary reconcile of indexed roots incomplete: %v", err)
		}
		for _, report := range reports {
			logAuxiliaryRootReport("daemon", report, "; no configured project owns this root — inspect and release with: maestro auxiliary reconcile --root "+report.ProjectDir)
		}
	}()
	return done
}
