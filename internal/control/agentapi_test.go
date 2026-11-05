package control

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
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

// The agent's API has no way to give the agent control.
func TestAgentCannotTakeControl(t *testing.T) {
	s := New(Human)
	h := AgentHandler(s)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/state", `{"owner":"agent"}`},
		{"PUT", "/state", `{"owner":"agent"}`},
		{"POST", "/set", `{"owner":"agent"}`},
		{"POST", "/control", `{"owner":"agent"}`},
		{"POST", "/request", `{"reason":"x","owner":"agent"}`},
		{"POST", "/wait?seq=0", ``},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got := s.Get().Owner; got != Human {
			t.Fatalf("%s %s: owner became %s", c.method, c.path, got)
		}
	}
}

// A timeout too large to multiply must still wait, not answer at once.
func TestWaitWithHugeTimeoutWaits(t *testing.T) {
	s := New(Agent)
	seq := strconv.FormatUint(s.Get().Seq, 10)
	for _, timeout := range []string{"9223372037", "9223372036854775807"} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req := httptest.NewRequest("GET", "/wait?seq="+seq+"&timeout="+timeout, nil).WithContext(ctx)
		start := time.Now()
		AgentHandler(s).ServeHTTP(httptest.NewRecorder(), req)
		cancel()
		if took := time.Since(start); took < 150*time.Millisecond {
			t.Errorf("timeout=%s: answered after %v", timeout, took)
		}
	}
	for _, bad := range []string{"-1", "x"} {
		w := httptest.NewRecorder()
		AgentHandler(s).ServeHTTP(w, httptest.NewRequest("GET", "/wait?seq="+seq+"&timeout="+bad, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("timeout=%s: %d", bad, w.Code)
		}
	}
}
