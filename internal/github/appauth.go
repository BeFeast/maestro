// Package github — GitHub App installation-token authentication (#823).
//
// When a GitHubAppConfig (app_id, private_key_path, installation_id) is
// present, the daemon signs a short-lived JWT with the App's RSA private key,
// exchanges it for a 1-hour installation access token via
// POST /app/installations/{id}/access_tokens, and caches the token until it is
// within a refresh margin of expiry. The token is injected into every `gh`
// command via the GH_TOKEN environment variable so the installation gets its
// own 5 000/hr rate-limit bucket, independent of the operator's PAT.
//
// When no App config is set, or when token issuance fails, the package falls
// back to the ambient `gh` auth (PAT / OAuth) with a loud journal log line so
// the operator knows which bucket is in use.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Auth mode — surfaced in diagnostics (#823).
// ---------------------------------------------------------------------------

// AuthMode describes the authentication method the gh wrapper is using.
type AuthMode string

const (
	AuthModePAT AuthMode = "pat" // ambient gh auth (PAT / OAuth)
	AuthModeApp AuthMode = "app" // GitHub App installation token
)

// AuthInfo is a diagnostic snapshot of the current authentication state.
type AuthInfo struct {
	Mode           AuthMode  `json:"mode"`
	InstallationID int64     `json:"installation_id,omitempty"`
	AppID          int64     `json:"app_id,omitempty"`
	TokenExpiry    time.Time `json:"token_expiry,omitempty"`
	LastRefresh    time.Time `json:"last_refresh,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	FallbackActive bool      `json:"fallback_active,omitempty"` // true when App is configured but we fell back to PAT
}

// authModeDigest renders the current auth mode for the hourly REST usage
// journal line so an operator reading journalctl sees which rate-limit bucket
// the daemon is spending against — the App installation's own 5 000/hr bucket
// or the shared PAT (#823). Empty for the plain PAT path so the log is
// unchanged when App auth is not configured.
func authModeDigest() string {
	info := GetAuthInfo()
	switch {
	case info.FallbackActive:
		return fmt.Sprintf("; auth=pat (app fallback: %s)", info.LastError)
	case info.Mode == AuthModeApp:
		exp := "unknown"
		if !info.TokenExpiry.IsZero() {
			exp = info.TokenExpiry.UTC().Format(time.RFC3339)
		}
		return fmt.Sprintf("; auth=app (installation=%d, bucket=installation, token expires %s)",
			info.InstallationID, exp)
	default:
		return ""
	}
}

// GetAuthInfo returns the current authentication diagnostic snapshot.
func GetAuthInfo() AuthInfo {
	appTokenMu.Lock()
	defer appTokenMu.Unlock()
	if appTokenSrc == nil {
		return AuthInfo{Mode: AuthModePAT}
	}
	info := AuthInfo{
		Mode:           AuthModeApp,
		InstallationID: appTokenSrc.installationID,
		AppID:          appTokenSrc.appID,
		TokenExpiry:    appTokenSrc.expiry,
		LastRefresh:    appTokenSrc.lastRefresh,
	}
	if appTokenSrc.lastErr != nil {
		info.LastError = appTokenSrc.lastErr.Error()
		info.FallbackActive = true
		info.Mode = AuthModePAT // currently using PAT as fallback
	}
	return info
}

// ---------------------------------------------------------------------------
// Token source — cached, auto-refreshing installation token.
// ---------------------------------------------------------------------------

// appTokenSource signs JWTs and exchanges them for installation tokens.
// The zero value is not usable; ConfigureAppAuth builds and arms one.
type appTokenSource struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey

	// Cached token state (guarded by appTokenMu, not an internal lock,
	// because the global accessor also reads these fields).
	token       string
	expiry      time.Time
	lastRefresh time.Time
	lastErr     error

	// refreshing/refreshDone serialize installation-token exchange without
	// holding appTokenMu across a network subprocess. A freshness caller can
	// therefore stop waiting on an in-flight refresh when its context expires;
	// the durable delivery execution lease is never stranded behind a mutex
	// owned by a hung, unrelated GitHub poll.
	refreshing  bool
	refreshDone chan struct{}
}

const (
	// jwtLifetime is the validity window for the signed JWT sent to GitHub.
	// GitHub rejects JWTs with exp > iat+10min; we use 9 min for safety.
	jwtLifetime = 9 * time.Minute

	// tokenRefreshMargin is how far before expiry we proactively refresh.
	// Installation tokens live 1 hour; refreshing at 55 min gives 5 min of
	// headroom so a slow exchange does not serve an expired token.
	tokenRefreshMargin = 5 * time.Minute

	// The installation-token response is a tiny JSON object. Bound both wall
	// time and decoded response bytes so a broken/malicious intermediary cannot
	// strand auth refreshes or grow memory without limit.
	appAuthHTTPTimeout       = 30 * time.Second
	appAuthResponseBodyLimit = 1 << 20
)

var appAuthHTTPClient = &http.Client{
	Timeout: appAuthHTTPTimeout,
	// The only valid exchange endpoint is the one we constructed. Refusing
	// redirects prevents forwarding the App JWT to a different origin.
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Process-wide singleton, guarded by appTokenMu.
var (
	appTokenMu          sync.Mutex
	appTokenConfigureMu sync.Mutex
	appTokenSrc         *appTokenSource

	// Indirection for tests.
	appTokenNow             = time.Now
	appTokenHTTPPost        func(url, body string) ([]byte, error)                // nil → real HTTP
	appTokenHTTPPostContext func(context.Context, string, string) ([]byte, error) // nil → real context-aware HTTP
)

// ConfigureAppAuth sets up (or tears down) GitHub App authentication for the
// process. Call with a valid config to enable; call with a zero config (or one
// where Configured() is false) to disable and revert to PAT. Safe to call
// multiple times — the last call wins.
//
// On success the gh wrapper will inject GH_TOKEN=<installation-token> into
// every exec.Command("gh", …) call. On failure the error is logged and the
// PAT path remains active.
func ConfigureAppAuth(appID, installationID int64, privateKeyPath string) error {
	// Serialize config changes separately from token-state reads. The initial
	// network exchange must not hold appTokenMu: a hot-reload with a stalled
	// GitHub call otherwise blocks a post-claim delivery freshness check beyond
	// its context deadline.
	appTokenConfigureMu.Lock()
	defer appTokenConfigureMu.Unlock()

	if appID <= 0 || installationID <= 0 || strings.TrimSpace(privateKeyPath) == "" {
		// Tear down: revert to PAT.
		appTokenMu.Lock()
		appTokenSrc = nil
		appTokenMu.Unlock()
		return nil
	}

	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("github app auth: read private key %s: %w", privateKeyPath, err)
	}
	key, err := parseRSAPrivateKey(keyData)
	if err != nil {
		return fmt.Errorf("github app auth: parse private key %s: %w", privateKeyPath, err)
	}

	src := &appTokenSource{
		appID:          appID,
		installationID: installationID,
		privateKey:     key,
	}

	// Eagerly fetch the first token so a misconfigured App (wrong installation
	// ID, revoked app, transient network failure) surfaces at startup. On
	// failure we still ARM the source with lastErr recorded — but no valid
	// token — so GetAuthInfo()/the digest report the App fallback state and
	// reason instead of a plain PAT path (#823 review). Subsequent gh calls
	// retry the exchange via appToken() and self-heal if the failure was
	// transient; until then appToken() returns an empty token and the gh
	// wrapper stays on PAT, so runtime behavior is byte-identical to today.
	token, expiry, err := src.fetchInstallationToken()
	if err != nil {
		src.lastErr = err
		appTokenMu.Lock()
		appTokenSrc = src
		appTokenMu.Unlock()
		return fmt.Errorf("github app auth: initial token fetch: %w", err)
	}
	now := appTokenNow()
	src.token = token
	src.expiry = expiry
	src.lastRefresh = now
	appTokenMu.Lock()
	appTokenSrc = src
	appTokenMu.Unlock()

	log.Printf("[github] app auth configured: app_id=%d installation_id=%d token_expiry=%s",
		appID, installationID, expiry.UTC().Format(time.RFC3339))
	return nil
}

// appToken returns a valid installation token, refreshing if needed. Returns
// ("", nil) when App auth is not configured (caller should use ambient gh auth).
// Returns ("", err) when App auth IS configured but refresh failed — the caller
// should fall back to PAT and log loudly.
func appToken() (string, error) {
	return appTokenContext(context.Background())
}

// appTokenContext is the cancellation-aware token accessor used by delivery
// freshness reads. Token exchange is single-flight per configured source, but
// the network subprocess runs without appTokenMu held. A caller waiting for an
// unrelated refresh selects on ctx and returns promptly even if that refresh
// is hung; ordinary non-delivery callers retain the legacy background behavior
// through appToken().
func appTokenContext(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		appTokenMu.Lock()
		src := appTokenSrc
		if src == nil {
			appTokenMu.Unlock()
			return "", nil
		}
		now := appTokenNow()
		if src.token != "" && now.Before(src.expiry.Add(-tokenRefreshMargin)) {
			token := src.token
			appTokenMu.Unlock()
			return token, nil
		}
		if src.refreshing {
			done := src.refreshDone
			appTokenMu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-done:
				// Re-read the current source and cache. The completed refresh may
				// have failed or a concurrent config reload may have replaced it.
				continue
			}
		}
		src.refreshing = true
		src.refreshDone = make(chan struct{})
		appTokenMu.Unlock()

		// Token is expired or within the refresh margin. This is the only
		// potentially blocking section and it deliberately owns no global lock.
		token, expiry, err := src.fetchInstallationTokenContext(ctx)

		appTokenMu.Lock()
		current := appTokenSrc == src
		if current {
			if err != nil {
				src.lastErr = err
			} else {
				src.token = token
				src.expiry = expiry
				src.lastRefresh = now
				src.lastErr = nil
			}
		}
		done := src.refreshDone
		src.refreshing = false
		src.refreshDone = nil
		if done != nil {
			close(done)
		}
		appTokenMu.Unlock()

		if !current {
			continue
		}
		if err != nil {
			log.Printf("[github] app auth: token refresh FAILED (falling back to PAT): %v", err)
			return "", err
		}
		log.Printf("[github] app auth: token refreshed, new expiry %s", expiry.UTC().Format(time.RFC3339))
		return token, nil
	}
}

// ---------------------------------------------------------------------------
// JWT signing (RS256) — minimal, dependency-free implementation.
// ---------------------------------------------------------------------------

// signJWT creates a signed JWT (RS256) with the standard GitHub App claims:
//   - iss: the App ID
//   - iat: now - 60s (clock skew buffer)
//   - exp: iat + jwtLifetime
func signJWT(appID int64, key *rsa.PrivateKey) (string, error) {
	now := appTokenNow()
	iat := now.Add(-60 * time.Second)
	exp := iat.Add(jwtLifetime)

	header := base64URLEncode([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64URLEncode([]byte(fmt.Sprintf(
		`{"iss":"%d","iat":%d,"exp":%d}`,
		appID, iat.Unix(), exp.Unix(),
	)))

	signingInput := header + "." + payload
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	return signingInput + "." + base64URLEncode(sig), nil
}

// base64URLEncode is the unpadded base64url encoding per RFC 7515.
func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// parseRSAPrivateKey decodes a PEM-encoded PKCS#1 or PKCS#8 RSA private key.
func parseRSAPrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	// Try PKCS#1 first (RSA PRIVATE KEY), then PKCS#8 (PRIVATE KEY).
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA (got %T)", parsed)
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// Installation token exchange — POST /app/installations/{id}/access_tokens.
// ---------------------------------------------------------------------------

type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// fetchInstallationToken signs a JWT and exchanges it for an installation
// access token via the GitHub API.
func (s *appTokenSource) fetchInstallationToken() (token string, expiry time.Time, err error) {
	return s.fetchInstallationTokenContext(context.Background())
}

func (s *appTokenSource) fetchInstallationTokenContext(ctx context.Context) (token string, expiry time.Time, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	jwt, err := signJWT(s.appID, s.privateKey)
	if err != nil {
		return "", time.Time{}, err
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", s.installationID)

	var respBody []byte
	if appTokenHTTPPostContext != nil {
		respBody, err = appTokenHTTPPostContext(ctx, url, jwt)
	} else if appTokenHTTPPost != nil {
		// Legacy test seam. Production always takes the context-aware path
		// below; tests that exercise cancellation use appTokenHTTPPostContext.
		respBody, err = appTokenHTTPPost(url, jwt)
	} else {
		respBody, err = doAppAuthHTTPPostContext(ctx, url, jwt)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("POST %s: %w", url, err)
	}

	var resp installationTokenResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", time.Time{}, fmt.Errorf("decode installation token response: %w", err)
	}
	if resp.Token == "" {
		return "", time.Time{}, errors.New("installation token response has empty token")
	}

	return resp.Token, resp.ExpiresAt, nil
}

// doAppAuthHTTPPost is the real HTTP POST for installation token exchange.
// It deliberately stays in-process: putting the App JWT in `gh -H ...` would
// expose the bearer through the subprocess argv (and putting it in GH_TOKEN
// would move the same secret to its environment). The request header is kept
// in Go memory and never enters argv, env, logs, or a persisted record.
func doAppAuthHTTPPost(url, jwt string) ([]byte, error) {
	return doAppAuthHTTPPostContext(context.Background(), url, jwt)
}

func doAppAuthHTTPPostContext(ctx context.Context, url, jwt string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return nil, errors.New("create GitHub App token request failed")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := appAuthHTTPClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("GitHub App token request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, appAuthResponseBodyLimit+1))
	if err != nil {
		return nil, errors.New("read GitHub App token response failed")
	}
	if len(body) > appAuthResponseBodyLimit {
		return nil, errors.New("GitHub App token response exceeded safe limit")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("GitHub App token request returned HTTP %d", resp.StatusCode)
	}
	return body, nil
}
