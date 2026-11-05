// Package browser drives the visible Chromium through the DevTools Protocol:
// navigation, tabs, reading pages as text and acting on page elements.
//
// It always works on the tab the human sees (the visible one), so that the
// structured tools and the screenshots describe the same desktop. The browser
// says which tab that is, not the pages. Its scripts run in an isolated world,
// out of reach of the JavaScript of the page.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
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

const (
	// tabProbeTimeout bounds a short question to a tab. A tab that does not
	// answer is busy, or showing a dialog.
	tabProbeTimeout = 3 * time.Second
	// tabWorkTimeout bounds a script that goes through the whole page, or an
	// input event that the page handles.
	tabWorkTimeout = 15 * time.Second
	// stuckProbes is how many probes in a row a loading page may leave without an
	// answer before it counts as not responding.
	stuckProbes = 3
)

// What a page gives back is kept to a size that makes sense in a tool result: a
// page can make any of these as long as it wants.
const (
	maxTitle = 500
	maxURL   = 2000
	maxType  = 40
	maxLabel = 120
)

// MaxPageText is the most text of a page that ReadPage is meant to read.
const MaxPageText = 100000

// ErrTabStuck says that the visible tab does not answer.
var ErrTabStuck = errors.New("the browser tab does not respond, because the page is busy or shows a dialog: take a screenshot and use computer_key, or close it with browser_close_tab")

// ThirdPartyError is an error holding text that a site chose. The MCP server
// marks that text as untrusted.
type ThirdPartyError struct {
	Message string // ours
	Detail  string // the site's
}

func (e *ThirdPartyError) Error() string { return e.Message + ": " + e.Detail }

// allowLocalPages lets the tests use pages served by this computer.
var allowLocalPages = false

// Browser is a lazily (re)connected DevTools client.
type Browser struct {
	Endpoint string // DevTools HTTP endpoint, e.g. http://127.0.0.1:9222

	mu    sync.Mutex
	conn  *cdp.Conn
	pages map[string]*cdp.Page // by target ID
}

// Tab describes a browser tab.
type Tab struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Active   bool   `json:"active"`
	OpenedBy string `json:"opened_by,omitempty"` // the tab that opened this one: a popup, a target=_blank link
	// Switched says that an action brought another tab or window on screen.
	Switched bool `json:"-"`
	// Unchecked says that a click led to another page so fast that it could not
	// be checked to have reached its element.
	Unchecked bool `json:"-"`
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
	// MoreElements says that the page has more elements than were numbered.
	MoreElements bool `json:"more_elements"`
	// Frames and ShadowRoots count what the tools cannot read or act on.
	Frames      int `json:"frames"`
	ShadowRoots int `json:"shadow_roots"`
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

// tabs returns the browser's tabs, the one on screen first. Browser UI that also
// shows up as pages, such as the omnibox popups, is left out. A tab the human
// opened on a browser page is a tab like another: only reading it is refused.
func (b *Browser) tabs(ctx context.Context) ([]cdp.Target, error) {
	targets, err := cdp.ListTargets(ctx, b.Endpoint)
	if err != nil {
		return nil, err
	}
	var tabs []cdp.Target
	for _, t := range targets {
		if t.Type == "page" && !strings.Contains(t.URL, ".top-chrome") && !strings.HasPrefix(t.URL, "chrome-untrusted://") {
			tabs = append(tabs, t)
		}
	}
	b.forgetOthers(tabs)
	return tabs, nil
}

// forgetOthers drops what is kept about the tabs that no longer exist.
func (b *Browser) forgetOthers(tabs []cdp.Target) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, p := range b.pages {
		open := false
		for _, t := range tabs {
			open = open || t.ID == id
		}
		if !open {
			b.conn.Forget(p)
			delete(b.pages, id)
		}
	}
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
	b.keep(ctx, p)
	return p, nil
}

