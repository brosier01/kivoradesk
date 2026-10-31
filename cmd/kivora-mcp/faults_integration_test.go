//go:build integration

// Fault injection against the VM (PRD scenario 7). It kills Chromium and cuts
// the VM's network, so it only runs with KIVORA_FAULTS=1, on the server that
// hosts the VM:
//
//	KIVORA_FAULTS=1 KIVORA_MCP_COMMAND=... KIVORA_WEB_URL=... KIVORA_WEB_TOKEN=... \
//	go test -tags integration -run Fault ./cmd/kivora-mcp
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func setupFaults(t *testing.T) *e2e {
	if os.Getenv("KIVORA_FAULTS") != "1" {
		t.Skip("set KIVORA_FAULTS=1 to inject faults into the VM")
	}
	return setupE2E(t)
}

// admin runs a command in the VM as the admin account.
func admin(t *testing.T, command string) string {
	t.Helper()
	out, err := exec.Command("../../scripts/vm/ssh.sh", command).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", command, err, out)
	}
	return string(out)
}

func TestFaultChromiumCrash(t *testing.T) {
	e := setupFaults(t)
	e.call("browser_navigate", map[string]any{"url": "https://example.com"}, false)

	admin(t, "doas pkill -KILL -f /usr/lib/chromium/chromium")
	start := time.Now()
	out, _ := e.call("browser_navigate", map[string]any{"url": "https://example.com"}, false)
	e.mustContain(out, "example.com")
	t.Logf("browser usable again %v after the crash", time.Since(start).Round(100*time.Millisecond))

	page, _ := e.call("browser_read_page", nil, false)
	e.mustContain(page, "Example Domain")
}

// TestFaultNetworkLoss removes the VM's default route: the Internet is gone
// while the server's SSH link to the VM (and so the MCP session) stays up.
func TestFaultNetworkLoss(t *testing.T) {
	e := setupFaults(t)
	route := strings.TrimSpace(admin(t, "ip route show default"))
	if !strings.HasPrefix(route, "default via ") {
		t.Fatalf("unexpected default route %q", route)
	}
	restore := func() { exec.Command("../../scripts/vm/ssh.sh", "doas ip route replace "+route).Run() }
	admin(t, "doas ip route del default")
	t.Cleanup(restore)

	// A never visited URL, so that the browser cache cannot answer.
	fresh := fmt.Sprintf("https://example.org/?kivora=%d", time.Now().UnixNano())
	start := time.Now()
	out, _ := e.call("browser_navigate", map[string]any{"url": fresh}, true)
	if !strings.Contains(out, "net::ERR_") && !strings.Contains(out, "timed out") {
		t.Fatalf("navigation without network did not report a network error: %s", out)
	}
	t.Logf("navigation failed after %v: %s", time.Since(start).Round(100*time.Millisecond), firstLine(out))

	out, _ = e.call("shell_run", map[string]any{"command": "curl -sS -m 5 https://example.org", "timeout_seconds": 20}, false)
	if strings.Contains(out, "exit code 0") {
		t.Fatalf("curl succeeded without network: %s", out)
	}

	restore()
	out, _ = e.call("browser_navigate", map[string]any{"url": "https://example.org"}, false)
	e.mustContain(out, "example.org")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
