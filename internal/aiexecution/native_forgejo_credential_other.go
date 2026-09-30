//go:build !linux

package aiexecution

func readNativeForgejoCredential(NativeContainmentProfile) (string, error) {
	return "", Held("containment_platform_unsupported")
}
