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
//	kivora-web login-code   print a sign-in code: works once, for 5 minutes
//	kivora-web session      print a new session, for tests
//	kivora-web sign-out     end every session
package main

import (
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

type server struct {
	sessions *sessionStore
	vncAddr  string
	state    *control.State
	log      *slog.Logger
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
	if err := run(*listen, *vncAddr, *novnc, *stateDir, *controlSocket, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// admin runs the commands managing sign-in.
func admin(sessions *sessionStore, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kivora-web login-code | session | sign-out")
	}
	var kind string
	switch args[0] {
	case "login-code":
		kind = codesDir
	case "session":
		kind = sessionsDir
	case "sign-out":
		return sessions.signOutAll()
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

func run(listen, vncAddr, novnc, stateDir, controlSocket string, log *slog.Logger) error {
	// The permanent token of older versions must stop working.
	os.Remove(filepath.Join(stateDir, "token"))
	sessions := &sessionStore{dir: stateDir}
	sessions.prune()
	controlFile := filepath.Join(stateDir, "control.json")
	s := &server{sessions: sessions, vncAddr: vncAddr, state: loadControl(controlFile, log), log: log}
	var saving sync.Mutex
	s.state.OnChange(func(control.Snapshot) {
		// The latest state, whatever the order of concurrent changes.
		saving.Lock()
		defer saving.Unlock()
		if err := saveControl(controlFile, s.state.Get()); err != nil {
			log.Error("save control state", "err", err)
		}
	})

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           s.routes(static, novnc),
		ReadHeaderTimeout: 10 * time.Second,
	}
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
	agentSrv := &http.Server{Handler: control.AgentHandler(s.state), ReadHeaderTimeout: 10 * time.Second}
	go agentSrv.Serve(ln)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		agentSrv.Close() // waiters hold long requests; do not wait for them
		srv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", listen, "vnc", vncAddr, "control", controlSocket)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) routes(static fs.FS, novnc string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", s.page(static))
	mux.HandleFunc("GET /login", s.login)
	mux.Handle("GET /ui/", s.auth(http.StripPrefix("/ui/", noListing(http.FileServerFS(static)))))
	// Only what the viewer loads from noVNC: not its own pages or its tests.
	novncFiles := s.auth(http.StripPrefix("/novnc/", noListing(http.FileServer(http.Dir(novnc)))))
	mux.Handle("GET /novnc/core/", novncFiles)
	mux.Handle("GET /novnc/vendor/", novncFiles)
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
// restart never hands the desktop to the agent behind the human's back. On the
// very first start the agent owns the desktop; if the saved state cannot be
// read, the agent is paused.
func loadControl(path string, log *slog.Logger) *control.State {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return control.New(control.Agent)
	}
	var saved control.Snapshot
	if err == nil {
		err = json.Unmarshal(b, &saved)
	}
	if err != nil {
		log.Error("read control state, pausing the agent", "err", err)
		return control.New(control.Paused)
	}
	return control.Restore(saved)
}

func saveControl(path string, snap control.Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

// login trades a sign-in code for a session, in a strict same-site cookie, and
// redirects to a clean URL.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if s.authorized(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.sessions.redeem(r.URL.Query().Get("code")) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "This sign-in link is invalid, expired or already used. Get a new one on the server with: kivora web-url.\n")
		return
	}
	session, err := s.sessions.issue(sessionsDir)
	if err != nil {
		s.log.Error("create a session", "err", err)
		http.Error(w, "could not create a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	s.log.Info("signed in", "remote", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
// filter.
func (s *server) websockify(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return // Accept already replied, including for cross-origin requests
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

	filter := rfb.NewFilter(vnc, func() bool { return s.state.Is(control.Human) })
	stop := s.state.OnChange(func(snap control.Snapshot) {
		if snap.Owner != control.Human {
			filter.Release()
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
		if err := filter.Run(client); err != nil {
			s.log.Warn("viewer stream", "err", err)
		}
		done <- struct{}{}
	}()
	<-done
	filter.Release()
	s.log.Info("viewer disconnected", "remote", r.RemoteAddr)
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
