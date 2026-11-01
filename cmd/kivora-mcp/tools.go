package main

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/brosier01/kivoradesk/internal/control"
	"github.com/brosier01/kivoradesk/internal/desktop"
	"github.com/brosier01/kivoradesk/internal/secrets"
	"github.com/brosier01/kivoradesk/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	downloadsDir  = "downloads"
	downloadLimit = 500 << 20
)

type none struct{}

type point struct {
	X int `json:"x" jsonschema:"horizontal screen coordinate in pixels, 0 to 1279"`
	Y int `json:"y" jsonschema:"vertical screen coordinate in pixels, 0 to 799"`
}

func (s *server) register(srv *mcp.Server) {
	s.registerComputer(srv)
	s.registerBrowser(srv)
	s.registerShellAndFiles(srv)
	s.registerSecrets(srv)
	s.registerHuman(srv)
}

func (s *server) registerComputer(srv *mcp.Server) {
	add(srv, s, observe, &mcp.Tool{Name: "computer_screenshot",
		Description: "Take a screenshot of the whole 1280x800 screen, mouse pointer included."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			return s.screenshot(ctx, "")
		})

	add(srv, s, observe, &mcp.Tool{Name: "computer_get_state",
		Description: "Describe the desktop: who controls the input, the active window, the screen size and free space in /workspace."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			snap, err := s.ctl.State(ctx)
			if err != nil {
				return nil, err
			}
			state := map[string]any{"control": snap, "screen": map[string]int{"width": s.desk.Width, "height": s.desk.Height}}
			if w, err := s.desk.ActiveWindow(ctx); err == nil {
				state["active_window"] = w
			}
			var st syscall.Statfs_t
			if syscall.Statfs("/workspace", &st) == nil {
				free, total := st.Bavail*uint64(st.Bsize), st.Blocks*uint64(st.Bsize)
				state["workspace_free_bytes"] = free
				if free < 1<<30 || free*10 < total {
					state["workspace_warning"] = "Low disk space in /workspace. Tell the human once, listing the largest items " +
						"(shell_run: du -sh /workspace/* | sort -h | tail), and delete nothing without their confirmation."
				}
			}
			return jsonText(state), nil
		})

	type clickIn struct {
		point
		Button string `json:"button,omitempty" jsonschema:"left (default), middle or right"`
		Clicks int    `json:"clicks,omitempty" jsonschema:"number of clicks, 1 (default) to 3; 2 for a double click"`
	}
	add(srv, s, act, &mcp.Tool{Name: "computer_click",
		Description: "Click at a screen position. Prefer browser_click for elements of a web page."},
		func(ctx context.Context, in clickIn) ([]mcp.Content, error) {
			if err := s.desk.CheckPoint(in.X, in.Y); err != nil {
				return nil, err
			}
			button, ok := map[string]int{"": 1, "left": 1, "middle": 2, "right": 3}[in.Button]
			if !ok {
				return nil, fmt.Errorf("unknown button %q", in.Button)
			}
			clicks := max(in.Clicks, 1)
			if clicks > 3 {
				return nil, fmt.Errorf("clicks must be 1 to 3")
			}
			if err := s.desk.Click(ctx, in.X, in.Y, button, clicks); err != nil {
				return nil, err
			}
			return text("Clicked %s ×%d at (%d, %d).", orDefault(in.Button, "left"), clicks, in.X, in.Y), nil
		})

	add(srv, s, act, &mcp.Tool{Name: "computer_move", Description: "Move the mouse pointer, for example to reveal a hover menu."},
		func(ctx context.Context, in point) ([]mcp.Content, error) {
			if err := s.desk.CheckPoint(in.X, in.Y); err != nil {
				return nil, err
			}
			if err := s.desk.Move(ctx, in.X, in.Y); err != nil {
				return nil, err
			}
			return text("Pointer at (%d, %d).", in.X, in.Y), nil
		})

	type dragIn struct {
		From point `json:"from"`
		To   point `json:"to"`
	}
	add(srv, s, act, &mcp.Tool{Name: "computer_drag", Description: "Drag with the left button from one screen position to another."},
		func(ctx context.Context, in dragIn) ([]mcp.Content, error) {
			for _, p := range []point{in.From, in.To} {
				if err := s.desk.CheckPoint(p.X, p.Y); err != nil {
					return nil, err
				}
			}
			if err := s.desk.Drag(ctx, in.From.X, in.From.Y, in.To.X, in.To.Y); err != nil {
				return nil, err
			}
			return text("Dragged from (%d, %d) to (%d, %d).", in.From.X, in.From.Y, in.To.X, in.To.Y), nil
		})

	type scrollIn struct {
		point
		Direction string `json:"direction" jsonschema:"up, down, left or right"`
		Amount    int    `json:"amount,omitempty" jsonschema:"wheel notches, 1 to 30, default 3"`
	}
	add(srv, s, act, &mcp.Tool{Name: "computer_scroll", Description: "Turn the mouse wheel with the pointer at a screen position."},
		func(ctx context.Context, in scrollIn) ([]mcp.Content, error) {
			if err := s.desk.CheckPoint(in.X, in.Y); err != nil {
				return nil, err
			}
			amount := in.Amount
			if amount == 0 {
				amount = 3
			}
			if amount < 1 || amount > 30 {
				return nil, fmt.Errorf("amount must be 1 to 30")
			}
			if err := s.desk.Scroll(ctx, in.X, in.Y, in.Direction, amount); err != nil {
				return nil, err
			}
			return text("Scrolled %s %d at (%d, %d).", in.Direction, amount, in.X, in.Y), nil
		})

	type typeIn struct {
		Text string `json:"text" jsonschema:"text to type into the focused window; any language; newlines press Enter"`
	}
	add(srv, s, act, &mcp.Tool{Name: "computer_type",
		Description: "Type text with the keyboard into the focused window. In a web page, browser_type is faster and more reliable."},
		func(ctx context.Context, in typeIn) ([]mcp.Content, error) {
			if in.Text == "" {
				return nil, fmt.Errorf("text is empty")
			}
			if utf8.RuneCountInString(in.Text) > 5000 {
				return nil, fmt.Errorf("text is longer than 5000 characters; write it to a file instead")
			}
			snap, err := s.ctl.State(ctx)
			if err != nil {
				return nil, err
			}
			typed := 0
			for _, seg := range desktop.SplitText(in.Text, 20) {
				if err := s.stillAgent(ctx, snap.Seq); err != nil {
					return nil, fmt.Errorf("stopped after %d characters: %v", typed, err)
				}
				if seg.ASCII {
					err = s.desk.TypeASCII(ctx, seg.Text)
				} else {
					err = s.desk.Paste(ctx, seg.Text)
				}
				if err != nil {
					return nil, fmt.Errorf("after %d characters: %v", typed, err)
				}
				typed += utf8.RuneCountInString(seg.Text)
			}
			return text("Typed %d characters.", typed), nil
		})

	type keyIn struct {
		Keys   string `json:"keys" jsonschema:"key or chord in xdotool syntax, e.g. Return, Escape, Tab, BackSpace, Page_Down, ctrl+l, ctrl+shift+t, alt+Left"`
		Repeat int    `json:"repeat,omitempty" jsonschema:"times to press, 1 (default) to 50"`
	}
	add(srv, s, act, &mcp.Tool{Name: "computer_key", Description: "Press a key or a key combination."},
		func(ctx context.Context, in keyIn) ([]mcp.Content, error) {
			repeat := max(in.Repeat, 1)
			if repeat > 50 {
				return nil, fmt.Errorf("repeat must be 1 to 50")
			}
			if err := s.desk.Key(ctx, in.Keys, repeat); err != nil {
				return nil, err
			}
			return text("Pressed %s ×%d.", in.Keys, repeat), nil
		})

	add(srv, s, observe, &mcp.Tool{Name: "computer_windows", Description: "List the visible windows with their ID, title and geometry."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			wins, err := s.desk.Windows(ctx)
			if err != nil {
				return nil, err
			}
			return jsonText(wins), nil
		})

	type focusIn struct {
		ID string `json:"id" jsonschema:"window ID from computer_windows"`
	}
	add(srv, s, act, &mcp.Tool{Name: "computer_focus_window", Description: "Raise and focus a window."},
		func(ctx context.Context, in focusIn) ([]mcp.Content, error) {
			if err := s.desk.Focus(ctx, in.ID); err != nil {
				return nil, err
			}
			return text("Window %s focused.", in.ID), nil
		})
}

