package aiexecution

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeForgejoCredentialSHA256IsDomainSeparated(t *testing.T) {
	token := "secret-token"
	got := NativeForgejoCredentialSHA256(token)
	if got == digest([]byte(token)) {
		t.Fatal("credential digest must not equal a plain SHA-256 of the token")
	}
	if got != digest([]byte("maestro-native-forgejo:v1\x00"+token)) {
		t.Fatal("credential digest must match the profile pin derivation")
	}
	if !validDigest(got) {
		t.Fatalf("digest %q is not a lowercase hex SHA-256", got)
	}
}

func TestNativeForgejoAuthorizationsRequiresPinnedManifest(t *testing.T) {
	var hold *Hold
	if _, err := NativeForgejoAuthorizations(Policy{}); !errors.As(err, &hold) || hold.Code != "manifest_drift" {
		t.Fatalf("an unpinned policy must hold on manifest_drift, got %v", err)
	}
	policy := pinManifest(t, Manifest{Version: 1})
	if err := os.WriteFile(policy.ManifestPath, []byte(`{"version":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NativeForgejoAuthorizations(policy); !errors.As(err, &hold) || hold.Code != "manifest_drift" {
		t.Fatalf("a manifest that drifted from its pin must hold on manifest_drift, got %v", err)
	}
}

func TestNativeForgejoAuthorizationsReportsUnverifiableProfiles(t *testing.T) {
	// A profile the manifest pins but whose file is not root-owned
	// containment evidence (here: a plain temp file) is not provably bound.
	// The reader must never guess a denial or an allowance from it.
	profile := []byte(`{"version":1}`)
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, profile, 0600); err != nil {
		t.Fatal(err)
	}
	policy := pinManifest(t, Manifest{Version: 1, Containment: map[string]FileProof{
		"worker": {Path: path, SHA256: digest(profile)},
		"sup-1":  {Path: "/nonexistent/profile.json", SHA256: strings.Repeat("a", 64)},
	}})
	got, err := NativeForgejoAuthorizations(policy)
	if err != nil {
		t.Fatalf("manifest is pinned and well-formed, got %v", err)
	}
	if len(got.Authorizations) != 0 {
		t.Fatalf("unverifiable profiles must bind nothing, got %+v", got.Authorizations)
	}
	if strings.Join(got.Unverified, ",") != "sup-1,worker" {
		t.Fatalf("unverified = %v, want both profiles reported", got.Unverified)
	}
	empty := pinManifest(t, Manifest{Version: 1})
	if got, err := NativeForgejoAuthorizations(empty); err != nil || len(got.Authorizations) != 0 || len(got.Unverified) != 0 {
		t.Fatalf("a manifest without containment profiles binds nothing, got %+v %v", got, err)
	}
}

// stubForgejoAuthorizationEvidence stands in for the root-owned profile and
// evidence files (the only part production reads through
// readRootContainmentEvidence) so the binding rules of
// NativeForgejoAuthorizations are exercised on their positive path.
func stubForgejoAuthorizationEvidence(t *testing.T, byPath map[string]func() (NativeContainmentProfile, nativeForgejoAuthorizationEvidence, bool, error)) {
	t.Helper()
	previous := loadNativeForgejoAuthorizationEvidence
	loadNativeForgejoAuthorizationEvidence = func(pin FileProof) (NativeContainmentProfile, nativeForgejoAuthorizationEvidence, bool, error) {
		load, ok := byPath[pin.Path]
		if !ok {
			return NativeContainmentProfile{}, nativeForgejoAuthorizationEvidence{}, false, Held("containment_profile_invalid")
		}
		return load()
	}
	t.Cleanup(func() { loadNativeForgejoAuthorizationEvidence = previous })
}

func TestNativeForgejoAuthorizationsReportsBoundMergeDenial(t *testing.T) {
	const repo = "BeFeast/example"
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	worker := NativeForgejoCredentialSHA256("worker-token")
	other := NativeForgejoCredentialSHA256("other-token")
	bound := func(credential string, mergeDenied bool, evidenceRepo, evidenceCredential string, expires time.Time) func() (NativeContainmentProfile, nativeForgejoAuthorizationEvidence, bool, error) {
		return func() (NativeContainmentProfile, nativeForgejoAuthorizationEvidence, bool, error) {
			return NativeContainmentProfile{ForgejoRepository: repo, ForgejoTokenSHA256: credential},
				nativeForgejoAuthorizationEvidence{Version: 1, Repository: evidenceRepo, CredentialSHA256: evidenceCredential, WorkerLogin: "native-worker", MergeDenied: mergeDenied, ExpiresAt: expires},
				true, nil
		}
	}
	stubForgejoAuthorizationEvidence(t, map[string]func() (NativeContainmentProfile, nativeForgejoAuthorizationEvidence, bool, error){
		"/p/worker":     bound(worker, true, repo, worker, future),
		"/p/merger":     bound(other, false, repo, other, future),
		"/p/wrong-repo": bound(worker, true, "BeFeast/elsewhere", worker, future),
		"/p/wrong-cred": bound(worker, true, repo, other, future),
		"/p/expired":    bound(worker, true, repo, worker, now),
		"/p/no-forgejo-auth": func() (NativeContainmentProfile, nativeForgejoAuthorizationEvidence, bool, error) {
			return NativeContainmentProfile{}, nativeForgejoAuthorizationEvidence{}, false, nil
		},
	})
	policy := pinManifest(t, Manifest{Version: 1, Containment: map[string]FileProof{
		"worker":     {Path: "/p/worker", SHA256: strings.Repeat("1", 64)},
		"merger":     {Path: "/p/merger", SHA256: strings.Repeat("2", 64)},
		"wrong-repo": {Path: "/p/wrong-repo", SHA256: strings.Repeat("3", 64)},
		"wrong-cred": {Path: "/p/wrong-cred", SHA256: strings.Repeat("4", 64)},
		"expired":    {Path: "/p/expired", SHA256: strings.Repeat("6", 64)},
		"reviewer":   {Path: "/p/no-forgejo-auth", SHA256: strings.Repeat("5", 64)},
	}})
	got, err := nativeForgejoAuthorizationsAt(policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Authorizations) != 2 {
		t.Fatalf("authorizations = %+v, want exactly the two bound profiles", got.Authorizations)
	}
	byKey := map[string]NativeForgejoAuthorization{}
	for _, a := range got.Authorizations {
		byKey[a.ProfileKey] = a
	}
	if a := byKey["worker"]; !a.MergeDenied || a.CredentialSHA256 != worker || a.WorkerLogin != "native-worker" || a.Repository != repo {
		t.Fatalf("worker authorization = %+v, want a bound merge-denied record for the worker credential", a)
	}
	if a := byKey["merger"]; a.MergeDenied || a.CredentialSHA256 != other {
		t.Fatalf("merger authorization = %+v, want a bound merge-allowed record", a)
	}
	// Evidence not bound to its profile (repository or credential mismatch)
	// or past its expiry is never reported as a denial; it is unverified. A
	// profile that pins no forgejo-authorization binds nothing and is fine.
	if strings.Join(got.Unverified, ",") != "expired,wrong-cred,wrong-repo" {
		t.Fatalf("unverified = %v, want the expired and the two unbound profiles", got.Unverified)
	}
}
