// Command kivora-web serves the live view of the KivoraDesk desktop and owns
// the input control state (agent, paused or human).
//
// It listens on localhost inside the VM; the human reaches it through an SSH
// tunnel. Every request must carry the session token, set as a cookie from
// the one-time URL printed by scripts/vm/web-url.sh. Viewer input is relayed
// to the VNC server only while the human owns the desktop.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
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
	"syscall"
	"time"

	"github.com/brosier01/kivoradesk/internal/control"
	"github.com/brosier01/kivoradesk/internal/rfb"
	"github.com/coder/websocket"
)

//go:embed web
var webFS embed.FS

const cookieName = "kivora_session"

type server struct {
	token   string
	vncAddr string
	state   *control.State
	log     *slog.Logger
}

func main() {
	listen := flag.String("listen", "127.0.0.1:6080", "address to listen on")
	vncAddr := flag.String("vnc", "127.0.0.1:5900", "VNC server address")
	novnc := flag.String("novnc", "/usr/share/novnc", "directory holding the noVNC client")
	stateDir := flag.String("state-dir", "/var/lib/kivora-web", "directory for the session token")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*listen, *vncAddr, *novnc, *stateDir, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(listen, vncAddr, novnc, stateDir string, log *slog.Logger) error {
	token, err := loadToken(filepath.Join(stateDir, "token"))
	if err != nil {
		return err
	}
	s := &server{token: token, vncAddr: vncAddr, state: control.New(control.Agent), log: log}

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", s.page(static))
	mux.Handle("GET /ui/", s.auth(http.StripPrefix("/ui/", http.FileServerFS(static))))
	mux.Handle("GET /novnc/", s.auth(http.StripPrefix("/novnc/", http.FileServer(http.Dir(novnc)))))
	mux.Handle("GET /api/control", s.auth(http.HandlerFunc(s.getControl)))
	mux.Handle("POST /api/control", s.auth(http.HandlerFunc(s.setControl)))
	mux.Handle("GET /websockify", s.auth(http.HandlerFunc(s.websockify)))

	srv := &http.Server{
		Addr:              listen,
		Handler:           s.checkHost(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", listen, "vnc", vncAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadToken reads the session token, creating it on first start. The file is
// only readable by the gateway user, so desktop processes cannot take control.
func loadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return token, nil
}

// checkHost rejects requests whose Host is not a loopback name, which defeats
// DNS rebinding through the SSH tunnel.
func (s *server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "[::1]" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) authorized(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1
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

// page serves the viewer. Opening /?token=... once stores the token in a
// strict same-site cookie and redirects to a clean URL.
func (s *server) page(static fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("token"); t != "" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) != 1 {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: cookieName, Value: s.token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
				MaxAge: int((30 * 24 * time.Hour).Seconds()),
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !s.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "Open the URL printed by scripts/vm/web-url.sh to sign in.\n")
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; connect-src 'self'")
		w.Header().Set("X-Frame-Options", "DENY")
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
