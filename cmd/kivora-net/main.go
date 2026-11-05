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
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/brosier01/kivoradesk/internal/relay"
	"github.com/brosier01/kivoradesk/internal/usernet"
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
	flag.Parse()

	if *writeHost != "" {
		return writeHostAddresses(*writeHost)
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
	n.SetPolicy(newPolicy(log, *allowFile, *hostFile).allow)

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
			go guestPort(ctx, log, n, f, uint16(port))
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

// guestPort forwards the connections of l to a port of the VM.
func guestPort(ctx context.Context, log *slog.Logger, n *usernet.Network, l net.Listener, port uint16) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			guest, err := n.DialGuest(ctx, port)
			if err != nil {
				log.Debug("VM unreachable", "port", port, "err", err)
				return
			}
			defer guest.Close()
			relay.Join(conn, guest)
		}()
	}
}
