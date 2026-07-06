package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testRSAKeyPEM generates a fresh RSA key and returns it PEM-encoded in the
// requested form ("pkcs1" or "pkcs8") plus the key itself for verification.
func testRSAKeyPEM(t *testing.T, form string) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var block *pem.Block
	switch form {
	case "pkcs8":
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal pkcs8: %v", err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	default:
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	}
	return pem.EncodeToMemory(block), key
}

// writeKeyFile writes PEM bytes to a temp file and returns its path.
func writeKeyFile(t *testing.T, pemBytes []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "app-key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// resetAppAuthForTest clears the process-wide token source and injectables and
// returns a cleanup restoring the defaults. The token source is a package-level
// singleton, so tests must isolate it the way the rate-limit tests isolate the
// primary-limit gate.
func resetAppAuthForTest(t *testing.T) func() {
	t.Helper()
	appTokenMu.Lock()
	origSrc := appTokenSrc
	appTokenSrc = nil
	appTokenMu.Unlock()
	origNow := appTokenNow
	origPost := appTokenHTTPPost
	return func() {
		appTokenMu.Lock()
		appTokenSrc = origSrc
		appTokenMu.Unlock()
		appTokenNow = origNow
		appTokenHTTPPost = origPost
	}
}

// tokenResponse builds a JSON installation-token response body.
func tokenResponse(token string, expiresAt time.Time) []byte {
	b, _ := json.Marshal(installationTokenResponse{Token: token, ExpiresAt: expiresAt})
	return b
}

func TestParseRSAPrivateKey_PKCS1AndPKCS8(t *testing.T) {
	for _, form := range []string{"pkcs1", "pkcs8"} {
		t.Run(form, func(t *testing.T) {
			pemBytes, want := testRSAKeyPEM(t, form)
			got, err := parseRSAPrivateKey(pemBytes)
			if err != nil {
				t.Fatalf("parse %s: %v", form, err)
			}
			if got.N.Cmp(want.N) != 0 {
				t.Fatalf("parsed key modulus mismatch")
			}
		})
	}
}

func TestParseRSAPrivateKey_Garbage(t *testing.T) {
	if _, err := parseRSAPrivateKey([]byte("not a pem")); err == nil {
		t.Fatal("expected error on non-PEM input")
	}
}

func TestSignJWT_ValidSignatureAndClaims(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	_, key := testRSAKeyPEM(t, "pkcs1")
	fixed := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	appTokenNow = func() time.Time { return fixed }

	jwt, err := signJWT(12345, key)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}

	// Verify the RS256 signature over header.payload.
	signingInput := parts[0] + "." + parts[1]
	h := sha256.Sum256([]byte(signingInput))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], sig); err != nil {
		t.Fatalf("signature verification failed: %v", err)
	}

	// Verify header + claims.
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if !strings.Contains(string(header), `"RS256"`) {
		t.Fatalf("header missing RS256: %s", header)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims.Iss != "12345" {
		t.Fatalf("iss = %q, want 12345", claims.Iss)
	}
	// iat is now-60s; exp is iat+jwtLifetime.
	wantIat := fixed.Add(-60 * time.Second).Unix()
	if claims.Iat != wantIat {
		t.Fatalf("iat = %d, want %d", claims.Iat, wantIat)
	}
	if claims.Exp != wantIat+int64(jwtLifetime.Seconds()) {
		t.Fatalf("exp = %d, want %d", claims.Exp, wantIat+int64(jwtLifetime.Seconds()))
	}
	// GitHub rejects JWTs whose exp exceeds iat+10min.
	if claims.Exp-claims.Iat > 600 {
		t.Fatalf("JWT lifetime %ds exceeds GitHub's 10min ceiling", claims.Exp-claims.Iat)
	}
}

func TestConfigureAppAuth_FetchesAndCachesToken(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)
	fixed := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	appTokenNow = func() time.Time { return fixed }

	var posts int
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		posts++
		if !strings.Contains(url, "/app/installations/99/access_tokens") {
			t.Fatalf("unexpected url: %s", url)
		}
		return tokenResponse("ghs_installation_token", fixed.Add(time.Hour)), nil
	}

	if err := ConfigureAppAuth(4242, 99, keyPath); err != nil {
		t.Fatalf("ConfigureAppAuth: %v", err)
	}
	if posts != 1 {
		t.Fatalf("expected 1 initial fetch, got %d", posts)
	}

	// A second appToken() well before expiry serves the cache (no new POST).
	tok, err := appToken()
	if err != nil {
		t.Fatalf("appToken: %v", err)
	}
	if tok != "ghs_installation_token" {
		t.Fatalf("token = %q", tok)
	}
	if posts != 1 {
		t.Fatalf("cache miss: expected 1 fetch total, got %d", posts)
	}

	info := GetAuthInfo()
	if info.Mode != AuthModeApp {
		t.Fatalf("mode = %q, want app", info.Mode)
	}
	if info.InstallationID != 99 || info.AppID != 4242 {
		t.Fatalf("unexpected auth info: %+v", info)
	}
}

