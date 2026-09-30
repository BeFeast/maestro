package aiexecution

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNativeTerminationSnapshotSurvivesLiveProfileRemoval(t *testing.T) {
	id := uuid.NewString()
	profile := FileProof{Path: "/no-longer-installed/profile.json", SHA256: strings.Repeat("a", 64)}
	unit := "maestro-native-" + strings.ReplaceAll(id, "-", "") + ".service"
	proof := NativeProcessTermination{Version: 1, Profile: profile, NativeSessionID: id, Unit: unit, Cgroup: "/system.slice/" + unit, BootID: uuid.NewString(), InvocationID: strings.Repeat("b", 32), StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(), LocalStatus: "succeeded", ExitCode: 0}
	proof.Digest = nativeTerminationDigest(proof)
	if err := ValidateNativeProcessTermination(proof, profile, id, unit); err != nil {
		t.Fatal(err)
	}
	changed := proof
	changed.Unit = "maestro-native-" + strings.Repeat("c", 32) + ".service"
	if err := ValidateNativeProcessTermination(changed, profile, id, unit); err == nil {
		t.Fatal("binding tamper accepted")
	}
	changed = proof
	changed.LocalStatus = "local_output_unknown"
	changed.Digest = nativeTerminationDigest(changed)
	if err := ValidateNativeProcessTermination(changed, profile, id, unit); err == nil {
		t.Fatal("unknown output accepted with exit0")
	}
	changed.ExitCode = -1
	changed.Digest = nativeTerminationDigest(changed)
	if err := ValidateNativeProcessTermination(changed, profile, id, unit); err != nil {
		t.Fatal(err)
	}
}
