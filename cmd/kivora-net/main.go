// kivora-net runs the network of the VM in the container: the VM's frames end in
// this process (internal/usernet), which needs no privilege and only opens
// connections to the Internet.
//
//	kivora-net -hypervisor unix:/path/net.sock -forward 0.0.0.0:2222=22 \
//	  -allowlist /path/allowlist -host-addresses /path/addresses \
//	  -console /path/console.sock=/path/console.log
//	kivora-net -write-host-addresses /path/addresses
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brosier01/kivoradesk/internal/relay"
	"github.com/brosier01/kivoradesk/internal/usernet"
	"golang.org/x/sys/unix"
)

// version is set when the image is built.
var version = "dev"

const (
	// Connections to a published port, in all and from one address. Each costs a
	// descriptor and a connection of the VM's SSH server, whoever opened it.
	maxClients          = 128
	maxClientsPerSource = 16
	guestDialTimeout    = 20 * time.Second
	// How long a client may keep its side open once the VM has finished.
	clientGrace = 5 * time.Second
	maxBackoff  = time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "kivora-net:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	hypervisor := flag.String("hypervisor", "", "where the hypervisor connects: tcp:ADDRESS or unix:PATH")
	forward := flag.String("forward", "", "LISTEN_ADDRESS=GUEST_PORT[,...]: ports of the VM to expose")
	allowFile := flag.String("allowlist", "", "file of the private services the VM may reach")
	hostFile := flag.String("host-addresses", "", "file of this server's addresses, which the VM may not reach")
	writeHost := flag.String("write-host-addresses", "", "write the addresses of this machine to this file, and exit")
	console := flag.String("console", "", "SOCKET=FILE: keep the VM's serial console, sent by QEMU to this socket")
	debug := flag.Bool("debug", false, "log blocked connections")
	showVersion := flag.Bool("version", false, "print the version, and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *writeHost != "" {
		return writeHostAddresses(*writeHost, os.Getenv("KIVORA_PUBLIC_ADDRESSES"))
	}

	// This process holds the VM's disks through its view of the volume, in
	// /proc/PID/root: no other process under this UID may look there.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("PR_SET_DUMPABLE: %w", err)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Before the hypervisor's socket: QEMU connects to both when it sees that one.
	if *console != "" {
		socket, file, ok := strings.Cut(*console, "=")
		if !ok {
			return fmt.Errorf("-console must be SOCKET=FILE")
		}
		l, err := net.Listen("unix", socket)
		if err != nil {
			return err
		}
		defer l.Close()
		go serveConsole(l, file)
	}

	network, address, ok := strings.Cut(*hypervisor, ":")
	if !ok {
		return fmt.Errorf("-hypervisor must be tcp:ADDRESS or unix:PATH")
	}
	l, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	defer l.Close()

	n, err := usernet.New(log)
	if err != nil {
		return err
	}
	defer n.Close()
	p := newPolicy(log, *allowFile, *hostFile)
	n.SetPolicy(p.allow)
	n.SetResolvable(p.resolvable)

	if *forward != "" {
		for _, spec := range strings.Split(*forward, ",") {
			listen, portText, ok := strings.Cut(spec, "=")
			port, err := strconv.ParseUint(portText, 10, 16)
			if !ok || err != nil {
				return fmt.Errorf("-forward %q: want LISTEN_ADDRESS=GUEST_PORT", spec)
			}
			f, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			defer f.Close()
			go guestPort(ctx, log, n.DialGuest, f, uint16(port))
		}
	}

	log.Info("waiting for the hypervisor", "address", *hypervisor)
	context.AfterFunc(ctx, func() { l.Close() })
	for {
		conn, err := l.Accept()
		if err != nil {
			return ctx.Err()
		}
		log.Info("hypervisor connected")
		err = n.Serve(ctx, conn)
		log.Info("hypervisor disconnected", "err", err)
		conn.Close()
	}
}

// clientSlots limits the connections to a published port. Those of this container,
// the admin commands, do not count: nothing outside can keep them out.
type clientSlots struct {
	mu       sync.Mutex
	total    int
	bySource map[netip.Addr]int
}

func (s *clientSlots) take(source netip.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= maxClients || s.bySource[source] >= maxClientsPerSource {
		return false
	}
	if s.bySource == nil {
		s.bySource = map[netip.Addr]int{}
	}
	s.total++
	s.bySource[source]++
	return true
}

func (s *clientSlots) release(source netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total--
	if s.bySource[source]--; s.bySource[source] == 0 {
		delete(s.bySource, source)
	}
}

func sourceOf(conn net.Conn) netip.Addr {
	if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		if ap := a.AddrPort(); ap.IsValid() {
			return ap.Addr().Unmap()
		}
	}
	return netip.Addr{}
}

// guestPort forwards the connections of l to a port of the VM. Its SSH server sees
// them all coming from one address, the gateway's: this is the only place that
// knows who connects, and what keeps one client from holding the port.
func guestPort(ctx context.Context, log *slog.Logger, dial func(context.Context, uint16) (net.Conn, error), l net.Listener, port uint16) {
	var slots clientSlots
	var refused time.Time
	backoff := 10 * time.Millisecond
	for {
		conn, err := l.Accept()
		if err != nil {
			// Out of descriptors, for one: nothing else would ever accept again.
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			log.Warn("published port: accept failed", "port", port, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, maxBackoff)
			continue
		}
		backoff = 10 * time.Millisecond

		source := sourceOf(conn)
		local := source.IsLoopback()
		if !local && !slots.take(source) {
			if now := time.Now(); now.Sub(refused) >= time.Minute {
				refused = now
				log.Warn("published port: too many connections", "port", port, "from", source)
			}
			conn.Close()
			continue
		}
		if !local {
			log.Info("published port: connection", "port", port, "from", conn.RemoteAddr())
		}
		go func() {
			if !local {
				defer slots.release(source)
			}
			defer conn.Close()
			dialCtx, cancel := context.WithTimeout(ctx, guestDialTimeout)
			guest, err := dial(dialCtx, port)
			cancel()
			if err != nil {
				log.Debug("VM unreachable", "port", port, "err", err)
				return
			}
			defer guest.Close()
			relay.JoinServer(conn, guest, clientGrace)
		}()
	}
}
