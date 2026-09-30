package aiexecution

import (
	"encoding/json"
	"strings"
	"testing"
)

func oauthBindingFixture() (Manifest, claudeBindingReceipt) {
	m, r := bindingFixture()
	p := &m.Bindings.Credentials[0]
	p.CredentialKind, p.CredentialSHA256 = "authorization-bearer", ""
	p.BindingMode, p.AccountIdentitySHA256 = OAuthAccountBindingMode, strings.Repeat("f", 64)
	o := &r.Inventory.Credentials[0]
	o.CredentialKind, o.BindingMode = p.CredentialKind, p.BindingMode
	o.AccountIdentitySHA256, o.VerifiedGeneration = p.AccountIdentitySHA256, 1
	return m, r
}

func TestOAuthBindingSurvivesVerifiedTokenRefresh(t *testing.T) {
	m, r := oauthBindingFixture()
	for generation, token := range []string{strings.Repeat("1", 64), strings.Repeat("2", 64)} {
		r.Inventory.Credentials[0].CredentialSHA256 = token
		r.Inventory.Credentials[0].VerifiedGeneration = uint64(generation + 1)
		r.Inventory.Credentials[0].AuthGeneration = uint64(generation + 1)
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateClaudeBindingReceipt(m, raw); err != nil {
			t.Fatalf("same-account verified token generation %d rejected: %v", generation+1, err)
		}
	}
	if m.Bindings.Credentials[0].CredentialSHA256 != "" {
		t.Fatal("refresh must not repin the manifest to an expiring token")
	}
}

func TestOAuthBindingRejectsUnprovenOrChangedIdentity(t *testing.T) {
	cases := map[string]func(*Manifest, *claudeBindingReceipt){
		"different account": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].AccountIdentitySHA256 = strings.Repeat("a", 64)
		},
		"missing proof":          func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].VerifiedGeneration = 0 },
		"metadata only":          func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].AccountIdentitySHA256 = "" },
		"unknown outgoing token": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].CredentialSHA256 = "" },
		"invalid outgoing token digest": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].CredentialSHA256 = "not-a-digest"
		},
		"unverified pin":   func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].PinMatches = false },
		"disabled account": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].AuthActive = false },
		"wrong alias":      func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].AccountAlias = "other-account" },
		"wrong model": func(_ *Manifest, r *claudeBindingReceipt) {
			r.Inventory.Credentials[0].Models[0].ModelSHA256 = strings.Repeat("a", 64)
		},
		"wrong mode": func(_ *Manifest, r *claudeBindingReceipt) { r.Inventory.Credentials[0].BindingMode = "" },
		"api key as oauth": func(m *Manifest, r *claudeBindingReceipt) {
			m.Bindings.Credentials[0].CredentialKind = "x-api-key"
			r.Inventory.Credentials[0].CredentialKind = "x-api-key"
		},
		"ambiguous manifest pins": func(m *Manifest, _ *claudeBindingReceipt) {
			m.Bindings.Credentials[0].CredentialSHA256 = strings.Repeat("d", 64)
		},
		"unknown mode": func(m *Manifest, r *claudeBindingReceipt) {
			m.Bindings.Credentials[0].BindingMode = "metadata"
			r.Inventory.Credentials[0].BindingMode = "metadata"
		},
		"exact pin cannot adopt account proof": func(m *Manifest, _ *claudeBindingReceipt) {
			m.Bindings.Credentials[0].BindingMode = ""
			m.Bindings.Credentials[0].AccountIdentitySHA256 = ""
			m.Bindings.Credentials[0].CredentialSHA256 = strings.Repeat("d", 64)
		},
		"wrong instance":     func(_ *Manifest, r *claudeBindingReceipt) { r.ProcessInstanceID = "different" },
		"wrong schema":       func(_ *Manifest, r *claudeBindingReceipt) { r.SchemaVersion = 2 },
		"wrong projection":   func(_ *Manifest, r *claudeBindingReceipt) { r.ProjectionVersion = "other" },
		"configuration only": func(_ *Manifest, r *claudeBindingReceipt) { r.ObservationScope = "configuration_only" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m, r := oauthBindingFixture()
			mutate(&m, &r)
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateClaudeBindingReceipt(m, raw); err == nil {
				t.Fatal("unproven binding accepted")
			}
		})
	}
}

func TestProvisioningBindingValidationPreservesExactMode(t *testing.T) {
	m, r := bindingFixture()
	raw, _ := json.Marshal(r)
	if err := ValidateClaudeBindingReceipt(m, raw); err != nil {
		t.Fatal(err)
	}
	r.Inventory.Credentials[0].CredentialSHA256 = strings.Repeat("f", 64)
	raw, _ = json.Marshal(r)
	if err := ValidateClaudeBindingReceipt(m, raw); err == nil {
		t.Fatal("exact pin silently accepted a changed credential")
	}
}
