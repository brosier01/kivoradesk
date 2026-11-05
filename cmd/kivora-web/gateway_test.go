package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/brosier01/kivoradesk/internal/control"
	"github.com/coder/websocket"
)

var rfbClientStart = []byte("RFB 003.008\n\x01\x01")

// fakeVNC records what reaches the VNC server.
type fakeVNC struct {
	l    net.Listener
	mu   sync.Mutex
	data bytes.Buffer
	conn net.Conn
}

func newFakeVNC(t *testing.T) *fakeVNC {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	v := &fakeVNC{l: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			v.mu.Lock()
			v.conn = c
			v.mu.Unlock()
			go func() {
				defer c.Close()
				buf := make([]byte, 64<<10)
				for {
					n, err := c.Read(buf)
					v.mu.Lock()
					v.data.Write(buf[:n])
					v.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return v
}

func (v *fakeVNC) received() []byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	return bytes.Clone(v.data.Bytes())
}

func (v *fakeVNC) reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.data.Reset()
}

func (v *fakeVNC) send(t *testing.T, data string) {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.conn.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
}

// waitFor reports whether want reaches the server in time.
func (v *fakeVNC) waitFor(want []byte) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if bytes.Contains(v.received(), want) {
			return true
		}
	}
	return false
}

func gateway(t *testing.T, options ...func(*server)) (*server, *httptest.Server, *fakeVNC) {
	t.Helper()
	vnc := newFakeVNC(t)
	s := testServer(t)
	s.vncAddr = vnc.l.Addr().String()
	for _, option := range options {
		option(s)
	}
	ts := httptest.NewServer(s.routes(fstest.MapFS{"index.html": {Data: []byte("viewer")}}, t.TempDir()))
	t.Cleanup(ts.Close)
	return s, ts, vnc
}

func dialViewer(ctx context.Context, ts *httptest.Server, session string) (*websocket.Conn, error) {
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/websockify", &websocket.DialOptions{
		HTTPHeader:   http.Header{"Cookie": {cookieName + "=" + session}},
		Subprotocols: []string{"binary"},
	})
	return ws, err
}

// viewer connects a browser that has done the RFB handshake.
func viewer(t *testing.T, ctx context.Context, ts *httptest.Server, vnc *fakeVNC, session string) net.Conn {
	t.Helper()
	ws, err := dialViewer(ctx, ts, session)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	c := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	if _, err := c.Write(rfbClientStart); err != nil {
		t.Fatal(err)
	}
	if !vnc.waitFor(rfbClientStart) {
		t.Fatal("the handshake did not reach the server")
	}
	vnc.reset()
	return c
}