// keep remembers an attached page and follows its dialogs. A page that does not
// answer now is reported by the call that comes next.
func (b *Browser) keep(ctx context.Context, p *cdp.Page) {
	sctx, cancel := context.WithTimeout(ctx, tabProbeTimeout)
	defer cancel()
	p.FollowDialogs(sctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pages != nil {
		b.pages[p.TargetID] = p
	}
}

// Tabs lists the tabs, the one on screen first and marked active.
func (b *Browser) Tabs(ctx context.Context) ([]Tab, error) {
	var tabs []Tab
	err := b.retry(ctx, func(conn *cdp.Conn) error {
		ts, err := b.tabs(ctx)
		if err != nil {
			return err
		}
		var infos struct {
			TargetInfos []struct {
				ID     string `json:"targetId"`
				Opener string `json:"openerId"`
			} `json:"targetInfos"`
		}
		if err := conn.Call(ctx, "", "Target.getTargets", nil, &infos); err != nil {
			return err
		}
		opener := make(map[string]string, len(infos.TargetInfos))
		for _, info := range infos.TargetInfos {
			opener[info.ID] = info.Opener
		}
		tabs = tabs[:0]
		for i, t := range ts {
			tabs = append(tabs, Tab{ID: t.ID, Title: cut(t.Title, maxTitle), URL: cut(t.URL, maxURL), Active: i == 0, OpenedBy: opener[t.ID]})
		}
		return nil
	})
	return tabs, err
}

// active returns the tab on screen.
func (b *Browser) active(ctx context.Context, conn *cdp.Conn) (*cdp.Page, error) {
	ts, err := b.tabs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, errors.New("no browser tab is open; use browser_new_tab")
	}
	return b.attach(ctx, conn, ts[0].ID)
}

// onPage runs a call to a page, within limit. A page showing a dialog or stuck in
// a loop answers nothing, and nothing would stop the call before the limit of the
// whole tool otherwise.
func onPage(ctx context.Context, p *cdp.Page, limit time.Duration, call func(context.Context) error) error {
	dialog, opening := p.Dialog()
	if dialog != nil {
		return dialogError(dialog)
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	go func() {
		select {
		case <-opening:
			cancel()
		case <-cctx.Done():
		}
	}()
	err := call(cctx)
	if err == nil {
		return nil
	}
	if dialog, _ := p.Dialog(); dialog != nil {
		return dialogError(dialog)
	}
	if ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return ErrTabStuck
	}
	return err
}

func dialogError(d *cdp.Dialog) error {
	return &ThirdPartyError{
		Message: fmt.Sprintf("the page shows a dialog (%s), and answers nothing until it closes: take a screenshot, then press Return or Escape with computer_key, or close the tab with browser_close_tab. Its text", d.Type),
		Detail:  d.Message,
	}
}

func eval(ctx context.Context, p *cdp.Page, expr string, out any) error {
	return onPage(ctx, p, tabProbeTimeout, func(ctx context.Context) error { return p.EvalIsolated(ctx, expr, out) })
}

// cut limits a text from a page to n bytes, on a character boundary. A cut text
// ends with a cut mark, so that the cleaning of the results hides what may be
// left of a stored value there.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n -= len(secrets.CutMark)
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:max(n, 0)] + secrets.CutMark
}

