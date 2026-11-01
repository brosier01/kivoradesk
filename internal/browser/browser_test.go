package browser

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsWebPage(t *testing.T) {
	cases := map[string]bool{
		"https://example.com/a":           true,
		"http://example.com":              true,
		"about:blank":                     true,
		"about:srcdoc":                    true,
		"data:text/plain,hello":           true,
		"blob:https://example.com/1234":   true,
		"file:///etc/passwd":              false,
		"FILE:///etc/passwd":              false,
		"blob:file:///1234":               false,
		"chrome://settings":               false,
		"chrome-error://chromewebdata/":   false,
		"chrome-extension://abc/x.html":   false,
		"devtools://devtools/bundled/x":   false,
		"view-source:https://example.com": false,
		"about:settings":                  false,
		"":                                false,
	}
	for in, want := range cases {
		if got := isWebPage(in); got != want {
			t.Errorf("isWebPage(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCheckURLRefusesLocalSchemes(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "chrome://settings", "javascript://x", "view-source:https://a.example"} {
		if got, err := checkURL(raw); err == nil {
			t.Errorf("checkURL(%q) = %q", raw, got)
		}
	}
}

func TestDownloadsNeverReachThisComputer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secret") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	client := &http.Client{Transport: downloadTransport}
	for _, u := range []string{srv.URL, "http://localhost:" + port} {
		resp, err := client.Get(u)
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s was reached", u)
		} else if !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v", u, err)
		}
	}
}
