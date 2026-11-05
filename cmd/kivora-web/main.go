// Command kivora-web serves the live view of the KivoraDesk desktop and owns
// the input control state (agent, paused or human).
//
// It listens on localhost inside the VM; the human reaches it through an SSH
// tunnel. Every request must carry a session, set as a cookie by the one-time
// URL printed by "kivora web-url". Viewer input is relayed to the VNC
// server only while the human owns the desktop.
//
// Run as its account, it also manages sign-in, without the gateway restarting:
//
//	kivora-web login-code    print a sign-in code: works once, for 5 minutes
//	kivora-web session       print a new session, for tests
//	kivora-web sign-out      end every session
//	kivora-web init-control  give the agent control on a new data disk
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/brosier01/kivoradesk/internal/control"
	"github.com/brosier01/kivoradesk/internal/rfb"
	"github.com/coder/websocket"
)

//go:embed web
var webFS embed.FS

const cookieName = "kivora_session"

const defaultStateDir = "/var/lib/kivora-web"

const (
	controlFile = "control.json"
	// The state is padded to this size and rewritten in place: once the file
	// exists, recording a change needs no free block, so a full disk cannot make
	// a restart resume from an older owner.
	controlFileSize = 4096

	// Enough for a few browsers: each one costs a connection to the VNC server.
	maxViewers = 16
	// A VNC server that stopped reading must not hold a change of owner, which
	// waits for held keys to be released.
	vncWriteTimeout = 5 * time.Second
	pruneInterval   = time.Hour
)

type server struct {
	sessions *sessionStore
	vncAddr  string
	state    *control.State
	log      *slog.Logger

	handshakeTimeout time.Duration
	// How long a viewer may stay connected once its session has ended.
	sessionCheckInterval time.Duration

	viewerCount atomic.Int32
	viewers     sync.WaitGroup
	// stopping closes when the gateway stops: viewers let go of what they hold.
	stopping chan struct{}
}

func newServer(sessions *sessionStore, vncAddr string, state *control.State, log *slog.Logger) *server {
	return &server{
		sessions: sessions, vncAddr: vncAddr, state: state, log: log,
		handshakeTimeout:     10 * time.Second,
		sessionCheckInterval: 2 * time.Second,
		stopping:             make(chan struct{}),
	}
}

func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		if err := admin(&sessionStore{dir: defaultStateDir}, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "kivora-web:", err)
			os.Exit(1)
		}
		return
	}
	listen := flag.String("listen", "127.0.0.1:6080", "address to listen on")
	vncAddr := flag.String("vnc", "127.0.0.1:5900", "VNC server address")
	novnc := flag.String("novnc", "/usr/share/novnc", "directory holding the noVNC client")
	stateDir := flag.String("state-dir", defaultStateDir, "directory for sessions and the control state")
	controlSocket := flag.String("control-socket", "/run/kivora/control.sock", "Unix socket serving the agent's control API")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *listen, *vncAddr, *novnc, *stateDir, *controlSocket, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// admin runs the commands managing sign-in.
func admin(sessions *sessionStore, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kivora-web login-code | session | sign-out | init-control")
	}
	var kind string
	switch args[0] {
	case "login-code":
		kind = codesDir
	case "session":
		kind = sessionsDir
	case "sign-out":
		return sessions.signOutAll()
	case "init-control":
		return initControl(filepath.Join(sessions.dir, controlFile))
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	secret, err := sessions.issue(kind)
	if err != nil {
		return err
	}
	fmt.Println(secret)
	return nil
}