// cutJS is cut for the page scripts, in UTF-16 units, as JavaScript counts.
var cutJS = fmt.Sprintf(`const cut = (s, n) => {
    if (s.length <= n) return s;
    let end = n - %[1]s.length;
    const code = s.charCodeAt(end - 1);
    if (code >= 0xD800 && code < 0xDC00) end--;
    return s.slice(0, Math.max(end, 0)) + %[1]s;
  };`, jsString(secrets.CutMark))

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
		strings.Contains(msg, "No target with given id") || strings.Contains(msg, "Session with given id not found") ||
		strings.Contains(msg, "reach DevTools endpoint")
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
		err := onPage(nctx, p, NavigationTimeout, func(ctx context.Context) error { return p.Navigate(ctx, u) })
		if err != nil {
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
		err := onPage(ctx, p, tabProbeTimeout, func(ctx context.Context) error {
			return p.Call(ctx, "Page.getNavigationHistory", nil, &res)
		})
		if err != nil {
			return err
		}
		i := res.CurrentIndex + delta
		if i < 0 || i >= len(res.Entries) {
			return errors.New("no page in that direction of the history")
		}
		err = onPage(ctx, p, tabProbeTimeout, func(ctx context.Context) error {
			return p.Call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": res.Entries[i].ID}, nil)
		})
		if err != nil {
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
		err := onPage(ctx, p, tabProbeTimeout, func(ctx context.Context) error {
			return p.Call(ctx, "Page.reload", nil, nil)
		})
		if err != nil {
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
		script := fmt.Sprintf(`(() => { %s return {Title: cut(document.title, %d), URL: cut(location.href, %d)}; })()`, cutJS, maxTitle, maxURL)
		if err := eval(ctx, p, script, &v); err != nil {
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
		b.keep(ctx, p)
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
// text. The first %d is the maximum text length. The numbers live in the isolated
// world: the page can neither see them nor give them to other elements. What it
// cuts ends with a cut mark.
const readPageJS = `(() => {
  %s
  const maxText = %d, maxTitle = %d, maxURL = %d, maxType = %d, maxLabel = %d;
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
  let more = false;
  for (const el of document.querySelectorAll(sel)) {
    if (!visible(el)) continue;
    if (elements.length >= 400) {
      more = true;
      break;
    }
    const id = elements.length + 1;
    kivora.elements.set(id, el);
    // Never return what is typed in a password field or a field filled
    // from the secrets store.
    const hidden = el.type === "password" || kivora.secrets.has(el);
    const value = hidden ? "" : el.value;
    const label = (el.getAttribute("aria-label") || el.innerText || value || el.placeholder ||
      el.title || el.alt || el.name || "").replace(/\s+/g, " ").trim();
    elements.push({id, tag: el.tagName.toLowerCase(), type: cut(el.getAttribute("type") || el.getAttribute("role") || "", maxType),
      label: cut(label, maxLabel), href: el.href && typeof el.href === "string" ? cut(el.href, maxURL) : ""});
  }
  let shadow_roots = 0, seen = 0;
  for (const el of document.querySelectorAll("*")) {
    if (el.shadowRoot) shadow_roots++;
    if (++seen >= 100000) break;
  }
  let text = (document.body ? document.body.innerText : "").replace(/[ \t]+/g, " ").replace(/\n\s*\n\s*\n+/g, "\n\n").trim();
  const truncated = text.length > maxText;
  text = cut(text, maxText);
  return {title: cut(document.title, maxTitle), url: cut(location.href, maxURL), text, truncated, elements,
    more_elements: more, frames: document.querySelectorAll("iframe, frame").length, shadow_roots};
})()`

// ReadPage returns the active tab's text and numbered interactive elements. A text
// longer than maxChars is cut and ends with a cut mark. A value that the cut
// leaves shorter than the cleaning recognizes would go out: a caller that lets
// the model choose the length reads MaxPageText, and cuts after cleaning.
func (b *Browser) ReadPage(ctx context.Context, maxChars int) (Page, error) {
	var page Page
	script := fmt.Sprintf(readPageJS, cutJS, maxChars, maxTitle, maxURL, maxType, maxLabel)
	err := b.withWebPage(ctx, func(p *cdp.Page) error {
		return onPage(ctx, p, tabWorkTimeout, func(ctx context.Context) error { return p.EvalIsolated(ctx, script, &page) })
	})
	return page, err
}

// withWebPage runs fn on the active tab if its content may go back to the
// model. The browser policies already block local files and browser pages;
// this holds if they are missing.
func (b *Browser) withWebPage(ctx context.Context, fn func(*cdp.Page) error) error {
	return b.withActive(ctx, func(_ *cdp.Conn, p *cdp.Page) error {
		var href string
		if err := eval(ctx, p, fmt.Sprintf("location.href.slice(0, %d)", maxURL), &href); err != nil {
			return err
		}
		if !isWebPage(href) {
			return &ThirdPartyError{Message: "refused: the visible tab is not a web page; use browser_navigate or browser_new_tab, or computer_* to work with it. Its address", Detail: href}
		}
		return fn(p)
	})
}

// isWebPage reports whether rawURL is a page from the web, and not one served by
// this computer, which only the keyboard can bring up. The browser policies block
// what they know of; this holds for any way of writing the address.
func isWebPage(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return !isThisComputer(u.Hostname())
	case "data":
		return true
	case "about":
		return u.Opaque == "blank" || u.Opaque == "srcdoc"
	case "blob":
		inner, err := url.Parse(u.Opaque)
		return err == nil && (inner.Scheme == "http" || inner.Scheme == "https") && !isThisComputer(inner.Hostname())
	}
	return false
}

// isThisComputer reports whether a URL's host reaches this computer, written in
// any way a browser understands: a name, or an address in decimal, hexadecimal or
// octal, whole or in parts, mapped to IPv6 or not.
func isThisComputer(host string) bool {
	if allowLocalPages {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ip = legacyIPv4(host)
	}
	return ip != nil && netguard.IsLocal(ip)
}

// legacyIPv4 reads the forms of an IPv4 address other than a.b.c.d, such as
// 2130706433, 0x7f.1 or 017700000001. It returns nil for anything else.
func legacyIPv4(host string) net.IP {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil
	}
	var value uint64
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 0, 32)
		if err != nil || strings.Contains(part, "_") {
			return nil
		}
		if i == len(parts)-1 {
			// The last part fills every byte left.
			if bits := 8 * uint(4-i); bits < 32 && n >= 1<<bits {
				return nil
			}
			value |= n
			break
		}
		if n > 255 {
			return nil
		}
		value |= n << (24 - 8*uint(i))
	}
	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
}

