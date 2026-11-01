package netguard

import (
	"net"
	"testing"
)

func TestIsLocal(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "127.8.8.8": true, "::1": true, "0.0.0.0": true, "::": true,
		"169.254.169.254": true, "fe80::1": true, "224.0.0.1": true,
		"8.8.8.8": false, "2606:4700:4700::1111": false, "192.168.1.20": false,
	} {
		if got := IsLocal(net.ParseIP(ip)); got != want {
			t.Errorf("IsLocal(%s) = %v, want %v", ip, got, want)
		}
	}
}
