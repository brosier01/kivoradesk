//go:build integration

// Tests that need the server's admin scripts (scripts/vm): fault injection
// (PRD scenario 7: Chromium is killed, the VM loses Internet) and secrets.
// They run only with KIVORA_HOST_SCRIPTS=1, on the server that hosts the VM:
//
//	KIVORA_HOST_SCRIPTS=1 KIVORA_MCP_COMMAND=... KIVORA_WEB_URL=... KIVORA_WEB_TOKEN=... \
//	go test -tags integration -run 'Fault|Secret' ./cmd/kivora-mcp
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func setupHost(t *testing.T) *e2e {
	if os.Getenv("KIVORA_HOST_SCRIPTS") != "1" {
		t.Skip("set KIVORA_HOST_SCRIPTS=1 to run tests that use the server's admin scripts")
	}
	return setupE2E(t)
}

// admin runs a command in the VM as the admin account.
func admin(t *testing.T, command string) string {
	t.Helper()
	return script(t, "", "ssh.sh", command)
}

// script runs scripts/vm/NAME with stdin.
func script(t *testing.T, stdin, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command("../../scripts/vm/"+name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return string(out)
}

func TestFaultChromiumCrash(t *testing.T) {
	e := setupHost(t)
	e.call("browser_navigate", map[string]any{"url": "https://example.com"}, false)

	admin(t, "doas pkill -KILL -f '[/]usr/lib/chromium/chromium'")
	start := time.Now()
	out, _ := e.call("browser_navigate", map[string]any{"url": "https://example.com"}, false)
	e.mustContain(out, "example.com")
	t.Logf("browser usable again %v after the crash", time.Since(start).Round(100*time.Millisecond))

	page, _ := e.call("browser_read_page", nil, false)
	e.mustContain(page, "Example Domain")
}

// TestFaultNetworkLoss rejects everything leaving the libvirt network with
// a temporary firewall table (loopback excepted): the Internet is gone while the server's SSH
// link to the VM (and so the MCP session) stays up.
func TestFaultNetworkLoss(t *testing.T) {
	e := setupHost(t)
	restore := func() { exec.Command("../../scripts/vm/ssh.sh", "doas nft delete table inet kivoradesk_fault").Run() }
	admin(t, `doas nft -f - <<'EOF'
table inet kivoradesk_fault {
	chain output {
		type filter hook output priority filter; policy accept;
		oifname "lo" accept
		ip daddr != 192.168.122.0/24 reject
	}
}
EOF`)
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

// TestSecretNeverReachesTheModel stores a random credential bound to
// httpbin.org and checks that the agent can fill it there, cannot use it
// elsewhere, and never gets the value back from any tool or the audit log.
func TestSecretNeverReachesTheModel(t *testing.T) {
	e := setupHost(t)
	value := fmt.Sprintf("Kv-%d-s3cr3t", time.Now().UnixNano())
	script(t, value, "secret.sh", "set", "e2e-secret", "https://httpbin.org")
	t.Cleanup(func() { script(t, "", "secret.sh", "delete", "e2e-secret") })

	leaks := func(out string) {
		t.Helper()
		if strings.Contains(out, value) {
			t.Fatalf("secret value returned to the model:\n%s", out)
		}
	}

	out, _ := e.call("secrets_list", nil, false)
	e.mustContain(out, "e2e-secret")
	leaks(out)

	// A credential bound to another site is refused here.
	script(t, value, "secret.sh", "set", "e2e-other", "https://example.com")
	t.Cleanup(func() { script(t, "", "secret.sh", "delete", "e2e-other") })
	e.call("browser_navigate", map[string]any{"url": "https://httpbin.org/forms/post"}, false)
	page, _ := e.call("browser_read_page", nil, false)
	field := element(t, page, `"custtel"`)
	out, _ = e.call("secrets_fill", map[string]any{"name": "e2e-other", "element": field}, true)
	e.mustContain(out, "may only be used on https://example.com")
	leaks(out)

	// Filled on its own site, hidden from the page reader.
	out, _ = e.call("secrets_fill", map[string]any{"name": "e2e-secret", "element": field}, false)
	leaks(out)
	page, _ = e.call("browser_read_page", nil, false)
	leaks(page)
	out, _ = e.call("browser_read_page", map[string]any{"max_chars": 100000}, false)
	leaks(out)

	// The site received it: httpbin echoes the submitted form.
	e.call("browser_click", map[string]any{"element": element(t, page, `button "Submit order"`)}, false)
	echo, _ := e.call("browser_read_page", nil, false)
	e.mustContain(echo, value)

	if n := strings.TrimSpace(admin(t, "doas grep -rc "+value+" /home/agent/.local/state/kivora/audit | grep -v ':0$' | wc -l")); n != "0" {
		t.Fatalf("secret value found in %s audit file(s)", n)
	}
}

// TestBrowserLockdown checks that local files and browser pages opened from
// the address bar never reach the model, and that the tools keep working.
func TestBrowserLockdown(t *testing.T) {
	e := setupHost(t)
	out, _ := e.call("browser_new_tab", map[string]any{"url": "https://example.com"}, false)
	id := regexp.MustCompile(`Tab (\S+):`).FindStringSubmatch(out)[1]
	t.Cleanup(func() { e.call("browser_close_tab", map[string]any{"id": id}, false) })

	for _, u := range []string{"file:///etc/passwd", "chrome://version", "view-source:https://example.com"} {
		e.call("computer_key", map[string]any{"keys": "ctrl+l"}, false)
		e.call("computer_type", map[string]any{"text": u + "\n"}, false)
		time.Sleep(time.Second)
		out, _ = e.call("browser_read_page", nil, true)
		e.mustContain(out, "not a web page")
	}

	e.call("browser_navigate", map[string]any{"url": "https://example.com"}, false)
	page, _ := e.call("browser_read_page", nil, false)
	e.mustContain(page, "Example Domain")
}
