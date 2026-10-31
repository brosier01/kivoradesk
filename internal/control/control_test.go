package control

import "testing"

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
