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

// Every address of this computer counts, not only the well-known ones: a server
// can have a public one.
func TestIsLocalKnowsTheAddressesOfThisComputer(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			checked++
			if !IsLocal(n.IP) {
				t.Errorf("IsLocal(%s) = false for an address of this computer", n.IP)
			}
			address := net.JoinHostPort(n.IP.String(), "443")
			if err := RefuseLocal("tcp", address, nil); err == nil {
				t.Errorf("RefuseLocal let %s through", address)
			}
		}
	}
	if checked == 0 {
		t.Skip("this computer only has loopback addresses")
	}
}

func TestRefuseLocal(t *testing.T) {
	for address, refused := range map[string]bool{
		"127.0.0.1:443": true, "[::1]:443": true, "0.0.0.0:80": true,
		"8.8.8.8:443": false, "not-an-address": true, "example.com:443": true,
	} {
		if err := RefuseLocal("tcp", address, nil); (err != nil) != refused {
			t.Errorf("RefuseLocal(%s) = %v", address, err)
		}
	}
}
