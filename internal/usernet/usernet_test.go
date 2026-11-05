package usernet

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func TestAllowedKeepsTheVMOffPrivateNetworks(t *testing.T) {
	blockedAddrs := []string{
		"0.0.0.0", "10.0.0.1", "10.86.0.1", "100.100.100.100", "127.0.0.1", "169.254.169.254",
		"172.16.5.1", "172.31.255.255", "192.168.1.1", "198.18.0.1", "224.0.0.251",
		"240.0.0.1", "255.255.255.255", "::1", "fe80::1", "fd00::1", "2606:4700:4700::1111",
		"::ffff:192.168.1.1", "::ffff:1.1.1.1",
	}
	for _, s := range blockedAddrs {
		if Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s is allowed", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "172.32.0.1", "11.0.0.1"} {
		if !Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s is blocked", s)
		}
	}
}

func TestAllowedRefusesWhatCloudsKeepOnPublicAddresses(t *testing.T) {
	for _, s := range []string{
		"168.63.129.16", // Azure platform services
		"192.88.99.1",   // 6to4 relays
		"161.26.0.1",    // IBM Cloud service network
		"169.254.169.254", "100.100.100.200", "192.0.0.192", "169.254.0.23",
	} {
		if Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s is allowed", s)
		}
	}
	for _, s := range []string{"168.63.129.15", "168.63.129.17", "161.27.0.1", "192.88.98.1"} {
		if !Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s is blocked", s)
		}
	}
}

func TestAllowedRefusesThisComputer(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(ipnet.IP.To4()); ok && Allowed(ip) {
			t.Errorf("address of this computer %s is allowed", ip)
		}
	}
}

// guest is a second TCP/IP stack standing for the VM.
type guest struct {
	s *stack.Stack
}

func newGuest(t *testing.T) (*Network, *guest) {
	t.Helper()
	n, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s, link, err := newStack(GuestIP, tcpip.LinkAddress("\x52\x54\x00\x6b\x64\x77"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go n.Serve(ctx, a)
	go pump(ctx, b, link)
	t.Cleanup(func() {
		cancel()
		n.Close()
		link.Close()
		s.Close()
	})
	return n, &guest{s}
}

func (g *guest) dial(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, g.s, tcpip.FullAddress{NIC: nicID, Addr: addr(ip), Port: port}, ipv4.ProtocolNumber)
}

func echoServer(t *testing.T) *net.TCPAddr {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr)
}

func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func roundTrip(t *testing.T, c net.Conn, size int) {
	t.Helper()
	sent := bytes.Repeat([]byte("kivora"), size/6+1)[:size]
	go c.Write(sent)
	got := make([]byte, size)
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("after sending %d bytes: %v", size, err)
	}
	if !bytes.Equal(got, sent) {
		t.Fatal("the data came back different")
	}
}

func TestVMCannotReachThisComputer(t *testing.T) {
	_, g := newGuest(t)
	srv := echoServer(t)
	// A refusal, not a timeout: the VM should not wait for a blocked address.
	for _, ip := range []netip.Addr{GatewayIP, netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("10.0.0.1")} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := g.dial(ctx, ip, uint16(srv.Port))
		if err == nil {
			c.Close()
			t.Errorf("the VM connected to %s", ip)
		} else if ctx.Err() != nil {
			t.Errorf("%s: no refusal: %v", ip, err)
		}
		cancel()
	}
	// The stack drops loopback addresses on arrival: this one only times out.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if c, err := g.dial(ctx, netip.MustParseAddr("127.0.0.1"), uint16(srv.Port)); err == nil {
		c.Close()
		t.Error("the VM connected to this computer's loopback")
	}
}

