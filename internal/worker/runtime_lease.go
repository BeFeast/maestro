package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/workerlease"
)

var workerRuntimeCurrentUser = user.Current

func prepareWorkerScratchLease(cfg *config.Config, slotName, reason string, processLease tmuxsession.ProcessLease) (*workerlease.Lease, *tmuxsession.ProcessLeaseRuntime, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("worker config is required")
	}
	if !cfg.WorkerRuntime.IsolatedEnabled() {
		return nil, nil, nil
	}
	if err := workerlease.EnsureScratchBase(cfg.WorkerRuntime.EffectiveScratchRoot()); err != nil {
		return nil, nil, err
	}
	if err := workerlease.EnsureWorkerSlice(cfg.WorkerRuntime.EffectiveScope()); err != nil {
		return nil, nil, err
	}
	maestroBin, ok := maestroExecutablePath()
	if !ok {
		return nil, nil, fmt.Errorf("resolve maestro executable for isolated worker lease")
	}
	currentUser, err := workerRuntimeCurrentUser()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve worker runtime user: %w", err)
	}
	lease, err := workerlease.Prepare(workerlease.Spec{
		Root:       workerLeaseProjectRoot(cfg),
		ProjectKey: workerLeaseProjectKey(cfg),
		Repo:       cfg.Repo,
		Slot:       slotName,
		Attempt:    reason,
		Unit:       processLease.Unit,
		Scope:      processLease.Manager,
		Now:        time.Now().UTC(),
	})
	if err != nil {
		return nil, nil, err
	}
	runtime := &tmuxsession.ProcessLeaseRuntime{
		ScratchID:    lease.ID,
		ScratchDir:   lease.ScratchDir,
		TempDir:      lease.TempDir,
		GoTempDir:    lease.GoTempDir,
		CargoTarget:  lease.CargoTarget,
		ManifestPath: lease.ManifestPath,
		CleanupExec:  workerlease.CleanupExec(maestroBin, lease.ManifestPath, lease.ID),
		Home:         currentUser.HomeDir,
		User:         currentUser.Username,
		MemoryMaxMB:  cfg.WorkerRuntime.MemoryMaxMB,
	}
	return &lease, runtime, nil
}

func workerLeaseProjectRoot(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	key := workerLeaseProjectKey(cfg)
	if len(key) > 20 {
		key = key[:20]
	}
	return filepath.Join(cfg.WorkerRuntime.EffectiveScratchRoot(), "project-"+key)
}

