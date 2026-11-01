package control

import (
	"context"
	"testing"
	"time"
)

func TestSetNotifiesAndBumpsSeq(t *testing.T) {
	s := New(Agent)
	var seen []Owner
	cancel := s.OnChange(func(snap Snapshot) { seen = append(seen, snap.Owner) })

	first := s.Get()
	snap := s.Set(Human)
	if snap.Owner != Human || snap.Seq != first.Seq+1 {
		t.Fatalf("Set(Human) = %+v, want owner human and seq %d", snap, first.Seq+1)
	}
	if again := s.Set(Human); again.Seq != snap.Seq {
		t.Fatalf("setting the same owner changed seq to %d", again.Seq)
	}

	cancel()
	s.Set(Paused)
	if len(seen) != 1 || seen[0] != Human {
		t.Fatalf("listener saw %v, want [human]", seen)
	}
	if !s.Is(Paused) {
		t.Fatalf("owner = %s, want paused", s.Get().Owner)
	}
}

func TestRequestHumanPausesAgentUntilReturned(t *testing.T) {
	s := New(Agent)
	snap := s.RequestHuman("solve the CAPTCHA")
	if snap.Owner != Paused || snap.Request != "solve the CAPTCHA" {
		t.Fatalf("RequestHuman = %+v, want paused with the request", snap)
	}
	if snap := s.Set(Human); snap.Request == "" {
		t.Fatal("taking control cleared the request; it should stay visible")
	}
	if snap := s.Set(Agent); snap.Request != "" {
		t.Fatalf("returning control kept request %q", snap.Request)
	}
}

func TestRequestHumanKeepsHumanInControl(t *testing.T) {
	s := New(Human)
	if snap := s.RequestHuman("help"); snap.Owner != Human {
		t.Fatalf("owner = %s, want human", snap.Owner)
	}
}

func TestWaitReturnsOnChangeOrContext(t *testing.T) {
	s := New(Paused)
	seq := s.Get().Seq

	go func() {
		time.Sleep(20 * time.Millisecond)
		s.Set(Agent)
	}()
	snap, err := s.Wait(context.Background(), seq)
	if err != nil || snap.Owner != Agent {
		t.Fatalf("Wait = %+v, %v; want agent", snap, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Wait(ctx, snap.Seq); err == nil {
		t.Fatal("Wait without change returned no error")
	}
}

func TestParseOwner(t *testing.T) {
	for _, ok := range []string{"agent", "paused", "human"} {
		if _, err := ParseOwner(ok); err != nil {
			t.Errorf("ParseOwner(%q): %v", ok, err)
		}
	}
	if _, err := ParseOwner("root"); err == nil {
		t.Error(`ParseOwner("root") succeeded`)
	}
}

func TestRestoreNeverHandsControlToTheAgent(t *testing.T) {
	cases := map[Owner]Owner{Agent: Agent, Paused: Paused, Human: Paused, "nonsense": Paused, "": Paused}
	for saved, want := range cases {
		snap := Restore(Snapshot{Owner: saved, Seq: 41, Request: "log in"}).Get()
		if snap.Owner != want || snap.Seq != 42 || snap.Request != "log in" {
			t.Errorf("Restore(%q) = %+v, want owner %q, seq 42 and the request", saved, snap, want)
		}
	}
}
