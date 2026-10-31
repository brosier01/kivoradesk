// Package control tracks who owns the desktop input: the agent, nobody
// (paused) or the human. Exactly one owner exists at any time.
//
// Only the human moves control freely. The agent can ask for help, which
// pauses it until the human hands control back.
package control

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Owner is the party allowed to send input to the desktop.
type Owner string

const (
	Agent  Owner = "agent"
	Paused Owner = "paused"
	Human  Owner = "human"
)

// ParseOwner validates an owner name.
func ParseOwner(s string) (Owner, error) {
	switch o := Owner(s); o {
	case Agent, Paused, Human:
		return o, nil
	}
	return "", fmt.Errorf("unknown owner %q (want agent, paused or human)", s)
}

// Snapshot is the state at a point in time. Seq increases on every change,
// so a holder can detect that control moved since it last looked.
type Snapshot struct {
	Owner Owner     `json:"owner"`
	Since time.Time `json:"since"`
	Seq   uint64    `json:"seq"`
	// Request is the agent's reason for asking the human to take over, kept
	// until control returns to the agent.
	Request string `json:"request,omitempty"`
}

// State is safe for concurrent use.
type State struct {
	mu        sync.Mutex
	cur       Snapshot
	changed   chan struct{} // closed and replaced on every change
	listeners map[int]func(Snapshot)
	nextID    int
}

// New returns a state owned by initial.
func New(initial Owner) *State {
	return &State{
		cur:       Snapshot{Owner: initial, Since: time.Now().UTC(), Seq: 1},
		changed:   make(chan struct{}),
		listeners: make(map[int]func(Snapshot)),
	}
}

// Get returns the current snapshot.
func (s *State) Get() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Is reports whether o currently owns the input.
func (s *State) Is(o Owner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.Owner == o
}

// Set changes the owner on behalf of the human. Returning control to the
// agent clears any pending request. Setting the current owner again is a
// no-op.
func (s *State) Set(o Owner) Snapshot {
	s.mu.Lock()
	if s.cur.Owner == o {
		snap := s.cur
		s.mu.Unlock()
		return snap
	}
	request := s.cur.Request
	if o == Agent {
		request = ""
	}
	return s.commit(o, request)
}

// RequestHuman records the agent's request for help. If the agent holds
// control, it is paused until the human hands control back.
func (s *State) RequestHuman(reason string) Snapshot {
	s.mu.Lock()
	owner := s.cur.Owner
	if owner == Agent {
		owner = Paused
	}
	return s.commit(owner, reason)
}

// Wait blocks until the sequence number differs from seq, or ctx ends.
func (s *State) Wait(ctx context.Context, seq uint64) (Snapshot, error) {
	for {
		s.mu.Lock()
		snap, changed := s.cur, s.changed
		s.mu.Unlock()
		if snap.Seq != seq {
			return snap, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return snap, ctx.Err()
		}
	}
}

// OnChange registers fn to run after every change and returns a function
// that unregisters it.
func (s *State) OnChange(fn func(Snapshot)) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.listeners[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.listeners, id)
	}
}

// commit stores a new snapshot and notifies listeners synchronously, so that
// by the time the caller returns, every listener has reacted (for example
// released held keys). It must be called with s.mu held and releases it.
func (s *State) commit(o Owner, request string) Snapshot {
	s.cur = Snapshot{Owner: o, Since: time.Now().UTC(), Seq: s.cur.Seq + 1, Request: request}
	snap := s.cur
	close(s.changed)
	s.changed = make(chan struct{})
	fns := make([]func(Snapshot), 0, len(s.listeners))
	for _, fn := range s.listeners {
		fns = append(fns, fn)
	}
	s.mu.Unlock()

	for _, fn := range fns {
		fn(snap)
	}
	return snap
}