func (s *server) registerBrowser(srv *mcp.Server) {
	add(srv, s, observe, &mcp.Tool{Name: "browser_tabs", Description: "List the browser tabs; the one on screen is marked active."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			tabs, err := s.br.Tabs(ctx)
			if err != nil {
				return nil, err
			}
			return jsonText(tabs), nil
		})

	type urlIn struct {
		URL string `json:"url" jsonschema:"http or https URL; a bare host name gets https"`
	}
	add(srv, s, act, &mcp.Tool{Name: "browser_navigate",
		Description: "Open a URL in the visible tab and wait for it to load. To search the web, open a search engine URL such as https://duckduckgo.com/?q=your+terms."},
		func(ctx context.Context, in urlIn) ([]mcp.Content, error) {
			tab, err := s.br.Navigate(ctx, in.URL)
			if err != nil {
				return nil, err
			}
			return text("Loaded. %s\nCall browser_read_page to read it.", tabText(tab)), nil
		})

	add(srv, s, act, &mcp.Tool{Name: "browser_back", Description: "Go back one page in the visible tab."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			tab, err := s.br.History(ctx, -1)
			if err != nil {
				return nil, err
			}
			return text("%s", tabText(tab)), nil
		})

	add(srv, s, act, &mcp.Tool{Name: "browser_forward", Description: "Go forward one page in the visible tab."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			tab, err := s.br.History(ctx, 1)
			if err != nil {
				return nil, err
			}
			return text("%s", tabText(tab)), nil
		})

	add(srv, s, act, &mcp.Tool{Name: "browser_reload", Description: "Reload the visible tab."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			tab, err := s.br.Reload(ctx)
			if err != nil {
				return nil, err
			}
			return text("%s", tabText(tab)), nil
		})

	type newTabIn struct {
		URL string `json:"url,omitempty" jsonschema:"URL to open; empty for a blank tab"`
	}
	add(srv, s, act, &mcp.Tool{Name: "browser_new_tab", Description: "Open a new tab, show it and load a URL."},
		func(ctx context.Context, in newTabIn) ([]mcp.Content, error) {
			tab, err := s.br.NewTab(ctx, in.URL)
			if err != nil {
				return nil, err
			}
			return text("%s", tabText(tab)), nil
		})

	type tabIn struct {
		ID string `json:"id" jsonschema:"tab ID from browser_tabs"`
	}
	add(srv, s, act, &mcp.Tool{Name: "browser_switch_tab", Description: "Show another tab."},
		func(ctx context.Context, in tabIn) ([]mcp.Content, error) {
			tab, err := s.br.SwitchTab(ctx, in.ID)
			if err != nil {
				return nil, err
			}
			return text("%s", tabText(tab)), nil
		})

	add(srv, s, act, &mcp.Tool{Name: "browser_close_tab", Description: "Close a tab."},
		func(ctx context.Context, in tabIn) ([]mcp.Content, error) {
			if err := s.br.CloseTab(ctx, in.ID); err != nil {
				return nil, err
			}
			return text("Tab %s closed.", in.ID), nil
		})

	type readIn struct {
		MaxChars int `json:"max_chars,omitempty" jsonschema:"maximum characters of page text, default 20000, up to 100000"`
	}
	add(srv, s, observe, &mcp.Tool{Name: "browser_read_page",
		Description: "Read the visible tab as text, with its interactive elements (links, buttons, fields) numbered for browser_click and browser_type. Numbers change when the page changes: read again after navigating."},
		func(ctx context.Context, in readIn) ([]mcp.Content, error) {
			maxChars := in.MaxChars
			if maxChars <= 0 {
				maxChars = 20000
			}
			page, err := s.br.ReadPage(ctx, min(maxChars, 100000))
			if err != nil {
				return nil, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "# %s\n%s\n\n%s\n", page.Title, page.URL, page.Text)
			if page.Truncated {
				fmt.Fprintf(&b, "\n[text truncated at %d characters]\n", maxChars)
			}
			b.WriteString("\n## Interactive elements\n")
			for _, e := range page.Elements {
				fmt.Fprintf(&b, "[%d] %s", e.ID, e.Tag)
				if e.Type != "" {
					fmt.Fprintf(&b, " (%s)", e.Type)
				}
				fmt.Fprintf(&b, " %q", e.Label)
				if e.Href != "" {
					fmt.Fprintf(&b, " -> %s", e.Href)
				}
				b.WriteByte('\n')
			}
			return text("%s", untrusted(page.URL, b.String())), nil
		})

	type clickElIn struct {
		Element int `json:"element" jsonschema:"element number from browser_read_page"`
	}
	add(srv, s, act, &mcp.Tool{Name: "browser_click", Description: "Click an element of the visible page, by its number from browser_read_page."},
		func(ctx context.Context, in clickElIn) ([]mcp.Content, error) {
			tab, err := s.br.ClickElement(ctx, in.Element)
			if err != nil {
				return nil, err
			}
			return text("Clicked element %d. %s", in.Element, tabText(tab)), nil
		})

	type btypeIn struct {
		Text    string `json:"text" jsonschema:"text to insert, any language"`
		Element int    `json:"element,omitempty" jsonschema:"element number from browser_read_page; omit to type into the focused element"`
		Clear   bool   `json:"clear,omitempty" jsonschema:"replace the current content of the field"`
		Submit  bool   `json:"submit,omitempty" jsonschema:"press Enter afterwards, e.g. to run a search"`
	}
	add(srv, s, act, &mcp.Tool{Name: "browser_type",
		Description: "Type text into a field of the visible page. Never use it for passwords or payment details: use secrets_fill, or ask the human with human_request_takeover."},
		func(ctx context.Context, in btypeIn) ([]mcp.Content, error) {
			tab, err := s.br.Type(ctx, in.Element, in.Text, in.Clear, in.Submit)
			if err != nil {
				return nil, err
			}
			return text("Typed %d characters. %s", utf8.RuneCountInString(in.Text), tabText(tab)), nil
		})

	type waitIn struct {
		Text           string `json:"text" jsonschema:"text expected on the page"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"1 to 60, default 10"`
	}
	add(srv, s, observe, &mcp.Tool{Name: "browser_wait_for", Description: "Wait until some text appears on the visible page."},
		func(ctx context.Context, in waitIn) ([]mcp.Content, error) {
			timeout := time.Duration(clamp(in.TimeoutSeconds, 10, 1, 60)) * time.Second
			if err := s.br.WaitForText(ctx, in.Text, timeout); err != nil {
				return nil, err
			}
			return text("Found %q.", in.Text), nil
		})

	type pdfIn struct {
		Filename string `json:"filename,omitempty" jsonschema:"file name in /workspace/downloads; default from the page title"`
	}
	add(srv, s, act, &mcp.Tool{Name: "browser_save_pdf", Description: "Save the visible page as a PDF in /workspace/downloads."},
		func(ctx context.Context, in pdfIn) ([]mcp.Content, error) {
			data, tab, err := s.br.PDF(ctx)
			if err != nil {
				return nil, err
			}
			name := in.Filename
			if name == "" {
				name = orDefault(tab.Title, "page")
			}
			if !strings.HasSuffix(strings.ToLower(name), ".pdf") {
				name += ".pdf"
			}
			rel, err := workspace.Create("/workspace", downloadsDir, name, strings.NewReader(string(data)), downloadLimit)
			if err != nil {
				return nil, err
			}
			return text("Saved /workspace/%s (%d bytes) from %s.", rel, len(data), tab.URL), nil
		})

	type dlIn struct {
		URL      string `json:"url" jsonschema:"http or https URL of the file"`
		Filename string `json:"filename,omitempty" jsonschema:"file name in /workspace/downloads; default from the server"`
	}
	add(srv, s, act, &mcp.Tool{Name: "browser_download",
		Description: "Download a file into /workspace/downloads with the browser's cookies, so files behind a login work. Up to 500 MB."},
		func(ctx context.Context, in dlIn) ([]mcp.Content, error) {
			body, suggested, err := s.br.Download(ctx, in.URL, downloadLimit)
			if err != nil {
				return nil, err
			}
			defer body.Close()
			rel, err := workspace.Create("/workspace", downloadsDir, orDefault(in.Filename, suggested), body, downloadLimit)
			if err != nil {
				return nil, err
			}
			return text("Saved /workspace/%s.", rel), nil
		})
}

