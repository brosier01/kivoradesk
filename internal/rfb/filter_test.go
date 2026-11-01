package rfb

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"
)

var handshake = []byte("RFB 003.008\n\x01\x01")

func keyMsg(down bool, key uint32) []byte {
	msg := make([]byte, 8)
	msg[0] = msgKeyEvent
	if down {
		msg[1] = 1
	}
	binary.BigEndian.PutUint32(msg[4:], key)
	return msg
}

func fbRequest() []byte { return []byte{msgFramebufferUpdateRequest, 1, 0, 0, 0, 0, 5, 0, 3, 32} }

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func run(t *testing.T, allow bool, stream []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	f := NewFilter(&out, func() bool { return allow })
	if err := f.Run(bytes.NewReader(stream)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out.Bytes()
}

func TestForwardsEverythingWhenAllowed(t *testing.T) {
	stream := concat(handshake, fbRequest(), pointerMsg(1, 10, 20), keyMsg(true, 'a'), keyMsg(false, 'a'))
	if got := run(t, true, stream); !bytes.Equal(got, stream) {
		t.Fatalf("forwarded %x, want %x", got, stream)
	}
}

func TestDropsInputWhenNotAllowed(t *testing.T) {
	cut := concat([]byte{msgClientCutText, 0, 0, 0, 0, 0, 0, 5}, []byte("hello"))
	stream := concat(handshake, pointerMsg(1, 10, 20), fbRequest(), keyMsg(true, 'a'), cut, fbRequest())
	want := concat(handshake, fbRequest(), fbRequest())
	if got := run(t, false, stream); !bytes.Equal(got, want) {
		t.Fatalf("forwarded %x, want %x", got, want)
	}
}

func TestNeverForwardsDesktopResize(t *testing.T) {
	resize := concat([]byte{msgSetDesktopSize, 0, 7, 128, 4, 56, 1, 0}, make([]byte, 16))
	stream := concat(handshake, resize, fbRequest())
	want := concat(handshake, fbRequest())
	if got := run(t, true, stream); !bytes.Equal(got, want) {
		t.Fatalf("forwarded %x, want %x", got, want)
	}
}

func TestParsesVariableLengthMessages(t *testing.T) {
	encodings := []byte{msgSetEncodings, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 7}
	fence := []byte{msgClientFence, 0, 0, 0, 0, 0, 0, 1, 3, 'a', 'b', 'c'}
	extKey := []byte{msgQEMU, 0, 0, 1, 0, 0, 0, 'q', 0, 0, 0, 24}
	audio := []byte{msgQEMU, 1, 0, 2, 0, 2, 0, 0, 0xac, 0x44}
	stream := concat(handshake, encodings, fence, extKey, audio, fbRequest())
	if got := run(t, true, stream); !bytes.Equal(got, stream) {
		t.Fatalf("forwarded %x, want %x", got, stream)
	}
}

func TestReleaseLetsGoOfHeldKeysAndButtons(t *testing.T) {
	var out bytes.Buffer
	f := NewFilter(&out, func() bool { return true })
	stream := concat(handshake, keyMsg(true, 'a'), keyMsg(true, 'b'), keyMsg(false, 'b'), pointerMsg(1, 300, 400))
	if err := f.Run(bytes.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := f.Release(); err != nil {
		t.Fatal(err)
	}
	want := concat(keyMsg(false, 'a'), pointerMsg(0, 300, 400))
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("release wrote %x, want %x", out.Bytes(), want)
	}
	out.Reset()
	if err := f.Release(); err != nil || out.Len() != 0 {
		t.Fatalf("second release wrote %x (err %v), want nothing", out.Bytes(), err)
	}
}

// The server answers each handshake step before the client sends the next,
// so the filter must forward the version without waiting for more input.
func TestHandshakeForwardsEachStepImmediately(t *testing.T) {
	clientR, clientW := io.Pipe()
	serverR, serverW := io.Pipe()
	f := NewFilter(serverW, func() bool { return true })
	go f.Run(clientR)

	go clientW.Write([]byte("RFB 003.008\n"))
	got := make([]byte, 12)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(serverR, got); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(got) != "RFB 003.008\n" {
			t.Fatalf("server read %q, %v", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("version was not forwarded before the security choice arrived")
	}
	clientW.Close()
}

func TestRejectsUnsupportedHandshakeAndMessages(t *testing.T) {
	cases := map[string][]byte{
		"old version":   []byte("RFB 003.003\n"),
		"vnc auth":      []byte("RFB 003.008\n\x02"),
		"unknown type":  concat(handshake, []byte{99}),
		"truncated key": concat(handshake, []byte{msgKeyEvent, 1}),
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			f := NewFilter(&bytes.Buffer{}, func() bool { return true })
			if err := f.Run(bytes.NewReader(stream)); err == nil {
				t.Fatal("Run succeeded, want an error")
			}
		})
	}
}

func TestRejectsHugeClipboard(t *testing.T) {
	stream := concat(handshake, []byte{msgClientCutText, 0, 0, 0, 0x7f, 0xff, 0xff, 0xff})
	f := NewFilter(&bytes.Buffer{}, func() bool { return true })
	err := f.Run(bytes.NewReader(stream))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Run error = %v, want clipboard too large", err)
	}
}

// A length out of range ends the connection. The most negative one has no
// positive counterpart: it must not get past the size check.
func TestRejectsInvalidClipboardLengths(t *testing.T) {
	for _, length := range []uint32{0x80000000, 0x7fffffff, 16<<20 + 1, -(16<<20 + 1) & 0xffffffff} {
		msg := make([]byte, 8)
		msg[0] = msgClientCutText
		binary.BigEndian.PutUint32(msg[4:], length)
		f := NewFilter(io.Discard, func() bool { return true })
		if err := f.Run(bytes.NewReader(concat(handshake, msg))); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Errorf("length %#x: Run = %v", length, err)
		}
	}
}

func TestForwardsExtendedClipboard(t *testing.T) {
	msg := make([]byte, 8)
	msg[0] = msgClientCutText
	binary.BigEndian.PutUint32(msg[4:], uint32(0xffffffff)-4) // -5: extended format, 5 bytes
	stream := concat(handshake, msg, []byte("hello"), fbRequest())
	if got := run(t, true, stream); !bytes.Equal(got, stream) {
		t.Fatalf("forwarded %x, want %x", got, stream)
	}
}
