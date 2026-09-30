//go:build !linux

package aiexecution

import "os"

func newLiveRevisionPin([]byte) (*os.File, error) {
	return nil, Held("controller_lease_platform_unsupported")
}