func TestVMReachesWhatIsAllowed(t *testing.T) {
	n, g := newGuest(t)
	srv := echoServer(t)
	allowed := netip.MustParseAddr("93.184.216.34")
	n.SetPolicy(func(ip netip.Addr, port uint16) bool { return ip == allowed && port == 443 })
	n.dial = func(ctx context.Context, address string) (net.Conn, error) {
		if address != netip.AddrPortFrom(allowed, 443).String() {
			t.Errorf("connected to %s", address)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", srv.String())
	}
	c, err := g.dial(timeout(t), allowed, 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, 2<<20)
}

func TestFailedConnectionIsARefusal(t *testing.T) {
	n, g := newGuest(t)
	n.SetPolicy(func(netip.Addr, uint16) bool { return true })
	n.dial = func(context.Context, string) (net.Conn, error) { return nil, net.ErrClosed }
	if c, err := g.dial(timeout(t), netip.MustParseAddr("93.184.216.34"), 443); err == nil {
		c.Close()
		t.Fatal("the VM connected to a server that refused")
	}
}

func TestDialGuest(t *testing.T) {
	n, g := newGuest(t)
	l, err := gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: nicID, Addr: addr(GuestIP), Port: 22}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	c, err := n.DialGuest(timeout(t), 22)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, 2<<20)
}

func (g *guest) udp(t *testing.T, ip netip.Addr, port uint16) *gonet.UDPConn {
	t.Helper()
	c, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: nicID, Addr: addr(ip), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func query(t *testing.T, g *guest, name string, typ dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	c := g.udp(t, GatewayIP, 53)
	q := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 77, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}},
	}
	out, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(out); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 1500)
	size, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:size]); err != nil {
		t.Fatal(err)
	}
	if m.ID != 77 || !m.Response {
		t.Fatalf("not the answer to this query: %+v", m.Header)
	}
	return m
}

func TestDNS(t *testing.T) {
	n, g := newGuest(t)
	n.lookup = func(_ context.Context, name string) ([]net.IP, error) {
		if name != "example.com." {
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}
		return []net.IP{net.IPv4(93, 184, 216, 34), net.ParseIP("2606:2800::1")}, nil
	}

	m := query(t, g, "example.com.", dnsmessage.TypeA)
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 {
		t.Fatalf("A: %+v", m)
	}
	if a := m.Answers[0].Body.(*dnsmessage.AResource).A; a != [4]byte{93, 184, 216, 34} {
		t.Fatalf("A: %v", a)
	}
	if m := query(t, g, "absent.example.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("absent name: %v", m.RCode)
	}
	if m := query(t, g, "example.com.", dnsmessage.TypeAAAA); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("AAAA: %+v", m)
	}
}

