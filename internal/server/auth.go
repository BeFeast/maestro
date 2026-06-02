package server

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/befeast/maestro/internal/config"
)

// authChecker enforces app-level auth on every mutating dashboard endpoint
// (#487, write-path premortem #4). When the configured token is non-empty,
// requests without a matching Authorization header are rejected with 401
// BEFORE the read-only gate, the cautious approval gate, or any state mutation
// runs. When the token is empty, the checker is disabled and handlers behave
// as before — preserving the historical LAN-only deployment until operators
// wire a secret in.
//
// The token is loaded once per Server / FleetServer construction via
// config.ServerAuthConfig.Token() — the operator populates the named env var
// from their secret manager (Infisical, 1Password, etc.). Tests inject a token
// directly via newAuthCheckerForTest / SetAuthForTest.
type authChecker struct {
	enabled   bool
	token     string
	actorName string
}

// newAuthChecker constructs the checker from a server auth config. An empty
// token (TokenEnv unset, env var unset, or env var blank) leaves the checker
// disabled — auth is opt-in per deployment.
func newAuthChecker(cfg config.ServerAuthConfig) authChecker {
	token := cfg.Token()
	return authChecker{
		enabled:   token != "",
		token:     token,
		actorName: cfg.ResolvedActorName(),
	}
}

// newAuthCheckerForTest builds a checker with an explicit token. Internal
// helper used by tests so unit tests do not have to mutate env vars.
func newAuthCheckerForTest(token, actorName string) authChecker {
	token = strings.TrimSpace(token)
	if actorName == "" {
		actorName = "dashboard-authenticated"
	}
	return authChecker{
		enabled:   token != "",
		token:     token,
		actorName: actorName,
	}
}

// Required reports whether the server will reject unauthenticated mutating
// requests.
func (a authChecker) Required() bool { return a.enabled }

// ActorName returns the audit actor recorded for any authenticated request.
func (a authChecker) ActorName() string { return a.actorName }

// Check resolves the actor for a mutating request.
//
//   - When auth is disabled, returns ("", true). Handlers fall back to the
//     request body's actor (preserving the historical "dashboard-anonymous"
//     placeholder when the body omits one).
//   - When auth is enabled AND the request carries a valid Authorization
//     header, returns (actor=authChecker.actorName, true).
//   - When auth is enabled AND the request lacks a valid credential, returns
//     ("", false). The caller MUST respond 401 without touching state. This
//     is the "always 401, never 200/403/405" guarantee in the issue's
//     acceptance criteria.
func (a authChecker) Check(r *http.Request) (actor string, ok bool) {
	if !a.enabled {
		return "", true
	}
	if r == nil {
		return "", false
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return "", false
	}
	lower := strings.ToLower(header)
	switch {
	case strings.HasPrefix(lower, "bearer "):
		presented := strings.TrimSpace(header[len("Bearer "):])
		if constantTimeEqual(presented, a.token) {
			return a.actorName, true
		}
		return "", false
	case strings.HasPrefix(lower, "basic "):
		encoded := strings.TrimSpace(header[len("Basic "):])
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", false
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return "", false
		}
		// Accept the password field matching the token — operators can use
		// a browser's Basic prompt with any username.
		if constantTimeEqual(parts[1], a.token) {
			return a.actorName, true
		}
		return "", false
	}
	return "", false
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// requireAuth is the single entry point every mutating handler calls to
// resolve the actor BEFORE any other work. It writes 401 + WWW-Authenticate
// on failure and returns ok=false; the caller MUST return immediately.
// When auth is disabled, returns ("", true) and the caller proceeds with
// its existing actor-fallback logic.
//
// The WWW-Authenticate challenge advertises Basic first so browsers prompt
// natively (the SPA inherits credentials via the cached realm) and Bearer
// second so curl / scripts / secret-manager-driven clients can attach a
// header directly. Both schemes accept the configured token (Basic
// password field for browser UX; Bearer for programmatic access).
func requireAuth(w http.ResponseWriter, r *http.Request, a authChecker) (actor string, ok bool) {
	actor, ok = a.Check(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="maestro-dashboard", Bearer realm="maestro-dashboard"`)
		writeError(w, http.StatusUnauthorized, "authentication required; provide HTTP Basic credentials (any user, password = token) or Authorization: Bearer <token> loaded from your secret manager")
		return "", false
	}
	return actor, true
}

// authMiddleware wraps a handler so every request — read AND write —
// passes the auth check before any inner handler runs. This is the
// "exposed install" posture (#616): when auth.Required() is true, the
// dashboard, the JSON read endpoints, and the mutating endpoints all
// reject unauthenticated callers with 401 so an attacker on a public
// network cannot enumerate state, harvest issue / PR / actor data, or
// trigger any side effect.
//
// When auth.Required() is false (default, trusted-LAN posture), the
// middleware is a pass-through — behaviour is byte-identical to a build
// without this middleware so existing LAN installs see no regression.
func authMiddleware(next http.Handler, a authChecker) http.Handler {
	if !a.Required() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAuth(w, r, a); !ok {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolveActor returns the final actor recorded in audit. authenticatedActor
// (from requireAuth) wins whenever it is non-empty — that is the issue's
// acceptance criterion that the request body's `actor` field MUST NOT be
// trusted once auth is configured. Fallback order when auth is disabled:
//
//  1. body's actor (kept for backward-compat dashboards without auth wired)
//  2. fallback ("dashboard" or similar) supplied by the handler
func resolveActor(authenticatedActor, bodyActor, fallback string) string {
	if a := strings.TrimSpace(authenticatedActor); a != "" {
		return a
	}
	if a := strings.TrimSpace(bodyActor); a != "" {
		return a
	}
	return fallback
}