// reachesJS defines what the click scripts share. at says what takes a click at
// a point: the element, or something that passes it on, such as its label; a
// frame, whose page would decide what the click does; or a cover. The page can
// change all of this when the pointer comes, so it is asked again just before the
// button goes down, and a listener says afterwards whether the element got it.
const reachesJS = `const kivora = globalThis.kivora;
  const el = kivora?.get(%d);
  const at = (x, y) => {
    const top = document.elementFromPoint(x, y);
    if (!top) return "covered";
    if (top !== el && (top.tagName === "IFRAME" || top.tagName === "FRAME" || top.tagName === "OBJECT" || top.tagName === "EMBED")) return "frame";
    const reaches = top === el || el.contains(top) || top.contains(el) ||
      [...(el.labels || [])].some(label => label === top || label.contains(top));
    return reaches ? "ok" : "covered";
  };`

// elementPointJS scrolls to an element and finds where a click reaches it: a
// transparent layer over it would take the click instead, and the page would
// decide what that does. A link over several lines has several boxes. It then
// listens for the click on the window, where it starts. A page that stops it
// there makes the click count as lost, which is the safe side.
const elementPointJS = `(() => {
  ` + reachesJS + `
  if (!el) return null;
  el.scrollIntoView({block: "center", inline: "center"});
  let found = "covered";
  for (const r of el.getClientRects()) {
    const x = r.left + r.width / 2, y = r.top + r.height / 2;
    if (r.width <= 0 || r.height <= 0) continue;
    const what = at(x, y);
    if (what === "ok") {
      kivora.click?.stop();
      const top = document.elementFromPoint(x, y);
      const click = {reached: false};
      // Only a click of the input devices counts, on the element, on what was
      // under the point for it, or on one of its labels, which pass it on.
      const listen = e => {
        const t = e.target;
        if (e.isTrusted && t instanceof Node && (el === t || el.contains(t) || t === top ||
            [...(el.labels || [])].some(label => label.contains(t)))) click.reached = true;
      };
      click.stop = () => removeEventListener("click", listen, true);
      addEventListener("click", listen, true);
      kivora.click = click;
      return {x, y, at: "ok"};
    }
    if (what === "frame") found = "frame";
  }
  return {at: found};
})()`

// elementStillAtJS asks again what takes a click at a point.
const elementStillAtJS = `(() => {
  ` + reachesJS + `
  if (!el || !kivora.click) return "missing";
  return at(%f, %f);
})()`

// clickedJS says whether the element got the click, and stops listening.
const clickedJS = `(() => {
  const click = globalThis.kivora?.click;
  if (!click) return "gone";
  click.stop();
  globalThis.kivora.click = null;
  return click.reached ? "reached" : "missed";
})()`