func run(ctx context.Context, listen, vncAddr, novnc, stateDir, controlSocket string, log *slog.Logger) error {
	// First: without the port, this instance must not disturb the one that has it.
	web, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	// The permanent token of older versions must stop working.
	os.Remove(filepath.Join(stateDir, "token"))
	sessions := &sessionStore{dir: stateDir}
	sessions.prune()
	path := filepath.Join(stateDir, controlFile)
	s := newServer(sessions, vncAddr, loadControl(path, log), log)
	if err := saveControl(path, s.state.Get()); err != nil {
		log.Error("save control state", "err", err)
	}
	var saving sync.Mutex
	s.state.Persist(func(control.Snapshot) {
		// The latest state, whatever the order of concurrent changes.
		saving.Lock()
		defer saving.Unlock()
		if err := saveControl(path, s.state.Get()); err != nil {
			log.Error("save control state", "err", err)
			// A restart must not resume from an older owner: without a file, it
			// starts paused.
			os.Remove(path)
		}
	})

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	srv := newWebServer(s.routes(static, novnc))
	// The agent's control API: group-accessible socket, so only accounts in
	// the gateway's group (the desktop user) can reach it.
	os.Remove(controlSocket)
	ln, err := net.Listen("unix", controlSocket)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(controlSocket, 0o660); err != nil {
		return err
	}
	// No limit on writing: a wait holds its answer for minutes.
	agentSrv := &http.Server{
		Handler:           control.AgentHandler(s.state),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go agentSrv.Serve(ln)

	go func() {
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sessions.prune()
			case <-ctx.Done():
				return
			}
		}
	}()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		close(s.stopping)
		agentSrv.Close() // waiters hold long requests; do not wait for them
		srv.Shutdown(shutdown)
		// Shutdown does not wait for WebSockets, and the viewers still have to
		// release what they hold.
		viewers := make(chan struct{})
		go func() {
			s.viewers.Wait()
			close(viewers)
		}()
		select {
		case <-viewers:
		case <-shutdown.Done():
		}
	}()

	log.Info("listening", "addr", listen, "vnc", vncAddr, "control", controlSocket)
	if err := srv.Serve(web); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}

// newWebServer limits ordinary requests. A WebSocket is hijacked, which clears
// these deadlines.
func newWebServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

func (s *server) routes(static fs.FS, novnc string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", s.page(static))
	mux.HandleFunc("GET /login", s.login)
	mux.Handle("GET /ui/", s.auth(http.StripPrefix("/ui/", noListing(http.FileServerFS(static)))))
	// Only what the viewer loads from noVNC: not its own pages or its tests. Each
	// prefix has its own root: an encoded ".." goes through the routes, and must
	// stay inside.
	for _, dir := range []string{"core", "vendor"} {
		prefix := "/novnc/" + dir + "/"
		files := http.FileServer(http.Dir(filepath.Join(novnc, dir)))
		mux.Handle("GET "+prefix, s.auth(http.StripPrefix(prefix, noListing(files))))
	}
	mux.Handle("GET /api/control", s.auth(http.HandlerFunc(s.getControl)))
	mux.Handle("POST /api/control", s.auth(http.HandlerFunc(s.setControl)))
	mux.Handle("GET /websockify", s.auth(http.HandlerFunc(s.websockify)))
	return secureHeaders(s.checkHost(mux))
}

// noListing refuses directory listings.
func noListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secureHeaders applies to every answer: a page of this origin holds the
// controls of the desktop.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// loadControl resumes the input control state of the previous run, so that a
// restart never hands the desktop to the agent behind the human's back. The agent
// is paused if the state is missing or cannot be read: only a new data disk starts
// with the agent in control (see initControl).
func loadControl(path string, log *slog.Logger) *control.State {
	b, err := os.ReadFile(path)
	var saved control.Snapshot
	if err == nil {
		err = json.Unmarshal(b, &saved)
	}
	if err != nil {
		log.Warn("no control state to resume, pausing the agent", "err", err)
		return control.New(control.Paused)
	}
	return control.Restore(saved)
}

func saveControl(path string, snap control.Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if len(b) < controlFileSize {
		b = append(b, bytes.Repeat([]byte{' '}, controlFileSize-len(b))...)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteAt(b, 0); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// initControl gives the agent control on a new data disk. It does nothing if a
// state exists, whatever it says: only the human gives control back.
func initControl(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return saveControl(path, control.New(control.Agent).Get())
}

// checkHost rejects requests whose Host is not a loopback name, which defeats
// DNS rebinding through the SSH tunnel. kivora.localhost is the name to use: a
// cookie set on localhost also goes to every other service of the laptop on it.
func (s *server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "kivora.localhost" && host != "localhost" && host != "127.0.0.1" && host != "[::1]" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) authorized(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && s.sessions.valid(c.Value)
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// login trades a sign-in code for a session, in a strict same-site cookie. The
// code is only used up once the session exists, and a preview tool asking for the
// headers does not use it up.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := r.URL.Query().Get("code")
	if s.authorized(r) {
		s.sessions.redeem(code) // the link works once, whoever opens it
		signedIn(w)
		return
	}
	if !s.sessions.pending(code) {
		invalidLink(w)
		return
	}
	session, err := s.sessions.issue(sessionsDir)
	if err != nil {
		s.log.Error("create a session", "err", err)
		http.Error(w, "could not create a session", http.StatusInternalServerError)
		return
	}
	if !s.sessions.redeem(code) { // another request was first
		s.sessions.revoke(session)
		invalidLink(w)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	s.log.Info("signed in", "remote", r.RemoteAddr)
	signedIn(w)
}

func invalidLink(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, "This sign-in link is invalid, expired or already used. Get a new one on the server with: kivora web-url.\n")
}

// signedIn answers with a page that moves to the viewer, not with a redirect. A
// link opened from another site carries that site through its redirects, and a
// strict cookie is not sent then. From this page, the browser comes from this site.
func signedIn(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	io.WriteString(w, `<!doctype html>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0; url=/">
<title>KivoraDesk</title>
<p>Signed in. <a href="/">Open the live view</a>.</p>
`)
}

// page serves the viewer.
func (s *server) page(static fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "Open the URL printed on the server by kivora web-url to sign in.\n")
			return
		}
		http.ServeFileFS(w, r, static, "index.html")
	})
}

