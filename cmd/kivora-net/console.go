package main

import (
	"io"
	"net"
	"os"
)

// maxConsole is the size of the console's file, kept twice: the VM writes what it
// wants on its serial console, and must not be able to fill the server's disk.
const maxConsole = 1 << 20

// serveConsole keeps what QEMU sends from the VM's serial console in a file.
func serveConsole(l net.Listener, path string) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		keepConsole(conn, path, maxConsole)
		conn.Close()
	}
}

// keepConsole appends r to the file at path. A file that reaches limit becomes
// path.1, replacing the one before.
func keepConsole(r io.Reader, path string, limit int64) {
	open := func() (*os.File, int64, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, 0, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		return f, info.Size(), nil
	}
	f, size, err := open()
	if err == nil {
		defer func() { f.Close() }()
	}
	buf := make([]byte, 32<<10)
	for {
		n, readErr := r.Read(buf)
		if n > 0 && f != nil {
			if size+int64(n) > limit {
				f.Close()
				os.Rename(path, path+".1")
				if f, size, err = open(); err != nil {
					f = nil
				}
			}
			if f != nil {
				f.Write(buf[:n])
				size += int64(n)
			}
		}
		// Keep reading when the file fails: QEMU would otherwise block the VM's console.
		if readErr != nil {
			return
		}
	}
}