// ClickElement clicks an element numbered by the last ReadPage. It refuses a
// click that something else would take, and says if the page moved something
// under the pointer at the last moment, which then took it.
func (b *Browser) ClickElement(ctx context.Context, id int) (Tab, error) {
	var switched, unchecked bool
	err := b.withActive(ctx, func(conn *cdp.Conn, p *cdp.Page) error {
		var pt *struct {
			X, Y float64
			At   string
		}
		if err := eval(ctx, p, fmt.Sprintf(elementPointJS, id), &pt); err != nil {
			return err
		}
		if pt == nil {
			return fmt.Errorf("element %d not found; call browser_read_page again, the page may have changed", id)
		}
		if err := atError(id, pt.At, false); err != nil {
			return err
		}
		mouse := func(ev map[string]any) error {
			return onPage(ctx, p, tabWorkTimeout, func(ctx context.Context) error {
				return p.Call(ctx, "Input.dispatchMouseEvent", ev, nil)
			})
		}
		if err := mouse(map[string]any{"type": "mouseMoved", "x": pt.X, "y": pt.Y}); err != nil {
			return err
		}
		var at string
		if err := eval(ctx, p, fmt.Sprintf(elementStillAtJS, id, pt.X, pt.Y), &at); err != nil {
			return err
		}
		if err := atError(id, at, true); err != nil {
			eval(ctx, p, clickedJS, new(string))
			return err
		}
		for _, typ := range []string{"mousePressed", "mouseReleased"} {
			if err := mouse(map[string]any{"type": typ, "x": pt.X, "y": pt.Y, "button": "left", "clickCount": 1}); err != nil {
				return err
			}
		}
		var clicked string
		if err := eval(ctx, p, clickedJS, &clicked); err != nil {
			// The click led to another page at once: nothing is left to ask.
			clicked = "gone"
		}
		switch clicked {
		case "missed":
			return fmt.Errorf("the click did not reach element %d: the page moved something under the pointer, which got the click instead. Take a screenshot before going on", id)
		case "gone":
			unchecked = true
		}
		time.Sleep(300 * time.Millisecond)
		var err error
		switched, err = b.settle(ctx, conn, p)
		return err
	})
	if err != nil {
		return Tab{}, err
	}
	tab, err := b.Current(ctx)
	tab.Switched, tab.Unchecked = switched, unchecked
	return tab, err
}

// atError explains why a click at a point would not reach element id. Moved says
// that the page changed it when the pointer came.
func atError(id int, at string, moved bool) error {
	when := ""
	if moved {
		when = " when the pointer came over it; nothing was clicked"
	}
	switch at {
	case "ok":
		return nil
	case "frame":
		return fmt.Errorf("element %d is under a frame, whose page would take the click%s: take a screenshot, and use computer_click if that is what you mean", id, when)
	case "missing":
		return fmt.Errorf("element %d is gone%s; call browser_read_page again", id, when)
	}
	if moved {
		return fmt.Errorf("the page put something over element %d%s: take a screenshot, and use computer_click if that is what you mean", id, when)
	}
	return fmt.Errorf("element %d is out of view or covered by something else, which would take the click: take a screenshot, and use computer_click if that is what you mean", id)
}

// settle waits for what an action in tab p led to, which may be another tab or
// window, and says if it was.
func (b *Browser) settle(ctx context.Context, conn *cdp.Conn, p *cdp.Page) (switched bool, err error) {
	now, err := b.active(ctx, conn)
	if err != nil {
		return false, err
	}
	return now.TargetID != p.TargetID, waitLoaded(ctx, now)
}

// focusJS gives an element the focus and says whether text can go there. Text
// sent elsewhere goes nowhere, without any error. A frame or a shadow root may hold
// the real field.
const focusJS = `(() => {
  const focused = () => {
    let el = document.activeElement;
    while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
    return el;
  };
  const noText = ["button", "checkbox", "color", "file", "hidden", "image", "radio", "range", "reset", "submit"];
  const takesText = el => el && !el.disabled && !el.readOnly && (el.isContentEditable || el.tagName === "TEXTAREA" ||
    el.tagName === "IFRAME" || el.tagName === "FRAME" || (el.tagName === "INPUT" && !noText.includes(el.type)));
  const el = %s;
  if (!el) return "missing";
  el.scrollIntoView({block: "center"});
  el.focus();
  const field = focused();
  if (!takesText(field)) return "no field";
  if (%t) {
    if (typeof field.select === "function") field.select();
    else document.execCommand("selectAll");
  }
  return "ok";
})()`

