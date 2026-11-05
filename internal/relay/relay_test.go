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
