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

// MaxRequest is how many characters of the agent's reason the human is shown:
// it must not push the desktop out of the page, and it may come from a web page.
const MaxRequest = 300

// shorten cuts a reason that is too long, and says so.
func shorten(reason string) string {
	r := []rune(reason)
	if len(r) <= MaxRequest {
		return reason
	}
	return string(r[:MaxRequest]) + "… (shortened)"
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
	mu  sync.Mutex
	cur Snapshot
	// settled is the latest change whose listeners have all reacted.
	settled   Snapshot
	changed   chan struct{} // closed and replaced when a change has settled
	persist   func(Snapshot)
	listeners map[int]func(Snapshot)
	nextID    int
}

// New returns a state owned by initial.
func New(initial Owner) *State {
	snap := Snapshot{Owner: initial, Since: time.Now().UTC(), Seq: 1}
	return &State{
		cur:       snap,
		settled:   snap,
		changed:   make(chan struct{}),
		listeners: make(map[int]func(Snapshot)),
	}
}

// Restore returns a state resuming from a snapshot saved by a previous run.
// Control that was not the agent's does not go back to it: after a restart,
// a human owner may no longer be at the viewer, so the agent stays paused until
// the human hands control back. A pending request is kept, and seq keeps growing.
func Restore(saved Snapshot) *State {
	owner := Paused
	if saved.Owner == Agent {
		owner = Agent
	}
	s := New(owner)
	s.cur.Seq = saved.Seq + 1
	s.cur.Request = shorten(saved.Request)
	s.settled = s.cur
	return s
}

// Get returns the current snapshot.
func (s *State) Get() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// ForAgent returns what the agent may act on. It loses control at once, and gets
// it only once every listener has reacted, so never before the keys and buttons
// the human held are released.
func (s *State) ForAgent() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.Owner == Agent {
		return s.settled
	}
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
	return s.commit(owner, shorten(reason))
}

// Wait blocks until a change with another sequence number has settled, or ctx
// ends. It returns the settled snapshot.
func (s *State) Wait(ctx context.Context, seq uint64) (Snapshot, error) {
	for {
		s.mu.Lock()
		snap, changed := s.settled, s.changed
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

// Persist sets what records each change. It runs before the listeners, so that a
// listener that takes time does not delay the record.
func (s *State) Persist(fn func(Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persist = fn
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

// commit stores a new snapshot, records it and notifies listeners, each on its
// own so that a slow one does not hold the others. By the time the caller returns
// or a waiter wakes up, every listener has reacted (for example released held
// keys). It must be called with s.mu held and releases it.
func (s *State) commit(o Owner, request string) Snapshot {
	s.cur = Snapshot{Owner: o, Since: time.Now().UTC(), Seq: s.cur.Seq + 1, Request: request}
	snap := s.cur
	persist := s.persist
	fns := make([]func(Snapshot), 0, len(s.listeners))
	for _, fn := range s.listeners {
		fns = append(fns, fn)
	}
	s.mu.Unlock()

	if persist != nil {
		persist(snap)
	}
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Go(func() { fn(snap) })
	}
	wg.Wait()

	s.mu.Lock()
	if snap.Seq > s.settled.Seq {
		s.settled = snap
	}
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	return snap
}
