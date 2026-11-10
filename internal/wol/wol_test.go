package wol

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestPacket(t *testing.T) {
	p, err := Packet("04:d9:f5:cb:75:23")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 102 || !bytes.Equal(p[:6], bytes.Repeat([]byte{0xff}, 6)) {
		t.Fatalf("header %x", p[:6])
	}
	mac := []byte{0x04, 0xd9, 0xf5, 0xcb, 0x75, 0x23}
	for i := range 16 {
		if !bytes.Equal(p[6+6*i:12+6*i], mac) {
			t.Fatalf("repetition %d: %x", i, p[6+6*i:12+6*i])
		}
	}
	for _, bad := range []string{"not a mac", "", "00:11:22:33:44:55:66:77"} {
		if _, err := Packet(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestAddress(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.255":   "192.168.1.255:9",
		"192.168.1.255:7": "192.168.1.255:7",
		"255.255.255.255": "255.255.255.255:9",
	} {
		if got := Address(in); got != want {
			t.Errorf("Address(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSendReachesListener(t *testing.T) {
	ln, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := Send("04:d9:f5:cb:75:23", []string{ln.LocalAddr().String()}, 2, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 200)
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := ln.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Packet("04:d9:f5:cb:75:23")
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("received %x", buf[:n])
	}
}
