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
	"github.com/befeast/maestro/internal/workerlease"
)

var workerRuntimeCurrentUser = user.Current

func prepareAttemptRunner(cfg *config.Config, slotName, runnerPath string, args []string, stdinFile, logFile, worktree string, split *streamSplit, reason string) (*workerlease.Lease, error) {
	if cfg == nil {
		return nil, fmt.Errorf("worker config is required")
	}
	if !cfg.WorkerRuntime.IsolatedEnabled() {
		return nil, writeWorkerRunnerScript(cfg.StateDir, runnerPath, args, stdinFile, logFile, worktree, split)
	}
	if err := workerlease.EnsureScratchBase(cfg.WorkerRuntime.EffectiveScratchRoot()); err != nil {
		return nil, err
	}
	if err := workerlease.EnsureWorkerSlice(cfg.WorkerRuntime.EffectiveScope()); err != nil {
		return nil, err
	}
	maestroBin, ok := maestroExecutablePath()
	if !ok {
		return nil, fmt.Errorf("resolve maestro executable for isolated worker lease")
	}
	lease, err := workerlease.Prepare(workerlease.Spec{
		Root:       workerLeaseProjectRoot(cfg),
		ProjectKey: workerLeaseProjectKey(cfg),
		Repo:       cfg.Repo,
		Slot:       slotName,
		Attempt:    reason,
		Scope:      cfg.WorkerRuntime.EffectiveScope(),
		Now:        time.Now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = workerlease.CleanupManifest(lease.ManifestPath, lease.ID)
		}
	}()
	currentUser, err := workerRuntimeCurrentUser()
	if err != nil {
		return nil, fmt.Errorf("resolve worker runtime user: %w", err)
	}

	payloadPath := filepath.Join(lease.ScratchDir, "payload.sh")
	if err := writeWorkerRunnerScript(cfg.StateDir, payloadPath, args, stdinFile, logFile, worktree, split); err != nil {
		return nil, err
	}
	launcher, err := workerlease.BuildLauncherScript(workerlease.LaunchSpec{
		Lease: lease, UID: os.Getuid(), PATH: os.Getenv("PATH"), Home: currentUser.HomeDir, User: currentUser.Username,
		PayloadPath: payloadPath,
		MaestroBin:  maestroBin, MemoryMaxMB: cfg.WorkerRuntime.MemoryMaxMB,
	})
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomicMode(filepath.Dir(runnerPath), runnerPath, launcher, workerRunnerScriptMode); err != nil {
		return nil, fmt.Errorf("write isolated worker launcher: %w", err)
	}
	rollback = false
	return &lease, nil
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
	return &lease, nil
}

func rollbackPreparedLease(lease *workerlease.Lease) {
	if lease == nil {
		return
	}
	_ = workerlease.Stop(lease.Scope, lease.Unit)
	_ = workerlease.CleanupManifest(lease.ManifestPath, lease.ID)
}

func stopOwnedWorkerLease(cfg *config.Config, sess *state.Session) (bool, error) {
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
	if strings.TrimSpace(lease.Unit) != workerlease.UnitName(lease.ID) ||
		(lease.Scope != workerlease.ScopeSystem && lease.Scope != workerlease.ScopeUser) {
		sess.WorkerLeaseAttention = "persisted lease unit or scope is invalid"
		return true, fmt.Errorf("invalid persisted worker lease identity")
	}
	if err := workerlease.Stop(lease.Scope, lease.Unit); err != nil {
		sess.WorkerLeaseAttention = "exact worker lease could not be stopped"
		return true, err
	}
	if err := workerlease.CleanupManifest(lease.ManifestPath, lease.ID); err != nil {
		sess.WorkerLeaseAttention = "exact worker scratch could not be cleaned"
		return true, err
	}
	assignWorkerLease(sess, nil)
	return true, nil
}

// WorkerLeaseActive reports liveness from the durable OS lease when a session
// owns one. Callers must use this instead of pane ancestry for isolated
// workers; the transient service remains authoritative after reparenting.
func WorkerLeaseActive(cfg *config.Config, sess *state.Session) (active, owned bool, err error) {
	lease, err := leaseFromSession(cfg, sess)
	if err != nil {
		return false, true, err
	}
	if lease == nil {
		return false, false, nil
	}
	if strings.TrimSpace(lease.Unit) != workerlease.UnitName(lease.ID) ||
		(lease.Scope != workerlease.ScopeSystem && lease.Scope != workerlease.ScopeUser) {
		return false, true, fmt.Errorf("invalid persisted worker lease identity")
	}
	active, err = workerlease.Active(lease.Scope, lease.Unit)
	return active, true, err
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
	return workerlease.Active(scope, unit)
}

func (systemWorkerLeaseReconcileOps) Stop(scope, unit string) error {
	return workerlease.Stop(scope, unit)
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
		if strings.TrimSpace(lease.Unit) != workerlease.UnitName(id) ||
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

func workerLeaseSessionSnapshot(s *state.State) map[string][6]string {
	result := make(map[string][6]string)
	if s == nil {
		return result
	}
	for slot, sess := range s.Sessions {
		if sess == nil {
			continue
		}
		result[slot] = [6]string{
			sess.WorkerLeaseID,
			sess.WorkerLeaseUnit,
			sess.WorkerLeaseScope,
			sess.WorkerScratchDir,
			sess.WorkerLeaseManifest,
			sess.WorkerLeaseAttention,
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
	checks := [][2]string{
		{sess.WorkerLeaseID, lease.ID},
		{sess.WorkerLeaseUnit, lease.Unit},
		{sess.WorkerLeaseScope, lease.Scope},
		{sess.WorkerScratchDir, lease.ScratchDir},
		{sess.WorkerLeaseManifest, lease.ManifestPath},
	}
	for _, check := range checks {
		if strings.TrimSpace(check[0]) != "" && filepath.Clean(strings.TrimSpace(check[0])) != filepath.Clean(check[1]) {
			return false
		}
	}
	return true
}

func stopAndCleanupWorkerLease(ops workerLeaseReconcileOps, lease workerlease.Lease) bool {
	if strings.TrimSpace(lease.Unit) != workerlease.UnitName(lease.ID) ||
		(lease.Scope != workerlease.ScopeSystem && lease.Scope != workerlease.ScopeUser) {
		return false
	}
	if err := ops.Stop(lease.Scope, lease.Unit); err != nil {
		return false
	}
	return ops.Cleanup(lease.ManifestPath, lease.ID) == nil
}

func opaqueWorkerLeaseIdentity(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return "ambiguous-" + hex.EncodeToString(sum[:6])
}