func TestAppToken_RefreshBeforeExpiry(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)

	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	appTokenNow = func() time.Time { return now }

	var posts int
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		posts++
		// Each fetch returns a token valid one hour from the current clock.
		return tokenResponse(fmt.Sprintf("token-%d", posts), appTokenNow().Add(time.Hour)), nil
	}
	if err := ConfigureAppAuth(1, 2, keyPath); err != nil {
		t.Fatalf("ConfigureAppAuth: %v", err)
	}
	if posts != 1 {
		t.Fatalf("expected initial fetch, got %d", posts)
	}

	// Advance to within the refresh margin (token expires at +1h, margin 5m).
	now = now.Add(56 * time.Minute)
	tok, err := appToken()
	if err != nil {
		t.Fatalf("appToken: %v", err)
	}
	if posts != 2 {
		t.Fatalf("expected refresh within margin, got %d fetches", posts)
	}
	if tok != "token-2" {
		t.Fatalf("expected refreshed token, got %q", tok)
	}

	// The new token (expiry now+1h) is served from cache immediately after.
	tok, err = appToken()
	if err != nil {
		t.Fatalf("appToken: %v", err)
	}
	if posts != 2 || tok != "token-2" {
		t.Fatalf("expected cached refreshed token, got posts=%d tok=%q", posts, tok)
	}
}

func TestAppToken_ForcedExpiryRefreshes(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	appTokenNow = func() time.Time { return now }

	var posts int
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		posts++
		return tokenResponse(fmt.Sprintf("tok-%d", posts), appTokenNow().Add(time.Hour)), nil
	}
	if err := ConfigureAppAuth(1, 2, keyPath); err != nil {
		t.Fatalf("ConfigureAppAuth: %v", err)
	}

	// Force the clock past the cached token's hard expiry.
	now = now.Add(2 * time.Hour)
	tok, err := appToken()
	if err != nil {
		t.Fatalf("appToken after forced expiry: %v", err)
	}
	if posts != 2 || tok != "tok-2" {
		t.Fatalf("forced-expiry did not refresh: posts=%d tok=%q", posts, tok)
	}
}

func TestAppToken_FallbackOnRefreshFailure(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	appTokenNow = func() time.Time { return now }

	var posts int
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		posts++
		if posts == 1 {
			return tokenResponse("first", now.Add(time.Hour)), nil
		}
		return nil, fmt.Errorf("boom: installation revoked")
	}
	if err := ConfigureAppAuth(1, 2, keyPath); err != nil {
		t.Fatalf("ConfigureAppAuth: %v", err)
	}

	// Force expiry so the next appToken() must refresh — and fail.
	now = now.Add(2 * time.Hour)
	tok, err := appToken()
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if tok != "" {
		t.Fatalf("expected empty token on failure (PAT fallback), got %q", tok)
	}

	// GetAuthInfo reports the fallback with the reason, and mode degrades to PAT.
	info := GetAuthInfo()
	if !info.FallbackActive {
		t.Fatal("expected FallbackActive")
	}
	if info.Mode != AuthModePAT {
		t.Fatalf("mode = %q, want pat during fallback", info.Mode)
	}
	if !strings.Contains(info.LastError, "boom") {
		t.Fatalf("LastError = %q, want boom", info.LastError)
	}
}

func TestConfigureAppAuth_InitialFetchFailureIsError(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		return nil, fmt.Errorf("network down")
	}
	if err := ConfigureAppAuth(1, 2, keyPath); err == nil {
		t.Fatal("expected error when initial token fetch fails")
	}
	// On failure the source is NOT armed — appToken stays on PAT (empty, nil).
	tok, err := appToken()
	if tok != "" || err != nil {
		t.Fatalf("expected PAT fallback after failed setup, got tok=%q err=%v", tok, err)
	}
	if GetAuthInfo().Mode != AuthModePAT {
		t.Fatal("expected PAT mode after failed setup")
	}
}

