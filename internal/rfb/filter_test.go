package rfb

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
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

func TestDropsEveryKindOfInputWhenNotAllowed(t *testing.T) {
	for name, msg := range map[string][]byte{
		"key":                keyMsg(true, 'a'),
		"pointer":            pointerMsg(1, 10, 20),
		"clipboard":          {msgClientCutText, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'},
		"extended clipboard": {msgClientCutText, 0, 0, 0, 0xff, 0xff, 0xff, 0xfe, 'h', 'i'},
		"QEMU key":           {msgQEMU, 0, 0, 1, 0, 0, 0, 'q', 0, 0, 0, 24},
		"GII":                {msgGii, 0, 0, 4, 1, 2, 3, 4},
	} {
		t.Run(name, func(t *testing.T) {
			want := concat(handshake, fbRequest())
			if got := run(t, false, concat(handshake, msg, fbRequest())); !bytes.Equal(got, want) {
				t.Fatalf("forwarded %x, want %x", got, want)
			}
			stream := concat(handshake, msg, fbRequest())
			if got := run(t, true, stream); !bytes.Equal(got, stream) {
				t.Fatalf("with the human in control: forwarded %x, want %x", got, stream)
			}
		})
	}
}

func TestNeverForwardsPowerControl(t *testing.T) {
	stream := concat(handshake, []byte{msgXvp, 0, 1, 2}, fbRequest())
	want := concat(handshake, fbRequest())
	if got := run(t, true, stream); !bytes.Equal(got, want) {
		t.Fatalf("forwarded %x, want %x", got, want)
	}
}

// A complete stream: only the security type is wrong.
func TestRejectsOtherSecurityTypes(t *testing.T) {
	for _, security := range []byte{0, 2, 16, 255} {
		var out bytes.Buffer
		f := NewFilter(&out, func() bool { return true })
		stream := concat([]byte("RFB 003.008\n"), []byte{security, 1}, fbRequest())
		if err := f.Run(bytes.NewReader(stream)); err == nil {
			t.Errorf("security type %d accepted", security)
		}
		if out.String() != "RFB 003.008\n" {
			t.Errorf("security type %d: forwarded %x", security, out.Bytes())
		}
	}
}

func TestReleaseLetsGoOfExtendedKeys(t *testing.T) {
	var out bytes.Buffer
	f := NewFilter(&out, func() bool { return true })
	down := []byte{msgQEMU, 0, 0, 1, 0, 0, 0, 'q', 0, 0, 0, 24}
	if err := f.Run(bytes.NewReader(concat(handshake, down))); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := f.Release(); err != nil {
		t.Fatal(err)
	}
	if want := keyMsg(false, 'q'); !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("release wrote %x, want %x", out.Bytes(), want)
	}
}

// replay reads what reached the server as the server would, and returns the keys
// and buttons left held.
func replay(t *testing.T, out []byte) (keys map[uint32]bool, buttons uint8, inputs int) {
	t.Helper()
	keys = map[uint32]bool{}
	if len(out) < len(handshake) {
		return keys, 0, 0
	}
	r := bytes.NewReader(out[len(handshake):])
	for r.Len() > 0 {
		msg, input, err := readMessage(r)
		if err != nil {
			t.Fatalf("the filter forwarded what cannot be read: %v in %x", err, out)
		}
		if input {
			inputs++
		}
		switch msg[0] {
		case msgSetDesktopSize, msgXvp:
			t.Fatalf("forbidden message forwarded: %x", msg)
		case msgKeyEvent:
			k := binary.BigEndian.Uint32(msg[4:])
			if msg[1] != 0 {
				keys[k] = true
			} else {
				delete(keys, k)
			}
		case msgPointerEvent:
			buttons = msg[1]
		case msgQEMU:
			if msg[1] == 0 {
				k := binary.BigEndian.Uint32(msg[4:])
				if binary.BigEndian.Uint16(msg[2:]) != 0 {
					keys[k] = true
				} else {
					delete(keys, k)
				}
			}
		}
	}
	return keys, buttons, inputs
}

