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

func TestIsWebPageRefusesThisComputer(t *testing.T) {
	for _, in := range []string{
		"http://127.0.0.1:9222/json/list",
		"http://localhost:680/",
		"http://kivora.localhost:680/",
		"http://[::1]:9222/",
		"http://[::ffff:7f00:1]:9222/json/list",
		"http://[::ffff:127.0.0.1]:9222/json/list",
		"blob:http://localhost/1234",
	} {
		if isWebPage(in) {
			t.Errorf("isWebPage(%q) = true", in)
		}
	}
}

func TestCheckURLRefusesThisComputer(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1:9222/json", "http://127.0.0.1:9222", "http://127.1:9222", "http://127.0.0.1.:9222",
		"http://2130706433/", "http://0x7f000001/", "http://0x7f.1/", "http://017700000001/", "http://0177.0.0.1/",
		"http://localhost/", "http://LOCALHOST:680/", "http://localhost.:680/", "http://kivora.localhost:680/",
		"http://[::1]/", "http://[::ffff:127.0.0.1]:9222/", "http://[::ffff:7f00:1]:9222/", "http://0.0.0.0/", "http://0/", "http://[::]/",
		"http://169.254.169.254/",
	} {
		if got, err := checkURL(raw); err == nil {
			t.Errorf("checkURL(%q) = %q", raw, got)
		} else if !strings.Contains(err.Error(), "this computer") {
			t.Errorf("checkURL(%q): %v", raw, err)
		}
	}
	for raw, want := range map[string]string{
		"example.com":              "https://example.com",
		"http://example.com/a?b=c": "http://example.com/a?b=c",
		"about:blank":              "about:blank",
		"https://1.1.1.1/":         "https://1.1.1.1/",
		"https://16843009/":        "https://16843009/",
	} {
		if got, err := checkURL(raw); err != nil || got != want {
			t.Errorf("checkURL(%q) = %q, %v", raw, got, err)
		}
	}
}

func TestLegacyIPv4(t *testing.T) {
	for in, want := range map[string]string{
		"2130706433":   "127.0.0.1",
		"0x7f000001":   "127.0.0.1",
		"017700000001": "127.0.0.1",
		"127.1":        "127.0.0.1",
		"127.0.1":      "127.0.0.1",
		"0x7f.0.0.1":   "127.0.0.1",
		"10.0.513":     "10.0.2.1",
		"example.com":  "<nil>",
		"1.2.3.4.5":    "<nil>",
		"256.1":        "<nil>",
		"1.2.3.256":    "<nil>",
		"1_0":          "<nil>",
	} {
		if got := fmt.Sprint(legacyIPv4(in)); got != want {
			t.Errorf("legacyIPv4(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCutKeepsWholeCharacters(t *testing.T) {
	if got := cut("aé", 2); got != "a" {
		t.Errorf("cut = %q", got)
	}
	if got := cut("abc", 3); got != "abc" {
		t.Errorf("cut = %q", got)
	}
}
