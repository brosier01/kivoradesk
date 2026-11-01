package main

import (
	"bufio"
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
	"testing"
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
