// Package relay joins two connections.
package relay

import (
	"io"
	"net"
	"sync"
	"time"
)

// Join copies both ways until both sides have finished sending.
func Join(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		half(a, b)
	}()
	go func() {
		defer wg.Done()
		half(b, a)
	}()
	wg.Wait()
}

// JoinServer joins a client to its server. Once the server has finished, the
// client has grace to finish too, then both are closed: a client that keeps its
// side open must not hold the connection for ever.
func JoinServer(client, server net.Conn, grace time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)
	serverDone, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer wg.Done()
		half(client, server)
		close(serverDone)
	}()
	go func() {
		defer wg.Done()
		half(server, client)
	}()
	go func() {
		select {
		case <-finished:
			return
		case <-serverDone:
		}
		select {
		case <-finished:
		case <-time.After(grace):
			client.Close()
			server.Close()
		}
	}()
	wg.Wait()
	close(finished)
}

func half(dst, src net.Conn) {
	io.Copy(dst, src)
	if c, ok := dst.(interface{ CloseWrite() error }); ok {
		c.CloseWrite()
	} else {
		dst.Close()
	}
}
