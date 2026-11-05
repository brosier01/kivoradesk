package main

import (
	"bufio"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPolicy(t *testing.T) (p *policy, allowFile, hostFile string) {
	t.Helper()
	dir := t.TempDir()
	allowFile, hostFile = filepath.Join(dir, "allowlist"), filepath.Join(dir, "addresses")
	return newPolicy(slog.New(slog.NewTextHandler(io.Discard, nil)), allowFile, hostFile), allowFile, hostFile
}

// write changes a file so that its modification time differs, whatever the
// resolution of the filesystem's clock.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Duration(len(content)+1) * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyInternetOnlyByDefault(t *testing.T) {
	p, _, _ := testPolicy(t)
	for ip, want := range map[string]bool{
		"93.184.216.34":   true,
		"192.168.1.20":    false,
		"10.86.0.1":       false,
		"172.17.0.1":      false,
		"169.254.169.254": false,
		"127.0.0.1":       false,
	} {
		if got := p.allow(netip.MustParseAddr(ip), 443); got != want {
			t.Errorf("%s: got %v", ip, got)
		}
	}
}

func TestPolicyRefusesThisServer(t *testing.T) {
	p, _, hostFile := testPolicy(t)
	public := netip.MustParseAddr("93.184.216.34")
	if !p.allow(public, 443) {
		t.Fatal("refused before the file exists")
	}
	write(t, hostFile, "93.184.216.34 address\n203.0.113.9 router\n")
	if p.allow(public, 443) {
		t.Error("the VM reaches a public address of this server")
	}
	if router := netip.MustParseAddr("8.8.8.8"); !p.allow(router, 443) {
		t.Error("an unrelated address is refused")
	}
	write(t, hostFile, "203.0.113.9 router\n8.8.4.4 router\n")
	if !p.allow(public, 443) {
		t.Error("the list was not read again")
	}
	if p.allow(netip.MustParseAddr("192.168.1.1"), 443) {
		t.Error("a private address got through")
	}
}

func TestPolicyRouterLineIsNotAnAddress(t *testing.T) {
	p, _, hostFile := testPolicy(t)
	write(t, hostFile, "93.184.216.34 router\n")
	if !p.allow(netip.MustParseAddr("93.184.216.34"), 443) {
		t.Error("a router on the Internet is an address of this server")
	}
}

func TestPolicyAllowlist(t *testing.T) {
	p, allowFile, hostFile := testPolicy(t)
	service := netip.MustParseAddr("192.168.1.20")
	if p.allow(service, 8080) {
		t.Fatal("allowed before the list exists")
	}
	write(t, allowFile, "# comment\n\n192.168.1.20:8080 staging\n10.1.0.0/16\nnonsense\n192.168.1.21:0\n")
	if !p.allow(service, 8080) {
		t.Error("the entry does not apply at once")
	}
	if p.allow(service, 8081) {
		t.Error("another port of the service got through")
	}
	if p.allow(netip.MustParseAddr("192.168.1.21"), 22) {
		t.Error("an invalid entry opened an address")
	}
	if !p.allow(netip.MustParseAddr("10.1.200.3"), 22) {
		t.Error("the range does not apply")
	}
	if p.allow(netip.MustParseAddr("10.2.0.1"), 22) {
		t.Error("outside the range got through")
	}
	write(t, hostFile, "192.168.1.20 address\n")
	if !p.allow(service, 8080) {
		t.Error("an address of this server cannot be allowed by the human")
	}
	if p.allow(service, 8081) {
		t.Error("this server got through on another port")
	}
	if err := os.Remove(allowFile); err != nil {
		t.Fatal(err)
	}
	if p.allow(service, 8080) {
		t.Error("the entry outlives the list")
	}
}

func TestParseAllowlistReportsBadEntries(t *testing.T) {
	rules, bad := parseAllowlist("1.2.3.4\n1.2.3.4/33\n999.1.1.1\n1.2.3.4:70000\n::1\nfe80::/10\n5.6.7.8/8:22 ok\n")
	if len(rules) != 2 {
		t.Errorf("rules: %v", rules)
	}
	if rules[1].prefix != netip.MustParsePrefix("5.0.0.0/8") || rules[1].port != 22 {
		t.Errorf("range: %+v", rules[1])
	}
	if len(bad) != 5 {
		t.Errorf("bad: %v", bad)
	}
}

func TestWriteHostAddresses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "addresses")
	if err := writeHostAddresses(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "127.0.0.1") {
		t.Errorf("loopback in the list: %s", data)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("left files behind: %v", entries)
	}
}

func TestDefaultRouter(t *testing.T) {
	route := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
`
	got, ok := defaultRouter(bufio.NewScanner(strings.NewReader(route)))
	if !ok || got != netip.MustParseAddr("192.168.1.1") {
		t.Errorf("got %v %v", got, ok)
	}
	if _, ok := defaultRouter(bufio.NewScanner(strings.NewReader("eth0 0001A8C0 00000000\n"))); ok {
		t.Error("found a router without a default route")
	}
}
