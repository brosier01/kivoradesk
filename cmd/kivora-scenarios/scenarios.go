package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// unique returns a value that has never been seen by the browser cache.
func unique() string { return fmt.Sprint(time.Now().UnixNano()) }

var scenarios = []scenario{
	// Web research and reading.
	{name: "search_web", run: func(r *runner) error {
		if _, err := r.navigate("https://duckduckgo.com/?q=alpine+linux+musl"); err != nil {
			return err
		}
		if _, err := r.call("browser_wait_for", map[string]any{"text": "Alpine", "timeout_seconds": 20}); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		return contains(page, "alpinelinux.org")
	}},
	{name: "read_wikipedia", run: func(r *runner) error {
		if _, err := r.navigate("https://en.wikipedia.org/wiki/Linux"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		return contains(page, "Linus Torvalds", "<untrusted_content")
	}},
	{name: "read_truncated", run: func(r *runner) error {
		if _, err := r.navigate("https://en.wikipedia.org/wiki/Linux"); err != nil {
			return err
		}
		page, err := r.page(2000)
		if err != nil {
			return err
		}
		return contains(page, "[text truncated at 2000 characters]")
	}},
	{name: "follow_link", run: func(r *runner) error {
		if _, err := r.navigate("https://example.com"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		link, err := element(page, `a `)
		if err != nil {
			return err
		}
		out, err := r.call("browser_click", map[string]any{"element": link})
		if err != nil {
			return err
		}
		return contains(out, "iana.org")
	}},
	{name: "links_by_number", run: func(r *runner) error {
		if _, err := r.navigate("https://httpbin.org/links/10/0"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		link, err := element(page, `a "5"`)
		if err != nil {
			return err
		}
		out, err := r.call("browser_click", map[string]any{"element": link})
		if err != nil {
			return err
		}
		return contains(out, "/links/10/5")
	}},
	{name: "back_forward_reload", run: func(r *runner) error {
		for _, u := range []string{"https://example.com", "https://example.org"} {
			if _, err := r.navigate(u); err != nil {
				return err
			}
		}
		out, err := r.call("browser_back", nil)
		if err != nil {
			return err
		}
		if err := contains(out, "example.com"); err != nil {
			return err
		}
		if out, err = r.call("browser_forward", nil); err != nil {
			return err
		}
		if err := contains(out, "example.org"); err != nil {
			return err
		}
		out, err = r.call("browser_reload", nil)
		if err != nil {
			return err
		}
		return contains(out, "example.org")
	}},
	{name: "tabs", run: func(r *runner) error {
		if _, err := r.navigate("https://example.com"); err != nil {
			return err
		}
		if _, err := r.call("browser_new_tab", map[string]any{"url": "https://example.org"}); err != nil {
			return err
		}
		out, err := r.call("browser_tabs", nil)
		if err != nil {
			return err
		}
		if strings.Count(out, `"id"`) < 2 {
			return fmt.Errorf("expected two tabs: %s", oneLine(out))
		}
		var tabs []struct{ ID, URL string }
		if err := json.Unmarshal([]byte(out), &tabs); err != nil {
			return err
		}
		first := ""
		for _, t := range tabs {
			if strings.Contains(t.URL, "example.com") {
				first = t.ID
			}
		}
		if out, err = r.call("browser_switch_tab", map[string]any{"id": first}); err != nil {
			return err
		}
		return contains(out, "example.com")
	}},
	{name: "wait_for_slow_page", run: func(r *runner) error {
		if _, err := r.navigate("https://httpbin.org/delay/2"); err != nil {
			return err
		}
		_, err := r.call("browser_wait_for", map[string]any{"text": "httpbin.org/delay/2", "timeout_seconds": 20})
		return err
	}},
	{name: "redirects", run: func(r *runner) error {
		out, err := r.navigate("https://httpbin.org/redirect/3")
		if err != nil {
			return err
		}
		return contains(out, "httpbin.org/get")
	}},
	{name: "navigate_by_keyboard", run: func(r *runner) error {
		if _, err := r.navigate("https://example.com"); err != nil {
			return err
		}
		marker := unique()
		for _, step := range []struct {
			tool string
			args map[string]any
		}{
			{"computer_key", map[string]any{"keys": "ctrl+l"}},
			{"computer_type", map[string]any{"text": "https://httpbin.org/anything/" + marker + "\n"}},
		} {
			if _, err := r.call(step.tool, step.args); err != nil {
				return err
			}
		}
		_, err := r.call("browser_wait_for", map[string]any{"text": marker, "timeout_seconds": 20})
		return err
	}},

	// Forms and typing.
	{name: "form_fill_and_submit", run: func(r *runner) error {
		if _, err := r.navigate("https://httpbin.org/forms/post"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		fields := map[string]string{`"custname"`: "Ada Lovelace", `"custemail"`: "ada@example.com"}
		for re, text := range fields {
			id, err := element(page, re)
			if err != nil {
				return err
			}
			if _, err := r.call("browser_type", map[string]any{"element": id, "text": text, "clear": true}); err != nil {
				return err
			}
		}
		for _, re := range []string{`\(radio\) "medium"`, `\(checkbox\) "cheese"`, `button "Submit order"`} {
			id, err := element(page, re)
			if err != nil {
				return err
			}
			if _, err := r.call("browser_click", map[string]any{"element": id}); err != nil {
				return err
			}
		}
		echo, err := r.page(0)
		if err != nil {
			return err
		}
		return contains(echo, `"custname": "Ada Lovelace"`, `"size": "medium"`, `"topping": "cheese"`)
	}},
	{name: "browser_type_unicode", run: func(r *runner) error {
		if _, err := r.navigate("https://httpbin.org/forms/post"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		id, err := element(page, `textarea "comments"`)
		if err != nil {
			return err
		}
		text := "Livraison à 18 h, café crème, 5 € — Grüße, 東京, ✓"
		if _, err := r.call("browser_type", map[string]any{"element": id, "text": text, "clear": true}); err != nil {
			return err
		}
		page, err = r.page(0)
		if err != nil {
			return err
		}
		return contains(page, text)
	}},
	{name: "native_type_unicode", run: func(r *runner) error {
		if _, err := r.navigate("https://httpbin.org/forms/post"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		id, err := element(page, `textarea "comments"`)
		if err != nil {
			return err
		}
		if _, err := r.call("browser_click", map[string]any{"element": id}); err != nil {
			return err
		}
		text := "Bonjour, ça coûte 12 € pour l'œuvre « naïve » à Noël."
		if _, err := r.call("computer_type", map[string]any{"text": text}); err != nil {
			return err
		}
		page, err = r.page(0)
		if err != nil {
			return err
		}
		return contains(page, text)
	}},
	{name: "search_box_submit", run: func(r *runner) error {
		if _, err := r.navigate("https://en.wikipedia.org/wiki/Main_Page"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		id, err := element(page, `\((search|searchbox)\)`)
		if err != nil {
			return err
		}
		out, err := r.call("browser_type", map[string]any{"element": id, "text": "Alpine Linux", "submit": true})
		if err != nil {
			return err
		}
		if !strings.Contains(out, "Alpine_Linux") {
			if _, err := r.call("browser_wait_for", map[string]any{"text": "musl", "timeout_seconds": 20}); err != nil {
				return err
			}
		}
		page, err = r.page(0)
		if err != nil {
			return err
		}
		return contains(page, "musl")
	}},

	// Files from the web.
	{name: "save_pdf", run: func(r *runner) error {
		if _, err := r.navigate("https://example.com"); err != nil {
			return err
		}
		out, err := r.call("browser_save_pdf", map[string]any{"filename": "scenario-" + unique()})
		if err != nil {
			return err
		}
		p, err := path(out, ".pdf")
		if err != nil {
			return err
		}
		out, err = r.call("shell_run", map[string]any{"command": fmt.Sprintf("head -c 5 %q; rm -f %q", p, p)})
		if err != nil {
			return err
		}
		return contains(out, "%PDF-")
	}},
	{name: "download_binary", run: func(r *runner) error {
		if _, err := r.navigate("https://httpbin.org/"); err != nil {
			return err
		}
		out, err := r.call("browser_download", map[string]any{"url": "https://httpbin.org/image/png", "filename": "scenario-" + unique() + ".png"})
		if err != nil {
			return err
		}
		p, err := path(out, ".png")
		if err != nil {
			return err
		}
		out, err = r.call("shell_run", map[string]any{"command": fmt.Sprintf("head -c 4 %q | od -An -c; rm -f %q", p, p)})
		if err != nil {
			return err
		}
		return contains(out, "P   N   G")
	}},
	{name: "download_with_cookies", run: func(r *runner) error {
		v := unique()
		if _, err := r.navigate("https://httpbin.org/cookies/set?kivora=" + v); err != nil {
			return err
		}
		out, err := r.call("browser_download", map[string]any{"url": "https://httpbin.org/cookies", "filename": "scenario-cookies-" + v + ".json"})
		if err != nil {
			return err
		}
		p, err := path(out, ".json")
		if err != nil {
			return err
		}
		content, err := r.call("files_read", map[string]any{"path": p})
		r.call("files_delete", map[string]any{"path": p})
		if err != nil {
			return err
		}
		return contains(content, v)
	}},
	{name: "download_then_process", run: func(r *runner) error {
		if _, err := r.navigate("https://httpbin.org/"); err != nil {
			return err
		}
		out, err := r.call("browser_download", map[string]any{"url": "https://httpbin.org/json", "filename": "scenario-" + unique() + ".json"})
		if err != nil {
			return err
		}
		p, err := path(out, ".json")
		if err != nil {
			return err
		}
		out, err = r.call("shell_run", map[string]any{"command": fmt.Sprintf(`grep -o '"title": "[^"]*"' %q | wc -l; rm -f %q`, p, p)})
		if err != nil {
			return err
		}
		return contains(out, "3")
	}},

	// Desktop.
	{name: "screenshot_and_scroll", run: func(r *runner) error {
		if _, err := r.navigate("https://en.wikipedia.org/wiki/Linux"); err != nil {
			return err
		}
		if _, err := r.call("computer_scroll", map[string]any{"x": 640, "y": 500, "direction": "down", "amount": 10}); err != nil {
			return err
		}
		res, err := r.cs.CallTool(r.ctx, &mcp.CallToolParams{Name: "computer_screenshot"})
		if err != nil {
			return err
		}
		if res.IsError || len(res.Content) != 2 {
			return fmt.Errorf("screenshot returned %d contents", len(res.Content))
		}
		return nil
	}},
	{name: "windows_and_focus", run: func(r *runner) error {
		out, err := r.call("computer_windows", nil)
		if err != nil {
			return err
		}
		if err := contains(out, "Chromium"); err != nil {
			return err
		}
		id := strings.Split(strings.Split(out, `"id": "`)[1], `"`)[0]
		if _, err := r.call("computer_focus_window", map[string]any{"id": id}); err != nil {
			return err
		}
		state, err := r.call("computer_get_state", nil)
		if err != nil {
			return err
		}
		return contains(state, `"owner": "agent"`, "Chromium")
	}},

	// Shell and files.
	{name: "shell_pipeline", run: func(r *runner) error {
		out, err := r.call("shell_run", map[string]any{"command": "seq 1 100 | awk '{s+=$1} END {print \"sum=\" s}'"})
		if err != nil {
			return err
		}
		return contains(out, "exit code 0", "sum=5050")
	}},
	{name: "files_roundtrip", run: func(r *runner) error {
		p := "scenario/" + unique() + "/notes.md"
		if _, err := r.call("files_write", map[string]any{"path": p, "content": "# Notes\nKivoraDesk ✓\n"}); err != nil {
			return err
		}
		out, err := r.call("files_read", map[string]any{"path": p})
		if err != nil {
			return err
		}
		if err := contains(out, "KivoraDesk ✓"); err != nil {
			return err
		}
		dir := p[:strings.LastIndex(p, "/")]
		for _, step := range []string{p, dir} {
			if _, err := r.call("files_delete", map[string]any{"path": step}); err != nil {
				return err
			}
		}
		return nil
	}},
	{name: "shell_timeout", run: func(r *runner) error {
		out, err := r.call("shell_run", map[string]any{"command": "sleep 60 & sleep 60", "timeout_seconds": 2})
		if err != nil {
			return err
		}
		return contains(out, "killed after 2s")
	}},
	{name: "worker_isolation", run: func(r *runner) error {
		out, err := r.call("shell_run", map[string]any{"command": "id -un; cat /home/agent/.Xauthority >/dev/null 2>&1 && echo LEAK-X; " +
			"wget -q -T 3 -O- http://127.0.0.1:9222/json/version >/dev/null 2>&1 && echo LEAK-CDP; " +
			"wget -q -T 3 -O- http://192.168.1.1/ >/dev/null 2>&1 && echo LEAK-LAN; " +
			"cat /var/lib/kivora-vault/secrets.json >/dev/null 2>&1 && echo LEAK-VAULT; echo done"})
		if err != nil {
			return err
		}
		if strings.Contains(out, "LEAK") {
			return fmt.Errorf("isolation breach: %s", oneLine(out))
		}
		if _, err := r.refused("files_read", map[string]any{"path": "/etc/shadow"}); err != nil {
			return err
		}
		return contains(out, "worker", "done")
	}},

	// Human control.
	{name: "lock_blocks_agent", run: func(r *runner) error {
		if err := r.setOwner("human"); err != nil {
			return err
		}
		for _, tool := range []struct {
			name string
			args map[string]any
		}{
			{"computer_click", map[string]any{"x": 10, "y": 10}},
			{"browser_navigate", map[string]any{"url": "https://example.com"}},
			{"shell_run", map[string]any{"command": "true"}},
			{"computer_screenshot", nil},
		} {
			if _, err := r.refused(tool.name, tool.args); err != nil {
				return err
			}
		}
		if err := r.setOwner("agent"); err != nil {
			return err
		}
		_, err := r.navigate("https://example.com")
		return err
	}},
	{name: "takeover_request", run: func(r *runner) error {
		out, err := r.call("human_request_takeover", map[string]any{"reason": "Scenario: please hand control back"})
		if err != nil {
			return err
		}
		if err := contains(out, `"paused"`); err != nil {
			return err
		}
		go func() {
			time.Sleep(time.Second)
			r.setOwner("human")
			time.Sleep(500 * time.Millisecond)
			r.setOwner("agent")
		}()
		out, err = r.call("human_wait_for_control", map[string]any{"timeout_seconds": 30})
		if err != nil {
			return err
		}
		return contains(out, "control again")
	}},

	// Persistence, credentials and faults (server scripts).
	{name: "session_survives_browser_restart", host: true, run: func(r *runner) error {
		v := unique()
		if _, err := r.navigate("https://httpbin.org/response-headers?Set-Cookie=kivora_persist%3D" + v + "%3B%20Max-Age%3D3600%3B%20Secure"); err != nil {
			return err
		}
		if _, err := r.navigate("https://httpbin.org/cookies"); err != nil {
			return err
		}
		before, err := r.page(0)
		if err != nil {
			return err
		}
		if err := contains(before, v); err != nil {
			return fmt.Errorf("cookie not set before the restart: %w", err)
		}
		if _, err := r.script("", "ssh.sh", "doas rc-service -q kivora-browser restart"); err != nil {
			return err
		}
		if _, err := r.navigate("https://httpbin.org/cookies"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		return contains(page, v)
	}},
	{name: "secret_fill", host: true, run: func(r *runner) error {
		value := "sc-" + unique() + "@example.com"
		if _, err := r.script(value, "secret.sh", "set", "--field", "text", "scenario-secret", "https://httpbin.org"); err != nil {
			return err
		}
		defer r.script("", "secret.sh", "delete", "scenario-secret")
		if _, err := r.navigate("https://httpbin.org/forms/post"); err != nil {
			return err
		}
		page, err := r.page(0)
		if err != nil {
			return err
		}
		id, err := element(page, `"custemail"`)
		if err != nil {
			return err
		}
		out, err := r.call("secrets_fill", map[string]any{"name": "scenario-secret", "element": id})
		if err != nil {
			return err
		}
		if page, err = r.page(0); err != nil {
			return err
		}
		if strings.Contains(out+page, value) {
			return fmt.Errorf("secret value reached the model")
		}
		submit, err := element(page, `button "Submit order"`)
		if err != nil {
			return err
		}
		if _, err := r.call("browser_click", map[string]any{"element": submit}); err != nil {
			return err
		}
		echo, err := r.page(0)
		if err != nil {
			return err
		}
		return contains(echo, value)
	}},
	{name: "chromium_crash", host: true, run: func(r *runner) error {
		if _, err := r.script("", "ssh.sh", "doas pkill -KILL -f '[/]usr/lib/chromium/chromium'"); err != nil {
			return err
		}
		out, err := r.navigate("https://example.com")
		if err != nil {
			return err
		}
		return contains(out, "example.com")
	}},
	{name: "internet_loss", host: true, run: func(r *runner) error {
		// A temporary firewall table rejects everything leaving the libvirt
		// network: the Internet is gone, the server's SSH link stays up, and
		// no DHCP renewal can undo it.
		if _, err := r.script("", "ssh.sh", cutInternet); err != nil {
			return err
		}
		restored := false
		restore := func() error {
			if restored {
				return nil
			}
			restored = true
			_, err := r.script("", "ssh.sh", restoreInternet)
			return err
		}
		defer restore()
		out, err := r.refused("browser_navigate", map[string]any{"url": "https://example.org/?kivora=" + unique()})
		if err != nil {
			return err
		}
		// Chromium either fails at once or, when it reuses an open connection,
		// waits until the navigation timeout: both are explicit errors.
		if !strings.Contains(out, "net::ERR_") && !strings.Contains(out, "timed out") {
			return fmt.Errorf("no explicit network error: %s", oneLine(out))
		}
		if err := restore(); err != nil {
			return err
		}
		_, err = r.navigate("https://example.org/?kivora=" + unique())
		return err
	}},
}

const (
	cutInternet = `doas nft -f - <<'EOF'
table inet kivoradesk_fault {
	chain output {
		type filter hook output priority filter; policy accept;
		oifname "lo" accept
		ip daddr != 192.168.122.0/24 reject
	}
}
EOF`
	restoreInternet = "doas nft delete table inet kivoradesk_fault"
)