func filterSeeds() [][]byte {
	return [][]byte{
		nil, handshake,
		concat(handshake, keyMsg(true, 'a'), keyMsg(false, 'a')),
		concat(handshake, pointerMsg(1, 10, 20), pointerMsg(0, 10, 20)),
		concat(handshake, []byte{msgClientCutText, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'}),
		concat(handshake, []byte{msgClientCutText, 0, 0, 0, 0xff, 0xff, 0xff, 0xfe, 1, 2}),
		concat(handshake, []byte{msgClientCutText, 0, 0, 0, 0x80, 0, 0, 0}),
		concat(handshake, []byte{msgQEMU, 0, 0, 1, 0, 0, 0, 'a', 0, 0, 0, 30}),
		concat(handshake, []byte{msgQEMU, 1, 0, 2, 0, 0, 0, 0, 0, 0}),
		concat(handshake, []byte{msgGii, 0, 0, 2, 1, 2}),
		concat(handshake, []byte{msgSetDesktopSize, 0, 7, 128, 4, 56, 1, 0}, make([]byte, 16)),
		concat(handshake, []byte{msgXvp, 0, 1, 2}),
		concat(handshake, []byte{msgClientFence, 0, 0, 0, 0, 0, 0, 0, 2, 1, 2}),
		concat(handshake, []byte{msgEnableContinuousUpdates, 1, 0, 0, 0, 0, 5, 0, 3, 32}),
		concat(handshake, []byte{msgSetEncodings, 0, 0, 2, 0, 0, 0, 1, 0, 0, 0, 2}),
		concat(handshake, fbRequest()),
		concat([]byte("RFB 003.007\n\x01\x01"), fbRequest()),
	}
}

// Whatever the client sends: what is forwarded can be read back, no input gets
// through unless allowed, nothing forbidden does, and Release leaves nothing held.
func FuzzFilter(f *testing.F) {
	for _, s := range filterSeeds() {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, stream []byte, allow bool) {
		var out bytes.Buffer
		flt := NewFilter(&out, func() bool { return allow })
		_ = flt.Run(bytes.NewReader(stream))
		if _, _, inputs := replay(t, out.Bytes()); !allow && inputs != 0 {
			t.Fatalf("%d input messages forwarded while not allowed", inputs)
		}
		if err := flt.Release(); err != nil {
			t.Fatal(err)
		}
		if keys, buttons, _ := replay(t, out.Bytes()); len(keys) != 0 || buttons != 0 {
			t.Fatalf("still held after Release: keys %v, buttons %d", keys, buttons)
		}
	})
}

// The owner changes in the middle of the stream.
func FuzzFilterToggle(f *testing.F) {
	for _, s := range filterSeeds() {
		f.Add(s, []byte{1, 0, 1})
	}
	f.Fuzz(func(t *testing.T, stream, pattern []byte) {
		if len(pattern) == 0 {
			pattern = []byte{1}
		}
		i, allowed := 0, 0
		var out bytes.Buffer
		flt := NewFilter(&out, func() bool {
			ok := pattern[i%len(pattern)]&1 == 1
			i++
			if ok {
				allowed++
			}
			return ok
		})
		_ = flt.Run(bytes.NewReader(stream))
		if _, _, inputs := replay(t, out.Bytes()); inputs != allowed {
			t.Fatalf("%d input messages forwarded for %d allowed", inputs, allowed)
		}
		if err := flt.Release(); err != nil {
			t.Fatal(err)
		}
		if keys, buttons, _ := replay(t, out.Bytes()); len(keys) != 0 || buttons != 0 {
			t.Fatalf("still held after Release: keys %v, buttons %d", keys, buttons)
		}
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Concurrent changes of owner never leave a key or a button down once the owner is
// not the human any more, in the order the gateway does it: the owner changes,
// then the listener releases.
func TestNoInputLeftHeldWhenTheOwnerChangesDuringTheStream(t *testing.T) {
	for run := range 50 {
		var mu sync.Mutex
		var out bytes.Buffer
		dst := writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return out.Write(p)
		})
		var allow atomic.Bool
		allow.Store(true)
		flt := NewFilter(dst, allow.Load)
		var stream bytes.Buffer
		rng := rand.New(rand.NewSource(int64(run)))
		stream.Write(handshake)
		for range 200 {
			switch rng.Intn(3) {
			case 0:
				stream.Write(keyMsg(rng.Intn(2) == 0, uint32(rng.Intn(8))))
			case 1:
				stream.Write(pointerMsg(uint8(rng.Intn(4)), 5, 6))
			default:
				stream.Write(fbRequest())
			}
		}
		done := make(chan struct{})
		go func() {
			flt.Run(bytes.NewReader(stream.Bytes()))
			close(done)
		}()
		for range 3 {
			allow.Store(false)
			flt.Release()
			allow.Store(true)
		}
		allow.Store(false)
		flt.Release()
		<-done
		mu.Lock()
		keys, buttons, _ := replay(t, out.Bytes())
		mu.Unlock()
		if len(keys) != 0 || buttons != 0 {
			t.Fatalf("run %d: left down: keys %v, buttons %d", run, keys, buttons)
		}
	}
}
