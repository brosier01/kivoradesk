// Package usernet gives a VM its network without any privilege, on any system.
// The VM's frames end in a TCP/IP stack inside this process, which opens real
// connections on the VM's behalf, only to the Internet. Whatever the VM sends,
// it never touches the host's networks.
package usernet

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/brosier01/kivoradesk/internal/netguard"
	"github.com/brosier01/kivoradesk/internal/relay"
	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID = 1
	mtu   = 1500
	// A frame larger than this does not come from a VM with this MTU.
	maxFrame = 1 << 16
	// Each connection uses a file descriptor of the host.
	maxFlows    = 4096
	maxDNS      = 64
	dialTimeout = 20 * time.Second
	dnsTimeout  = 10 * time.Second
	// Enough for a name with many addresses, within a 512-byte answer.
	maxDNSAnswers = 16
)

var (
	// GatewayIP is the VM's default route and DNS server.
	GatewayIP = netip.MustParseAddr("10.86.0.1")
	// GuestIP is the fixed address of the VM.
	GuestIP = netip.MustParseAddr("10.86.0.2")
)

const gatewayMAC = tcpip.LinkAddress("\x52\x54\x00\x6b\x64\xfe")

// blocked lists what is not the Internet. The addresses of this computer are
// checked on top, since it may have a public one.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // private
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT, Tailscale
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local, cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),   // private
	netip.MustParsePrefix("192.0.0.0/24"),    // protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.168.0.0/16"),  // private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarks, some VPNs
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
}

// Allowed reports whether the VM may connect to ip: an IPv4 address on the
// Internet, which is not this computer.
func Allowed(ip netip.Addr) bool {
	if !ip.Is4() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return !netguard.IsLocal(net.IP(ip.AsSlice()))
}

// Network is the network of one VM.
type Network struct {
	stack *stack.Stack
	link  *channel.Endpoint
	log   *slog.Logger
	flows chan struct{}
	// When the warning about too many connections was last logged, in Unix
	// seconds. The VM decides how often it happens, and logs take disk space.
	flowsWarned atomic.Int64

	allow  func(netip.Addr, uint16) bool
	dial   func(ctx context.Context, address string) (net.Conn, error)
	lookup func(ctx context.Context, name string) ([]net.IP, error)
}

// New starts the network. Connect the VM to it with Serve.
func New(log *slog.Logger) (*Network, error) {
	s, link, err := newStack(GatewayIP, gatewayMAC)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: dialTimeout}
	n := &Network{
		stack: s,
		link:  link,
		log:   log,
		flows: make(chan struct{}, maxFlows),
		allow: func(ip netip.Addr, _ uint16) bool { return Allowed(ip) },
		dial: func(ctx context.Context, address string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp4", address)
		},
		lookup: func(ctx context.Context, name string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip4", name)
		},
	}
	// Without a handler or an ICMP stack, every other datagram, QUIC included,
	// is silently dropped, and browsers fall back on TCP.
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(s, 0, 1024, n.forward).HandlePacket)
	conn, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: nicID, Addr: addr(GatewayIP), Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("DNS: %w", err)
	}
	go n.serveDNS(conn)
	return n, nil
}

// newStack creates a TCP/IP stack with one Ethernet interface. It accepts every
// destination, and answers for it: the address the VM asked for is what the
// forwarder then connects to.
func newStack(ip netip.Addr, mac tcpip.LinkAddress) (*stack.Stack, *channel.Endpoint, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	sack := tcpip.TCPSACKEnabled(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		return nil, nil, fmt.Errorf("TCP options: %s", err)
	}
	link := channel.New(1024, mtu, mac)
	if err := s.CreateNIC(nicID, ethernet.New(link)); err != nil {
		return nil, nil, fmt.Errorf("network interface: %s", err)
	}
	address := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr(ip), PrefixLen: 24},
	}
	if err := s.AddProtocolAddress(nicID, address, stack.AddressProperties{}); err != nil {
		return nil, nil, fmt.Errorf("address: %s", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, nil, fmt.Errorf("promiscuous mode: %s", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, nil, fmt.Errorf("spoofing: %s", err)
	}
	return s, link, nil
}

func addr(ip netip.Addr) tcpip.Address { return tcpip.AddrFrom4(ip.As4()) }

// SetPolicy replaces what decides the VM's connections, Allowed by default. Call
// it before Serve.
func (n *Network) SetPolicy(allow func(ip netip.Addr, port uint16) bool) { n.allow = allow }

// Close stops the network and every connection of the VM.
func (n *Network) Close() {
	n.link.Close()
	n.stack.Close()
}

// DialGuest opens a connection from this process to a port of the VM. It needs
// no port on the host: nothing else on this computer can reach the VM this way.
func (n *Network) DialGuest(ctx context.Context, port uint16) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, n.stack, tcpip.FullAddress{NIC: nicID, Addr: addr(GuestIP), Port: port}, ipv4.ProtocolNumber)
}

