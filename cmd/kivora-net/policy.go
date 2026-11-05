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
type policy struct {
	log *slog.Logger

	mu        sync.Mutex
	allowFile watchedFile
	hostFile  watchedFile
	rules     []rule
	host      map[netip.Addr]bool
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
	if data, changed, err := p.allowFile.read(); err != nil {
		p.log.Warn("allowlist not read", "err", err)
	} else if changed {
		var bad []string
		p.rules, bad = parseAllowlist(data)
		for _, entry := range bad {
			p.log.Warn("allowlist entry ignored", "entry", entry)
		}
	}
	if data, changed, err := p.hostFile.read(); err != nil {
		p.log.Warn("addresses of this server not read", "err", err)
	} else if changed {
		p.host = parseHostAddresses(data)
	}
	for _, r := range p.rules {
		if r.prefix.Contains(ip) && (r.port == 0 || r.port == port) {
			return true
		}
	}
	return !p.host[ip] && usernet.Allowed(ip)
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

// parseHostAddresses reads the file of writeHostAddresses.
func parseHostAddresses(data string) map[netip.Addr]bool {
	host := map[netip.Addr]bool{}
	for line := range strings.Lines(data) {
		address, kind, _ := strings.Cut(strings.TrimSpace(line), " ")
		if ip, err := netip.ParseAddr(address); err == nil && kind == "address" {
			host[ip] = true
		}
	}
	return host
}

// writeHostAddresses records the IPv4 addresses of the machine this runs on,
// and its default router. In a container with the host's network, this tells the
// VM's network what it could not see from its own container: a public address of
// the server is an address like any other.
func writeHostAddresses(path string) error {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if v4 := n.IP.To4(); v4 != nil && !v4.IsLoopback() {
				fmt.Fprintf(&b, "%s address\n", v4)
			}
		}
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
