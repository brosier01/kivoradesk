//go:build integration

// End-to-end test of the MCP server running in the VM, reached over SSH like
// a laptop would, with the web gateway reachable through a tunnel to switch
// control:
//
//	KIVORA_MCP_COMMAND="ssh -i KEY agent@192.168.122.50 kivora-mcp" \
//	KIVORA_WEB_URL=http://localhost:6080 KIVORA_WEB_TOKEN=... \
//	go test -tags integration -run MCP ./cmd/kivora-mcp
package main

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type e2e struct {
	t     *testing.T
	ctx   context.Context
	cs    *mcp.ClientSession
	web   string
	token string
}

func setupE2E(t *testing.T) *e2e {
	command := strings.Fields(os.Getenv("KIVORA_MCP_COMMAND"))
	web, token := os.Getenv("KIVORA_WEB_URL"), os.Getenv("KIVORA_WEB_TOKEN")
	if len(command) == 0 || web == "" || token == "" {
		t.Skip("KIVORA_MCP_COMMAND, KIVORA_WEB_URL and KIVORA_WEB_TOKEN are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	transport := &mcp.CommandTransport{Command: exec.Command(command[0], command[1:]...)}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	e := &e2e{t: t, ctx: ctx, cs: cs, web: web, token: token}
	e.setOwner("agent")
	t.Cleanup(func() { e.setOwner("agent") })
	return e
}

func (e *e2e) setOwner(owner string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.web+"/api/control", strings.NewReader(fmt.Sprintf(`{"owner":%q}`, owner)))
	req.Header.Set("Cookie", "kivora_session="+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		e.t.Fatalf("set owner %s: %v %v", owner, err, resp)
	}
	resp.Body.Close()
}

// call runs a tool and returns its text; wantErr says whether it must fail.
func (e *e2e) call(name string, args map[string]any, wantErr bool) (string, *mcp.CallToolResult) {
	e.t.Helper()
	res, err := e.cs.CallTool(e.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError != wantErr {
		e.t.Fatalf("%s(%v): IsError=%v, want %v: %s", name, args, res.IsError, wantErr, b.String())
	}
	return b.String(), res
}

func (e *e2e) mustContain(out, want string) {
	e.t.Helper()
	if !strings.Contains(out, want) {
		e.t.Fatalf("output lacks %q:\n%s", want, out)
	}
}

// element finds the number of the first element whose line matches re.
func element(t *testing.T, page, re string) int {
	t.Helper()
	m := regexp.MustCompile(`\[(\d+)\] [^\n]*` + re).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no element matching %q in:\n%s", re, page)
	}
	var n int
	fmt.Sscan(m[1], &n)
	return n
}

func TestMCPScenario(t *testing.T) {
	e := setupE2E(t)

	out, _ := e.call("computer_get_state", nil, false)
	e.mustContain(out, `"owner": "agent"`)

	_, res := e.call("computer_screenshot", nil, false)
	img, err := png.Decode(bytes.NewReader(res.Content[1].(*mcp.ImageContent).Data))
	if err != nil || img.Bounds().Dx() != 1280 || img.Bounds().Dy() != 800 {
		t.Fatalf("screenshot: %v %v", err, img.Bounds())
	}

	// Visit a site, read it, follow a link, come back.
	e.call("browser_navigate", map[string]any{"url": "https://example.com"}, false)
	page, _ := e.call("browser_read_page", nil, false)
	e.mustContain(page, "Example Domain")
	e.mustContain(page, "<untrusted_content")
	link := element(t, page, `a `)
	out, _ = e.call("browser_click", map[string]any{"element": link}, false)
	e.mustContain(out, "iana.org")
	out, _ = e.call("browser_back", nil, false)
	e.mustContain(out, "example.com")

	// Fill a form with structured typing, any script.
	e.call("browser_navigate", map[string]any{"url": "https://httpbin.org/forms/post"}, false)
	page, _ = e.call("browser_read_page", nil, false)
	name := element(t, page, `input "custname"`)
	e.call("browser_type", map[string]any{"element": name, "text": "Café Crème € œ 東京", "clear": true}, false)
	page, _ = e.call("browser_read_page", nil, false)
	e.mustContain(page, "Café Crème € œ 東京")

	// Native typing through the keyboard and the clipboard.
	e.call("browser_click", map[string]any{"element": name}, false)
	e.call("computer_key", map[string]any{"keys": "ctrl+a"}, false)
	e.call("computer_type", map[string]any{"text": "Hello, déjà vu: ça coûte 5 € — naïve œuvre"}, false)
	page, _ = e.call("browser_read_page", nil, false)
	e.mustContain(page, "Hello, déjà vu: ça coûte 5 € — naïve œuvre")

	// Save a PDF and download a file into the workspace.
	e.call("browser_navigate", map[string]any{"url": "https://example.com"}, false)
	out, _ = e.call("browser_save_pdf", map[string]any{"filename": "e2e-example"}, false)
	pdf := regexp.MustCompile(`/workspace/(downloads/e2e-example[^ ]*\.pdf)`).FindStringSubmatch(out)
	if pdf == nil {
		t.Fatalf("no PDF path in %q", out)
	}
	out, _ = e.call("shell_run", map[string]any{"command": fmt.Sprintf("head -c 5 %q; echo; id -un", pdf[1])}, false)
	e.mustContain(out, "%PDF-")
	e.mustContain(out, "worker")
	out, _ = e.call("browser_download", map[string]any{"url": "https://example.com/", "filename": "e2e-page.html"}, false)
	html := regexp.MustCompile(`/workspace/(downloads/e2e-page[^ ]*\.html)`).FindStringSubmatch(out)
	if html == nil {
		t.Fatalf("no download path in %q", out)
	}
	out, _ = e.call("files_read", map[string]any{"path": html[1]}, false)
	e.mustContain(out, "Example Domain")

	// Files round trip, and the boundaries of the workspace and the worker.
	e.call("files_write", map[string]any{"path": "e2e/note.md", "content": "# note\n"}, false)
	out, _ = e.call("files_list", map[string]any{"path": "e2e"}, false)
	e.mustContain(out, "note.md")
	e.call("files_read", map[string]any{"path": "/etc/passwd"}, true)
	out, _ = e.call("shell_run", map[string]any{"command": "cat /home/agent/.Xauthority; curl -s -m 3 http://127.0.0.1:9222/json/version; echo exit=$?"}, false)
	if strings.Contains(out, "webSocketDebuggerUrl") || strings.Contains(out, "MIT-MAGIC") {
		t.Fatalf("worker reached the desktop:\n%s", out)
	}
	out, _ = e.call("shell_run", map[string]any{"command": "sleep 30", "timeout_seconds": 1}, false)
	e.mustContain(out, "killed after 1s")
	e.call("shell_run", map[string]any{"command": "rm -rf e2e " + pdf[1] + " " + html[1]}, false)
}

func TestMCPLockAndTakeover(t *testing.T) {
	e := setupE2E(t)

	e.setOwner("human")
	out, _ := e.call("computer_click", map[string]any{"x": 640, "y": 400}, true)
	e.mustContain(out, "not under agent control")
	e.call("computer_screenshot", nil, true)
	e.call("shell_run", map[string]any{"command": "true"}, true)

	e.setOwner("agent")
	out, _ = e.call("human_request_takeover", map[string]any{"reason": "E2E test: please hand control back"}, false)
	e.mustContain(out, `"paused"`)
	out, _ = e.call("browser_navigate", map[string]any{"url": "https://example.com"}, true)
	e.mustContain(out, "E2E test: please hand control back")

	// The human hands control back while the agent waits.
	go func() {
		time.Sleep(time.Second)
		e.setOwner("agent")
	}()
	start := time.Now()
	out, res := e.call("human_wait_for_control", map[string]any{"timeout_seconds": 30}, false)
	e.mustContain(out, "control again")
	if len(res.Content) != 2 || time.Since(start) > 15*time.Second {
		t.Fatalf("wait returned %d contents after %v", len(res.Content), time.Since(start))
	}

	out, _ = e.call("human_wait_for_control", map[string]any{"timeout_seconds": 1}, false)
	e.mustContain(out, "control again") // already the agent's: returns at once
}

// TestMCPTypingReliability types random accented text with computer_type
// and checks it exactly. Runs: KIVORA_TYPE_RUNS (default 20).
func TestMCPTypingReliability(t *testing.T) {
	e := setupE2E(t)
	runs := 20
	fmt.Sscan(os.Getenv("KIVORA_TYPE_RUNS"), &runs)

	e.call("browser_navigate", map[string]any{"url": "https://httpbin.org/forms/post"}, false)
	page, _ := e.call("browser_read_page", nil, false)
	field := element(t, page, `textarea "comments"`)
	charset := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .,;:-_/@#()?!éèàçùâêîôûëïüœÉÀÇ€")
	failures := 0
	for i := 0; i < runs; i++ {
		want := make([]rune, 15+i%25)
		for j := range want {
			want[j] = charset[(i*31+j*17+j*j)%len(charset)]
		}
		e.call("browser_click", map[string]any{"element": field}, false)
		e.call("computer_key", map[string]any{"keys": "ctrl+a"}, false)
		e.call("computer_key", map[string]any{"keys": "BackSpace"}, false)
		e.call("computer_type", map[string]any{"text": string(want)}, false)
		page, _ = e.call("browser_read_page", nil, false)
		got := regexp.MustCompile(`\[\d+\] textarea "([^\n]*)"`).FindStringSubmatch(page)
		if got == nil || got[1] != strings.TrimSpace(string(want)) {
			failures++
			t.Logf("run %d: typed %q, field shows %v", i, string(want), got)
		}
	}
	t.Logf("accented typing: %d/%d exact", runs-failures, runs)
	if failures > 0 {
		t.Fail()
	}
}