func keyEvent(down byte, k uint32) []byte {
	m := []byte{4, down, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(m[4:], k)
	return m
}

func pointer(buttons byte, x, y uint16) []byte {
	m := []byte{5, buttons, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(m[2:], x)
	binary.BigEndian.PutUint16(m[4:], y)
	return m
}

var fbRequest = []byte{3, 0, 0, 0, 0, 0, 5, 0, 3, 32}

func TestViewerInputReachesTheDesktopOnlyWhileTheHumanOwnsIt(t *testing.T) {
	s, ts, vnc := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := viewer(t, ctx, ts, vnc, session)

	for _, owner := range []control.Owner{control.Agent, control.Paused} {
		s.state.Set(owner)
		vnc.reset()
		c.Write(keyEvent(1, 'x'))
		c.Write(pointer(1, 1, 1))
		c.Write(fbRequest) // it follows the input: seeing it means the input was handled
		if !vnc.waitFor(fbRequest) {
			t.Fatalf("%s: the framebuffer request did not arrive", owner)
		}
		if got := vnc.received(); !bytes.Equal(got, fbRequest) {
			t.Fatalf("%s: forwarded %x", owner, got)
		}
	}

	s.state.Set(control.Human)
	vnc.reset()
	c.Write(keyEvent(1, 'y'))
	if !vnc.waitFor(keyEvent(1, 'y')) {
		t.Fatal("a key of the human did not arrive")
	}
}

func TestChangeOfOwnerReleasesWhatTheViewerHolds(t *testing.T) {
	s, ts, vnc := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.state.Set(control.Human)
	c := viewer(t, ctx, ts, vnc, session)

	c.Write(keyEvent(1, 'q'))
	c.Write(pointer(1, 10, 20))
	if !vnc.waitFor(pointer(1, 10, 20)) {
		t.Fatal("input did not arrive")
	}
	vnc.reset()
	s.state.Set(control.Paused)
	// Set waits for the listeners: it is all there when it returns, soon after.
	if !vnc.waitFor(keyEvent(0, 'q')) || !vnc.waitFor(pointer(0, 10, 20)) {
		t.Fatalf("not released: %x", vnc.received())
	}
}

func TestLeavingViewerReleasesWhatItHolds(t *testing.T) {
	s, ts, vnc := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.state.Set(control.Human)
	c := viewer(t, ctx, ts, vnc, session)
	c.Write(keyEvent(1, 'q'))
	if !vnc.waitFor(keyEvent(1, 'q')) {
		t.Fatal("the key did not arrive")
	}
	c.Close()
	if !vnc.waitFor(keyEvent(0, 'q')) {
		t.Fatal("the key was not released")
	}
}

func shortChecks(s *server) { s.sessionCheckInterval = 20 * time.Millisecond }

func shortHandshake(s *server) { s.handshakeTimeout = 100 * time.Millisecond }

// A sign-out comes from another process, and a session ends with time: neither
// may leave a viewer watching and typing.
func TestEndOfSessionClosesItsViewer(t *testing.T) {
	for name, end := range map[string]func(s *server, session string){
		"sign-out": func(s *server, _ string) { s.sessions.signOutAll() },
		"expiry": func(s *server, session string) {
			old := time.Now().Add(-sessionTTL - time.Hour)
			os.Chtimes(filepath.Join(s.sessions.dir, sessionsDir, hashName(session)), old, old)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, ts, vnc := gateway(t, shortChecks)
			session, _ := s.sessions.issue(sessionsDir)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s.state.Set(control.Human)
			c := viewer(t, ctx, ts, vnc, session)
			c.Write(keyEvent(1, 'h'))
			if !vnc.waitFor(keyEvent(1, 'h')) {
				t.Fatal("the key did not arrive")
			}
			vnc.reset()

			closed := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, c)
				closed <- err
			}()

			end(s, session)

			// The viewer does nothing: the gateway lets go of what it holds, and
			// closes its connection.
			if !vnc.waitFor(keyEvent(0, 'h')) {
				t.Fatal("the key was not released")
			}
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("the viewer is still connected")
			}
		})
	}
}

// A session that lasts keeps its viewer through these checks.
func TestValidSessionKeepsItsViewer(t *testing.T) {
	s, ts, vnc := gateway(t, shortChecks)
	session, _ := s.sessions.issue(sessionsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.state.Set(control.Human)
	c := viewer(t, ctx, ts, vnc, session)
	time.Sleep(200 * time.Millisecond)
	c.Write(keyEvent(1, 'k'))
	if !vnc.waitFor(keyEvent(1, 'k')) {
		t.Fatal("the viewer was closed")
	}
}

func TestViewerThatDoesNotShakeHandsIsClosed(t *testing.T) {
	s, ts, _ := gateway(t, shortHandshake)
	session, _ := s.sessions.issue(sessionsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, err := dialViewer(ctx, ts, session)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	start := time.Now()
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("read something")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("closed after %v", took)
	}
}

// The handshake's limit is over once it is done.
func TestHandshakeLimitDoesNotApplyAfterwards(t *testing.T) {
	s, ts, vnc := gateway(t, shortHandshake)
	session, _ := s.sessions.issue(sessionsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.state.Set(control.Human)
	c := viewer(t, ctx, ts, vnc, session)
	time.Sleep(300 * time.Millisecond)
	c.Write(keyEvent(1, 'k'))
	if !vnc.waitFor(keyEvent(1, 'k')) {
		t.Fatal("the viewer was closed")
	}
}

func TestViewersAreLimited(t *testing.T) {
	s, ts, _ := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var open []*websocket.Conn
	for i := range maxViewers {
		ws, err := dialViewer(ctx, ts, session)
		if err != nil {
			t.Fatalf("viewer %d: %v", i, err)
		}
		defer ws.CloseNow()
		open = append(open, ws)
	}
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/websockify", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {cookieName + "=" + session}}, Subprotocols: []string{"binary"},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("one too many: %v, %v", resp, err)
	}
	// A place comes back when a viewer leaves.
	open[0].CloseNow()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		ws, err := dialViewer(ctx, ts, session)
		if err == nil {
			ws.CloseNow()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no place back: %v", err)
		}
	}
}