// Type inserts text into an element numbered by ReadPage, or into the
// focused element when id is 0. clear replaces the current value; submit
// presses Enter afterwards. Text of any script is inserted exactly.
func (b *Browser) Type(ctx context.Context, id int, text string, clear, submit bool) (Tab, error) {
	if id < 0 {
		return Tab{}, fmt.Errorf("element %d does not exist: use a number from browser_read_page, or none for the focused field", id)
	}
	var switched bool
	err := b.withActive(ctx, func(conn *cdp.Conn, p *cdp.Page) error {
		target := "document.activeElement"
		if id > 0 {
			target = fmt.Sprintf(`globalThis.kivora?.get(%d)`, id)
		}
		var status string
		if err := eval(ctx, p, fmt.Sprintf(focusJS, target, clear), &status); err != nil {
			return err
		}
		switch {
		case status == "missing" && id > 0:
			return fmt.Errorf("element %d not found; call browser_read_page again", id)
		case status != "ok" && id > 0:
			return fmt.Errorf("element %d is not a field that takes text", id)
		case status != "ok":
			return errors.New("no field has the focus: give the number of one from browser_read_page")
		}
		err := onPage(ctx, p, tabWorkTimeout, func(ctx context.Context) error {
			if text != "" {
				if err := p.Call(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
					return err
				}
			}
			if !submit {
				return nil
			}
			for _, typ := range []string{"keyDown", "keyUp"} {
				ev := map[string]any{"type": typ, "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13}
				if typ == "keyDown" {
					ev["text"] = "\r"
				}
				if err := p.Call(ctx, "Input.dispatchKeyEvent", ev, nil); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil || !submit {
			return err
		}
		time.Sleep(300 * time.Millisecond)
		switched, err = b.settle(ctx, conn, p)
		return err
	})
	if err != nil {
		return Tab{}, err
	}
	tab, err := b.Current(ctx)
	tab.Switched = switched
	return tab, err
}

const secretTargetJS = `(() => {
  const el = globalThis.kivora?.get(%d);
  if (!el) return {ok: false, why: "element not found; call browser_read_page again"};
  if (el.tagName !== "INPUT") return {ok: false, why: "element is not an input field"};
  return {ok: true, url: location.href.slice(0, %d)};
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
		if err := eval(ctx, p, fmt.Sprintf(secretTargetJS, id, maxURL), &res); err != nil {
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
		if err := eval(ctx, p, script, &status); err != nil {
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
		err := b.withWebPage(ctx, func(p *cdp.Page) error { return eval(ctx, p, js, &found) })
		if err == nil && found {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("could not look for text %q: %w", text, err)
			}
			return fmt.Errorf("text %q did not appear within %v", text, timeout)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// PDF prints the active tab to PDF. The result is read from the browser as it is
// consumed: it can be larger than a single message of the protocol.
func (b *Browser) PDF(ctx context.Context) (io.ReadCloser, Tab, error) {
	var body io.ReadCloser
	err := b.withWebPage(ctx, func(p *cdp.Page) error {
		var res struct {
			Stream string `json:"stream"`
		}
		err := onPage(ctx, p, time.Minute, func(ctx context.Context) error {
			return p.Call(ctx, "Page.printToPDF", map[string]any{"printBackground": true, "transferMode": "ReturnAsStream"}, &res)
		})
		if err != nil {
			return err
		}
		body = p.Stream(ctx, res.Stream)
		return nil
	})
	if err != nil {
		return nil, Tab{}, err
	}
	tab, err := b.Current(ctx)
	if err != nil {
		body.Close()
		return nil, Tab{}, err
	}
	return body, tab, nil
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
		if ctx.Err() != nil {
			return nil, "", err
		}
		// The error can hold an address that a site redirected to.
		return nil, "", &ThirdPartyError{Message: "download failed", Detail: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		// The status code only: a site writes the rest of that line.
		return nil, "", fmt.Errorf("download failed: the server answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
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
	// The browser answers this itself, also for a tab showing a dialog.
	err := b.retry(ctx, func(conn *cdp.Conn) error {
		p, err := b.active(ctx, conn)
		if err != nil {
			return err
		}
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
// Pages that keep loading forever are not an error: the content is usable. A page
// showing a dialog or not answering at all is reported.
func waitLoaded(ctx context.Context, p *cdp.Page) error {
	deadline := time.Now().Add(30 * time.Second)
	stuck := 0
	for time.Now().Before(deadline) {
		var state string
		err := eval(ctx, p, "document.readyState", &state)
		var site *ThirdPartyError
		switch {
		case err == nil && state == "complete":
			return nil
		case errors.As(err, &site):
			return err
		case errors.Is(err, ErrTabStuck):
			if stuck++; stuck >= stuckProbes {
				return err
			}
		default:
			stuck = 0
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
	if isThisComputer(u.Hostname()) {
		return "", fmt.Errorf("refused: %s is an address of this computer", u.Hostname())
	}
	return u.String(), nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
