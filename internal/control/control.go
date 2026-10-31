// Package control tracks who owns the desktop input: the agent, nobody
// (paused) or the human. Exactly one owner exists at any time.
package control

import (
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
}

// State is safe for concurrent use.
type State struct {
	mu        sync.Mutex
	cur       Snapshot
	listeners map[int]func(Snapshot)
	nextID    int
}

// New returns a state owned by initial.
func New(initial Owner) *State {
	return &State{
		cur:       Snapshot{Owner: initial, Since: time.Now().UTC(), Seq: 1},
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

// Set changes the owner and notifies listeners synchronously, so that by the
// time Set returns, every listener has reacted (for example released keys).
// Setting the current owner again is a no-op.
func (s *State) Set(o Owner) Snapshot {
	s.mu.Lock()
	if s.cur.Owner == o {
		snap := s.cur
		s.mu.Unlock()
		return snap
	}
	s.cur = Snapshot{Owner: o, Since: time.Now().UTC(), Seq: s.cur.Seq + 1}
	snap := s.cur
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

// OnChange registers fn to run after every owner change and returns a
// function that unregisters it.
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
