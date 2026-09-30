package config

import (
	"testing"
)

func TestRepositoryIdentityOrigins(t *testing.T) {
	forge := ForgeConfig{Kind: ForgeKindForgejo, BaseURL: "https://forge.example:8443/instance"}
	identity, err := forge.RepositoryIdentity("BeFeast/demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{
		"https://forge.example:8443/instance/BeFeast/demo.git",
		"https://FORGE.EXAMPLE:8443/instance/befeast/DEMO",
		"git@forge.example:BeFeast/demo.git",
		"ssh://git@forge.example:2222/BeFeast/demo.git",
	} {
		if !identity.MatchesOrigin(remote, true) {
			t.Errorf("rejected canonical identity %s", remote)
		}
	}
	for _, remote := range []string{
		"https://github.com/BeFeast/demo.git", "https://127.0.0.1:8443/instance/BeFeast/demo.git",
		"http://forge.example:8443/instance/BeFeast/demo.git", "https://forge.example/instance/BeFeast/demo.git",
		"https://forge.example:8443/BeFeast/demo.git", "https://forge.example:8443/INSTANCE/BeFeast/demo.git",
		"https://forge.example:8443/instance/BeFeast/other.git", "https://forge.example:8443/instance/other/demo.git",
		"https://user:secret@forge.example:8443/instance/BeFeast/demo.git",
		"https://forge.example:8443/instance/BeFeast/demo.git?", "https://forge.example:8443/instance/BeFeast/demo.git#",
		"https://forge.example:8443/instance/BeFeast/%64emo.git", "https://forge.example:8443/instance//BeFeast/demo.git",
		"https://forge.example:8443/instance/x/../BeFeast/demo.git", "git@forge.example:/BeFeast/demo.git",
		"ssh://other@forge.example/BeFeast/demo.git", "ssh://git:secret@forge.example/BeFeast/demo.git",
		"ssh://git@forge.example:0/BeFeast/demo.git", "ssh://git@forge.example:/BeFeast/demo.git",
		"ssh://git@forge.example/instance/BeFeast/demo.git", "git://forge.example/BeFeast/demo.git",
		"https://forge.example:8443/instance/BeFeast/demo.git\nhttps://github.com/BeFeast/demo.git",
	} {
		if identity.MatchesOrigin(remote, true) {
			t.Errorf("accepted wrong identity %s", remote)
		}
	}
	github, err := (ForgeConfig{}).RepositoryIdentity("BeFeast/demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"https://github.com/BeFeast/demo.git", "https://GITHUB.COM:443/befeast/demo", "git@github.com:BeFeast/demo.git", "ssh://git@github.com:22/BeFeast/demo.git"} {
		if !github.MatchesOrigin(remote, true) {
			t.Errorf("GitHub canonical identity rejected: %s", remote)
		}
	}
	for _, remote := range []string{"http://github.com/BeFeast/demo.git", "git://github.com/BeFeast/demo.git"} {
		if github.MatchesOrigin(remote, true) || !github.MatchesOrigin(remote, false) {
			t.Errorf("delivery/onboarding transport separation lost: %s", remote)
		}
	}
	for _, remote := range []string{"https://github.com:/BeFeast/demo.git", "https://github.com:8443/BeFeast/demo.git", "ssh://git@github.com:2222/BeFeast/demo.git"} {
		if github.MatchesOrigin(remote, true) {
			t.Errorf("noncanonical GitHub port accepted: %s", remote)
		}
	}
}

func TestCanonicalForgeBase(t *testing.T) {
	for _, base := range []string{"https://FORGE.EXAMPLE:443/instance/", "https://forge.example/instance"} {
		got, err := (ForgeConfig{Kind: ForgeKindForgejo, BaseURL: base}).CanonicalBaseURL()
		if err != nil || got != "https://forge.example/instance" {
			t.Fatalf("base %q => %q, %v", base, got, err)
		}
	}
	for _, base := range []string{"https://forge.example:", "https://forge.example:0", "https://forge.example:65536", "https://u:p@forge.example", "https://forge.example?", "https://forge.example#", "https://forge.example/%61", "https://forge.example/a/../b", "https://forge.example//a", "https://forge.example/api/v1/", "git://forge.example"} {
		if _, err := (ForgeConfig{Kind: ForgeKindForgejo, BaseURL: base}).CanonicalBaseURL(); err == nil {
			t.Errorf("ambiguous base accepted: %s", base)
		}
	}
}

func TestDeliveryDigestCanonicalForgeIdentity(t *testing.T) {
	base := DeliveryConfig{Mode: DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh", LocalPath: "/srv/app"}
	// Fixed receipt of the pre-forge digest: existing GitHub approvals stay valid.
	if got := base.ApprovalDigest(); got != "sha256:dc02da68ca3ee16ec69258a12248cce3b1115b8069f105a3749e18f1592194de" {
		t.Fatalf("legacy GitHub approval digest changed: %s", got)
	}
	explicit := base
	explicit.Forge = ForgeConfig{Kind: ForgeKindGitHub}
	if explicit.ApprovalDigest() != base.ApprovalDigest() {
		t.Fatal("implicit/explicit GitHub identity differs")
	}
	forge := base
	forge.Forge = ForgeConfig{Kind: ForgeKindForgejo, BaseURL: "https://forge.example/instance"}
	for _, fc := range []ForgeConfig{
		{}, {Kind: ForgeKindForgejo, BaseURL: "http://forge.example/instance"},
		{Kind: ForgeKindForgejo, BaseURL: "https://other.example/instance"},
		{Kind: ForgeKindForgejo, BaseURL: "https://forge.example:8443/instance"},
		{Kind: ForgeKindForgejo, BaseURL: "https://forge.example/other"},
		{Kind: ForgeKindForgejo, BaseURL: "https://github.com"},
	} {
		changed := forge
		changed.Forge = fc
		if changed.ApprovalDigest() == forge.ApprovalDigest() {
			t.Errorf("forge identity drift not bound: %+v", fc)
		}
	}
	equivalent := forge
	equivalent.Forge.BaseURL = "https://FORGE.EXAMPLE:443/instance/"
	equivalent.Forge.TokenEnv = "OTHER_TOKEN_NAME"
	if equivalent.ApprovalDigest() != forge.ApprovalDigest() {
		t.Fatal("equivalent source or token env rename invalidates approval")
	}
	t.Setenv("OTHER_TOKEN_NAME", "synthetic-credential-never-persist")
	if equivalent.ApprovalDigest() != forge.ApprovalDigest() {
		t.Fatal("credential rotation invalidates approval")
	}
	for _, cfg := range []Config{{Forge: forge.Forge, Delivery: base}, {Forge: forge.Forge, DeployCmd: "deploy"}, {Forge: forge.Forge}} {
		if cfg.EffectiveDelivery().Forge != forge.Forge {
			t.Fatal("EffectiveDelivery lost configured forge")
		}
	}
}
