package control

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func serve(t *testing.T, s *State) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: AgentHandler(s)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return NewClient(path)
}

func TestAgentAPI(t *testing.T) {
	s := New(Agent)
	c := serve(t, s)
	ctx := context.Background()

	snap, err := c.State(ctx)
	if err != nil || snap.Owner != Agent {
		t.Fatalf("State = %+v, %v", snap, err)
	}

	snap, err = c.RequestHuman(ctx, "log in with a passkey")
	if err != nil || snap.Owner != Paused || snap.Request != "log in with a passkey" {
		t.Fatalf("RequestHuman = %+v, %v", snap, err)
	}
	if _, err := c.RequestHuman(ctx, "  "); err == nil {
		t.Fatal("empty reason accepted")
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		s.Set(Human)
		s.Set(Agent)
	}()
	for snap.Owner != Agent {
		if snap, err = c.Wait(ctx, snap.Seq, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	same, err := c.Wait(ctx, snap.Seq, time.Second)
	if err != nil || same.Seq != snap.Seq || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("Wait without change = %+v, %v after %v", same, err, time.Since(start))
	}
}
