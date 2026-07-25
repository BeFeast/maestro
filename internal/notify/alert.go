package notify

// AlertClass is a small, closed enum of operator-alert categories the
// supervisor, orchestrator, and watchdog emit (#1018). Each class maps to a
// fixed ntfy priority + tag set via the router below, so callers name the
// situation ("gate_fail_streak") rather than hand-picking a priority.
type AlertClass string

const (
	// AlertGateFailStreak: a project's review/merge gate has failed N times in
	// a row without advancing.
	AlertGateFailStreak AlertClass = "gate_fail_streak"
	// AlertIdleStall: no worker/feed progress for longer than the idle budget.
	AlertIdleStall AlertClass = "idle_stall"
	// AlertFutileRecovery: automated recovery kept retrying without effect.
	AlertFutileRecovery AlertClass = "futile_recovery"
	// AlertBackendCooldownExhausted: every backend is cooling down / quota-gated
	// and dispatch has nothing to run.
	AlertBackendCooldownExhausted AlertClass = "backend_cooldown_exhausted"
	// AlertFloorBreach: fleet live workers dropped below fleet.min_live_workers.
	// Priority 5 / CRITICAL — operator must hear this without polling (#1106).
	AlertFloorBreach AlertClass = "floor_breach"
	// AlertDeliveryAdvance: a positive event — the delivery/feed advanced.
	AlertDeliveryAdvance AlertClass = "delivery_advance"
	// AlertEmergency: notify_red-grade — emergency stop / supervisor degraded.
	AlertEmergency AlertClass = "emergency"
)

// AlertRoute is the transport-shaping metadata a class maps to.
type AlertRoute struct {
	// Priority is the ntfy priority header value (1=min … 5=max/urgent).
	Priority int
	// Tags are ntfy tag shortcodes rendered as emoji/badges on the client.
	Tags []string
}

// alertRoutes is the class→route table. A class absent here (never expected,
// the enum is closed) falls back to defaultRoute via RouteFor.
var alertRoutes = map[AlertClass]AlertRoute{
	AlertGateFailStreak:           {Priority: 4, Tags: []string{"warning", "no_entry"}},
	AlertIdleStall:                {Priority: 4, Tags: []string{"hourglass", "warning"}},
	AlertFutileRecovery:           {Priority: 4, Tags: []string{"recycle", "warning"}},
	AlertBackendCooldownExhausted: {Priority: 4, Tags: []string{"battery", "warning"}},
	AlertFloorBreach:              {Priority: 5, Tags: []string{"rotating_light", "sos", "warning"}},
	AlertDeliveryAdvance:          {Priority: 3, Tags: []string{"white_check_mark", "rocket"}},
	AlertEmergency:                {Priority: 5, Tags: []string{"rotating_light", "sos"}},
}

// defaultRoute is used for an unknown class so an unmapped alert still sends at
// a sane default priority rather than being dropped.
var defaultRoute = AlertRoute{Priority: 3, Tags: []string{"bell"}}

// RouteFor returns the priority + tags for an alert class.
func RouteFor(class AlertClass) AlertRoute {
	if r, ok := alertRoutes[class]; ok {
		return r
	}
	return defaultRoute
}