func (s *server) getControl(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.state.Get())
}

func (s *server) setControl(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request", http.StatusForbidden)
		return
	}
	var req struct {
		Owner string `json:"owner"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	owner, err := control.ParseOwner(req.Owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	snap := s.state.Set(owner)
	s.log.Info("control changed", "owner", snap.Owner, "seq", snap.Seq, "by", "web")
	writeJSON(w, snap)
}

// websockify relays a noVNC connection to the VNC server through the input
// filter, as long as its session lasts.
func (s *server) websockify(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request", http.StatusForbidden)
		return
	}
	if s.viewerCount.Add(1) > maxViewers {
		s.viewerCount.Add(-1)
		http.Error(w, "too many viewers", http.StatusServiceUnavailable)
		return
	}
	defer s.viewerCount.Add(-1)
	s.viewers.Add(1)
	defer s.viewers.Done()

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return // Accept already replied
	}
	defer ws.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	vnc, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", s.vncAddr)
	if err != nil {
		s.log.Error("dial VNC", "err", err)
		ws.Close(websocket.StatusInternalError, "VNC server unavailable")
		return
	}
	defer vnc.Close()
	client := websocket.NetConn(ctx, ws, websocket.MessageBinary)

	var leaving atomic.Bool
	filter := rfb.NewFilter(deadlineWriter{vnc, vncWriteTimeout}, func() bool {
		return !leaving.Load() && s.state.Is(control.Human)
	})
	stop := s.state.OnChange(func(snap control.Snapshot) {
		if snap.Owner != control.Human {
			if err := filter.Release(); err != nil {
				// Only a new connection puts the two sides back in step.
				s.log.Warn("release held input", "err", err)
				vnc.Close()
			}
		}
	})
	defer stop()

	s.log.Info("viewer connected", "remote", r.RemoteAddr)
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(client, vnc)
		done <- struct{}{}
	}()
	go func() {
		client.SetReadDeadline(time.Now().Add(s.handshakeTimeout))
		err := filter.Handshake(client)
		if err == nil {
			client.SetReadDeadline(time.Time{})
			err = filter.Forward(client)
		}
		if err != nil {
			s.log.Warn("viewer stream", "err", err)
		}
		done <- struct{}{}
	}()

	s.waitForViewerEnd(r, done)
	// In this order: nothing more gets through, then what is held is let go.
	leaving.Store(true)
	filter.Release()
	s.log.Info("viewer disconnected", "remote", r.RemoteAddr)
}

// waitForViewerEnd returns when the viewer's connection ends, its session ends
// (a sign-out comes from another process), or the gateway stops.
func (s *server) waitForViewerEnd(r *http.Request, done <-chan struct{}) {
	check := time.NewTicker(s.sessionCheckInterval)
	defer check.Stop()
	for {
		select {
		case <-done:
			return
		case <-s.stopping:
			return
		case <-check.C:
			if !s.authorized(r) {
				s.log.Info("session ended, closing its viewer", "remote", r.RemoteAddr)
				return
			}
		}
	}
}

// deadlineWriter bounds each write to a connection.
type deadlineWriter struct {
	conn    net.Conn
	timeout time.Duration
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	d.conn.SetWriteDeadline(time.Now().Add(d.timeout))
	return d.conn.Write(p)
}

// sameOrigin accepts requests without an Origin header (non-browser clients)
// and browser requests whose Origin matches the Host.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://"+r.Host || origin == "https://"+r.Host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