// net.Pipe has no buffer: a write blocks until someone reads.
func TestVNCWritesHaveADeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	start := time.Now()
	_, err := deadlineWriter{a, 50 * time.Millisecond}.Write([]byte("x"))
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("write = %v after %v", err, time.Since(start))
	}
}

func rawStatus(t *testing.T, addr, request string) int {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, request)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("%q: %v", request, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestOnlyLoopbackNamesAreAccepted(t *testing.T) {
	s, ts, _ := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	addr := ts.Listener.Addr().String()
	for host, want := range map[string]int{
		"kivora.localhost:6080": 200, "kivora.localhost:16090": 200, "kivora.localhost": 200,
		"localhost:6080": 200, "localhost": 200, "127.0.0.1:680": 200, "[::1]:6080": 200, "[::1]": 200,
		"evil.example": 403, "evil.example:6080": 403, "KIVORA.LOCALHOST:6080": 403, "localhost.:6080": 403,
		"kivora.localhost.:6080": 403, "kivora.localhost.evil.example": 403, "127.0.0.1.nip.io": 403,
		"127.1:6080": 403, "0.0.0.0:6080": 403, "[::ffff:127.0.0.1]:6080": 403, "2130706433:6080": 403,
		"foo.localhost:6080": 403, "": 403,
	} {
		got := rawStatus(t, addr, fmt.Sprintf("GET /api/control HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\nConnection: close\r\n\r\n", host, cookieName, session))
		if got != want {
			t.Errorf("Host %q = %d, want %d", host, got, want)
		}
	}
	if got := rawStatus(t, addr, fmt.Sprintf("GET /api/control HTTP/1.0\r\nCookie: %s=%s\r\n\r\n", cookieName, session)); got != 403 {
		t.Errorf("no Host at all = %d", got)
	}
}

func TestChangingControlNeedsTheSameOrigin(t *testing.T) {
	s, ts, _ := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	addr := ts.Listener.Addr().String()
	const host = "kivora.localhost:6080"
	origins := map[string]bool{
		"": true, "http://kivora.localhost:6080": true, "https://kivora.localhost:6080": true,
		"null": false, "http://evil.example": false, "http://kivora.localhost:6081": false,
		"http://kivora.localhost": false, "http://localhost:6080": false, "HTTP://KIVORA.LOCALHOST:6080": false,
		"http://kivora.localhost:6080.evil.example": false, "http://kivora.localhost:6080/": false,
		"http://kivora.localhost:6080@evil.example": false, "chrome-extension://abc": false,
		"weird://kivora.localhost:6080": false, "http://KIVORA.localhost:6080": false,
	}
	const websocketHeaders = "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: binary\r\n"
	for origin, allowed := range origins {
		header := ""
		if origin != "" {
			header = "Origin: " + origin + "\r\n"
		}
		s.state.Set(control.Agent)
		body := `{"owner":"paused"}`
		got := rawStatus(t, addr, fmt.Sprintf("POST /api/control HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\n%sContent-Length: %d\r\nConnection: close\r\n\r\n%s", host, cookieName, session, header, len(body), body))
		if allowed && (got != 200 || !s.state.Is(control.Paused)) {
			t.Errorf("POST, Origin %q = %d, owner %s", origin, got, s.state.Get().Owner)
		}
		if !allowed && (got != 403 || !s.state.Is(control.Agent)) {
			t.Errorf("POST, Origin %q = %d, owner %s", origin, got, s.state.Get().Owner)
		}

		want := http.StatusSwitchingProtocols
		if !allowed {
			want = http.StatusForbidden
		}
		if got := rawStatus(t, addr, fmt.Sprintf("GET /websockify HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\n%s%s\r\n", host, cookieName, session, header, websocketHeaders)); got != want {
			t.Errorf("WebSocket, Origin %q = %d, want %d", origin, got, want)
		}
	}
	if got := rawStatus(t, addr, fmt.Sprintf("GET /websockify HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\n%s\r\n", host, host, websocketHeaders)); got != 401 {
		t.Errorf("WebSocket without a session = %d", got)
	}
	if got := rawStatus(t, addr, fmt.Sprintf("GET /websockify HTTP/1.1\r\nHost: evil.example\r\nCookie: %s=%s\r\nOrigin: http://evil.example\r\n%s\r\n", cookieName, session, websocketHeaders)); got != 403 {
		t.Errorf("WebSocket after a rebinding = %d", got)
	}
}

func TestControlAPI(t *testing.T) {
	s, ts, _ := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	addr := ts.Listener.Addr().String()
	post := func(body string) int {
		return rawStatus(t, addr, fmt.Sprintf("POST /api/control HTTP/1.1\r\nHost: localhost\r\nCookie: %s=%s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", cookieName, session, len(body), body))
	}
	for _, c := range []struct {
		body  string
		want  int
		owner control.Owner
	}{
		{`{"owner":"human"}`, 200, control.Human},
		{`{"owner":"root"}`, 400, control.Human},
		{`{"owner":""}`, 400, control.Human},
		{`{"owner":"HUMAN"}`, 400, control.Human},
		{`not json`, 400, control.Human},
		{strings.Repeat(" ", 2000) + `{"owner":"agent"}`, 400, control.Human},
		{`{"owner":"paused"}`, 200, control.Paused},
		{`{"owner":"agent"}`, 200, control.Agent},
	} {
		if got := post(c.body); got != c.want || !s.state.Is(c.owner) {
			t.Errorf("POST %.30q = %d, owner %s; want %d, %s", c.body, got, s.state.Get().Owner, c.want, c.owner)
		}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/control", nil)
	req.Host = "localhost"
	req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(string(body), `"owner":"agent"`) {
		t.Errorf("GET /api/control: %v %s", resp.Header, body)
	}
	for _, method := range []string{"PUT", "DELETE", "OPTIONS"} {
		if got := rawStatus(t, addr, fmt.Sprintf("%s /api/control HTTP/1.1\r\nHost: localhost\r\nCookie: %s=%s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", method, cookieName, session)); got != 405 {
			t.Errorf("%s /api/control = %d", method, got)
		}
	}
}

func TestSignInLinkEdgeCases(t *testing.T) {
	s, ts, _ := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	sessionFiles := func() int {
		files, _ := filepath.Glob(filepath.Join(s.sessions.dir, sessionsDir, "*"))
		return len(files)
	}
	do := func(method, path, cookie string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Host = "kivora.localhost:6080"
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// A browser already signed in uses up the link too.
	code, _ := s.sessions.issue(codesDir)
	if resp := do("GET", "/login?code="+code, session); resp.StatusCode != 200 {
		t.Fatalf("signed in: %d", resp.StatusCode)
	}
	if resp := do("GET", "/login?code="+code, ""); resp.StatusCode != 401 {
		t.Fatalf("the link still works elsewhere: %d", resp.StatusCode)
	}

	// A signed-in browser with no code or a wrong one just goes on.
	for _, path := range []string{"/login", "/login?code=nonsense"} {
		if resp := do("GET", path, session); resp.StatusCode != 200 {
			t.Errorf("signed in, %s: %d", path, resp.StatusCode)
		}
	}

	// HEAD uses nothing up, and creates no session.
	code, _ = s.sessions.issue(codesDir)
	before := sessionFiles()
	if resp := do("HEAD", "/login?code="+code, ""); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
		t.Fatalf("HEAD: %d %v", resp.StatusCode, resp.Header)
	}
	if sessionFiles() != before {
		t.Fatal("HEAD created a session")
	}
	if resp := do("GET", "/login?code="+code, ""); resp.StatusCode != 200 || len(resp.Cookies()) != 1 {
		t.Fatalf("after a HEAD: %d %v", resp.StatusCode, resp.Cookies())
	}

	// A wrong or spent code creates no session.
	before = sessionFiles()
	for _, path := range []string{"/login?code=nonsense", "/login?code=" + code, "/login"} {
		if resp := do("GET", path, ""); resp.StatusCode != 401 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	if sessionFiles() != before {
		t.Fatal("a refused link created a session")
	}
}

func TestFailedSignInKeepsTheCode(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs an unprivileged account")
	}
	s, ts, _ := gateway(t)
	code, _ := s.sessions.issue(codesDir)
	dir := filepath.Join(s.sessions.dir, sessionsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	get := func() int {
		req, _ := http.NewRequest("GET", ts.URL+"/login?code="+code, nil)
		req.Host = "localhost"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(); got != http.StatusInternalServerError {
		t.Fatalf("without room for a session: %d", got)
	}
	os.Chmod(dir, 0o700)
	if got := get(); got != 200 {
		t.Fatalf("the same link afterwards: %d", got)
	}
}

func TestSimultaneousUsesOfALinkMakeOneSession(t *testing.T) {
	s, ts, _ := gateway(t)
	code, _ := s.sessions.issue(codesDir)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 16 {
		wg.Go(func() {
			req, _ := http.NewRequest("GET", ts.URL+"/login?code="+code, nil)
			req.Host = "localhost"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				mu.Lock()
				won++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	files, _ := filepath.Glob(filepath.Join(s.sessions.dir, sessionsDir, "*"))
	if won != 1 || len(files) != 1 {
		t.Fatalf("%d uses won, %d sessions left", won, len(files))
	}
}

func TestPruneRemovesWhatExpired(t *testing.T) {
	s := &sessionStore{dir: t.TempDir()}
	oldCode, _ := s.issue(codesDir)
	newCode, _ := s.issue(codesDir)
	oldSession, _ := s.issue(sessionsDir)
	newSession, _ := s.issue(sessionsDir)
	age := func(kind, secret string, d time.Duration) {
		old := time.Now().Add(-d)
		os.Chtimes(filepath.Join(s.dir, kind, hashName(secret)), old, old)
	}
	age(codesDir, oldCode, loginCodeTTL+time.Minute)
	age(sessionsDir, oldSession, sessionTTL+time.Minute)
	s.prune()
	if s.pending(oldCode) || !s.pending(newCode) || !s.valid(newSession) {
		t.Fatal("the wrong secrets went")
	}
	if _, err := os.Stat(filepath.Join(s.dir, sessionsDir, hashName(oldSession))); err == nil {
		t.Fatal("an expired session is still there")
	}
}

func TestControlFileIsRewrittenInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), controlFile)
	state := control.New(control.Agent)
	if err := saveControl(path, state.Get()); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != controlFileSize {
		t.Fatalf("size = %d", before.Size())
	}
	state.RequestHuman(strings.Repeat("<é", control.MaxRequest))
	if err := saveControl(path, state.Get()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Size() != controlFileSize {
		t.Fatalf("the file was replaced, or grew to %d", after.Size())
	}
	// Directory not writable, as for a full disk: nothing new can be created.
	if os.Getuid() != 0 {
		dir := filepath.Dir(path)
		os.Chmod(dir, 0o500)
		defer os.Chmod(dir, 0o700)
		state.Set(control.Human)
		if err := saveControl(path, state.Get()); err != nil {
			t.Fatalf("saving needs more than the file: %v", err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := loadControl(path, log).Get(); got.Owner != control.Paused || got.Request == "" {
		t.Fatalf("after a restart: %+v", got)
	}
}

// A file of an older version is shorter: it must still load, then get its full size.
func TestControlFileOfAnOlderVersion(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), controlFile)
	os.WriteFile(path, []byte(`{"owner":"agent","since":"2026-11-01T00:00:00Z","seq":7}`), 0o600)
	state := loadControl(path, log)
	if got := state.Get(); got.Owner != control.Agent || got.Seq != 8 {
		t.Fatalf("loaded %+v", got)
	}
	state.Set(control.Paused)
	if err := saveControl(path, state.Get()); err != nil {
		t.Fatal(err)
	}
	if got := loadControl(path, log).Get(); got.Owner != control.Paused {
		t.Fatalf("after saving: %+v", got)
	}
	if st, _ := os.Stat(path); st.Size() != controlFileSize {
		t.Fatalf("size = %d", st.Size())
	}
}

func TestInitControl(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := filepath.Join(t.TempDir(), "new")
	sessions := &sessionStore{dir: dir}
	path := filepath.Join(dir, controlFile)

	if err := admin(sessions, []string{"init-control"}); err != nil {
		t.Fatal(err)
	}
	if got := loadControl(path, log).Get().Owner; got != control.Agent {
		t.Fatalf("new disk: owner = %s, want agent", got)
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("directory: %v, %v", st, err)
	}

	// Whatever exists stays: only the human gives control back.
	paused := control.New(control.Paused)
	if err := saveControl(path, paused.Get()); err != nil {
		t.Fatal(err)
	}
	if err := admin(sessions, []string{"init-control"}); err != nil {
		t.Fatal(err)
	}
	if got := loadControl(path, log).Get().Owner; got != control.Paused {
		t.Fatalf("existing state: owner = %s, want paused", got)
	}
	os.WriteFile(path, []byte("damaged"), 0o600)
	if err := admin(sessions, []string{"init-control"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "damaged" {
		t.Fatalf("a damaged state was replaced: %q", got)
	}
}

// A change that cannot be recorded must not let a restart resume an older owner.
func TestFailedRecordPausesTheNextStart(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs an unprivileged account")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, controlFile)
	if err := initControl(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := startGateway(t, ctx, dir, newFakeVNC(t), io.Discard)
	// Read-only, as after an I/O error: the record of the takeover fails.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	session, _ := (&sessionStore{dir: dir}).issue(sessionsDir)
	body := `{"owner":"human"}`
	if got := rawStatus(t, addr, fmt.Sprintf("POST /api/control HTTP/1.1\r\nHost: localhost\r\nCookie: %s=%s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", cookieName, session, len(body), body)); got != 200 {
		t.Fatalf("takeover = %d", got)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := loadControl(path, log).Get().Owner; got != control.Paused {
		t.Fatalf("after a restart: owner = %s, want paused", got)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startGateway runs the real gateway until ctx ends, and waits for it to answer.
// The channel closes when it has stopped.
func startGateway(t *testing.T, ctx context.Context, dir string, vnc *fakeVNC, logs io.Writer) (string, <-chan struct{}) {
	t.Helper()
	addr := freeAddr(t)
	done := make(chan struct{})
	log := slog.New(slog.NewTextHandler(logs, nil))
	novnc := t.TempDir()
	go func() {
		defer close(done)
		if err := run(ctx, addr, vnc.l.Addr().String(), novnc, dir, filepath.Join(dir, "control.sock"), log); err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the gateway did not stop")
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return addr, done
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
}

// Stopping lets go of what a viewer holds: a key must not stay down until the next
// start, and the requests under way may end.
func TestStoppingReleasesWhatViewersHold(t *testing.T) {
	dir := t.TempDir()
	if err := initControl(filepath.Join(dir, controlFile)); err != nil {
		t.Fatal(err)
	}
	vnc := newFakeVNC(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &lockedBuffer{}
	addr, stopped := startGateway(t, ctx, dir, vnc, logs)
	session, _ := (&sessionStore{dir: dir}).issue(sessionsDir)

	body := `{"owner":"human"}`
	if got := rawStatus(t, addr, fmt.Sprintf("POST /api/control HTTP/1.1\r\nHost: localhost\r\nCookie: %s=%s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", cookieName, session, len(body), body)); got != 200 {
		t.Fatalf("takeover = %d", got)
	}
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer wsCancel()
	ws, _, err := websocket.Dial(wsCtx, "ws://"+addr+"/websockify", &websocket.DialOptions{
		Host:         "localhost",
		HTTPHeader:   http.Header{"Cookie": {cookieName + "=" + session}},
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	c := websocket.NetConn(wsCtx, ws, websocket.MessageBinary)
	c.Write(rfbClientStart)
	c.Write(keyEvent(1, 'q'))
	if !vnc.waitFor(keyEvent(1, 'q')) {
		t.Fatal("the key did not arrive")
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway did not stop")
	}
	// The process may exit now: the viewer must be done with.
	if !strings.Contains(logs.String(), "viewer disconnected") {
		t.Fatalf("stopped before its viewer: %s", logs)
	}
	if !vnc.waitFor(keyEvent(0, 'q')) {
		t.Fatal("the key was not released")
	}
}

func TestStartWithoutThePortChangesNothing(t *testing.T) {
	dir := t.TempDir()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	socket := filepath.Join(dir, "control.sock")
	if err := os.WriteFile(socket, []byte("the running gateway's"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	err = run(context.Background(), taken.Addr().String(), "127.0.0.1:1", t.TempDir(), dir, socket, slog.New(slog.NewTextHandler(logs, nil)))
	if err == nil {
		t.Fatal("run succeeded")
	}
	if strings.Contains(logs.String(), "listening") {
		t.Fatalf("it said it was listening: %s", logs)
	}
	if got, _ := os.ReadFile(socket); string(got) != "the running gateway's" {
		t.Fatal("it touched the control socket")
	}
}

func TestStartRecordsTheState(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startGateway(t, ctx, dir, newFakeVNC(t), io.Discard)
	st, err := os.Stat(filepath.Join(dir, controlFile))
	if err != nil || st.Size() != controlFileSize {
		t.Fatalf("no room kept for the state: %v, %v", st, err)
	}
	// No state: paused, and it stays that way.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := loadControl(filepath.Join(dir, controlFile), log).Get().Owner; got != control.Paused {
		t.Fatalf("owner = %s", got)
	}
}

func TestServersHaveTimeouts(t *testing.T) {
	srv := newWebServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Fatalf("%+v", srv)
	}
	// The body of a request that never comes ends the connection.
	srv.ReadTimeout = 100 * time.Millisecond
	s := testServer(t)
	session, _ := s.sessions.issue(sessionsDir)
	srv.Handler = s.routes(fstest.MapFS{}, t.TempDir())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /api/control HTTP/1.1\r\nHost: localhost\r\nCookie: %s=%s\r\nContent-Length: 100\r\n\r\n{", cookieName, session)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("still open: %v", err)
	}
	if !s.state.Is(control.Agent) {
		t.Fatal("the owner changed")
	}
}

// A WebSocket outlives the limits of ordinary requests.
func TestWebSocketOutlivesRequestTimeouts(t *testing.T) {
	s, _, vnc := gateway(t)
	session, _ := s.sessions.issue(sessionsDir)
	srv := newWebServer(s.routes(fstest.MapFS{}, t.TempDir()))
	srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout = 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.Config = srv
	ts.Start()
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.state.Set(control.Human)
	c := viewer(t, ctx, ts, vnc, session)
	time.Sleep(300 * time.Millisecond)
	c.Write(keyEvent(1, 'k'))
	if !vnc.waitFor(keyEvent(1, 'k')) {
		t.Fatal("the viewer was closed")
	}
	vnc.send(t, "screen")
	buf := make([]byte, 6)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("the screen did not arrive: %v", err)
	}
}
