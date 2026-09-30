//go:build !linux

package aiexecution

func VerifyNativeProcessTermination(FileProof, string, string) (*NativeProcessTermination, error) {
	return nil, Held("containment_platform_unsupported")
}
