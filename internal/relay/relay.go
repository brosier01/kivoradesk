// Package relay joins two connections.
package relay

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
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

// JoinQuiet joins a to b like Join, for a peer that may forget a connection
// without telling: both are closed once nothing has moved either way for idle,
// or once a has finished sending and b has then sent nothing for grace. Each
// byte from b gives it grace again, so a long answer after a's end goes on.
func JoinQuiet(a, b net.Conn, grace, idle time.Duration) {
	var moved, fromB atomic.Int64
	moved.Store(time.Now().UnixNano())
	aDone, finished := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		half(b, &noting{r: a, stamps: []*atomic.Int64{&moved}})
		// The grace starts when a has finished.
		fromB.Store(time.Now().UnixNano())
		close(aDone)
	}()
	go func() {
		defer wg.Done()
		half(a, &noting{r: b, stamps: []*atomic.Int64{&moved, &fromB}})
	}()
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		waiting := aDone
		for {
			select {
			case <-finished:
				return
			case <-waiting:
				waiting = nil
			case <-timer.C:
			}
			end := time.Unix(0, moved.Load()).Add(idle)
			if waiting == nil {
				if e := time.Unix(0, fromB.Load()).Add(grace); e.Before(end) {
					end = e
				}
			}
			left := time.Until(end)
			if left <= 0 {
				a.Close()
				b.Close()
				return
			}
			timer.Reset(left)
		}
	}()
	wg.Wait()
	close(finished)
}

// noting reads from r, and records in stamps when data last came.
type noting struct {
	r      io.Reader
	stamps []*atomic.Int64
}

func (n *noting) Read(p []byte) (int, error) {
	size, err := n.r.Read(p)
	if size > 0 {
		now := time.Now().UnixNano()
		for _, s := range n.stamps {
			s.Store(now)
		}
	}
	return size, err
}

func half(dst net.Conn, src io.Reader) {
	io.Copy(dst, src)
	if c, ok := dst.(interface{ CloseWrite() error }); ok {
		c.CloseWrite()
	} else {
		dst.Close()
	}
}
