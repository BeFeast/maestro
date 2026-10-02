//go:build !linux

package aiexecution

import "os"

func MkdirWorkspaceAll(path string, mode os.FileMode) error { return os.MkdirAll(path, mode) }

func ReadWorkspaceFile(path string) ([]byte, error) { return os.ReadFile(path) }
func WriteWorkspaceFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}
