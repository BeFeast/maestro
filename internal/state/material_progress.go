package state

import (
	"fmt"
	"sort"
	"time"

	"github.com/befeast/maestro/internal/progress"
)

// MaterialProgress is the durable stalled-progress watchdog state for a
// project. Targets are independently watermarked exact workers, PR gates, or
// delivery leases: progress by worker B can never reset worker A's deadline.
// An idle project has no active target and therefore no armed deadline.
type MaterialProgress struct {
	Targets             map[string]*MaterialProgressTarget `json:"targets,omitempty"`
	BudgetSeconds       int                                `json:"budget_seconds,omitempty"`
	EvalIntervalSeconds int                                `json:"eval_interval_seconds,omitempty"`
	LastEvaluatedAt     time.Time                          `json:"last_evaluated_at,omitempty"`
	ConfigChangedAt     time.Time                          `json:"config_changed_at,omitempty"`
}

// MaterialProgressTarget contains one exact target's durable watermark and
// audit records. LastDecision is the latest evaluator verdict.
// LastRecommendation is the first durable recommendation for the current
// overdue episode and is not refreshed by repeated overdue evaluations.
// Recoveries contains only actual attempts recorded explicitly by an actuator;
// evaluation alone never claims a recovery occurred.
type MaterialProgressTarget struct {
	Target             progress.Target     `json:"target"`
	Active             bool                `json:"active"`
	UpdatedAt          time.Time           `json:"updated_at,omitempty"`
	RetiredAt          time.Time           `json:"retired_at,omitempty"`
	Watermark          progress.Watermark  `json:"watermark"`
	LastEvaluatedAt    time.Time           `json:"last_evaluated_at,omitempty"`
	LastDecision       *progress.Decision  `json:"last_decision,omitempty"`
	LastRecommendation *progress.Decision  `json:"last_recommendation,omitempty"`
	Recoveries         []progress.Recovery `json:"recoveries,omitempty"`
}

// EvaluationDue reports whether the independent watchdog scheduler should run.
// Configuration transitions bypass cadence so disabled/enabled or budget
// changes establish a fresh baseline immediately. A backwards clock jump also
// evaluates rather than suppressing the watchdog indefinitely.
func (m *MaterialProgress) EvaluationDue(budget, evalInterval time.Duration, now time.Time) bool {
	if m == nil || m.LastEvaluatedAt.IsZero() || m.BudgetSeconds != durationSeconds(budget) {
		return true
	}
	interval := evalInterval
	if interval <= 0 {
		return true
	}
	now = now.UTC()
	last := m.LastEvaluatedAt.UTC()
	if now.Before(last) {
		return true
	}
	return !now.Before(last.Add(interval))
}

// Deadline returns this active target's next deadline under the container's
// current budget. Retired/disabled/terminal targets have no active deadline.
func (t *MaterialProgressTarget) Deadline(budgetSeconds int) time.Time {
	if t == nil || !t.Active || budgetSeconds <= 0 || t.Watermark.Phase == progress.PhaseDelivered {
		return time.Time{}
	}
	return t.Watermark.Deadline(time.Duration(budgetSeconds) * time.Second)
}

// LastRecovery returns the latest actual recovery attempt, never a mere
// evaluator recommendation.
func (t *MaterialProgressTarget) LastRecovery() *progress.Recovery {
	if t == nil || len(t.Recoveries) == 0 {
		return nil
	}
	last := &t.Recoveries[0]
	for i := 1; i < len(t.Recoveries); i++ {
		candidate := &t.Recoveries[i]
		if candidate.AttemptedAt.After(last.AttemptedAt) {
			last = candidate
		}
	}
	return last
}

