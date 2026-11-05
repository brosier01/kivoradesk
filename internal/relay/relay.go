// Package relay joins two connections.
package relay

import (
	"io"
	"net"
	"sync"
)

// Join copies both ways until both sides have finished sending.
func Join(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go half(a, b)
	go half(b, a)
	wg.Wait()
}