func workerLeaseProjectKey(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	identity := strings.TrimSpace(cfg.ProjectID)
	if identity == "" {
		identity = strings.TrimSpace(cfg.Repo) + "\x00" + strings.TrimSpace(cfg.StateDir)
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func recoverWorkerScratchLease(cfg *config.Config, slotName string, processLease tmuxsession.ProcessLease) (*workerlease.Lease, error) {
	if cfg == nil || !cfg.WorkerRuntime.IsolatedEnabled() {
		return nil, nil
	}
	leases, _, err := workerlease.List(workerLeaseProjectRoot(cfg))
	if err != nil {
		return nil, fmt.Errorf("recover worker scratch receipt: %w", err)
	}
	var match *workerlease.Lease
	for i := range leases {
		lease := leases[i]
		if lease.ProjectKey != workerLeaseProjectKey(cfg) || lease.Slot != strings.TrimSpace(slotName) ||
			lease.Unit != processLease.Unit || lease.Scope != processLease.Manager {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("recover worker scratch receipt: multiple manifests claim process lease %s", processLease.Unit)
		}
		candidate := lease
		match = &candidate
	}
	if match == nil {
		return nil, fmt.Errorf("recover worker scratch receipt: active isolated process lease %s has no exact manifest", processLease.Unit)
	}
	return match, nil
}

func assignWorkerLease(sess *state.Session, lease *workerlease.Lease) {
	if sess == nil {
		return
	}
	if lease == nil {
		sess.WorkerLeaseID = ""
		sess.WorkerLeaseUnit = ""
		sess.WorkerLeaseScope = ""
		sess.WorkerScratchDir = ""
		sess.WorkerLeaseManifest = ""
		sess.WorkerLeaseAttention = ""
		return
	}
	sess.WorkerLeaseID = lease.ID
	sess.WorkerLeaseUnit = lease.Unit
	sess.WorkerLeaseScope = lease.Scope
	sess.WorkerScratchDir = lease.ScratchDir
	sess.WorkerLeaseManifest = lease.ManifestPath
	sess.WorkerLeaseAttention = ""
}

func leaseFromSession(cfg *config.Config, sess *state.Session) (*workerlease.Lease, error) {
	if sess == nil || strings.TrimSpace(sess.WorkerLeaseID) == "" {
		return nil, nil
	}
	id := strings.TrimSpace(sess.WorkerLeaseID)
	scratchDir := filepath.Clean(strings.TrimSpace(sess.WorkerScratchDir))
	manifestPath := filepath.Clean(strings.TrimSpace(sess.WorkerLeaseManifest))
	if !workerlease.ValidLeaseID(id) || filepath.Base(scratchDir) != id || manifestPath != filepath.Join(scratchDir, workerlease.ManifestName) {
		return nil, fmt.Errorf("persisted worker lease path identity is invalid")
	}
	m, err := workerlease.ValidateManifest(sess.WorkerLeaseManifest, sess.WorkerLeaseID)
	if os.IsNotExist(err) {
		// ExecStopPost may already have removed a terminal lease. Preserve enough
		// identity to stop a still-loaded unit, but never invent a cleanup path.
		return &workerlease.Lease{
			ID: sess.WorkerLeaseID, Unit: sess.WorkerLeaseUnit, Scope: sess.WorkerLeaseScope,
			ScratchDir: sess.WorkerScratchDir, ManifestPath: sess.WorkerLeaseManifest,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if cfg != nil && m.ProjectKey != workerLeaseProjectKey(cfg) {
		return nil, fmt.Errorf("persisted worker lease belongs to another project")
	}
	lease := workerlease.LeaseFromManifest(m, sess.WorkerLeaseManifest)
	if lease.Unit != strings.TrimSpace(sess.WorkerLeaseUnit) || lease.Scope != strings.TrimSpace(sess.WorkerLeaseScope) ||
		lease.ScratchDir != filepath.Clean(strings.TrimSpace(sess.WorkerScratchDir)) {
		return nil, fmt.Errorf("persisted worker lease identity does not match ownership manifest")
	}
	if strings.TrimSpace(sess.ProcessLeaseUnit) != "" &&
		(lease.Unit != strings.TrimSpace(sess.ProcessLeaseUnit) || lease.Scope != strings.TrimSpace(sess.ProcessLeaseManager)) {
		return nil, fmt.Errorf("persisted worker scratch does not match process lease ownership")
	}
	return &lease, nil
}

func cleanupOwnedWorkerScratch(cfg *config.Config, sess *state.Session) (bool, error) {
	lease, err := leaseFromSession(cfg, sess)
	if err != nil {
		if sess != nil {
			sess.WorkerLeaseAttention = "persisted lease identity is ambiguous"
		}
		return true, err
	}
	if lease == nil {
		return false, nil
	}
	if !workerlease.ValidProcessLeaseUnit(lease.Unit) ||
		(lease.Scope != workerlease.ScopeSystem && lease.Scope != workerlease.ScopeUser) {
		sess.WorkerLeaseAttention = "persisted lease unit or scope is invalid"
		return true, fmt.Errorf("invalid persisted worker lease identity")
	}
	if strings.TrimSpace(sess.ProcessLeaseUnit) != lease.Unit || strings.TrimSpace(sess.ProcessLeaseManager) != lease.Scope {
		sess.WorkerLeaseAttention = "worker scratch does not match the process lease"
		return true, fmt.Errorf("worker scratch ownership does not match process lease")
	}
	if err := workerlease.CleanupManifest(lease.ManifestPath, lease.ID); err != nil {
		sess.WorkerLeaseAttention = "exact worker scratch could not be cleaned"
		return true, err
	}
	assignWorkerLease(sess, nil)
	return true, nil
}

type WorkerLeaseReconcileResult struct {
	Cleaned   []string
	Attention int
	Changed   bool
}

type workerLeaseReconcileOps interface {
	List(string) ([]workerlease.Lease, []workerlease.Attention, error)
	Active(string, string) (bool, error)
	Stop(string, string) error
	Cleanup(string, string) error
}

type systemWorkerLeaseReconcileOps struct{}

func (systemWorkerLeaseReconcileOps) List(root string) ([]workerlease.Lease, []workerlease.Attention, error) {
	return workerlease.List(root)
}

func (systemWorkerLeaseReconcileOps) Active(scope, unit string) (bool, error) {
	return tmuxsession.ProcessLeaseActive(tmuxsession.ProcessLease{Unit: unit, Manager: scope})
}

func (systemWorkerLeaseReconcileOps) Stop(scope, unit string) error {
	return tmuxsession.TerminateProcessLease(tmuxsession.ProcessLease{Unit: unit, Manager: scope})
}

func (systemWorkerLeaseReconcileOps) Cleanup(manifestPath, leaseID string) error {
	return workerlease.CleanupManifest(manifestPath, leaseID)
}

type workerLeaseSessionRef struct {
	slot string
	sess *state.Session
}

// ReconcileWorkerLeases converges the configured project's private scratch
// manifests with persisted session ownership. It enumerates only the
// deterministic per-project root, stops/removes only validated exact leases,
// and persists ambiguity as path-free attention instead of guessing by age,
// executable, or directory prefix.
func ReconcileWorkerLeases(cfg *config.Config, s *state.State, now time.Time) WorkerLeaseReconcileResult {
	return reconcileWorkerLeasesWithOps(cfg, s, now, systemWorkerLeaseReconcileOps{})
}

func reconcileWorkerLeasesWithOps(cfg *config.Config, s *state.State, now time.Time, ops workerLeaseReconcileOps) WorkerLeaseReconcileResult {
	var result WorkerLeaseReconcileResult
	if cfg == nil || s == nil || ops == nil {
		return result
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	previousAttention := append([]state.WorkerLeaseAttention(nil), s.WorkerLeaseAttention...)
	previousSessions := workerLeaseSessionSnapshot(s)
	attention := make([]state.WorkerLeaseAttention, 0)
	projectKey := workerLeaseProjectKey(cfg)
	for _, sess := range s.Sessions {
		if sess != nil {
			sess.WorkerLeaseAttention = ""
		}
	}

	addAttention := func(identity, slot, reason string, sess *state.Session) {
		identity = strings.TrimSpace(identity)
		if !workerlease.ValidLeaseID(identity) {
			identity = opaqueWorkerLeaseIdentity(identity)
		}
		record := state.WorkerLeaseAttention{
			Identity:   identity,
			Slot:       strings.TrimSpace(slot),
			Reason:     strings.TrimSpace(reason),
			NextAction: "Inspect the exact persisted lease and ownership manifest; do not delete by age, path prefix, or executable name.",
			DetectedAt: now,
		}
		for _, old := range previousAttention {
			if old.Identity == record.Identity && old.Slot == record.Slot && old.Reason == record.Reason {
				record.DetectedAt = old.DetectedAt
				break
			}
		}
		attention = append(attention, record)
		if sess != nil {
			sess.WorkerLeaseAttention = record.Reason
		}
	}

	leasesByID := make(map[string]workerlease.Lease)
	invalidByID := make(map[string]bool)
	leases, ambiguous, err := ops.List(workerLeaseProjectRoot(cfg))
	if err != nil {
		addAttention("scratch-scan", "", "worker scratch ownership could not be enumerated", nil)
	} else {
		for _, item := range ambiguous {
			if workerlease.ValidLeaseID(item.Entry) {
				invalidByID[item.Entry] = true
			}
			addAttention(item.Entry, "", item.Reason, nil)
		}
		for _, lease := range leases {
			if lease.ProjectKey != projectKey {
				addAttention(lease.ID, lease.Slot, "ownership manifest belongs to another project", nil)
				continue
			}
			leasesByID[lease.ID] = lease
		}
	}

	// A config rollback/root change must still stop leases already persisted on
	// sessions. Read those exact manifests directly; orphan enumeration remains
	// limited to the currently configured per-project root.
	for _, sess := range s.Sessions {
		if sess == nil {
			continue
		}
		id := strings.TrimSpace(sess.WorkerLeaseID)
		if id == "" {
			continue
		}
		if _, ok := leasesByID[id]; ok || invalidByID[id] {
			continue
		}
		manifest, err := workerlease.ValidateManifest(sess.WorkerLeaseManifest, id)
		if err != nil {
			if !os.IsNotExist(err) {
				invalidByID[id] = true
			}
			continue
		}
		if manifest.ProjectKey != projectKey {
			invalidByID[id] = true
			continue
		}
		leasesByID[id] = workerlease.LeaseFromManifest(manifest, sess.WorkerLeaseManifest)
	}

	sessionsByID := make(map[string][]workerLeaseSessionRef)
	liveBySlot := make(map[string][]workerLeaseSessionRef)
	for slot, sess := range s.Sessions {
		if sess == nil {
			continue
		}
		id := strings.TrimSpace(sess.WorkerLeaseID)
		if id != "" {
			sessionsByID[id] = append(sessionsByID[id], workerLeaseSessionRef{slot: slot, sess: sess})
		}
		if sessionMayOwnLiveLease(sess) {
			liveBySlot[slot] = append(liveBySlot[slot], workerLeaseSessionRef{slot: slot, sess: sess})
		}
	}
	claimedBySlot := make(map[string]*state.FreshDispatchClaim)
	for _, claim := range s.FreshDispatchClaims {
		if claim != nil && claim.Status == state.FreshDispatchClaimStatusClaimed && strings.TrimSpace(claim.Slot) != "" {
			claimedBySlot[strings.TrimSpace(claim.Slot)] = claim
		}
	}
	idsByUnit := make(map[string][]string)
	for id, lease := range leasesByID {
		idsByUnit[lease.Unit] = append(idsByUnit[lease.Unit], id)
	}
	ambiguousUnits := make(map[string]bool)
	for unit, leaseIDs := range idsByUnit {
		if len(leaseIDs) > 1 {
			ambiguousUnits[unit] = true
		}
	}

	ids := make([]string, 0, len(leasesByID))
	for id := range leasesByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		lease := leasesByID[id]
		seen[id] = true
		refs := sessionsByID[id]
		if ambiguousUnits[lease.Unit] {
			if len(refs) == 0 {
				addAttention(id, lease.Slot, "multiple scratch manifests claim the same process lease", nil)
			} else {
				for _, ref := range refs {
					addAttention(id, ref.slot, "multiple scratch manifests claim the same process lease", ref.sess)
				}
			}
			continue
		}
		if len(refs) > 1 {
			for _, ref := range refs {
				addAttention(id, ref.slot, "multiple sessions claim the same worker lease", ref.sess)
			}
			continue
		}
		if len(refs) == 1 {
			ref := refs[0]
			if ref.slot != lease.Slot || !leaseMetadataCompatible(ref.sess, lease) {
				addAttention(id, ref.slot, "persisted session and ownership manifest disagree", ref.sess)
				continue
			}
			leaseAttention := ref.sess.WorkerLeaseAttention
			assignWorkerLease(ref.sess, &lease)
			ref.sess.WorkerLeaseAttention = leaseAttention
			expectedLive := sessionMayOwnLiveLease(ref.sess)
			if expectedLive {
				active, err := ops.Active(lease.Scope, lease.Unit)
				if err != nil {
					addAttention(id, ref.slot, "worker lease liveness could not be inspected", ref.sess)
					continue
				}
				if active {
					continue
				}
			}
			if stopAndCleanupWorkerLease(ops, lease) {
				result.Cleaned = append(result.Cleaned, id)
				assignWorkerLease(ref.sess, nil)
				clearMatchingProcessLease(ref.sess, lease)
				if expectedLive {
					ref.sess.PID = 0
					ref.sess.TmuxSession = ""
					state.MarkWorkerEnded(ref.sess, now)
				}
			} else {
				addAttention(id, ref.slot, "exact worker lease or scratch cleanup failed", ref.sess)
			}
			continue
		}

		if claimedBySlot[lease.Slot] != nil {
			expected, err := workerProcessLease(cfg, lease.Slot, 1)
			if err != nil || expected.Unit != lease.Unit || expected.Manager != lease.Scope {
				addAttention(id, lease.Slot, "fresh dispatch claim and worker lease identity disagree", nil)
				continue
			}
			active, err := ops.Active(lease.Scope, lease.Unit)
			if err != nil {
				addAttention(id, lease.Slot, "claimed worker lease liveness could not be inspected", nil)
				continue
			}
			if active {
				// StartReserved can adopt this exact launch/state-gap receipt. Killing
				// it here would defeat the transactional fresh-dispatch lease.
				continue
			}
		}

		if len(liveBySlot[lease.Slot]) > 0 {
			for _, ref := range liveBySlot[lease.Slot] {
				addAttention(id, lease.Slot, "an unclaimed lease conflicts with a live session for the same slot", ref.sess)
			}
			continue
		}
		if stopAndCleanupWorkerLease(ops, lease) {
			result.Cleaned = append(result.Cleaned, id)
		} else {
			addAttention(id, lease.Slot, "orphan worker lease cleanup failed", nil)
		}
	}

	for slot, sess := range s.Sessions {
		if sess == nil {
			continue
		}
		id := strings.TrimSpace(sess.WorkerLeaseID)
		if id == "" || seen[id] {
			continue
		}
		if invalidByID[id] {
			addAttention(id, slot, "worker lease ownership manifest is invalid", sess)
			continue
		}
		lease, err := leaseFromSession(cfg, sess)
		if err != nil || lease == nil {
			if os.IsNotExist(err) {
				// handled by the exact persisted unit below
			} else if err != nil {
				addAttention(id, slot, "persisted worker lease identity is ambiguous", sess)
				continue
			}
			lease = &workerlease.Lease{
				ID: id, Unit: sess.WorkerLeaseUnit, Scope: sess.WorkerLeaseScope,
				ScratchDir: sess.WorkerScratchDir, ManifestPath: sess.WorkerLeaseManifest,
			}
		}
		if !workerlease.ValidProcessLeaseUnit(lease.Unit) ||
			(lease.Scope != workerlease.ScopeSystem && lease.Scope != workerlease.ScopeUser) {
			addAttention(id, slot, "persisted worker lease unit or scope is invalid", sess)
			continue
		}
		active, activeErr := ops.Active(lease.Scope, lease.Unit)
		if activeErr != nil {
			addAttention(id, slot, "worker lease liveness could not be inspected", sess)
			continue
		}
		if active {
			addAttention(id, slot, "worker lease is active but its ownership manifest is missing", sess)
			continue
		}
		expectedLive := sessionMayOwnLiveLease(sess)
		assignWorkerLease(sess, nil)
		clearMatchingProcessLease(sess, *lease)
		if expectedLive {
			sess.PID = 0
			sess.TmuxSession = ""
			state.MarkWorkerEnded(sess, now)
		}
	}

	sort.Slice(attention, func(i, j int) bool {
		if attention[i].Identity != attention[j].Identity {
			return attention[i].Identity < attention[j].Identity
		}
		if attention[i].Slot != attention[j].Slot {
			return attention[i].Slot < attention[j].Slot
		}
		return attention[i].Reason < attention[j].Reason
	})
	s.WorkerLeaseAttention = attention
	s.WorkerLeaseReconciledAt = now
	result.Attention = len(attention)
	result.Changed = !reflect.DeepEqual(previousAttention, attention) ||
		!reflect.DeepEqual(previousSessions, workerLeaseSessionSnapshot(s)) || len(result.Cleaned) > 0
	return result
}

func workerLeaseSessionSnapshot(s *state.State) map[string][8]string {
	result := make(map[string][8]string)
	if s == nil {
		return result
	}
	for slot, sess := range s.Sessions {
		if sess == nil {
			continue
		}
		result[slot] = [8]string{
			sess.WorkerLeaseID,
			sess.WorkerLeaseUnit,
			sess.WorkerLeaseScope,
			sess.WorkerScratchDir,
			sess.WorkerLeaseManifest,
			sess.WorkerLeaseAttention,
			sess.ProcessLeaseUnit,
			sess.ProcessLeaseManager,
		}
	}
	return result
}

func sessionMayOwnLiveLease(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	if sess.Status == state.StatusRunning {
		return true
	}
	return sess.Status == state.StatusPROpen && sess.PID > 0 && sess.WorkerEndedAt == nil
}

func leaseMetadataCompatible(sess *state.Session, lease workerlease.Lease) bool {
	if sess == nil {
		return false
	}
	if strings.TrimSpace(sess.ProcessLeaseUnit) == "" || strings.TrimSpace(sess.ProcessLeaseManager) == "" {
		return false
	}
	checks := [][2]string{
		{sess.WorkerLeaseID, lease.ID},
		{sess.WorkerLeaseUnit, lease.Unit},
		{sess.WorkerLeaseScope, lease.Scope},
		{sess.WorkerScratchDir, lease.ScratchDir},
		{sess.WorkerLeaseManifest, lease.ManifestPath},
		{sess.ProcessLeaseUnit, lease.Unit},
		{sess.ProcessLeaseManager, lease.Scope},
	}
	for _, check := range checks {
		if strings.TrimSpace(check[0]) != "" && filepath.Clean(strings.TrimSpace(check[0])) != filepath.Clean(check[1]) {
			return false
		}
	}
	return true
}

func stopAndCleanupWorkerLease(ops workerLeaseReconcileOps, lease workerlease.Lease) bool {
	if !workerlease.ValidProcessLeaseUnit(lease.Unit) ||
		(lease.Scope != workerlease.ScopeSystem && lease.Scope != workerlease.ScopeUser) {
		return false
	}
	if err := ops.Stop(lease.Scope, lease.Unit); err != nil {
		return false
	}
	return ops.Cleanup(lease.ManifestPath, lease.ID) == nil
}

func clearMatchingProcessLease(sess *state.Session, lease workerlease.Lease) {
	if sess == nil {
		return
	}
	if strings.TrimSpace(sess.ProcessLeaseUnit) == lease.Unit && strings.TrimSpace(sess.ProcessLeaseManager) == lease.Scope {
		clearSessionProcessLease(sess)
	}
}

func opaqueWorkerLeaseIdentity(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return "ambiguous-" + hex.EncodeToString(sum[:6])
}
