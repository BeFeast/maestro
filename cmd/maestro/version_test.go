package main

import "testing"

func TestResolveVersion_LdflagsSet(t *testing.T) {
	original := version
	defer func() { version = original }()

	version = "1.2.3"
	got := resolveVersion()
	if got != "1.2.3" {
		t.Errorf("resolveVersion() = %q, want %q", got, "1.2.3")
	}
}

func TestResolveVersion_Dev(t *testing.T) {
	original := version
	defer func() { version = original }()

	version = "dev"
	got := resolveVersion()
	// In test binary, debug.ReadBuildInfo returns test module info.
	// The result should not be empty and should not be plain "dev"
	// in a VCS-aware build, but we accept any non-empty string.
	if got == "" {
		t.Error("resolveVersion() returned empty string")
	}
}
