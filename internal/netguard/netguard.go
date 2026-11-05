// Package netguard keeps outgoing connections off this computer's own services.
package netguard

import (
	"fmt"
	"net"
	"syscall"
)

// RefuseLocal is a net.Dialer Control function that refuses the addresses of
// this computer. It sees the address after name resolution, so a name pointing
// here does not get through, at a redirect either. Private networks are left to
// the filter on the host, which knows the allowed exceptions.
func RefuseLocal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || IsLocal(ip) {
		return fmt.Errorf("refused: %s is an address of this computer", host)
	}
	return nil
}

// IsLocal reports whether ip is an address of this computer, or one that
// reaches it.
func IsLocal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
