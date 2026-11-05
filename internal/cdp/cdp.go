// Package cdp is a minimal Chrome DevTools Protocol client: one WebSocket
// connection to the browser, request/response calls, and flattened sessions
// for talking to individual targets. Of the events, only JavaScript dialogs are
// followed: they stop a page from answering anything else.
package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// Conn is a connection to a browser's DevTools endpoint.
type Conn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan response
	pages   map[string]*Page // by session ID
	err     error
}

type request struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

// response is a message from the browser: the answer to a call or, without an
// ID, an event.
type response struct {
	ID        int64           `json:"id"`
	Result    json.RawMessage `json:"result"`
	Error     *Error          `json:"error"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

// Error is a protocol-level error returned by the browser.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

// BrowserURL resolves the browser WebSocket URL from an HTTP endpoint such as
// http://127.0.0.1:9222.
func BrowserURL(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach DevTools endpoint: %w", err)
	}
	defer resp.Body.Close()
	var v struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("decode DevTools version: %w", err)
	}
	if v.URL == "" {
		return "", errors.New("DevTools endpoint returned no WebSocket URL")
	}
	return v.URL, nil
}

// Target describes a tab or another target of the browser.
type Target struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// maxTargetList bounds the answer of ListTargets: titles and addresses come from
// web pages.
const maxTargetList = 16 << 20

// ListTargets returns the targets from an HTTP endpoint such as
// http://127.0.0.1:9222. The browser sorts them by the time they were last shown,
// whatever their pages do: of the tabs, the first is the one on screen, in the
// window on top.
func ListTargets(ctx context.Context, endpoint string) ([]Target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach DevTools endpoint: %w", err)
	}
	defer resp.Body.Close()
	var targets []Target
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTargetList)).Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode DevTools targets: %w", err)
	}
	return targets, nil
}

// Dial connects to a browser WebSocket URL.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial DevTools: %w", err)
	}
	ws.SetReadLimit(64 << 20) // screenshots can be large
	c := &Conn{ws: ws, pending: make(map[int64]chan response), pages: make(map[string]*Page)}
	go c.readLoop()
	return c, nil
}

// Close closes the connection; pending calls fail.
func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

// Call sends method with params, on sessionID if non-empty, and decodes the
// result into out if out is non-nil.
func (c *Conn) Call(ctx context.Context, sessionID, method string, params, out any) error {
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	msg, err := json.Marshal(request{ID: id, Method: method, Params: params, SessionID: sessionID})
	if err != nil {
		return err
	}
	if err := c.ws.Write(ctx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: %w", method, c.err)
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %w", method, resp.Error)
		}
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

func (c *Conn) readLoop() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.err = fmt.Errorf("DevTools connection closed: %w", err)
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var resp response
		if json.Unmarshal(data, &resp) != nil {
			continue
		}
		if resp.ID == 0 {
			c.event(resp)
			continue
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (c *Conn) event(ev response) {
	switch ev.Method {
	case "Page.javascriptDialogOpening", "Page.javascriptDialogClosed":
	default:
		return
	}
	c.mu.Lock()
	p := c.pages[ev.SessionID]
	c.mu.Unlock()
	if p == nil {
		return
	}
	if ev.Method == "Page.javascriptDialogClosed" {
		p.setDialog(nil)
		return
	}
	var d Dialog
	if json.Unmarshal(ev.Params, &d) == nil {
		p.setDialog(&d)
	}
}

// Dialog is a JavaScript dialog a page has open. Until it closes, the page answers
// nothing.
type Dialog struct {
	Type    string `json:"type"` // alert, confirm, prompt or beforeunload
	Message string `json:"message"`
}

// Page is a browser tab attached through a flattened session.
type Page struct {
	conn      *Conn
	TargetID  string
	SessionID string

	mu       sync.Mutex
	dialog   *Dialog
	dialogUp chan struct{} // closed when a dialog opens
}

func (p *Page) setDialog(d *Dialog) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dialog = d
	if d != nil {
		close(p.dialogUp)
		p.dialogUp = make(chan struct{})
	}
}

// Dialog returns the dialog the page has open, if any, and a channel closed when
// the next one opens.
func (p *Page) Dialog() (*Dialog, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dialog, p.dialogUp
}

// Forget stops following the page. Its tab is gone.
func (c *Conn) Forget(p *Page) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pages, p.SessionID)
}

// NewPage opens a new tab, attaches to it, then navigates to url. Creating
// the tab directly at url races with the attach: the session can end up bound
// to the initial about:blank document.
func (c *Conn) NewPage(ctx context.Context, url string) (*Page, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return nil, err
	}
	p, err := c.Attach(ctx, created.TargetID)
	if err != nil {
		return nil, err
	}
	if url != "about:blank" {
		if err := p.Navigate(ctx, url); err != nil {
			p.Close(ctx)
			return nil, err
		}
	}
	return p, nil
}

// Attach opens a flattened session on an existing target, such as a tab.
func (c *Conn) Attach(ctx context.Context, targetID string) (*Page, error) {
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil {
		return nil, err
	}
	p := &Page{conn: c, TargetID: targetID, SessionID: attached.SessionID, dialogUp: make(chan struct{})}
	c.mu.Lock()
	c.pages[p.SessionID] = p
	c.mu.Unlock()
	return p, nil
}

// FollowDialogs asks for the events about dialogs. A page already stuck does not
// answer: the caller bounds it with ctx.
func (p *Page) FollowDialogs(ctx context.Context) error {
	return p.Call(ctx, "Page.enable", nil, nil)
}

// Navigate loads url in the page. It returns once the navigation has been
// committed, not when the page has finished loading.
func (p *Page) Navigate(ctx context.Context, url string) error {
	var res struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.Call(ctx, "Page.navigate", map[string]any{"url": url}, &res); err != nil {
		return err
	}
	if res.ErrorText != "" {
		return fmt.Errorf("navigate to %s: %s", url, res.ErrorText)
	}
	return nil
}

// Close closes the tab.
func (p *Page) Close(ctx context.Context) error {
	return p.conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": p.TargetID}, nil)
}

// Call sends a method on the page session.
func (p *Page) Call(ctx context.Context, method string, params, out any) error {
	return p.conn.Call(ctx, p.SessionID, method, params, out)
}

// Eval evaluates a JavaScript expression in the page and decodes its value
// into out if out is non-nil.
func (p *Page) Eval(ctx context.Context, expr string, out any) error {
	return p.evalIn(ctx, 0, expr, out)
}

// EvalIsolated evaluates a JavaScript expression in a world of its own. It
// shares the DOM with the page but not its JavaScript: what the page overrides,
// such as document.visibilityState, does not apply, and what the expression
// keeps in globalThis stays out of the page's reach until the next navigation.
//
// The world's number is asked for each time. Numbers are reused across renderer
// processes: one kept across a navigation can name a context of the new page,
// even the page's own. Chromium gives back the same world for the same name, so
// what the expressions keep there stays. If the page navigated in between, the
// number was already out of date: the result is dropped and the expression
// runs again.
func (p *Page) EvalIsolated(ctx context.Context, expr string, out any) error {
	for attempt := 0; ; attempt++ {
		world, loader, err := p.isolatedWorld(ctx)
		if err != nil {
			return err
		}
		err = p.evalIn(ctx, world, expr, out)
		if err == nil {
			_, now, frameErr := p.mainFrame(ctx)
			if frameErr != nil {
				return frameErr
			}
			if now == loader {
				return nil
			}
			err = errors.New("the page changed while it was read")
		} else if !strings.Contains(err.Error(), "Cannot find context") {
			return err
		}
		if attempt == 2 {
			return err
		}
	}
}

// mainFrame returns the ID of the page's main frame and the ID of the load of its
// document, which changes with each navigation.
func (p *Page) mainFrame(ctx context.Context) (frame, loader string, err error) {
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := p.Call(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return "", "", err
	}
	return tree.FrameTree.Frame.ID, tree.FrameTree.Frame.LoaderID, nil
}

func (p *Page) isolatedWorld(ctx context.Context) (world int, loader string, err error) {
	frame, loader, err := p.mainFrame(ctx)
	if err != nil {
		return 0, "", err
	}
	var created struct {
		ID int `json:"executionContextId"`
	}
	params := map[string]any{"frameId": frame, "worldName": "kivora"}
	if err := p.Call(ctx, "Page.createIsolatedWorld", params, &created); err != nil {
		return 0, "", err
	}
	return created.ID, loader, nil
}

// Stream reads what a call left in a stream of the browser, such as a printed PDF,
// piece by piece: the protocol carries each one in a single message.
func (p *Page) Stream(ctx context.Context, handle string) io.ReadCloser {
	return &stream{ctx: ctx, page: p, handle: handle}
}

type stream struct {
	ctx    context.Context
	page   *Page
	handle string
	rest   []byte
	eof    bool
}

// streamChunk is well under the limit of a WebSocket message once in base64.
const streamChunk = 1 << 20

func (s *stream) Read(b []byte) (int, error) {
	for len(s.rest) == 0 {
		if s.eof {
			return 0, io.EOF
		}
		var res struct {
			Data          string `json:"data"`
			Base64Encoded bool   `json:"base64Encoded"`
			EOF           bool   `json:"eof"`
		}
		params := map[string]any{"handle": s.handle, "size": streamChunk}
		if err := s.page.Call(s.ctx, "IO.read", params, &res); err != nil {
			return 0, err
		}
		s.eof = res.EOF
		s.rest = []byte(res.Data)
		if res.Base64Encoded {
			decoded, err := base64.StdEncoding.DecodeString(res.Data)
			if err != nil {
				return 0, err
			}
			s.rest = decoded
		}
	}
	n := copy(b, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}

func (s *stream) Close() error {
	return s.page.Call(s.ctx, "IO.close", map[string]any{"handle": s.handle}, nil)
}

// evalIn evaluates in an execution context, or the page's own if it is 0.
func (p *Page) evalIn(ctx context.Context, contextID int, expr string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	params := map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}
	if contextID != 0 {
		params["contextId"] = contextID
	}
	if err := p.Call(ctx, "Runtime.evaluate", params, &res); err != nil {
		return err
	}
	if ex := res.ExceptionDetails; ex != nil {
		return fmt.Errorf("javascript: %s %s", ex.Text, ex.Exception.Description)
	}
	if out != nil && len(res.Result.Value) > 0 {
		return json.Unmarshal(res.Result.Value, out)
	}
	return nil
}
