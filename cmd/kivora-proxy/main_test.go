package main

import (
	"bufio"
	"bytes"
	"context"
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
	"time"
)

func TestAllowed(t *testing.T) {
	list := "# comment\n\nexample.com\nExample.org:8443\n*.pythonhosted.org\n"
	cases := []struct {
		host, port string
		want       bool
	}{
		{"example.com", "443", true},
		{"EXAMPLE.com.", "443", true},
		{"example.org", "8443", true},
		{"files.pythonhosted.org", "443", true},
		{"example.com", "80", false},
		{"example.com", "8443", false},
		{"example.org", "443", false},
		{"pythonhosted.org", "443", false},
		{"evil-example.com", "443", false},
		{"example.com.evil.example", "443", false},
		{"evilpythonhosted.org", "443", false},
		{"127.0.0.1", "443", false},
	}
	for _, c := range cases {
		if got := allowed(list, c.host, c.port); got != c.want {
			t.Errorf("allowed(%s, %s) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	if allowed("", "example.com", "443") {
		t.Error("an empty list allows something")
	}
}

// start runs a proxy whose connections all reach an echo server, whatever the
// name, as the test cannot depend on the network.
func start(t *testing.T, list string) (addr string) {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	path := filepath.Join(t.TempDir(), "allowlist")
	if err := os.WriteFile(path, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newProxy(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func connect(t *testing.T, addr, target string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	return resp, c, r
}

func TestTunnelsOnlyToAllowedSites(t *testing.T) {
	addr := start(t, "allowed.example\n")

	resp, c, r := connect(t, addr, "allowed.example:443")
	if resp.StatusCode != 200 {
		t.Fatalf("allowed site: %s", resp.Status)
	}
	fmt.Fprintln(c, "through the tunnel")
	if line, err := r.ReadString('\n'); err != nil || line != "through the tunnel\n" {
		t.Fatalf("tunnel answered %q, %v", line, err)
	}

	for _, target := range []string{"other.example:443", "allowed.example:80", "allowed.example:22"} {
		if resp, _, _ := connect(t, addr, target); resp.StatusCode != 403 {
			t.Errorf("%s: %s", target, resp.Status)
		}
	}
}

func TestPlainHTTPIsRefused(t *testing.T) {
	addr := start(t, "allowed.example\n")
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET http://allowed.example/ HTTP/1.1\r\nHost: allowed.example\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("plain HTTP: %v, %v", resp, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "only HTTPS") {
		t.Fatalf("body = %q", body)
	}
}

func TestAMissingListAllowsNothing(t *testing.T) {
	p := newProxy(filepath.Join(t.TempDir(), "absent"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(p)
	defer srv.Close()
	if resp, _, _ := connect(t, srv.Listener.Addr().String(), "example.com:443"); resp.StatusCode != 403 {
		t.Fatalf("status = %s", resp.Status)
	}
}

func TestANameResolvingHereIsRefused(t *testing.T) {
	// The real dialer refuses this computer's addresses after resolution, even
	// for an allowed name.
	path := filepath.Join(t.TempDir(), "allowlist")
	os.WriteFile(path, []byte("localhost:443\n"), 0o644)
	srv := httptest.NewServer(newProxy(path, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()
	if resp, _, _ := connect(t, srv.Listener.Addr().String(), "localhost:443"); resp.StatusCode != 502 {
		t.Fatalf("status = %s", resp.Status)
	}
}

func TestTunnelsAreLimited(t *testing.T) {
	addr := start(t, "allowed.example\n")
	for i := range maxTunnels {
		if resp, _, _ := connect(t, addr, "allowed.example:443"); resp.StatusCode != 200 {
			t.Fatalf("tunnel %d: %s", i, resp.Status)
		}
	}
	if resp, _, _ := connect(t, addr, "allowed.example:443"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("one tunnel too many: %s", resp.Status)
	}
}

// The worker chooses what it asks for, and the log is on the system disk.
func TestRefusalsTakeLittleRoomInTheLog(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	path := filepath.Join(t.TempDir(), "allowlist")
	os.WriteFile(path, nil, 0o644)
	p := newProxy(path, slog.New(slog.NewTextHandler(lockedWriter{&mu, &logged}, nil)))
	srv := httptest.NewUnstartedServer(p)
	srv.Config = newServer("", p)
	srv.Start()
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	huge := strings.Repeat("a", 1<<20)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", huge, huge)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("a huge request: %v, %v", resp, err)
	}

	long := strings.Repeat("b", 2000) + ".example:443"
	if resp, _, _ := connect(t, addr, long); resp.StatusCode != 403 {
		t.Fatalf("a long name: %s", resp.Status)
	}
	for range 100 {
		connect(t, addr, "other.example:443")
	}
	mu.Lock()
	defer mu.Unlock()
	if lines := strings.Count(logged.String(), "\n"); lines > 5 {
		t.Errorf("%d lines for one second of requests:\n%s", lines, logged.String())
	}
	if logged.Len() > 2000 {
		t.Errorf("%d bytes logged", logged.Len())
	}
}

func TestLogLimitCountsWhatItDrops(t *testing.T) {
	var l logLimit
	now := time.Now()
	if ok, n := l.allow(now); !ok || n != 0 {
		t.Fatalf("first line: %v, %d", ok, n)
	}
	for range 3 {
		if ok, _ := l.allow(now.Add(time.Millisecond)); ok {
			t.Fatal("a line within the second was allowed")
		}
	}
	if ok, n := l.allow(now.Add(2 * time.Second)); !ok || n != 3 {
		t.Fatalf("next line: %v, %d", ok, n)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
