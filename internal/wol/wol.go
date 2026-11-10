// Package wol sends Wake-on-LAN magic packets.
package wol

import (
	"bytes"
	"fmt"
	"net"
	"time"
)

// DefaultPort is the UDP port magic packets are sent to when an address has none.
const DefaultPort = "9"

// Packet builds the magic packet for mac: six 0xFF bytes, then the address 16 times.
func Packet(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return nil, fmt.Errorf("invalid MAC address %q", mac)
	}
	return append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(hw, 16)...), nil
}

// Address adds the default port to a broadcast address given without one.
func Address(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, DefaultPort)
}

// Send sends the packet over UDP to each address ("192.168.1.255" or
// "192.168.1.255:7"), repeat times, gap apart. It needs no privileges. It fails only if
// no packet could be sent at all.
func Send(mac string, addrs []string, repeat int, gap time.Duration) error {
	pkt, err := Packet(mac)
	if err != nil {
		return err
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return err
	}
	defer conn.Close()
	if uc, ok := conn.(*net.UDPConn); ok {
		if err := setBroadcast(uc); err != nil {
			return fmt.Errorf("enable broadcast: %w", err)
		}
	}
	var lastErr error
	sent := 0
	for i := range repeat {
		for _, addr := range addrs {
			dst, err := net.ResolveUDPAddr("udp4", Address(addr))
			if err != nil {
				lastErr = err
				continue
			}
			if _, err := conn.WriteTo(pkt, dst); err != nil {
				lastErr = err
				continue
			}
			sent++
		}
		if i < repeat-1 {
			time.Sleep(gap)
		}
	}
	if sent == 0 {
		return fmt.Errorf("magic packet not sent: %v", lastErr)
	}
	return nil
}
