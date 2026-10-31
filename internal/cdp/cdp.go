// Package cdp is a minimal Chrome DevTools Protocol client: one WebSocket
// connection to the browser, request/response calls, and flattened sessions
// for talking to individual targets. Events are ignored.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// Conn is a connection to a browser's DevTools endpoint.
type Conn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan response
	err     error
}

type request struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
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

// Dial connects to a browser WebSocket URL.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial DevTools: %w", err)
	}
	ws.SetReadLimit(64 << 20) // screenshots can be large
	c := &Conn{ws: ws, pending: make(map[int64]chan response)}
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
		if json.Unmarshal(data, &resp) != nil || resp.ID == 0 {
			continue // event or malformed message
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

// Page is a browser tab attached through a flattened session.
type Page struct {
	conn      *Conn
	TargetID  string
	SessionID string
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
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil {
		return nil, err
	}
	p := &Page{conn: c, TargetID: created.TargetID, SessionID: attached.SessionID}
	if url != "about:blank" {
		if err := p.Navigate(ctx, url); err != nil {
			p.Close(ctx)
			return nil, err
		}
	}
	return p, nil
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
