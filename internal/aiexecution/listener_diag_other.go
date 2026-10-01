//go:build !linux

package aiexecution

func dualStackListener(string, int) bool { return false }