func (s *server) registerShellAndFiles(srv *mcp.Server) {
	type shellIn struct {
		Command        string `json:"command" jsonschema:"shell command run with /bin/sh in /workspace"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"1 to 600, default 60; the whole process group is killed after it"`
	}
	add(srv, s, act, &mcp.Tool{Name: "shell_run",
		Description: "Run a shell command in the VM as an unprivileged user without access to the screen or browser. Returns the exit code and output (long output keeps its head and tail). Available: busybox, curl, ripgrep, bash."},
		func(ctx context.Context, in shellIn) ([]mcp.Content, error) {
			if strings.TrimSpace(in.Command) == "" {
				return nil, fmt.Errorf("command is empty")
			}
			timeout := time.Duration(clamp(in.TimeoutSeconds, 60, 1, 600)) * time.Second
			res, err := s.work.Exec(ctx, in.Command, timeout, 32<<10)
			if err != nil {
				return nil, err
			}
			status := fmt.Sprintf("exit code %d", res.ExitCode)
			if res.TimedOut {
				status = fmt.Sprintf("killed after %v", timeout)
			}
			return text("%s\n%s", status, untrusted("shell output", res.Output)), nil
		})

	type pathIn struct {
		Path string `json:"path,omitempty" jsonschema:"path inside /workspace, absolute or relative to it; default /workspace"`
	}
	add(srv, s, observe, &mcp.Tool{Name: "files_list", Description: "List a directory of /workspace."},
		func(ctx context.Context, in pathIn) ([]mcp.Content, error) {
			entries, err := s.work.List(ctx, orDefault(in.Path, "/workspace"))
			if err != nil {
				return nil, err
			}
			return jsonText(entries), nil
		})

	type readIn struct {
		Path     string `json:"path" jsonschema:"file inside /workspace"`
		MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"read at most this many bytes, default 100000, up to 1000000"`
	}
	add(srv, s, observe, &mcp.Tool{Name: "files_read", Description: "Read a text file of /workspace."},
		func(ctx context.Context, in readIn) ([]mcp.Content, error) {
			maxBytes := clamp(in.MaxBytes, 100000, 1, 1000000)
			data, truncated, err := s.work.ReadFile(ctx, in.Path, maxBytes)
			if err != nil {
				return nil, err
			}
			if !utf8.Valid(data) && !truncated {
				return nil, fmt.Errorf("%s is not a text file; inspect it with shell_run (e.g. file, pdftotext) or copy it with scp", in.Path)
			}
			body := string(data)
			if truncated {
				body += fmt.Sprintf("\n[truncated at %d bytes]", maxBytes)
			}
			return text("%s", untrusted(in.Path, body)), nil
		})

	type writeIn struct {
		Path    string `json:"path" jsonschema:"file inside /workspace; parent directories are created"`
		Content string `json:"content" jsonschema:"text content; replaces the file"`
	}
	add(srv, s, act, &mcp.Tool{Name: "files_write", Description: "Create or replace a text file in /workspace."},
		func(ctx context.Context, in writeIn) ([]mcp.Content, error) {
			if err := s.work.WriteFile(ctx, in.Path, []byte(in.Content)); err != nil {
				return nil, err
			}
			return text("Wrote %d bytes to %s.", len(in.Content), in.Path), nil
		})

	type delIn struct {
		Path string `json:"path" jsonschema:"file or empty directory inside /workspace"`
	}
	add(srv, s, act, &mcp.Tool{Name: "files_delete", Description: "Delete a file or an empty directory of /workspace."},
		func(ctx context.Context, in delIn) ([]mcp.Content, error) {
			if err := s.work.Delete(ctx, in.Path); err != nil {
				return nil, err
			}
			return text("Deleted %s.", in.Path), nil
		})
}

func (s *server) registerSecrets(srv *mcp.Server) {
	add(srv, s, observe, &mcp.Tool{Name: "secrets_list",
		Description: "List the credentials the human stored (names, the sites where each may be used and the kind of field it goes into). Values are never shown."},
		func(ctx context.Context, _ none) ([]mcp.Content, error) {
			infos, err := s.vault.List(ctx)
			if err != nil {
				return nil, err
			}
			if len(infos) == 0 {
				return text("No credentials stored. Ask the human to add one, or use human_request_takeover."), nil
			}
			return jsonText(infos), nil
		})

	type fillIn struct {
		Name    string `json:"name" jsonschema:"credential name from secrets_list"`
		Element int    `json:"element" jsonschema:"text or password field number from browser_read_page"`
	}
	add(srv, s, act, &mcp.Tool{Name: "secrets_fill",
		Description: "Fill a stored credential into a field of the visible page without revealing it. Refused unless the page is on one of the credential's sites and the field suits the credential: password fields for \"password\" credentials, also login, email and one-time code fields for \"text\" ones. Submit the form yourself afterwards."},
		func(ctx context.Context, in fillIn) ([]mcp.Content, error) {
			tab, err := s.br.FillSecret(ctx, in.Element, func(pageURL string) (secrets.Grant, error) {
				grant, err := s.vault.ValueFor(ctx, in.Name, pageURL)
				if err == nil {
					s.filled.remember(in.Name, grant.Value)
				}
				return grant, err
			})
			if err != nil {
				return nil, err
			}
			return text("Filled %s into element %d. %s", in.Name, in.Element, tabText(tab)), nil
		})
}

func (s *server) registerHuman(srv *mcp.Server) {
	readOnly := false
	type requestIn struct {
		Reason string `json:"reason" jsonschema:"what the human should do, shown on their screen, e.g. 'Log in to example.com with your passkey'"`
	}
	add(srv, s, free, &mcp.Tool{Name: "human_request_takeover",
		Description: "Ask the human to take over the desktop for a step only a person should do (login, SSO, passkey, CAPTCHA, two-factor code, payment, irreversible confirmation). The agent is paused; then call human_wait_for_control.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}},
		func(ctx context.Context, in requestIn) ([]mcp.Content, error) {
			snap, err := s.ctl.RequestHuman(ctx, in.Reason)
			if err != nil {
				return nil, err
			}
			return text("Request shown to the human. Control is now %q. Call human_wait_for_control to wait until it comes back.", snap.Owner), nil
		})

	type waitIn struct {
		TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"1 to 600, default 300"`
	}
	add(srv, s, free, &mcp.Tool{Name: "human_wait_for_control",
		Description: "Wait until the human hands control back to the agent. Returns a fresh screenshot, since the screen may have changed. Call it again if it times out.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, in waitIn) ([]mcp.Content, error) {
			timeout := time.Duration(clamp(in.TimeoutSeconds, 300, 1, 600)) * time.Second
			deadline := time.Now().Add(timeout)
			snap, err := s.ctl.State(ctx)
			for err == nil && snap.Owner != control.Agent {
				left := time.Until(deadline)
				if left <= 0 {
					return text("Still waiting: control is %q. Call human_wait_for_control again to keep waiting.", snap.Owner), nil
				}
				snap, err = s.ctl.Wait(ctx, snap.Seq, left)
			}
			if err != nil {
				return nil, err
			}
			return s.screenshot(ctx, "The agent has control again. The screen may have changed:")
		})
}

func (s *server) screenshot(ctx context.Context, caption string) ([]mcp.Content, error) {
	png, err := s.desk.Screenshot(ctx)
	if err != nil {
		return nil, err
	}
	if caption == "" {
		caption = fmt.Sprintf("Screenshot %dx%d at %s.", s.desk.Width, s.desk.Height, time.Now().UTC().Format(time.RFC3339))
	}
	return []mcp.Content{&mcp.TextContent{Text: caption}, &mcp.ImageContent{Data: png, MIMEType: "image/png"}}, nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// clamp returns def when v is zero, and v bounded to [lo, hi] otherwise.
func clamp(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	return min(max(v, lo), hi)
}
