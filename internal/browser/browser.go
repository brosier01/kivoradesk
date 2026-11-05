// Package browser drives the visible Chromium through the DevTools Protocol:
// navigation, tabs, reading pages as text and acting on page elements.
//
// It always works on the tab the human sees (the visible one), so that the
// structured tools and the screenshots describe the same desktop. Its scripts
// run in an isolated world, out of reach of the JavaScript of the page.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/brosier01/kivoradesk/internal/cdp"
	"github.com/brosier01/kivoradesk/internal/netguard"
	"github.com/brosier01/kivoradesk/internal/secrets"
)

// StartupWait is how long a call waits for Chromium to come back after a
// crash or a restart.
const StartupWait = 30 * time.Second

// NavigationTimeout bounds a navigation, including the page load.
const NavigationTimeout = 45 * time.Second

// tabProbeTimeout bounds asking a tab whether it is visible. A tab that does
// not answer is busy, or showing a dialog.
const tabProbeTimeout = 3 * time.Second

// Browser is a lazily (re)connected DevTools client.
type Browser struct {
	Endpoint string // DevTools HTTP endpoint, e.g. http://127.0.0.1:9222

	mu    sync.Mutex
	conn  *cdp.Conn
	pages map[string]*cdp.Page // by target ID
}

// Tab describes a browser tab.
type Tab struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// Element is an interactive element found by ReadPage, addressed by ID.
type Element struct {
	ID    int    `json:"id"`
	Tag   string `json:"tag"`
	Type  string `json:"type,omitempty"`
	Label string `json:"label"`
	Href  string `json:"href,omitempty"`
}

// Page is the readable content of the active tab.
type Page struct {
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Text      string    `json:"text"`
	Truncated bool      `json:"truncated"`
	Elements  []Element `json:"elements"`
}

func (b *Browser) connect(ctx context.Context) (*cdp.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return b.conn, nil
	}
	// After a crash, the service manager restarts Chromium within seconds:
	// wait for its DevTools endpoint rather than failing at once.
	var ws string
	var err error
	deadline := time.Now().Add(StartupWait)
	for {
		if ws, err = cdp.BrowserURL(ctx, b.Endpoint); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("browser not reachable after %v (is Chromium running?): %w", StartupWait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	conn, err := cdp.Dial(ctx, ws)
	if err != nil {
		return nil, err
	}
	b.conn, b.pages = conn, make(map[string]*cdp.Page)
	return conn, nil
}

// reset drops the connection after an error; the next call reconnects.
func (b *Browser) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		b.conn.Close()
	}
	b.conn, b.pages = nil, nil
}

