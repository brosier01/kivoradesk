// Package browser drives the visible Chromium through the DevTools Protocol:
// navigation, tabs, reading pages as text and acting on page elements.
//
// It always works on the tab the human sees (the visible one), so that the
// structured tools and the screenshots describe the same desktop.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/brosier01/kivoradesk/internal/cdp"
)

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
	ws, err := cdp.BrowserURL(ctx, b.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("browser not reachable (is Chromium running?): %w", err)
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
				var state string
				if p.Eval(ctx, "document.visibilityState", &state) == nil && state == "visible" {
					tab.Active = true
				}
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
	for _, t := range ts {
		p, err := b.attach(ctx, conn, t.ID)
		if err != nil {
			continue
		}
		if first == nil {
			first = p
		}
		var state string
		if p.Eval(ctx, "document.visibilityState", &state) == nil && state == "visible" {
			return p, nil
		}
	}
	if first == nil {
		return nil, errors.New("could not attach to any tab")
	}
	return first, nil
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
	return strings.Contains(msg, "connection closed") || strings.Contains(msg, "No session with given id") ||
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
		if err := p.Navigate(ctx, u); err != nil {
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
		if err := p.Eval(ctx, `({Title: document.title, URL: location.href})`, &v); err != nil {
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

// readPageJS numbers the visible interactive elements (data-kivora-id) and
// returns the page text. %d is the maximum text length.
const readPageJS = `(() => {
  const maxText = %d;
  const visible = el => {
    const r = el.getBoundingClientRect(), s = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && s.visibility !== "hidden" && s.display !== "none";
  };
  document.querySelectorAll("[data-kivora-id]").forEach(e => e.removeAttribute("data-kivora-id"));
  const sel = 'a[href], button, input:not([type=hidden]), select, textarea, summary, [contenteditable=""], ' +
    '[contenteditable=true], [role=button], [role=link], [role=checkbox], [role=radio], [role=tab], ' +
    '[role=menuitem], [role=option], [role=switch], [role=combobox], [role=searchbox], [role=textbox]';
  const elements = [];
  for (const el of document.querySelectorAll(sel)) {
    if (elements.length >= 400) break;
    if (!visible(el)) continue;
    const id = elements.length + 1;
    el.setAttribute("data-kivora-id", id);
    const label = (el.getAttribute("aria-label") || el.innerText || el.value || el.placeholder ||
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
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		return p.Eval(ctx, fmt.Sprintf(readPageJS, maxChars), &page)
	})
	return page, err
}

const elementCenterJS = `(() => {
  const el = document.querySelector('[data-kivora-id="%d"]');
  if (!el) return null;
  el.scrollIntoView({block: "center", inline: "center"});
  const r = el.getBoundingClientRect();
  return {x: r.left + r.width / 2, y: r.top + r.height / 2};
})()`

// ClickElement clicks an element numbered by the last ReadPage.
func (b *Browser) ClickElement(ctx context.Context, id int) (Tab, error) {
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		var pt *struct{ X, Y float64 }
		if err := p.Eval(ctx, fmt.Sprintf(elementCenterJS, id), &pt); err != nil {
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
			target = fmt.Sprintf(`document.querySelector('[data-kivora-id="%d"]')`, id)
		}
		var ok bool
		if err := p.Eval(ctx, fmt.Sprintf(focusJS, target, clear), &ok); err != nil {
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

// WaitForText waits until the active tab's text contains text.
func (b *Browser) WaitForText(ctx context.Context, text string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	js := fmt.Sprintf(`document.body ? document.body.innerText.includes(%s) : false`, jsString(text))
	for {
		var found bool
		err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error { return p.Eval(ctx, js, &found) })
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
	err := b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
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
	var cookies []struct{ Name, Value string }
	var ua string
	err = b.withActive(ctx, func(conn *cdp.Conn, p *cdp.Page) error {
		var res struct {
			Cookies []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
		}
		if err := p.Call(ctx, "Network.getCookies", map[string]any{"urls": []string{u}}, &res); err != nil {
			return err
		}
		cookies = cookies[:0]
		for _, c := range res.Cookies {
			cookies = append(cookies, struct{ Name, Value string }{c.Name, c.Value})
		}
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
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	resp, err := http.DefaultClient.Do(req)
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
		if err := p.Eval(ctx, "document.readyState", &state); err == nil && state == "complete" {
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
