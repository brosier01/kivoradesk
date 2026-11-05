// Package rfb filters the client-to-server half of an RFB (VNC) connection.
//
// The web gateway sits between the viewer and the VNC server. Framebuffer
// updates flow back untouched, but every client message is parsed so that
// keyboard, pointer and clipboard input can be dropped whenever the human
// does not own the desktop. This makes the gateway, not the viewer, the
// authority on human input.
package rfb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Client message types (RFC 6143 and common extensions).
const (
	msgSetPixelFormat           = 0
	msgSetEncodings             = 2
	msgFramebufferUpdateRequest = 3
	msgKeyEvent                 = 4
	msgPointerEvent             = 5
	msgClientCutText            = 6
	msgEnableContinuousUpdates  = 150
	msgClientFence              = 248
	msgXvp                      = 250
	msgSetDesktopSize           = 251
	msgGii                      = 253
	msgQEMU                     = 255
)

const securityNone = 1

// Filter forwards client messages to the server, dropping input when Allow
// returns false. Writes to the server are serialized, so Release may be
// called from another goroutine while Run is active.
type Filter struct {
	// Allow reports whether input from this client may reach the server.
	Allow func() bool

	mu      sync.Mutex
	dst     io.Writer
	keys    map[uint32]bool // keysyms currently held down by this client
	buttons uint8
	x, y    uint16
}

// NewFilter returns a filter writing to dst, the connection to the server.
func NewFilter(dst io.Writer, allow func() bool) *Filter {
	return &Filter{Allow: allow, dst: dst, keys: make(map[uint32]bool)}
}

// Run reads the client stream until it ends or a protocol error occurs.
func (f *Filter) Run(src io.Reader) error {
	if err := f.Handshake(src); err != nil {
		return err
	}
	return f.Forward(src)
}

// Forward does the work of Run after Handshake.
func (f *Filter) Forward(src io.Reader) error {
	for {
		msg, input, err := readMessage(src)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := f.forward(msg, input); err != nil {
			return err
		}
	}
}

// Release sends key-up and button-up events for everything this client is
// holding, so that a change of owner never leaves a key or button stuck.
func (f *Filter) Release() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.keys {
		msg := make([]byte, 8)
		msg[0] = msgKeyEvent
		binary.BigEndian.PutUint32(msg[4:], k)
		if _, err := f.dst.Write(msg); err != nil {
			return err
		}
		delete(f.keys, k)
	}
	if f.buttons != 0 {
		f.buttons = 0
		if _, err := f.dst.Write(pointerMsg(0, f.x, f.y)); err != nil {
			return err
		}
	}
	return nil
}

// Handshake forwards the version, security choice and ClientInit. Only RFB
// 3.7/3.8 with security type None is supported: the server is a local x11vnc
// without password, and the gateway handles authentication itself.
//
// Each step is forwarded as soon as it is read: the server answers the
// version before the client sends its security choice, so batching would
// deadlock.
func (f *Filter) Handshake(src io.Reader) error {
	if err := f.handshake(src); err != nil {
		return fmt.Errorf("rfb handshake: %w", err)
	}
	return nil
}

func (f *Filter) handshake(src io.Reader) error {
	version := make([]byte, 12)
	if _, err := io.ReadFull(src, version); err != nil {
		return err
	}
	switch string(version) {
	case "RFB 003.007\n", "RFB 003.008\n":
	default:
		return fmt.Errorf("unsupported protocol version %q", version)
	}
	if err := f.write(version); err != nil {
		return err
	}
	security := make([]byte, 1)
	if _, err := io.ReadFull(src, security); err != nil {
		return err
	}
	if security[0] != securityNone {
		return fmt.Errorf("unsupported security type %d", security[0])
	}
	if err := f.write(security); err != nil {
		return err
	}
	clientInit := make([]byte, 1)
	if _, err := io.ReadFull(src, clientInit); err != nil {
		return err
	}
	return f.write(clientInit)
}

func (f *Filter) forward(msg []byte, input bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if input && !f.Allow() {
		return nil
	}
	switch msg[0] {
	case msgKeyEvent:
		k := binary.BigEndian.Uint32(msg[4:])
		if msg[1] != 0 {
			f.keys[k] = true
		} else {
			delete(f.keys, k)
		}
	case msgPointerEvent:
		f.buttons = msg[1]
		f.x, f.y = binary.BigEndian.Uint16(msg[2:]), binary.BigEndian.Uint16(msg[4:])
	case msgQEMU:
		if msg[1] == 0 { // extended key event
			k := binary.BigEndian.Uint32(msg[4:])
			if binary.BigEndian.Uint16(msg[2:]) != 0 {
				f.keys[k] = true
			} else {
				delete(f.keys, k)
			}
		}
	case msgSetDesktopSize, msgXvp:
		// The screen geometry is the agent's coordinate space and power
		// control is not the viewer's business: never forward these.
		return nil
	}
	_, err := f.dst.Write(msg)
	return err
}

func (f *Filter) write(parts ...[]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range parts {
		if _, err := f.dst.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// readMessage reads one complete client message. input reports whether it
// carries user input (keyboard, pointer, clipboard).
func readMessage(r io.Reader) (msg []byte, input bool, err error) {
	head := make([]byte, 1)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, false, err
	}
	// read appends n more bytes to msg.
	read := func(n int) error {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return unexpected(err)
		}
		msg = append(msg, buf...)
		return nil
	}
	msg = head

	switch head[0] {
	case msgSetPixelFormat:
		err = read(19)
	case msgSetEncodings:
		if err = read(3); err == nil {
			err = read(4 * int(binary.BigEndian.Uint16(msg[2:])))
		}
	case msgFramebufferUpdateRequest:
		err = read(9)
	case msgKeyEvent:
		err, input = read(7), true
	case msgPointerEvent:
		err, input = read(5), true
	case msgClientCutText:
		if err = read(7); err == nil {
			n := int64(int32(binary.BigEndian.Uint32(msg[4:])))
			if n < 0 { // extended clipboard
				n = -n
			}
			if n > 16<<20 {
				return nil, false, fmt.Errorf("clipboard message too large (%d bytes)", n)
			}
			err = read(int(n))
		}
		input = true
	case msgEnableContinuousUpdates:
		err = read(9)
	case msgClientFence:
		if err = read(8); err == nil {
			err = read(int(msg[8]))
		}
	case msgXvp:
		err = read(3)
	case msgSetDesktopSize:
		if err = read(7); err == nil {
			err = read(16 * int(msg[6]))
		}
	case msgGii:
		if err = read(3); err == nil {
			err = read(int(binary.BigEndian.Uint16(msg[2:])))
		}
		input = true
	case msgQEMU:
		if err = read(1); err != nil {
			break
		}
		switch msg[1] {
		case 0: // extended key event
			err, input = read(10), true
		case 1: // audio
			if err = read(2); err == nil && binary.BigEndian.Uint16(msg[2:]) == 2 {
				err = read(6)
			}
		default:
			err = fmt.Errorf("unsupported QEMU message subtype %d", msg[1])
		}
	default:
		err = fmt.Errorf("unsupported client message type %d", head[0])
	}
	if err != nil {
		return nil, false, err
	}
	return msg, input, nil
}

func pointerMsg(buttons uint8, x, y uint16) []byte {
	msg := make([]byte, 6)
	msg[0] = msgPointerEvent
	msg[1] = buttons
	binary.BigEndian.PutUint16(msg[2:], x)
	binary.BigEndian.PutUint16(msg[4:], y)
	return msg
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
