//go:build !linux

package admissioncontrol

import "net"

func verifyPeer(net.Conn, uint32) error { return &Hold{Code: "registration_platform_unsupported"} }