func TestDNSKeepsTheNetworksAroundTheVMOutOfItsAnswers(t *testing.T) {
	n, g := newGuest(t)
	looked := 0
	n.lookup = func(_ context.Context, name string) ([]net.IP, error) {
		looked++
		switch name {
		case "kivoradesk.", "nas.lan.example.":
			return []net.IP{net.IPv4(172, 21, 0, 2)}, nil
		case "mixed.example.":
			return []net.IP{net.IPv4(192, 168, 1, 7), net.IPv4(93, 184, 216, 34)}, nil
		case "staging.example.":
			return []net.IP{net.IPv4(192, 168, 1, 20)}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	n.SetResolvable(func(ip netip.Addr) bool {
		return Allowed(ip) || ip == netip.MustParseAddr("192.168.1.20")
	})

	if m := query(t, g, "kivoradesk.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError || len(m.Answers) != 0 {
		t.Errorf("a name of one label: %+v", m)
	}
	if looked != 0 {
		t.Error("a name of one label reached the resolver of this computer")
	}
	if m := query(t, g, "nas.lan.example.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError || len(m.Answers) != 0 {
		t.Errorf("a name for a private address: %+v", m)
	}
	m := query(t, g, "mixed.example.", dnsmessage.TypeA)
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{93, 184, 216, 34} {
		t.Errorf("a name with both kinds of addresses: %+v", m)
	}
	m = query(t, g, "staging.example.", dnsmessage.TypeA)
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{192, 168, 1, 20} {
		t.Errorf("a name for an allowed private service: %+v", m)
	}
}

func TestDNSManyAddressesFitInOneAnswer(t *testing.T) {
	n, _ := newGuest(t)
	n.lookup = func(context.Context, string) ([]net.IP, error) {
		ips := make([]net.IP, 200)
		for i := range ips {
			ips[i] = net.IPv4(93, 184, 216, byte(i))
		}
		return ips, nil
	}
	q := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	out, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	answer := n.answer(context.Background(), out)
	if len(answer) == 0 || len(answer) > 512 {
		t.Fatalf("answer of %d bytes", len(answer))
	}
}

func TestDNSIgnoresGarbage(t *testing.T) {
	n, _ := newGuest(t)
	for _, q := range [][]byte{nil, {1}, bytes.Repeat([]byte{0xff}, 100)} {
		if a := n.answer(context.Background(), q); a != nil {
			t.Errorf("answer to %x: %x", q, a)
		}
	}
}

func TestOtherDatagramsAreDropped(t *testing.T) {
	_, g := newGuest(t)
	c := g.udp(t, netip.MustParseAddr("1.1.1.1"), 53)
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if size, err := c.Read(make([]byte, 100)); err == nil {
		t.Fatalf("got %d bytes back", size)
	}
}

// arpRequest asks who has the gateway's address: the stack answers it.
func arpRequest() []byte {
	mac := []byte{0x52, 0x54, 0x00, 0x6b, 0x64, 0x77}
	frame := append(bytes.Repeat([]byte{0xff}, 6), mac...)
	frame = append(frame, 0x08, 0x06, 0, 1, 0x08, 0, 6, 4, 0, 1)
	frame = append(frame, mac...)
	guest, gateway := GuestIP.As4(), GatewayIP.As4()
	frame = append(frame, guest[:]...)
	frame = append(frame, make([]byte, 6)...)
	return append(frame, gateway[:]...)
}

func TestServeSkipsFramesThatCannotBeReal(t *testing.T) {
	n, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Serve(ctx, a) }()

	too := maxFrame + 1
	var out []byte
	out = binary.BigEndian.AppendUint32(out, 0)
	out = binary.BigEndian.AppendUint32(out, uint32(too))
	out = append(out, make([]byte, too)...)
	request := arpRequest()
	out = binary.BigEndian.AppendUint32(out, uint32(len(request)))
	out = append(out, request...)
	go b.Write(out)

	// The link is still up, and the stack answers the next real frame.
	b.SetReadDeadline(time.Now().Add(10 * time.Second))
	var size [4]byte
	if _, err := io.ReadFull(b, size[:]); err != nil {
		t.Fatalf("no answer after the invalid frames: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("stopped serving: %v", err)
	default:
	}
}

// A far end that never closes, and a VM that goes away.
func TestConnectionsEndWithTheVM(t *testing.T) {
	n, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.SetPolicy(func(netip.Addr, uint16) bool { return true })
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	far := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			far <- c
		}
	}()
	n.dial = func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", l.Addr().String())
	}

	s, link, err := newStack(GuestIP, tcpip.LinkAddress("\x52\x54\x00\x6b\x64\x77"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer link.Close()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serving := make(chan struct{})
	go func() { n.Serve(ctx, a); close(serving) }()
	go pump(ctx, b, link)

	c, err := (&guest{s}).dial(timeout(t), netip.MustParseAddr("93.184.216.34"), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	remote := <-far
	defer remote.Close()
	// Data from the far end reaches the VM once the connection is set up.
	remote.Write([]byte("x"))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if len(n.flows) != 1 {
		t.Fatalf("%d slots in use", len(n.flows))
	}

	b.Close() // QEMU is gone
	<-serving
	remote.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Error("the far end got data from a VM that is gone")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("the connection outlives the VM")
	}
	if n.currentGeneration() == 0 {
		t.Error("connections opening now would not know the VM has gone")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(n.flows) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(n.flows) != 0 {
		t.Errorf("%d slots still in use", len(n.flows))
	}
}

func TestBuffersAreBounded(t *testing.T) {
	s, link, err := newStack(GuestIP, tcpip.LinkAddress("\x52\x54\x00\x6b\x64\x77"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer link.Close()
	var receive tcpip.TCPReceiveBufferSizeRangeOption
	if err := s.TransportProtocolOption(tcp.ProtocolNumber, &receive); err != nil || receive.Max != maxTCPBuffer {
		t.Errorf("receive buffer: %+v, %v", receive, err)
	}
	var send tcpip.TCPSendBufferSizeRangeOption
	if err := s.TransportProtocolOption(tcp.ProtocolNumber, &send); err != nil || send.Max != maxTCPBuffer {
		t.Errorf("send buffer: %+v, %v", send, err)
	}
	if maxFlows*2*(maxTCPBuffer+32<<10) > 2200<<20 {
		t.Errorf("the VM can make this process hold %d MiB", maxFlows*2*(maxTCPBuffer+32<<10)>>20)
	}
}

func TestTooManyConnectionsDoNotFillTheLogs(t *testing.T) {
	n, g := newGuest(t)
	var logged bytes.Buffer
	n.log = slog.New(slog.NewTextHandler(&lockedWriter{w: &logged}, nil))
	n.SetPolicy(func(netip.Addr, uint16) bool { return true })
	for range cap(n.flows) {
		n.flows <- struct{}{}
	}
	ip := netip.MustParseAddr("93.184.216.34")
	for i := range 5 {
		if c, err := g.dial(timeout(t), ip, uint16(1000+i)); err == nil {
			c.Close()
			t.Fatal("connected beyond the limit")
		}
	}
	if got := strings.Count(logged.String(), "too many connections"); got != 1 {
		t.Errorf("logged %d times", got)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// farEnd makes the VM's connections reach a server of the test, and returns
// each connection it accepts.
func farEnd(t *testing.T, n *Network) <-chan net.Conn {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	far := make(chan net.Conn, 1)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
			far <- c
		}
	}()
	n.SetPolicy(func(netip.Addr, uint16) bool { return true })
	n.dial = func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", l.Addr().String())
	}
	return far
}

func waitFlows(t *testing.T, n *Network, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for len(n.flows) != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(n.flows); got != want {
		t.Fatalf("%d slots in use, want %d", got, want)
	}
}

// A far end that never closes, after the VM has closed its connection.
func TestSilentFarEndLosesTheConnection(t *testing.T) {
	n, g := newGuest(t)
	n.grace = 200 * time.Millisecond
	far := farEnd(t, n)
	c, err := g.dial(timeout(t), netip.MustParseAddr("93.184.216.34"), 443)
	if err != nil {
		t.Fatal(err)
	}
	remote := <-far
	waitFlows(t, n, 1, 5*time.Second)
	c.Close()
	remote.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(remote); err != nil {
		t.Fatal(err)
	}
	waitFlows(t, n, 0, 5*time.Second)
	// The far end's side is closed too, not only the VM's.
	remote.SetWriteDeadline(time.Now().Add(5 * time.Second))
	var werr error
	for range 50 {
		if _, werr = remote.Write([]byte("x")); werr != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if werr == nil {
		t.Error("the far end can still write")
	}
}

// A long answer, after the VM has finished sending its request.
func TestFarEndCanAnswerAfterTheVMFinished(t *testing.T) {
	n, g := newGuest(t)
	n.grace = 200 * time.Millisecond
	far := farEnd(t, n)
	c, err := g.dial(timeout(t), netip.MustParseAddr("93.184.216.34"), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	remote := <-far
	c.(*gonet.TCPConn).CloseWrite()
	remote.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(remote); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 1)
	for range 20 {
		time.Sleep(50 * time.Millisecond)
		if _, err := remote.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("cut while the far end was answering: %v", err)
		}
	}
	if len(n.flows) != 1 {
		t.Fatalf("%d slots in use", len(n.flows))
	}
	remote.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	waitFlows(t, n, 0, 5*time.Second)
}

func TestIdleConnectionsEnd(t *testing.T) {
	n, g := newGuest(t)
	n.idle = 300 * time.Millisecond
	far := farEnd(t, n)
	c, err := g.dial(timeout(t), netip.MustParseAddr("93.184.216.34"), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-far
	waitFlows(t, n, 1, 5*time.Second)
	waitFlows(t, n, 0, 5*time.Second)
}