func TestConfigureAppAuth_DisableRevertsToPAT(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		return tokenResponse("t", time.Now().Add(time.Hour)), nil
	}
	if err := ConfigureAppAuth(1, 2, keyPath); err != nil {
		t.Fatalf("ConfigureAppAuth: %v", err)
	}
	// Tear down with an incomplete config.
	if err := ConfigureAppAuth(0, 0, ""); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	tok, err := appToken()
	if tok != "" || err != nil {
		t.Fatalf("expected PAT after teardown, got tok=%q err=%v", tok, err)
	}
	if GetAuthInfo().Mode != AuthModePAT {
		t.Fatal("expected PAT mode after teardown")
	}
}

func TestConfigureAppAuth_MissingKeyFile(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()
	if err := ConfigureAppAuth(1, 2, "/nonexistent/key.pem"); err == nil {
		t.Fatal("expected error for missing key file")
	}
}

// TestNoAppConfig_ByteIdenticalPATPath asserts that with no App configured,
// appToken() yields the empty token that leaves gh's ambient auth untouched —
// i.e. behavior is byte-identical to the pre-#823 PAT path.
func TestNoAppConfig_ByteIdenticalPATPath(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	tok, err := appToken()
	if tok != "" || err != nil {
		t.Fatalf("expected no token on the PAT path, got tok=%q err=%v", tok, err)
	}
	if info := GetAuthInfo(); info.Mode != AuthModePAT {
		t.Fatalf("mode = %q, want pat", info.Mode)
	}
	if d := authModeDigest(); d != "" {
		t.Fatalf("authModeDigest should be empty on the PAT path, got %q", d)
	}
}

// TestGHApplyAuth_InjectsTokenOnlyWhenConfigured verifies the env-injection
// wiring: GH_TOKEN appears on the gh command exactly when App auth is active.
func TestGHApplyAuth_InjectsTokenOnlyWhenConfigured(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	// PAT path: no GH_TOKEN override injected.
	cmd := ghCommand("api", "rate_limit")
	if hasGHTokenEnv(cmd.Env) {
		t.Fatal("PAT path must not inject GH_TOKEN")
	}

	// App path: GH_TOKEN injected with the installation token.
	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		return tokenResponse("ghs_app_tok", time.Now().Add(time.Hour)), nil
	}
	if err := ConfigureAppAuth(1, 2, keyPath); err != nil {
		t.Fatalf("ConfigureAppAuth: %v", err)
	}
	cmd = ghCommand("api", "rate_limit")
	if !envContains(cmd.Env, "GH_TOKEN=ghs_app_tok") {
		t.Fatalf("App path must inject GH_TOKEN=<installation token>; env=%v", redactEnv(cmd.Env))
	}
}

// TestAuthModeDigest_ShowsBucketWhenApp verifies the hourly journal digest
// fragment names the installation bucket and expiry when App auth is active,
// and the fallback reason when the App token could not be refreshed.
func TestAuthModeDigest_ShowsBucketWhenApp(t *testing.T) {
	cleanup := resetAppAuthForTest(t)
	defer cleanup()

	pemBytes, _ := testRSAKeyPEM(t, "pkcs1")
	keyPath := writeKeyFile(t, pemBytes)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	appTokenNow = func() time.Time { return now }
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		return tokenResponse("t", now.Add(time.Hour)), nil
	}
	if err := ConfigureAppAuth(1, 77, keyPath); err != nil {
		t.Fatalf("ConfigureAppAuth: %v", err)
	}
	d := authModeDigest()
	if !strings.Contains(d, "auth=app") || !strings.Contains(d, "installation=77") || !strings.Contains(d, "bucket=installation") {
		t.Fatalf("digest missing app bucket details: %q", d)
	}

	// Force a failed refresh and confirm the digest flips to the PAT fallback.
	appTokenHTTPPost = func(url, jwt string) ([]byte, error) {
		return nil, fmt.Errorf("revoked")
	}
	now = now.Add(2 * time.Hour)
	if _, err := appToken(); err == nil {
		t.Fatal("expected refresh failure")
	}
	d = authModeDigest()
	if !strings.Contains(d, "auth=pat") || !strings.Contains(d, "app fallback") {
		t.Fatalf("digest missing fallback details: %q", d)
	}
}

func hasGHTokenEnv(env []string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			return true
		}
	}
	return false
}

func envContains(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// redactEnv strips values so a test failure never prints a token.
func redactEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GH_TOKEN=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			out = append(out, strings.SplitN(kv, "=", 2)[0]+"=<redacted>")
			continue
		}
		out = append(out, kv)
	}
	return out
}
