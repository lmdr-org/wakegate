//go:build unix

package wol

import (
	"net"
	"syscall"
)

// setBroadcast allows the socket to send to broadcast addresses (SO_BROADCAST).
func setBroadcast(c *net.UDPConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
