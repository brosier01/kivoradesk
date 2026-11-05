package relay

import (
	"io"
	"net"
	"testing"
	"time"
)

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a.(*net.TCPConn), b.(*net.TCPConn)
}

func waitDone(t *testing.T, done <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatal(what)
	}
}

func TestJoinCarriesBothWaysUntilBothFinish(t *testing.T) {
	client, relayClient := tcpPair(t)
	relayServer, server := tcpPair(t)
	done := make(chan struct{})
	go func() { Join(relayClient, relayServer); close(done) }()

	client.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(server, got); err != nil || string(got) != "ping" {
		t.Fatalf("server read %q, %v", got, err)
	}
	server.Write([]byte("pong"))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "pong" {
		t.Fatalf("client read %q, %v", got, err)
	}
	// One side finished: the other can still send.
	client.CloseWrite()
	if _, err := io.ReadAll(server); err != nil {
		t.Fatal(err)
	}
	server.Write([]byte("last"))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "last" {
		t.Fatalf("client read %q, %v", got, err)
	}
	select {
	case <-done:
		t.Fatal("ended while the server was still sending")
	case <-time.After(50 * time.Millisecond):
	}
	server.CloseWrite()
	waitDone(t, done, 5*time.Second, "both sides finished, and the relay goes on")
}

func TestJoinServerClosesAClientThatKeepsItsSideOpen(t *testing.T) {
	_, relayClient := tcpPair(t)
	relayServer, server := tcpPair(t)
	done := make(chan struct{})
	go func() { JoinServer(relayClient, relayServer, 50*time.Millisecond); close(done) }()
	server.Close()
	waitDone(t, done, 5*time.Second, "the client holds the connection after the server finished")
}

func TestJoinServerLetsTheClientFinishItsOutput(t *testing.T) {
	client, relayClient := tcpPair(t)
	relayServer, server := tcpPair(t)
	done := make(chan struct{})
	go func() { JoinServer(relayClient, relayServer, time.Minute); close(done) }()
	server.Write([]byte("bye"))
	server.CloseWrite()
	got := make([]byte, 3)
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "bye" {
		t.Fatalf("client read %q, %v", got, err)
	}
	client.Close()
	waitDone(t, done, 5*time.Second, "both sides finished, and the relay goes on")
}

func TestJoinServerWaitsForTheServer(t *testing.T) {
	client, relayClient := tcpPair(t)
	relayServer, server := tcpPair(t)
	done := make(chan struct{})
	go func() { JoinServer(relayClient, relayServer, time.Millisecond); close(done) }()
	// The client finishing alone does not end it, whatever the grace.
	client.CloseWrite()
	if _, err := io.ReadAll(server); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("ended while the server was still sending")
	case <-time.After(50 * time.Millisecond):
	}
	server.Close()
	waitDone(t, done, 5*time.Second, "both sides finished, and the relay goes on")
}

func TestJoinQuietClosesAServerSilentAfterTheClientFinished(t *testing.T) {
	client, relayClient := tcpPair(t)
	relayServer, server := tcpPair(t)
	done := make(chan struct{})
	go func() { JoinQuiet(relayClient, relayServer, 100*time.Millisecond, time.Minute); close(done) }()
	// Quiet before the client finishes: the grace does not apply yet.
	select {
	case <-done:
		t.Fatal("ended while the client was still sending")
	case <-time.After(300 * time.Millisecond):
	}
	client.CloseWrite()
	if _, err := io.ReadAll(server); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done, 5*time.Second, "a silent server holds the connection after the client finished")
}

func TestJoinQuietLetsTheServerGoOnSending(t *testing.T) {
	client, relayClient := tcpPair(t)
	relayServer, server := tcpPair(t)
	done := make(chan struct{})
	go func() { JoinQuiet(relayClient, relayServer, 100*time.Millisecond, time.Minute); close(done) }()
	client.CloseWrite()
	if _, err := io.ReadAll(server); err != nil {
		t.Fatal(err)
	}
	// Far longer than the grace in all, never longer than it between two writes.
	got := make([]byte, 1)
	for range 20 {
		time.Sleep(30 * time.Millisecond)
		if _, err := server.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		client.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(client, got); err != nil {
			t.Fatalf("cut while the server was sending: %v", err)
		}
	}
	select {
	case <-done:
		t.Fatal("ended while the server was still sending")
	default:
	}
	server.CloseWrite()
	waitDone(t, done, 5*time.Second, "both sides finished, and the relay goes on")
}

func TestJoinQuietClosesAnIdleConnection(t *testing.T) {
	client, relayClient := tcpPair(t)
	relayServer, server := tcpPair(t)
	done := make(chan struct{})
	go func() { JoinQuiet(relayClient, relayServer, time.Minute, 300*time.Millisecond); close(done) }()
	// Data either way keeps it open.
	got := make([]byte, 1)
	for range 5 {
		time.Sleep(100 * time.Millisecond)
		client.Write([]byte("x"))
		if _, err := io.ReadFull(server, got); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
		t.Fatal("ended while data was moving")
	default:
	}
	waitDone(t, done, 5*time.Second, "an idle connection stays open")
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(got); err == nil {
		t.Error("the client got data")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("the client's side is still open")
	}
}
