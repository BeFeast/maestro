package aiexecution

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// VerifyNativePrelaunchAbsence rejects any surviving monitor claim for an
// explicitly recovered native registration. Absence alone is not launch
// authority: the caller must also hold the registration lock and prove an
// acknowledged pre-launch receipt plus absent exact OS runtime.
func VerifyNativePrelaunchAbsence(pin FileProof, projectID, nativeID, unit string) error {
	p, err := readContainmentProfile(pin)
	if err != nil {
		return err
	}
	if p.ProjectID != projectID || p.UID != uint32(os.Getuid()) || uuid.Validate(nativeID) != nil || filepath.Base(unit) != unit || unit == "" {
		return Held("containment_prelaunch_identity_invalid")
	}
	return verifyNativePrelaunchClaimAbsence(p, nativeID, unit)
}

func verifyNativePrelaunchClaimAbsence(p NativeContainmentProfile, nativeID, unit string) error {
	if verifyOwnedPath(p.ClaimDir, p.UID, true) != nil {
		return Held("containment_claim_store_unsafe")
	}
	for _, suffix := range []string{".launch.json", ".termination.json", ".recovery.lock"} {
		if _, err := os.Lstat(filepath.Join(p.ClaimDir, nativeID+suffix)); !os.IsNotExist(err) {
			return Held("containment_prelaunch_claim_conflict")
		}
	}
	path := filepath.Join(p.ClaimDir, fmt.Sprintf("%d-%d.json", p.NamespaceDev, p.NamespaceIno))
	b, err := readNativeEvidence(path, p.UID, 16<<10)
	if os.IsNotExist(err) {
		return nil
	}
	var old nativeNamespaceClaim
	if err != nil || DecodeStrict(b, &old) != nil || old.Version != 1 || old.NamespaceDev != p.NamespaceDev || old.NamespaceIno != p.NamespaceIno {
		return Held("containment_claim_unavailable")
	}
	if old.NativeSessionID == nativeID || old.Unit == unit || !cgroupEmpty(old.Cgroup) {
		return Held("containment_prelaunch_claim_conflict")
	}
	return nil
}
