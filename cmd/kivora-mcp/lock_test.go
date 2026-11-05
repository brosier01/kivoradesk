package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brosier01/kivoradesk/internal/control"
	"github.com/brosier01/kivoradesk/internal/desktop"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// controlled returns a control API for owner, and a server using it.
func controlled(t *testing.T, owner control.Owner) (*control.State, *server, func()) {
	t.Helper()
	state := control.New(owner)
	sock := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &http.Server{Handler: control.AgentHandler(state)}
	go httpSrv.Serve(ln)
	t.Cleanup(func() { httpSrv.Close() })
	return state, &server{ctl: control.NewClient(sock)}, func() { httpSrv.Close() }
}

func TestStillAgent(t *testing.T) {
	state, s, _ := controlled(t, control.Agent)
	ctx := context.Background()
	seq := state.Get().Seq
	if err := s.stillAgent(ctx, seq); err != nil {
		t.Fatal(err)
	}
	state.Set(control.Human)
	if err := s.stillAgent(ctx, seq); err == nil || !strings.Contains(err.Error(), `"human"`) {
		t.Fatalf("after a takeover: %v", err)
	}
	// Back to the agent: it has lost its turn all the same.
	state.Set(control.Agent)
	if err := s.stillAgent(ctx, seq); err == nil {
		t.Fatal("control moved and came back, unnoticed")
	}
}

func TestActionReportsControlMovedMeanwhile(t *testing.T) {
	state, s, _ := controlled(t, control.Agent)
	_, err := s.guard(context.Background(), act, func() ([]mcp.Content, error) {
		state.Set(control.Human)
		return text("purchase confirmed"), nil
	})
	if err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("guard = %v", err)
	}
}

// A shell command's result does not depend on who owns the desktop.
func TestWorkKeepsItsResultWhenControlMoves(t *testing.T) {
	state, s, _ := controlled(t, control.Agent)
	content, err := s.guard(context.Background(), work, func() ([]mcp.Content, error) {
		state.Set(control.Human)
		return text("exit code 0"), nil
	})
	if err != nil || len(content) != 2 || !strings.Contains(content[1].(*mcp.TextContent).Text, "human_wait_for_control") {
		t.Fatalf("guard = %v, %v", content, err)
	}
}

// An action that ran is not reported as one that failed: the model could repeat it.
func TestActionWithoutControlServiceIsUncertain(t *testing.T) {
	_, s, stop := controlled(t, control.Agent)
	_, err := s.guard(context.Background(), act, func() ([]mcp.Content, error) {
		stop()
		return text("purchase confirmed"), nil
	})
	if err == nil || !strings.Contains(err.Error(), "the action ran") || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("guard = %v", err)
	}
}

func TestObservationIsDroppedWhenTheHumanTakesOver(t *testing.T) {
	state, s, _ := controlled(t, control.Agent)
	content, err := s.guard(context.Background(), observe, func() ([]mcp.Content, error) {
		state.Set(control.Human)
		return text("what the human is doing"), nil
	})
	if err == nil || content != nil || !strings.Contains(err.Error(), "human_wait_for_control") {
		t.Fatalf("guard = %v, %v", content, err)
	}
	// Paused, looking is allowed.
	state.Set(control.Paused)
	if _, err := s.guard(context.Background(), observe, func() ([]mcp.Content, error) { return text("ok"), nil }); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedActionRespectsItsDeadline(t *testing.T) {
	_, s, _ := controlled(t, control.Agent)
	running, release := make(chan struct{}), make(chan struct{})
	go s.guard(context.Background(), act, func() ([]mcp.Content, error) {
		close(running)
		<-release
		return nil, nil
	})
	<-running
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.guard(ctx, act, func() ([]mcp.Content, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "another action") {
		t.Fatalf("guard = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
}

// Two MCP sessions, two processes: the file keeps their actions apart.
func TestSessionsActOneAtATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "action.lock")
	var mu sync.Mutex
	running, peak := 0, 0
	var wg sync.WaitGroup
	for range 2 {
		l := &actionLock{path: path}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				release, err := l.acquire(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				running++
				peak = max(peak, running)
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				running--
				mu.Unlock()
				release()
			}
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("%d actions at once", peak)
	}
}

func TestLockFromAnotherSessionRespectsTheDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "action.lock")
	holder := &actionLock{path: path}
	release, err := holder.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := (&actionLock{path: path}).acquire(ctx); err == nil {
		t.Fatal("two sessions held the lock")
	}
}

// fakeXdotool logs each call to a file and returns that file.
func fakeXdotool(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$*\" >>" + log + "\n"
	if err := os.WriteFile(filepath.Join(bin, "xdotool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}

// A long press stops when the human takes over, and says how far it went.
func TestRepeatedKeysStopAtATakeover(t *testing.T) {
	log := fakeXdotool(t)
	state, s, _ := controlled(t, control.Agent)
	s.desk = &desktop.Desktop{Display: ":99", Width: 1280, Height: 800, ShotDir: t.TempDir()}
	ctx := context.Background()
	done, err := s.whileAgent(ctx, 50, func() error {
		if err := s.desk.Key(ctx, "k", 1); err != nil {
			return err
		}
		if data, _ := os.ReadFile(log); strings.Count(string(data), "\n") == 3 {
			state.Set(control.Human)
		}
		return nil
	})
	if err == nil || done != 3 {
		t.Fatalf("whileAgent = %d, %v", done, err)
	}
	if data, _ := os.ReadFile(log); strings.Count(string(data), "\n") != 3 {
		t.Fatalf("keys sent: %s", data)
	}
}
