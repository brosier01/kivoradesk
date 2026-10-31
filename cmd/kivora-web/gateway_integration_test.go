//go:build integration

// Integration test against a running gateway, through the SSH tunnel:
//
//	KIVORA_WEB_URL=http://localhost:6080 \
//	KIVORA_WEB_TOKEN=$(...) \
//	KIVORA_XDOTOOL="scripts/vm/ssh.sh doas -u agent env DISPLAY=:1 xdotool" \
//	go test -tags integration ./cmd/kivora-web
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type env struct {
	url, token string
	xdotool    []string
}

func setup(t *testing.T) env {
	e := env{url: os.Getenv("KIVORA_WEB_URL"), token: os.Getenv("KIVORA_WEB_TOKEN")}
	e.xdotool = strings.Fields(os.Getenv("KIVORA_XDOTOOL"))
	if e.url == "" || e.token == "" || len(e.xdotool) == 0 {
		t.Skip("KIVORA_WEB_URL, KIVORA_WEB_TOKEN and KIVORA_XDOTOOL are required")
	}
	return e
}

func (e env) request(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookieName+"="+e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e env) setOwner(t *testing.T, owner string) {
	t.Helper()
	if resp := e.request(t, "POST", "/api/control", fmt.Sprintf(`{"owner":%q}`, owner)); resp.StatusCode != 200 {
		t.Fatalf("set owner %s: HTTP %d", owner, resp.StatusCode)
	}
}

func (e env) run(t *testing.T, args ...string) string {
	t.Helper()
	cmd := append(append([]string{}, e.xdotool...), args...)
	out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", cmd, err, out)
	}
	return string(out)
}

func (e env) pointer(t *testing.T) string {
	var x, y string
	for _, line := range strings.Split(e.run(t, "getmouselocation", "--shell"), "\n") {
		if v, ok := strings.CutPrefix(line, "X="); ok {
			x = v
		}
		if v, ok := strings.CutPrefix(line, "Y="); ok {
			y = v
		}
	}
	return x + "," + y
}

// rfbConnect performs the RFB 3.8 handshake and returns the screen size.
func rfbConnect(t *testing.T, ctx context.Context, e env) (io.ReadWriter, uint16, uint16) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(e.url, "http") + "/websockify"
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Cookie": {cookieName + "=" + e.token}},
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	c := websocket.NetConn(ctx, ws, websocket.MessageBinary)

	read := func(n int) []byte {
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		return buf
	}
	if v := read(12); !bytes.HasPrefix(v, []byte("RFB 003.")) {
		t.Fatalf("server version %q", v)
	}
	c.Write([]byte("RFB 003.008\n"))
	types := read(int(read(1)[0]))
	if !bytes.Contains(types, []byte{1}) {
		t.Fatalf("server does not offer security type None: %v", types)
	}
	c.Write([]byte{1})
	if r := binary.BigEndian.Uint32(read(4)); r != 0 {
		t.Fatalf("security result %d", r)
	}
	c.Write([]byte{1}) // shared session
	init := read(24)
	read(int(binary.BigEndian.Uint32(init[20:]))) // desktop name
	return c, binary.BigEndian.Uint16(init[0:]), binary.BigEndian.Uint16(init[2:])
}

func TestViewerInputFollowsOwner(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	e.setOwner(t, "agent")
	t.Cleanup(func() { e.setOwner(t, "agent") })
	c, w, h := rfbConnect(t, ctx, e)
	if w != 1280 || h != 800 {
		t.Errorf("screen is %dx%d, want 1280x800", w, h)
	}

	move := func(x, y uint16) {
		if _, err := c.Write(pointerEvent(0, x, y)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	e.run(t, "mousemove", "5", "5")
	move(111, 222)
	if got := e.pointer(t); got != "5,5" {
		t.Fatalf("pointer moved to %s while the agent owns the desktop", got)
	}

	e.setOwner(t, "paused")
	move(111, 222)
	if got := e.pointer(t); got != "5,5" {
		t.Fatalf("pointer moved to %s while paused", got)
	}

	e.setOwner(t, "human")
	move(333, 444)
	if got := e.pointer(t); got != "333,444" {
		t.Fatalf("pointer at %s after a human move to 333,444", got)
	}
}

func TestRejectsMissingToken(t *testing.T) {
	e := setup(t)
	resp, err := http.Get(e.url + "/api/control")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/control without token: HTTP %d, want 401", resp.StatusCode)
	}
}

func TestRejectsCrossOriginControl(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("POST", e.url+"/api/control", strings.NewReader(`{"owner":"human"}`))
	req.Header.Set("Cookie", cookieName+"="+e.token)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: HTTP %d, want 403", resp.StatusCode)
	}
}

func pointerEvent(buttons uint8, x, y uint16) []byte {
	msg := []byte{5, buttons, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(msg[2:], x)
	binary.BigEndian.PutUint16(msg[4:], y)
	return msg
}
