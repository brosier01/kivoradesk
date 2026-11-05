package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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
	s := testServer(t)
	h := s.routes(static, novnc)
	session, err := s.sessions.issue(sessionsDir)
	if err != nil {
		t.Fatal(err)
	}

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
			req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
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

func testServer(t *testing.T) *server {
	t.Helper()
	return &server{
		sessions: &sessionStore{dir: t.TempDir()},
		state:    control.New(control.Agent),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSignInLinkWorksOnce(t *testing.T) {
	s := testServer(t)
	h := s.routes(fstest.MapFS{"index.html": {Data: []byte("viewer")}}, t.TempDir())
	get := func(path, session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "kivora.localhost:6080"
		if session != "" {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: session})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	code, err := s.sessions.issue(codesDir)
	if err != nil {
		t.Fatal(err)
	}
	w := get("/login?code="+code, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `http-equiv="refresh" content="0; url=/"`) {
		t.Fatalf("sign-in = %d, %q", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), code) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("sign-in page: %q, %v", w.Body, w.Header())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", cookies)
	}
	session := cookies[0].Value
	if session == code {
		t.Fatal("the session is the code of the link")
	}
	if w := get("/", session); w.Code != 200 {
		t.Fatalf("with the session: %d", w.Code)
	}

	// The same link does not work twice.
	if w := get("/login?code="+code, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("second use: %d", w.Code)
	}
	for _, bad := range []string{"/login", "/login?code=", "/login?code=nonsense", "/?token=" + code, "/?token=" + session} {
		if w := get(bad, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d", bad, w.Code)
		}
	}
	// A link that was not used expires, and stops working after a sign-out.
	expired, _ := s.sessions.issue(codesDir)
	old := time.Now().Add(-loginCodeTTL - time.Minute)
	os.Chtimes(filepath.Join(s.sessions.dir, codesDir, hashName(expired)), old, old)
	if w := get("/login?code="+expired, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired link: %d", w.Code)
	}
	unused, _ := s.sessions.issue(codesDir)
	if err := s.sessions.signOutAll(); err != nil {
		t.Fatal(err)
	}
	if w := get("/", session); w.Code != http.StatusUnauthorized {
		t.Fatalf("session after sign-out: %d", w.Code)
	}
	if w := get("/login?code="+unused, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("link after sign-out: %d", w.Code)
	}
}

func TestStoredFilesDoNotGiveAccess(t *testing.T) {
	s := &sessionStore{dir: t.TempDir()}
	secret, err := s.issue(sessionsDir)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(s.dir, sessionsDir, "*"))
	if len(files) != 1 || strings.Contains(files[0], secret) {
		t.Fatalf("files = %v", files)
	}
	if s.valid(filepath.Base(files[0])) {
		t.Fatal("the name of a file works as a session")
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	old := time.Now().Add(-sessionTTL - time.Hour)
	os.Chtimes(files[0], old, old)
	if s.valid(secret) {
		t.Fatal("an expired session works")
	}
}
