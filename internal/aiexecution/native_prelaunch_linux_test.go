package aiexecution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNativePrelaunchClaimsRejectPriorLaunchAndUnknownEvidence(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 with a private TMPDIR for filesystem guards")
	}
	const nativeID = "00000000-0000-4000-8000-000000000001"
	const unit = "maestro-fixture.service"
	for _, kind := range []string{"absent", "launch", "termination", "recovery", "symlink", "namespace_same_native", "namespace_same_unit", "namespace_invalid", "namespace_live_cgroup"} {
		t.Run(kind, func(t *testing.T) {
			p := NativeContainmentProfile{UID: uint32(os.Getuid()), ClaimDir: t.TempDir(), NamespaceDev: 42, NamespaceIno: 43}
			var file string
			data := []byte("{}")
			switch kind {
			case "launch":
				file = nativeID + ".launch.json"
			case "termination":
				file = nativeID + ".termination.json"
			case "recovery":
				file = nativeID + ".recovery.lock"
			case "symlink":
				if err := os.Symlink("/nonexistent", filepath.Join(p.ClaimDir, nativeID+".launch.json")); err != nil {
					t.Fatal(err)
				}
			case "namespace_same_native", "namespace_same_unit", "namespace_invalid", "namespace_live_cgroup":
				file = "42-43.json"
				old := nativeNamespaceClaim{Version: 1, NamespaceDev: 42, NamespaceIno: 43, NativeSessionID: "other", Unit: "other.service", Cgroup: "/"}
				if kind == "namespace_same_native" {
					old.NativeSessionID = nativeID
				}
				if kind == "namespace_same_unit" {
					old.Unit = unit
				}
				if kind != "namespace_invalid" {
					data, _ = json.Marshal(old)
				}
			}
			if file != "" {
				if err := os.WriteFile(filepath.Join(p.ClaimDir, file), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := verifyNativePrelaunchClaimAbsence(p, nativeID, unit)
			if (kind == "absent") != (err == nil) {
				t.Fatalf("claim %s: %v", kind, err)
			}
		})
	}
}
