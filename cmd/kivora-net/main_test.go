package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// failingListener runs out of descriptors a few times, then works.
type failingListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

func echoDial(context.Context, uint16) (net.Conn, error) {
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		io.Copy(b, b)
	}()
	return a, nil
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func echoes(t *testing.T, address string) {
	t.Helper()
	c, err := net.DialTimeout("tcp4", address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hi"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 2)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "hi" {
		t.Fatalf("read %q: %v", got, err)
	}
}

func TestPublishedPortSurvivesRunningOutOfDescriptors(t *testing.T) {
	l := &failingListener{Listener: listen(t)}
	l.failures.Store(3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go guestPort(ctx, discardLog(), echoDial, l, 22)
	echoes(t, l.Addr().String())
}

func TestPublishedPortStopsWithItsListener(t *testing.T) {
	l := listen(t)
	done := make(chan struct{})
	go func() {
		guestPort(context.Background(), discardLog(), echoDial, l, 22)
		close(done)
	}()
	l.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still accepting a closed listener")
	}
}

func TestClientSlotsLimitOneSourceAndAll(t *testing.T) {
	var s clientSlots
	one := netip.MustParseAddr("203.0.113.1")
	for range maxClientsPerSource {
		if !s.take(one) {
			t.Fatal("refused within the limit of a source")
		}
	}
	if s.take(one) {
		t.Error("one source holds more than its share")
	}
	s.release(one)
	if !s.take(one) {
		t.Error("a released slot does not come back")
	}
	for i := range maxClients - maxClientsPerSource {
		if !s.take(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})) {
			t.Fatal("refused within the total")
		}
	}
	if s.take(netip.MustParseAddr("203.0.113.2")) {
		t.Error("more clients than the total")
	}
	s.release(one)
	if !s.take(netip.MustParseAddr("203.0.113.2")) {
		t.Error("a new client is refused below the total")
	}
	if s.bySource[one] != maxClientsPerSource-1 {
		t.Errorf("source count %d", s.bySource[one])
	}
}

func TestLocalAdminCommandsDoNotCount(t *testing.T) {
	l := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A VM that never answers: every client stays connected.
	hold := make(chan struct{})
	defer close(hold)
	go guestPort(ctx, discardLog(), func(context.Context, uint16) (net.Conn, error) {
		<-hold
		return nil, errors.New("gone")
	}, l, 22)
	conns := make([]net.Conn, 0, maxClientsPerSource+2)
	for range maxClientsPerSource + 2 {
		c, err := net.DialTimeout("tcp4", l.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	// None is closed by the limit: loopback is this container.
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("a local client was refused: %v", err)
		}
	}
}

func TestPublishedPortLogsWhoConnects(t *testing.T) {
	l := listen(t)
	var logged lockedBuilder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go guestPort(ctx, log, echoDial, sourceListener{l, "203.0.113.7"}, 22)
	echoes(t, l.Addr().String())
	if got := logged.String(); !strings.Contains(got, "203.0.113.7") {
		t.Errorf("no address in the log: %q", got)
	}
}

// sourceListener makes connections look like they come from an address outside.
type sourceListener struct {
	net.Listener
	source string
}

func (l sourceListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return sourceConn{c, &net.TCPAddr{IP: net.ParseIP(l.source), Port: 40000}}, nil
}

type sourceConn struct {
	net.Conn
	remote net.Addr
}

func (c sourceConn) RemoteAddr() net.Addr { return c.remote }

func TestClientsOverTheLimitAreClosed(t *testing.T) {
	l := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold := make(chan struct{})
	defer close(hold)
	go guestPort(ctx, discardLog(), func(context.Context, uint16) (net.Conn, error) {
		<-hold
		return nil, errors.New("gone")
	}, sourceListener{l, "203.0.113.7"}, 22)
	var closed int
	for range maxClientsPerSource + 3 {
		c, err := net.DialTimeout("tcp4", l.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); errors.Is(err, io.EOF) {
			closed++
		}
	}
	if closed != 3 {
		t.Errorf("%d clients closed, want 3", closed)
	}
}

// The real process, with its flags: what it sets on itself concerns no test.
func TestNetworkProcessCannotBeLookedInto(t *testing.T) {
	if os.Getenv("KIVORA_NET_TEST_DUMPABLE") == "1" {
		// Re-executed below: the same call as run.
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		got, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		if err != nil || got != 0 {
			t.Fatalf("dumpable %d, %v", got, err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestNetworkProcessCannotBeLookedInto")
	cmd.Env = append(os.Environ(), "KIVORA_NET_TEST_DUMPABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