type target struct {
	ID    string `json:"targetId"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func (b *Browser) targets(ctx context.Context, conn *cdp.Conn) ([]target, error) {
	var res struct {
		TargetInfos []target `json:"targetInfos"`
	}
	if err := conn.Call(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	var tabs []target
	for _, t := range res.TargetInfos {
		// Browser UI (omnibox popups, extensions) also shows up as pages.
		if t.Type == "page" && !strings.HasPrefix(t.URL, "chrome://omnibox-popup") &&
			!strings.HasPrefix(t.URL, "chrome-untrusted://") && !strings.HasPrefix(t.URL, "chrome-extension://") {
			tabs = append(tabs, t)
		}
	}
	return tabs, nil
}

func (b *Browser) attach(ctx context.Context, conn *cdp.Conn, id string) (*cdp.Page, error) {
	b.mu.Lock()
	p := b.pages[id]
	b.mu.Unlock()
	if p != nil {
		return p, nil
	}
	p, err := conn.Attach(ctx, id)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.pages != nil {
		b.pages[id] = p
	}
	b.mu.Unlock()
	return p, nil
}

// Tabs lists the tabs; the visible one is marked active.
func (b *Browser) Tabs(ctx context.Context) ([]Tab, error) {
	var tabs []Tab
	err := b.retry(ctx, func(conn *cdp.Conn) error {
		ts, err := b.targets(ctx, conn)
		if err != nil {
			return err
		}
		tabs = tabs[:0]
		for _, t := range ts {
			tab := Tab{ID: t.ID, Title: t.Title, URL: t.URL}
			if p, err := b.attach(ctx, conn, t.ID); err == nil {
				tab.Active, _ = visible(ctx, p)
			}
			tabs = append(tabs, tab)
		}
		return nil
	})
	return tabs, err
}

// active returns the visible tab, or the first tab if none reports visible.
func (b *Browser) active(ctx context.Context, conn *cdp.Conn) (*cdp.Page, error) {
	ts, err := b.targets(ctx, conn)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, errors.New("no browser tab is open; use browser_new_tab")
	}
	var first *cdp.Page
	stuck := false
	for _, t := range ts {
		p, err := b.attach(ctx, conn, t.ID)
		if err != nil {
			continue
		}
		if first == nil {
			first = p
		}
		ok, err := visible(ctx, p)
		if ok {
			return p, nil
		}
		stuck = stuck || errors.Is(err, context.DeadlineExceeded)
	}
	if stuck {
		return nil, errors.New("a browser tab does not respond, perhaps because of a dialog: take a screenshot and use computer_key, or close it with browser_close_tab")
	}
	if first == nil {
		return nil, errors.New("could not attach to any tab")
	}
	return first, nil
}

func visible(ctx context.Context, p *cdp.Page) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, tabProbeTimeout)
	defer cancel()
	var state string
	if err := p.EvalIsolated(ctx, "document.visibilityState", &state); err != nil {
		return false, err
	}
	return state == "visible", nil
}

// retry runs fn, reconnecting once if the connection was lost (Chromium
// restarted, tab closed).
func (b *Browser) retry(ctx context.Context, fn func(*cdp.Conn) error) error {
	for attempt := 0; ; attempt++ {
		conn, err := b.connect(ctx)
		if err != nil {
			return err
		}
		err = fn(conn)
		if err == nil || attempt == 1 || ctx.Err() != nil || !retryable(err) {
			return err
		}
		b.reset()
	}
}

func retryable(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "connection closed") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "No session with given id") ||
		strings.Contains(msg, "No target with given id") || strings.Contains(msg, "Session with given id not found")
}

func (b *Browser) withActive(ctx context.Context, fn func(*cdp.Conn, *cdp.Page) error) error {
	return b.retry(ctx, func(conn *cdp.Conn) error {
		p, err := b.active(ctx, conn)
		if err != nil {
			return err
		}
		return fn(conn, p)
	})
}

// Navigate loads url in the active tab and waits for it to load.
func (b *Browser) Navigate(ctx context.Context, rawURL string) (Tab, error) {
	u, err := checkURL(rawURL)
	if err != nil {
		return Tab{}, err
	}
	err = b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		nctx, cancel := context.WithTimeout(ctx, NavigationTimeout)
		defer cancel()
		if err := p.Navigate(nctx, u); err != nil {
			if errors.Is(nctx.Err(), context.DeadlineExceeded) {
				// Left pending, it keeps the tab from answering, even once the network is back.
				sctx, cancel := context.WithTimeout(ctx, tabProbeTimeout)
				defer cancel()
				p.Call(sctx, "Page.stopLoading", nil, nil)
				return fmt.Errorf("navigation to %s timed out after %v (network down or site unreachable?)", u, NavigationTimeout)
			}
			return err
		}
		return waitLoaded(ctx, p)
	})
	if err != nil {
		return Tab{}, err
	}
	return b.Current(ctx)
}

// History goes back (-1) or forward (+1) in the active tab.
func (b *Browser) History(ctx context.Context, delta int) (Tab, error) {
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		var res struct {
			CurrentIndex int `json:"currentIndex"`
			Entries      []struct {
				ID int `json:"id"`
			} `json:"entries"`
		}
		if err := p.Call(ctx, "Page.getNavigationHistory", nil, &res); err != nil {
			return err
		}
		i := res.CurrentIndex + delta
		if i < 0 || i >= len(res.Entries) {
			return errors.New("no page in that direction of the history")
		}
		if err := p.Call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": res.Entries[i].ID}, nil); err != nil {
			return err
		}
		return waitLoaded(ctx, p)
	})
	if err != nil {
		return Tab{}, err
	}
	return b.Current(ctx)
}

// Reload reloads the active tab.
func (b *Browser) Reload(ctx context.Context) (Tab, error) {
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		if err := p.Call(ctx, "Page.reload", nil, nil); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
		return waitLoaded(ctx, p)
	})
	if err != nil {
		return Tab{}, err
	}
	return b.Current(ctx)
}

// Current describes the active tab.
func (b *Browser) Current(ctx context.Context) (Tab, error) {
	var tab Tab
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		var v struct{ Title, URL string }
		if err := p.EvalIsolated(ctx, `({Title: document.title, URL: location.href})`, &v); err != nil {
			return err
		}
		tab = Tab{ID: p.TargetID, Title: v.Title, URL: v.URL, Active: true}
		return nil
	})
	return tab, err
}

// NewTab opens url in a new tab and shows it.
func (b *Browser) NewTab(ctx context.Context, rawURL string) (Tab, error) {
	u := "about:blank"
	if rawURL != "" {
		var err error
		if u, err = checkURL(rawURL); err != nil {
			return Tab{}, err
		}
	}
	err := b.retry(ctx, func(conn *cdp.Conn) error {
		p, err := conn.NewPage(ctx, u)
		if err != nil {
			return err
		}
		b.mu.Lock()
		if b.pages != nil {
			b.pages[p.TargetID] = p
		}
		b.mu.Unlock()
		if err := conn.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": p.TargetID}, nil); err != nil {
			return err
		}
		return waitLoaded(ctx, p)
	})
	if err != nil {
		return Tab{}, err
	}
	return b.Current(ctx)
}

// SwitchTab shows the tab with the given ID.
func (b *Browser) SwitchTab(ctx context.Context, id string) (Tab, error) {
	err := b.retry(ctx, func(conn *cdp.Conn) error {
		return conn.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": id}, nil)
	})
	if err != nil {
		return Tab{}, err
	}
	time.Sleep(200 * time.Millisecond)
	return b.Current(ctx)
}

// CloseTab closes the tab with the given ID.
func (b *Browser) CloseTab(ctx context.Context, id string) error {
	return b.retry(ctx, func(conn *cdp.Conn) error {
		return conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": id}, nil)
	})
}

// readPageJS numbers the visible interactive elements and returns the page
// text. %d is the maximum text length. The numbers live in the isolated world:
// the page can neither see them nor give them to other elements.
const readPageJS = `(() => {
  const maxText = %d;
  const kivora = globalThis.kivora ??= {
    elements: new Map(),
    secrets: new WeakSet(),
    get(id) {
      const el = this.elements.get(id);
      return el && el.isConnected ? el : null;
    },
  };
  kivora.elements.clear();
  const visible = el => {
    const r = el.getBoundingClientRect(), s = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && s.visibility !== "hidden" && s.display !== "none";
  };
  const sel = 'a[href], button, input:not([type=hidden]), select, textarea, summary, [contenteditable=""], ' +
    '[contenteditable=true], [role=button], [role=link], [role=checkbox], [role=radio], [role=tab], ' +
    '[role=menuitem], [role=option], [role=switch], [role=combobox], [role=searchbox], [role=textbox]';
  const elements = [];
  for (const el of document.querySelectorAll(sel)) {
    if (elements.length >= 400) break;
    if (!visible(el)) continue;
    const id = elements.length + 1;
    kivora.elements.set(id, el);
    // Never return what is typed in a password field or a field filled
    // from the secrets store.
    const hidden = el.type === "password" || kivora.secrets.has(el);
    const value = hidden ? "" : el.value;
    const label = (el.getAttribute("aria-label") || el.innerText || value || el.placeholder ||
      el.title || el.alt || el.name || "").replace(/\s+/g, " ").trim().slice(0, 120);
    elements.push({id, tag: el.tagName.toLowerCase(), type: el.getAttribute("type") || el.getAttribute("role") || "",
      label, href: el.href && typeof el.href === "string" ? el.href : ""});
  }
  let text = (document.body ? document.body.innerText : "").replace(/[ \t]+/g, " ").replace(/\n\s*\n\s*\n+/g, "\n\n").trim();
  const truncated = text.length > maxText;
  if (truncated) text = text.slice(0, maxText);
  return {title: document.title, url: location.href, text, truncated, elements};
})()`

// ReadPage returns the active tab's text and numbered interactive elements.
func (b *Browser) ReadPage(ctx context.Context, maxChars int) (Page, error) {
	var page Page
	err := b.withWebPage(ctx, func(p *cdp.Page) error {
		return p.EvalIsolated(ctx, fmt.Sprintf(readPageJS, maxChars), &page)
	})
	return page, err
}

// withWebPage runs fn on the active tab if its content may go back to the
// model. The browser policies already block local files and browser pages;
// this holds if they are missing.
func (b *Browser) withWebPage(ctx context.Context, fn func(*cdp.Page) error) error {
	return b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		var href string
		if err := p.EvalIsolated(ctx, "location.href", &href); err != nil {
			return err
		}
		if !isWebPage(href) {
			return fmt.Errorf("refused: %s is not a web page", href)
		}
		return fn(p)
	})
}

// isWebPage reports whether rawURL is a page from the web, or empty.
func isWebPage(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https", "data":
		return true
	case "about":
		return u.Opaque == "blank" || u.Opaque == "srcdoc"
	case "blob":
		return strings.HasPrefix(u.Opaque, "http:") || strings.HasPrefix(u.Opaque, "https:")
	}
	return false
}

const elementCenterJS = `(() => {
  const el = globalThis.kivora?.get(%d);
  if (!el) return null;
  el.scrollIntoView({block: "center", inline: "center"});
  const r = el.getBoundingClientRect();
  return {x: r.left + r.width / 2, y: r.top + r.height / 2};
})()`

// ClickElement clicks an element numbered by the last ReadPage.
func (b *Browser) ClickElement(ctx context.Context, id int) (Tab, error) {
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		var pt *struct{ X, Y float64 }
		if err := p.EvalIsolated(ctx, fmt.Sprintf(elementCenterJS, id), &pt); err != nil {
			return err
		}
		if pt == nil {
			return fmt.Errorf("element %d not found; call browser_read_page again, the page may have changed", id)
		}
		for _, ev := range []map[string]any{
			{"type": "mouseMoved", "x": pt.X, "y": pt.Y},
			{"type": "mousePressed", "x": pt.X, "y": pt.Y, "button": "left", "clickCount": 1},
			{"type": "mouseReleased", "x": pt.X, "y": pt.Y, "button": "left", "clickCount": 1},
		} {
			if err := p.Call(ctx, "Input.dispatchMouseEvent", ev, nil); err != nil {
				return err
			}
		}
		time.Sleep(300 * time.Millisecond)
		return waitLoaded(ctx, p)
	})
	if err != nil {
		return Tab{}, err
	}
	return b.Current(ctx)
}

const focusJS = `(() => {
  const el = %s;
  if (!el) return false;
  el.scrollIntoView({block: "center"});
  el.focus();
  if (%t) {
    if (typeof el.select === "function") el.select();
    else document.execCommand("selectAll");
  }
  return true;
})()`

// Type inserts text into an element numbered by ReadPage, or into the
// focused element when id is 0. clear replaces the current value; submit
// presses Enter afterwards. Text of any script is inserted exactly.
func (b *Browser) Type(ctx context.Context, id int, text string, clear, submit bool) (Tab, error) {
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		target := "document.activeElement"
		if id > 0 {
			target = fmt.Sprintf(`globalThis.kivora?.get(%d)`, id)
		}
		var ok bool
		if err := p.EvalIsolated(ctx, fmt.Sprintf(focusJS, target, clear), &ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("element %d not found; call browser_read_page again", id)
		}
		if text != "" {
			if err := p.Call(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
				return err
			}
		}
		if submit {
			for _, typ := range []string{"keyDown", "keyUp"} {
				ev := map[string]any{"type": typ, "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13}
				if typ == "keyDown" {
					ev["text"] = "\r"
				}
				if err := p.Call(ctx, "Input.dispatchKeyEvent", ev, nil); err != nil {
					return err
				}
			}
			time.Sleep(300 * time.Millisecond)
			return waitLoaded(ctx, p)
		}
		return nil
	})
	if err != nil {
		return Tab{}, err
	}
	return b.Current(ctx)
}

const secretTargetJS = `(() => {
  const el = globalThis.kivora?.get(%d);
  if (!el) return {ok: false, why: "element not found; call browser_read_page again"};
  if (el.tagName !== "INPUT") return {ok: false, why: "element is not an input field"};
  return {ok: true, url: location.href};
})()`

// secretFillJS checks the origin and the field and fills it in one script, so
// that no navigation can happen in between. A password credential only goes
// into a password field; other fields would let a site display it, for example
// a search box or a comment form. The mask stays on whatever the site does to
// the field, such as a button that shows the password. It uses the native
// value setter and input events, which frameworks such as React listen to.
const secretFillJS = `((origin, id, kind, value) => {
  if (location.origin !== origin) return "page";
  const el = globalThis.kivora?.get(id);
  if (!el || el.tagName !== "INPUT") return "page";
  const hints = [el.autocomplete, el.name, el.id, el.getAttribute("aria-label"), el.placeholder].join(" ");
  const allowed = el.type === "password" || (kind === "text" && (el.type === "email" ||
    (["text", "tel", "number"].includes(el.type) && /user|login|e-?mail|account|identifier|phone|one-time|otp|2fa|mfa|token|code/i.test(hints))));
  if (!allowed) return "field";
  el.scrollIntoView({block: "center"});
  el.focus();
  globalThis.kivora.secrets.add(el);
  el.style.setProperty("-webkit-text-security", "disc", "important");
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(el, value);
  el.dispatchEvent(new Event("input", {bubbles: true}));
  el.dispatchEvent(new Event("change", {bubbles: true}));
  return "filled";
})(%s, %d, %s, %s)`

// FillSecret puts a secret into a field numbered by ReadPage, replacing its
// content. valueFor receives the page URL and returns a grant if the secret
// may be used there; the fill is refused if the page is no longer on the
// granted origin when it happens, or if the field does not suit the secret.
// ReadPage hides filled fields, but the site can still display the value
// elsewhere after a submit: the MCP server scrubs its results for that.
func (b *Browser) FillSecret(ctx context.Context, id int, valueFor func(pageURL string) (secrets.Grant, error)) (Tab, error) {
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		var res struct {
			OK  bool   `json:"ok"`
			Why string `json:"why"`
			URL string `json:"url"`
		}
		if err := p.EvalIsolated(ctx, fmt.Sprintf(secretTargetJS, id), &res); err != nil {
			return err
		}
		if !res.OK {
			return fmt.Errorf("element %d: %s", id, res.Why)
		}
		grant, err := valueFor(res.URL)
		if err != nil {
			return err
		}
		var status string
		script := fmt.Sprintf(secretFillJS, jsString(grant.Origin), id, jsString(grant.Field), jsString(grant.Value))
		if err := p.EvalIsolated(ctx, script, &status); err != nil {
			return errors.New("could not fill the field") // never echo the script: it holds the value
		}
		switch status {
		case "filled":
		case "field":
			if grant.Field == secrets.FieldPassword {
				return fmt.Errorf("element %d is not a password field, the only kind this credential may go into", id)
			}
			return fmt.Errorf("element %d is not a password, login, email or one-time code field", id)
		default:
			return errors.New("the page changed before the field could be filled; read it again")
		}
		time.Sleep(200 * time.Millisecond) // let the masked field paint before a screenshot
		return nil
	})
	if err != nil {
		return Tab{}, err
	}
	return b.Current(ctx)
}

// WaitForText waits until the active tab's text contains text.
func (b *Browser) WaitForText(ctx context.Context, text string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	js := fmt.Sprintf(`document.body ? document.body.innerText.includes(%s) : false`, jsString(text))
	for {
		var found bool
		err := b.withWebPage(ctx, func(p *cdp.Page) error { return p.EvalIsolated(ctx, js, &found) })
		if err == nil && found {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("text %q did not appear within %v", text, timeout)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// PDF prints the active tab to PDF.
func (b *Browser) PDF(ctx context.Context) ([]byte, Tab, error) {
	var data []byte
	err := b.withWebPage(ctx, func(p *cdp.Page) error {
		var res struct {
			Data string `json:"data"`
		}
		if err := p.Call(ctx, "Page.printToPDF", map[string]any{"printBackground": true}, &res); err != nil {
			return err
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(res.Data)
		return err
	})
	if err != nil {
		return nil, Tab{}, err
	}
	tab, err := b.Current(ctx)
	return data, tab, err
}

// Download fetches rawURL with the browser's cookies and user agent, so that
// files behind a login work, and returns the body and a suggested name.
func (b *Browser) Download(ctx context.Context, rawURL string, limit int64) (io.ReadCloser, string, error) {
	u, err := checkURL(rawURL)
	if err != nil {
		return nil, "", err
	}
	var ua string
	err = b.retry(ctx, func(conn *cdp.Conn) error {
		var v struct {
			UserAgent string `json:"userAgent"`
		}
		if err := conn.Call(ctx, "", "Browser.getVersion", nil, &v); err != nil {
			return err
		}
		ua = v.UserAgent
		return nil
	})
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", ua)
	if err := b.addCookies(ctx, req); err != nil {
		return nil, "", err
	}
	client := &http.Client{Transport: downloadTransport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return b.addCookies(ctx, req)
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("download %s: %s", u, resp.Status)
	}
	if resp.ContentLength > limit {
		resp.Body.Close()
		return nil, "", fmt.Errorf("file is %d bytes, over the %d byte limit", resp.ContentLength, limit)
	}
	return resp.Body, suggestName(resp, u), nil
}

// addCookies gives req the browser's cookies for its URL, and no others: at
// each redirect, the new site gets its own, and an http URL no secure ones.
func (b *Browser) addCookies(ctx context.Context, req *http.Request) error {
	var res struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		return p.Call(ctx, "Network.getCookies", map[string]any{"urls": []string{req.URL.String()}}, &res)
	})
	if err != nil {
		return err
	}
	req.Header.Del("Cookie")
	for _, c := range res.Cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return nil
}

// downloadTransport never connects to this computer: downloads run as the
// desktop account, which reaches services that web pages and the worker must
// not reach.
var downloadTransport = &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 30 * time.Second, Control: netguard.RefuseLocal}).DialContext,
	TLSHandshakeTimeout: 30 * time.Second,
	ForceAttemptHTTP2:   true,
}

func suggestName(resp *http.Response, rawURL string) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		return params["filename"]
	}
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "/" && base != "." {
			return base
		}
	}
	return "download"
}

// waitLoaded waits for the document to finish loading, up to 30 seconds.
// Pages that keep loading forever are not an error: the content is usable.
func waitLoaded(ctx context.Context, p *cdp.Page) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := p.EvalIsolated(ctx, "document.readyState", &state); err == nil && state == "complete" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return nil
}

// checkURL accepts http(s) URLs and bare host names, which get https.
func checkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "about:blank" {
		return raw, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("only http and https URLs are allowed, not %s", u.Scheme)
	}
	return u.String(), nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