// Serve exchanges Ethernet frames with the hypervisor over conn, until either
// side stops. Each frame has a 4-byte big-endian length before it, the format
// of QEMU's stream network backend.
func (n *Network) Serve(ctx context.Context, conn net.Conn) error {
	return pump(ctx, conn, n.link)
}

func pump(ctx context.Context, conn net.Conn, link *channel.Endpoint) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	errc := make(chan error, 2)
	go func() { errc <- readFrames(conn, link) }()
	go func() { errc <- writeFrames(ctx, conn, link) }()
	err := <-errc
	cancel()
	<-errc
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func readFrames(conn net.Conn, link *channel.Endpoint) error {
	r := bufio.NewReaderSize(conn, maxFrame)
	var size [4]byte
	for {
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return err
		}
		length := binary.BigEndian.Uint32(size[:])
		if length == 0 || length > maxFrame {
			return fmt.Errorf("invalid frame of %d bytes", length)
		}
		frame := make([]byte, length)
		if _, err := io.ReadFull(r, frame); err != nil {
			return err
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
		link.InjectInbound(0, pkt)
		pkt.DecRef()
	}
}

func writeFrames(ctx context.Context, conn net.Conn, link *channel.Endpoint) error {
	var out []byte
	for {
		pkt := link.ReadContext(ctx)
		if pkt == nil {
			return ctx.Err()
		}
		out = binary.BigEndian.AppendUint32(out[:0], uint32(pkt.Size()))
		for _, part := range pkt.AsSlices() {
			out = append(out, part...)
		}
		pkt.DecRef()
		if _, err := conn.Write(out); err != nil {
			return err
		}
	}
}

// forward carries one TCP connection of the VM to where it asked to go, if that
// is allowed. The connection is only accepted once the other end has, so the VM
// sees a refusal as a refusal.
func (n *Network) forward(r *tcp.ForwarderRequest) {
	id := r.ID()
	ip, ok := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	if !ok || !n.allow(ip, id.LocalPort) {
		n.log.Debug("connection blocked", "to", ip, "port", id.LocalPort)
		r.Complete(true)
		return
	}
	select {
	case n.flows <- struct{}{}:
		defer func() { <-n.flows }()
	default:
		now, last := time.Now().Unix(), n.flowsWarned.Load()
		if now-last >= 60 && n.flowsWarned.CompareAndSwap(last, now) {
			n.log.Warn("too many connections from the VM")
		}
		r.Complete(true)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	out, err := n.dial(ctx, netip.AddrPortFrom(ip, id.LocalPort).String())
	if err != nil {
		r.Complete(true)
		return
	}
	defer out.Close()

	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)
	in := gonet.NewTCPConn(&wq, ep)
	defer in.Close()
	relay.Join(in, out)
}

// serveDNS answers the VM's name lookups with the resolver of this computer.
func (n *Network) serveDNS(conn *gonet.UDPConn) {
	defer conn.Close()
	busy := make(chan struct{}, maxDNS)
	buf := make([]byte, 1500)
	for {
		size, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		select {
		case busy <- struct{}{}:
		default:
			continue
		}
		query := append([]byte(nil), buf[:size]...)
		go func() {
			defer func() { <-busy }()
			ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
			defer cancel()
			if answer := n.answer(ctx, query); answer != nil {
				conn.WriteTo(answer, from)
			}
		}()
	}
}

// answer builds the reply to a DNS query, or nil to ignore it. The VM only has
// IPv4 and only needs addresses: other types get an empty answer. A name may
// resolve to a private address: the connection is what gets refused.
func (n *Network) answer(ctx context.Context, query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	reply := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true},
		Questions: []dnsmessage.Question{q},
	}
	if q.Class == dnsmessage.ClassINET && q.Type == dnsmessage.TypeA {
		// The final dot keeps the host's search domains out of the lookup.
		ips, err := n.lookup(ctx, q.Name.String())
		var dnsErr *net.DNSError
		switch {
		case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
			reply.RCode = dnsmessage.RCodeNameError
		case err != nil:
			reply.RCode = dnsmessage.RCodeServerFailure
		}
		for _, ip := range ips {
			v4 := ip.To4()
			if v4 == nil || len(reply.Answers) == maxDNSAnswers {
				continue
			}
			reply.Answers = append(reply.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: [4]byte(v4)},
			})
		}
	}
	out, err := reply.Pack()
	if err != nil {
		return nil
	}
	return out
}