// RecordMaterialProgress evaluates a complete snapshot of independently scoped
// observations. Targets absent from the snapshot are retired (not evaluated),
// so an idle project cannot become a synthetic delivery stall. A newly seen or
// reactivated target gets a fresh baseline. Disabled->enabled and every budget
// change also establish a fresh baseline for all observed targets.
func (s *State) RecordMaterialProgress(observations []progress.Observation, budget, evalInterval time.Duration, now time.Time) ([]progress.Decision, error) {
	if s == nil {
		return nil, fmt.Errorf("material-progress state is nil")
	}
	now = now.UTC()
	budgetSeconds := durationSeconds(budget)
	intervalSeconds := durationSeconds(evalInterval)

	// Validate and de-duplicate the complete snapshot before mutating state so
	// one malformed target cannot partially retire or reconfigure good records.
	byKey := make(map[string]progress.Observation, len(observations))
	keys := make([]string, 0, len(observations))
	for _, observation := range observations {
		if err := observation.Target.Validate(); err != nil {
			return nil, fmt.Errorf("invalid material-progress target: %w", err)
		}
		key := observation.Target.Key()
		if _, duplicate := byKey[key]; duplicate {
			return nil, fmt.Errorf("duplicate material-progress target %s", key)
		}
		byKey[key] = observation
		keys = append(keys, key)
	}
	sort.Strings(keys)

	mp := s.MaterialProgress
	if mp == nil {
		mp = &MaterialProgress{Targets: make(map[string]*MaterialProgressTarget)}
		s.MaterialProgress = mp
	}
	if mp.Targets == nil {
		mp.Targets = make(map[string]*MaterialProgressTarget)
	}
	configChanged := mp.ConfigChangedAt.IsZero() || mp.BudgetSeconds != budgetSeconds
	if configChanged {
		mp.ConfigChangedAt = now
	}
	mp.BudgetSeconds = budgetSeconds
	mp.EvalIntervalSeconds = intervalSeconds
	mp.LastEvaluatedAt = now

	// Retire targets that are no longer active before evaluating the complete
	// snapshot. Records/history remain durable, but no deadline remains armed.
	for key, target := range mp.Targets {
		if target == nil || !target.Active {
			continue
		}
		if _, present := byKey[key]; !present {
			target.Active = false
			target.RetiredAt = now
			target.UpdatedAt = now
		}
	}

	decisions := make([]progress.Decision, 0, len(keys))
	for _, key := range keys {
		observation := byKey[key]
		target := mp.Targets[key]
		freshBaseline := configChanged || target == nil || !target.Active
		if target == nil {
			target = &MaterialProgressTarget{Target: observation.Target}
			mp.Targets[key] = target
		}
		prev := target.Watermark
		if freshBaseline {
			prev = progress.Watermark{}
			target.LastRecommendation = nil
		}
		wm, decision := progress.EvaluateTarget(observation.Target, prev, observation.Signals, observation.Phase, budget, now)
		target.Target = observation.Target
		target.Active = true
		target.RetiredAt = time.Time{}
		target.UpdatedAt = now
		target.Watermark = wm
		target.LastEvaluatedAt = now
		d := decision
		target.LastDecision = &d
		if decision.RecommendsRecovery() && !sameRecommendation(target.LastRecommendation, decision) {
			r := decision
			target.LastRecommendation = &r
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func durationSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	seconds := int(d / time.Second)
	if seconds == 0 {
		return 1
	}
	return seconds
}

func sameRecommendation(previous *progress.Decision, next progress.Decision) bool {
	return previous != nil && previous.RecommendationID != "" && previous.RecommendationID == next.RecommendationID
}

// RecordMaterialRecovery records an actual actuator attempt/result for a prior
// recommendation. RecommendationID is an exactly-once key: repeated attempted
// records are idempotent, and a terminal result may complete that same attempt
// but a second attempt for the episode is rejected.
func (s *State) RecordMaterialRecovery(targetKey, recommendationID string, outcome progress.RecoveryOutcome, now time.Time) error {
	if s == nil || s.MaterialProgress == nil {
		return fmt.Errorf("material-progress state does not exist")
	}
	target := s.MaterialProgress.Targets[targetKey]
	if target == nil || target.LastRecommendation == nil ||
		target.LastRecommendation.RecommendationID != recommendationID {
		return fmt.Errorf("material-progress recommendation does not exist")
	}
	if outcome != progress.RecoveryAttempted && outcome != progress.RecoverySucceeded && outcome != progress.RecoveryFailed {
		return fmt.Errorf("invalid material-progress recovery outcome %q", outcome)
	}
	now = now.UTC()
	for i := range target.Recoveries {
		recovery := &target.Recoveries[i]
		if recovery.RecommendationID != recommendationID {
			continue
		}
		if recovery.Outcome == outcome {
			return nil
		}
		if recovery.Outcome != progress.RecoveryAttempted || outcome == progress.RecoveryAttempted {
			return fmt.Errorf("material-progress recovery already completed as %q", recovery.Outcome)
		}
		recovery.Outcome = outcome
		recovery.CompletedAt = now
		target.UpdatedAt = now
		return nil
	}
	recovery := progress.Recovery{
		RecommendationID: recommendationID,
		Target:           target.Target,
		Action:           target.LastRecommendation.Action,
		Outcome:          outcome,
		AttemptedAt:      now,
	}
	if outcome != progress.RecoveryAttempted {
		recovery.CompletedAt = now
	}
	target.Recoveries = append(target.Recoveries, recovery)
	target.UpdatedAt = now
	return nil
}

// mergeMaterialProgress merges concurrent complete-snapshot writers per exact
// target. Progress fields choose the furthest watermark; active/retired state
// chooses the latest UpdatedAt; decisions/recommendations choose the latest
// evaluation/recommendation; actual recovery history is unioned independently.
func mergeMaterialProgress(merged, current, ours *State) {
	a, b := current.MaterialProgress, ours.MaterialProgress
	if a == nil && b == nil {
		merged.MaterialProgress = nil
		return
	}
	if a == nil {
		merged.MaterialProgress = cloneMaterialProgress(b)
		return
	}
	if b == nil {
		merged.MaterialProgress = cloneMaterialProgress(a)
		return
	}

	out := &MaterialProgress{Targets: make(map[string]*MaterialProgressTarget)}
	config := a
	if b.ConfigChangedAt.After(a.ConfigChangedAt) ||
		(b.ConfigChangedAt.Equal(a.ConfigChangedAt) && b.LastEvaluatedAt.After(a.LastEvaluatedAt)) {
		config = b
	}
	out.BudgetSeconds = config.BudgetSeconds
	out.EvalIntervalSeconds = config.EvalIntervalSeconds
	out.ConfigChangedAt = config.ConfigChangedAt
	out.LastEvaluatedAt = a.LastEvaluatedAt
	if b.LastEvaluatedAt.After(out.LastEvaluatedAt) {
		out.LastEvaluatedAt = b.LastEvaluatedAt
	}

	keys := make(map[string]struct{}, len(a.Targets)+len(b.Targets))
	for key := range a.Targets {
		keys[key] = struct{}{}
	}
	for key := range b.Targets {
		keys[key] = struct{}{}
	}
	for key := range keys {
		out.Targets[key] = mergeMaterialProgressTarget(a.Targets[key], b.Targets[key])
	}
	merged.MaterialProgress = out
}

func mergeMaterialProgressTarget(a, b *MaterialProgressTarget) *MaterialProgressTarget {
	if a == nil {
		return cloneMaterialProgressTarget(b)
	}
	if b == nil {
		return cloneMaterialProgressTarget(a)
	}
	base := a
	switch {
	case b.Watermark.At.After(a.Watermark.At):
		base = b
	case b.Watermark.At.Equal(a.Watermark.At) && b.LastEvaluatedAt.After(a.LastEvaluatedAt):
		base = b
	}
	out := cloneMaterialProgressTarget(base)
	if b.UpdatedAt.After(a.UpdatedAt) {
		out.Active, out.RetiredAt, out.UpdatedAt = b.Active, b.RetiredAt, b.UpdatedAt
	} else {
		out.Active, out.RetiredAt, out.UpdatedAt = a.Active, a.RetiredAt, a.UpdatedAt
	}
	out.LastDecision = laterDecision(a.LastDecision, b.LastDecision)
	out.LastRecommendation = laterRecommendation(a.LastRecommendation, b.LastRecommendation)
	out.Recoveries = mergeRecoveries(a.Recoveries, b.Recoveries)
	return out
}

func laterDecision(a, b *progress.Decision) *progress.Decision {
	if a == nil {
		return cloneDecision(b)
	}
	if b == nil {
		return cloneDecision(a)
	}
	if b.EvaluatedAt.After(a.EvaluatedAt) {
		return cloneDecision(b)
	}
	return cloneDecision(a)
}

func laterRecommendation(a, b *progress.Decision) *progress.Decision {
	if a == nil {
		return cloneDecision(b)
	}
	if b == nil {
		return cloneDecision(a)
	}
	if b.RecommendedAt.After(a.RecommendedAt) {
		return cloneDecision(b)
	}
	return cloneDecision(a)
}

func mergeRecoveries(a, b []progress.Recovery) []progress.Recovery {
	byID := make(map[string]progress.Recovery, len(a)+len(b))
	for _, recovery := range append(append([]progress.Recovery(nil), a...), b...) {
		prior, exists := byID[recovery.RecommendationID]
		if !exists || recovery.CompletedAt.After(prior.CompletedAt) ||
			(recovery.CompletedAt.Equal(prior.CompletedAt) && recovery.AttemptedAt.After(prior.AttemptedAt)) {
			byID[recovery.RecommendationID] = recovery
		}
	}
	out := make([]progress.Recovery, 0, len(byID))
	for _, recovery := range byID {
		out = append(out, recovery)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AttemptedAt.Equal(out[j].AttemptedAt) {
			return out[i].RecommendationID < out[j].RecommendationID
		}
		return out[i].AttemptedAt.Before(out[j].AttemptedAt)
	})
	return out
}

func cloneMaterialProgress(mp *MaterialProgress) *MaterialProgress {
	if mp == nil {
		return nil
	}
	out := *mp
	out.Targets = make(map[string]*MaterialProgressTarget, len(mp.Targets))
	for key, target := range mp.Targets {
		out.Targets[key] = cloneMaterialProgressTarget(target)
	}
	return &out
}

func cloneMaterialProgressTarget(target *MaterialProgressTarget) *MaterialProgressTarget {
	if target == nil {
		return nil
	}
	out := *target
	out.Watermark.Signals = append([]progress.Signal(nil), target.Watermark.Signals...)
	out.LastDecision = cloneDecision(target.LastDecision)
	out.LastRecommendation = cloneDecision(target.LastRecommendation)
	out.Recoveries = append([]progress.Recovery(nil), target.Recoveries...)
	return &out
}

func cloneDecision(decision *progress.Decision) *progress.Decision {
	if decision == nil {
		return nil
	}
	out := *decision
	out.ObservedSignals = append([]progress.SignalKind(nil), decision.ObservedSignals...)
	return &out
}
