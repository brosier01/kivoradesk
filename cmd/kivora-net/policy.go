package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brosier01/kivoradesk/internal/usernet"
)

// policy decides what the VM may connect to: the Internet, without this server's
// own addresses, plus the private services the human allowed. Both lists are
// files, read again when they change, so a change applies to the next connection.
// While the addresses of this server are unknown, nothing but the allowed services
// is reachable: a public one could be any address.
type policy struct {
	log *slog.Logger

	mu        sync.Mutex
	allowFile watchedFile
	hostFile  watchedFile
	rules     []rule
	host      map[netip.Addr]bool
	hostKnown bool
	warned    map[string]time.Time
}

// rule is an entry of the allowlist. Port 0 stands for any port.
type rule struct {
	prefix netip.Prefix
	port   uint16
}

func newPolicy(log *slog.Logger, allowFile, hostFile string) *policy {
	return &policy{log: log, allowFile: watchedFile{path: allowFile}, hostFile: watchedFile{path: hostFile}}
}

func (p *policy) allow(ip netip.Addr, port uint16) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refresh()
	for _, r := range p.rules {
		if r.prefix.Contains(ip) && (r.port == 0 || r.port == port) {
			return true
		}
	}
	return p.internet(ip)
}

// resolvable reports whether the VM may learn that a name leads to ip: it may
// connect to it, on one port at least.
func (p *policy) resolvable(ip netip.Addr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refresh()
	for _, r := range p.rules {
		if r.prefix.Contains(ip) {
			return true
		}
	}
	return p.internet(ip)
}

func (p *policy) internet(ip netip.Addr) bool {
	return p.hostKnown && !p.host[ip] && usernet.Allowed(ip)
}

func (p *policy) refresh() {
	if data, changed, err := p.allowFile.read(); err != nil {
		p.warn("allowlist not read", "err", err)
	} else if changed {
		var bad []string
		p.rules, bad = parseAllowlist(data)
		for _, entry := range bad {
			p.log.Warn("allowlist entry ignored", "entry", entry)
		}
	}
	data, changed, err := p.hostFile.read()
	switch {
	case err != nil:
		p.hostKnown = false
		p.warn("addresses of this server not read: the VM only reaches its allowed services", "err", err)
	case p.hostFile.path != "" && !p.hostFile.seen:
		p.hostKnown = false
		p.warn("no addresses of this server: the VM only reaches its allowed services; run docker compose up", "file", p.hostFile.path)
	default:
		p.hostKnown = true
		if changed {
			p.host = parseHostAddresses(data)
		}
	}
}

// warn logs at most once a minute per message: the VM decides how often this runs,
// and logs take disk space.
func (p *policy) warn(msg string, args ...any) {
	if now := time.Now(); now.Sub(p.warned[msg]) >= time.Minute {
		if p.warned == nil {
			p.warned = map[string]time.Time{}
		}
		p.warned[msg] = now
		p.log.Warn(msg, args...)
	}
}

// watchedFile tells when a file has changed. A file that does not exist reads as
// empty.
type watchedFile struct {
	path    string
	modTime time.Time
	size    int64
	seen    bool
}

func (f *watchedFile) read() (data string, changed bool, err error) {
	if f.path == "" {
		return "", false, nil
	}
	info, err := os.Stat(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		if !f.seen {
			return "", false, nil
		}
		f.seen = false
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if f.seen && info.ModTime().Equal(f.modTime) && info.Size() == f.size {
		return "", false, nil
	}
	content, err := os.ReadFile(f.path)
	if err != nil {
		return "", false, err
	}
	f.modTime, f.size, f.seen = info.ModTime(), info.Size(), true
	return string(content), true, nil
}

// parseAllowlist reads lines of IP[/BITS][:PORT] [COMMENT], and returns the
// entries it could not read apart.
func parseAllowlist(data string) (rules []rule, bad []string) {
	for line := range strings.Lines(data) {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		r, err := parseRule(fields[0])
		if err != nil {
			bad = append(bad, fields[0])
			continue
		}
		rules = append(rules, r)
	}
	return rules, bad
}

func parseRule(entry string) (rule, error) {
	var r rule
	address, portText, hasPort := strings.Cut(entry, ":")
	if hasPort {
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			return r, fmt.Errorf("invalid port %q", portText)
		}
		r.port = uint16(port)
	}
	if strings.Contains(address, "/") {
		prefix, err := netip.ParsePrefix(address)
		if err != nil {
			return r, err
		}
		r.prefix = prefix.Masked()
	} else {
		ip, err := netip.ParseAddr(address)
		if err != nil {
			return r, err
		}
		r.prefix = netip.PrefixFrom(ip, ip.BitLen())
	}
	if !r.prefix.Addr().Is4() {
		return r, fmt.Errorf("%s is not IPv4", address)
	}
	return r, nil
}

// parseHostAddresses reads the file of writeHostAddresses. The router counts too:
// a public one is the way into the server's own network.
func parseHostAddresses(data string) map[netip.Addr]bool {
	host := map[netip.Addr]bool{}
	for line := range strings.Lines(data) {
		address, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if ip, err := netip.ParseAddr(address); err == nil {
			host[ip] = true
		}
	}
	return host
}

// parsePublicAddresses reads a list of IPv4 addresses, separated by commas or
// spaces.
func parsePublicAddresses(list string) ([]netip.Addr, error) {
	var public []netip.Addr
	for _, field := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		ip, err := netip.ParseAddr(field)
		if err != nil || !ip.Is4() {
			return nil, fmt.Errorf("KIVORA_PUBLIC_ADDRESSES: %q is not an IPv4 address", field)
		}
		public = append(public, ip)
	}
	return public, nil
}

// writeHostAddresses records the IPv4 addresses of the machine this runs on, its
// default router, and the public addresses it was told about. In a container with
// the host's network, this tells the VM's network what it could not see from its
// own container. An address on an interface is found: one that a router or a cloud
// maps to this server is on no interface, and must be given.
func writeHostAddresses(path, public string) error {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	given, err := parsePublicAddresses(public)
	if err != nil {
		return err
	}
	var b strings.Builder
	written := map[string]bool{}
	address := func(ip string) {
		if !written[ip] {
			written[ip] = true
			fmt.Fprintf(&b, "%s address\n", ip)
		}
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if v4 := n.IP.To4(); v4 != nil && !v4.IsLoopback() {
				address(v4.String())
			}
		}
	}
	for _, ip := range given {
		address(ip.String())
	}
	if f, err := os.Open("/proc/net/route"); err == nil {
		defer f.Close()
		if router, ok := defaultRouter(bufio.NewScanner(f)); ok {
			fmt.Fprintf(&b, "%s router\n", router)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".addresses-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// defaultRouter reads the gateway of the default route from /proc/net/route,
// whose addresses are in hexadecimal, least significant byte first.
func defaultRouter(lines *bufio.Scanner) (netip.Addr, bool) {
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		gw, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil || gw == 0 {
			continue
		}
		return netip.AddrFrom4([4]byte{byte(gw), byte(gw >> 8), byte(gw >> 16), byte(gw >> 24)}), true
	}
	return netip.Addr{}, false
}
