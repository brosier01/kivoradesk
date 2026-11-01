package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/brosier01/kivoradesk/internal/control"
)

func TestControlStateAcrossRestarts(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "control.json")

	if got := loadControl(path, log).Get().Owner; got != control.Agent {
		t.Fatalf("first start: owner = %s, want agent", got)
	}

	state := control.New(control.Agent)
	save := func() {
		t.Helper()
		if err := saveControl(path, state.Get()); err != nil {
			t.Fatal(err)
		}
	}
	state.Set(control.Human)
	save()
	if got := loadControl(path, log).Get().Owner; got != control.Paused {
		t.Fatalf("restart during a takeover: owner = %s, want paused", got)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v, %v", st, err)
	}

	state.Set(control.Agent)
	save()
	if got := loadControl(path, log).Get().Owner; got != control.Agent {
		t.Fatalf("restart with the agent in control: owner = %s, want agent", got)
	}

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadControl(path, log).Get().Owner; got != control.Paused {
		t.Fatalf("unreadable state: owner = %s, want paused", got)
	}
}

func TestRoutesServeOnlyWhatTheViewerNeeds(t *testing.T) {
	novnc := t.TempDir()
	for _, f := range []string{"core/rfb.js", "vendor/pako.js", "vnc.html", "tests/x.js"} {
		os.MkdirAll(filepath.Dir(filepath.Join(novnc, f)), 0o755)
		os.WriteFile(filepath.Join(novnc, f), []byte("x"), 0o644)
	}
	static := fstest.MapFS{"index.html": {Data: []byte("viewer")}, "app.js": {Data: []byte("x")}}
	s := &server{token: "tok", state: control.New(control.Agent), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.routes(static, novnc)

	for _, c := range []struct {
		path     string
		signedIn bool
		host     string
		want     int
	}{
		{"/novnc/core/rfb.js", true, "localhost:6080", 200},
		{"/novnc/vendor/pako.js", true, "localhost:6080", 200},
		{"/ui/app.js", true, "localhost:6080", 200},
		{"/novnc/core/rfb.js", false, "localhost:6080", 401},
		{"/novnc/vnc.html", true, "localhost:6080", 404},
		{"/novnc/tests/x.js", true, "localhost:6080", 404},
		{"/novnc/core/", true, "localhost:6080", 404},
		{"/ui/", true, "localhost:6080", 404},
		{"/", true, "evil.example", 403},
	} {
		req := httptest.NewRequest("GET", c.path, nil)
		req.Host = c.host
		if c.signedIn {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: "tok"})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("GET %s (signed in: %v, host %s) = %d, want %d", c.path, c.signedIn, c.host, w.Code, c.want)
		}
		for name, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
			if got := w.Header().Get(name); got != want {
				t.Errorf("GET %s: %s = %q", c.path, name, got)
			}
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s: no Content-Security-Policy", c.path)
		}
	}
}
