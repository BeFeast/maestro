//go:build !linux

package aiexecution

func readRootContainmentEvidence(FileProof) ([]byte, error) {
	return nil, Held("containment_platform_unsupported")
}

func verifyOwnedPath(string, uint32, bool) error { return Held("containment_platform_unsupported") }
func observeContainmentNetwork(NativeContainmentProfile) error {
	return Held("containment_platform_unsupported")
}
func RunNativeMonitor() error { return Held("containment_platform_unsupported") }
func RunNativeEntry() error   { return Held("containment_platform_unsupported") }

func verifyNativeGitGuards(string, uint32) error { return Held("containment_platform_unsupported") }
