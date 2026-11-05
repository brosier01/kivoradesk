// Command kivora-proxy is the only way out of the VM for the worker, which runs
// the agent's shell commands: the local firewall rejects everything else, DNS
// included. It opens HTTPS tunnels to the sites the human allowed and to nothing
// else, so that a command misled by a web page cannot send the workspace anywhere.
// The list is read at each request: "kivora shell-allow" changes it at once.
//
// A list entry is a host name, or *.example.com for its subdomains, with an
// optional port (443 by default). Tunnels are opaque: this checks where they go,
// not what goes through them.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brosier01/kivoradesk/internal/netguard"
)

const (
	maxTunnels    = 32
	dialTimeout   = 15 * time.Second
	tunnelTimeout = 15 * time.Minute // above the longest shell command
	// A destination fits in this, and the worker decides what comes in.
	maxHeaderBytes = 8 << 10
	maxLoggedName  = 255
)

func main() {
	listen := flag.String("listen", "127.0.0.1:3128", "address to listen on")
	list := flag.String("allowlist", "/var/lib/kivora-proxy/allowlist", "file listing the allowed sites")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := newServer(*listen, newProxy(*list, log))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info("listening", "addr", *listen, "allowlist", *list)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newServer(listen string, h http.Handler) *http.Server {
	return &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: maxHeaderBytes}
}

type proxy struct {
	list    string
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
	log     *slog.Logger
	tunnels chan struct{}
	quiet   logLimit
}

// logLimit keeps the log of what the worker asks small, since it asks what it
// wants as often as it wants: at most one line per second, the others counted.
type logLimit struct {
	mu      sync.Mutex
	next    time.Time
	dropped int
}

// allow reports whether a line may be written, and how many were not since the
// last one.
func (l *logLimit) allow(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.next) {
		l.dropped++
		return false, 0
	}
	l.next = now.Add(time.Second)
	dropped := l.dropped
	l.dropped = 0
	return true, dropped
}

// note logs what the worker caused, with its destination cut to a sane length.
func (p *proxy) note(level slog.Level, msg, host, port string, attrs ...any) {
	ok, dropped := p.quiet.allow(time.Now())
	if !ok {
		return
	}
	attrs = append([]any{"host", cutName(host), "port", cutName(port)}, attrs...)
	if dropped > 0 {
		attrs = append(attrs, "not_logged", dropped)
	}
	p.log.Log(context.Background(), level, msg, attrs...)
}

func cutName(name string) string {
	if len(name) > maxLoggedName {
		return name[:maxLoggedName] + "..."
	}
	return name
}

func newProxy(list string, log *slog.Logger) *proxy {
	dialer := &net.Dialer{Timeout: dialTimeout, Control: netguard.RefuseLocal}
	return &proxy{list: list, dial: dialer.DialContext, log: log, tunnels: make(chan struct{}, maxTunnels)}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "kivora-proxy: only HTTPS is allowed", http.StatusForbidden)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "kivora-proxy: invalid destination", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(p.list)
	if err != nil && !os.IsNotExist(err) {
		p.log.Error("read the allowlist", "err", err)
		http.Error(w, "kivora-proxy: cannot read the allowlist", http.StatusInternalServerError)
		return
	}
	if !allowed(string(data), host, port) {
		p.note(slog.LevelWarn, "refused", host, port)
		http.Error(w, fmt.Sprintf("kivora-proxy: %s is not allowed for shell commands. Use browser_download, or ask the human to allow it.", r.Host), http.StatusForbidden)
		return
	}
	select {
	case p.tunnels <- struct{}{}:
		defer func() { <-p.tunnels }()
	default:
		http.Error(w, "kivora-proxy: too many open tunnels", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	remote, err := p.dial(ctx, "tcp", r.Host)
	if err != nil {
		p.note(slog.LevelWarn, "connect", host, port, "err", err)
		http.Error(w, "kivora-proxy: cannot reach "+r.Host, http.StatusBadGateway)
		return
	}
	defer remote.Close()
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "kivora-proxy: cannot open the tunnel", http.StatusInternalServerError)
		return
	}
	defer client.Close()
	deadline := time.Now().Add(tunnelTimeout)
	client.SetDeadline(deadline)
	remote.SetDeadline(deadline)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	p.note(slog.LevelInfo, "tunnel", host, port)
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(remote, buffered)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(client, remote)
		done <- struct{}{}
	}()
	<-done
}

// allowed reports whether a list allows a destination.
func allowed(list, host, port string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, line := range strings.Split(list, "\n") {
		entry := strings.ToLower(strings.TrimSpace(line))
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		name, entryPort := entry, "443"
		if h, p, err := net.SplitHostPort(entry); err == nil {
			name, entryPort = h, p
		}
		if entryPort != port {
			continue
		}
		if suffix, ok := strings.CutPrefix(name, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == name {
			return true
		}
	}
	return false
}
