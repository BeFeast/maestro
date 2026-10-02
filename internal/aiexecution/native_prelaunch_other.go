//go:build !linux

package aiexecution

func VerifyNativePrelaunchAbsence(FileProof, string, string, string) error {
	return Held("containment_platform_unsupported")
}
