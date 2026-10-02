//go:build !linux

package aiexecution

func ObserveNativeProcessLaunch(FileProof, string, string, string) (*NativeProcessTermination, int, error) {
	return nil, 0, Held("containment_platform_unsupported")
}
