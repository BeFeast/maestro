package webhook

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"action":"opened"}`)
	valid := Sign(secret, body)

	cases := []struct {
		name   string
		secret string
		body   []byte
		sig    string
		want   bool
	}{
		{"valid", secret, body, valid, true},
		{"wrong secret", "other", body, valid, false},
		{"tampered body", secret, []byte(`{"action":"closed"}`), valid, false},
		{"missing prefix", secret, body, "abcdef", false},
		{"empty header", secret, body, "", false},
		{"empty secret fails closed", "", body, valid, false},
		{"non-hex digest", secret, body, "sha256=zzzz", false},
		{"sha1 header rejected", secret, body, "sha1=" + valid[len("sha256="):], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifySignature(tc.secret, tc.body, tc.sig); got != tc.want {
				t.Fatalf("VerifySignature=%v want %v", got, tc.want)
			}
		})
	}
}

func TestParseEnvelope(t *testing.T) {
	body := readFixture(t, "issues_opened.json")
	env := ParseEnvelope(body)
	if env.Action != "opened" {
		t.Fatalf("action=%q", env.Action)
	}
	if env.Repo != "BeFeast/maestro" {
		t.Fatalf("repo=%q", env.Repo)
	}
	if env.Sender != "octocat" {
		t.Fatalf("sender=%q", env.Sender)
	}
}

func TestParseEnvelopeMalformedIsEmptyNotError(t *testing.T) {
	// A non-JSON body must not panic or block ingestion — the raw payload is
	// stored regardless; only the denormalised envelope is empty.
	env := ParseEnvelope([]byte("not json at all"))
	if env != (Envelope{}) {
		t.Fatalf("want zero envelope for malformed body, got %+v", env)
	}
}

func TestSupported(t *testing.T) {
	for _, ev := range []string{"issues", "label", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "check_run", "check_suite", "status", "projects_v2_item", "ping"} {
		if !Supported(ev) {
			t.Errorf("event %q should be supported", ev)
		}
	}
	if Supported("deployment") {
		t.Error("deployment should not be in the phase-B supported set")
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}
