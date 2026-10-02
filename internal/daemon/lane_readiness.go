package daemon

import (
	"log"
	"sync"

	"github.com/befeast/maestro/internal/aiexecution"
)

// managedLaneReadiness is the supported controller's managed-lane probe
// (aiexecution.ObserveBindingReadiness) shared by every flow and role in the
// daemon. It adds operator visibility only: an accepted lane whose standby
// credentials carry no identity proof is logged once per change, because the
// binding projection cannot show whether such a credential is still
// selectable on part of its routes. If it is, the gateway holds those attempts
// (managed_admission_hold) instead of sending them, so the visible symptom is
// availability, not an unverified request.
type managedLaneReadiness struct {
	mu   sync.Mutex
	last map[string]int
}

var managedLaneProbe = &managedLaneReadiness{}

func (r *managedLaneReadiness) ObserveLaneReadiness(policy aiexecution.Policy) error {
	report, err := aiexecution.ObserveBindingReadinessReport(policy)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]int{}
	}
	key := policy.ManifestSHA256
	if previous, seen := r.last[key]; !seen && report.UnverifiedStandbyCredentials == 0 || seen && previous == report.UnverifiedStandbyCredentials {
		r.last[key] = report.UnverifiedStandbyCredentials
		return nil
	}
	r.last[key] = report.UnverifiedStandbyCredentials
	if report.UnverifiedStandbyCredentials > 0 {
		log.Printf("[daemon] managed lane ready with %d unverified standby credential(s); gateway managed_admission_hold responses would indicate one is still selectable on part of its routes", report.UnverifiedStandbyCredentials)
	} else {
		log.Printf("[daemon] managed lane ready; every standby credential carries an identity proof")
	}
	return nil
}
