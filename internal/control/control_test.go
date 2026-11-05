package control

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

// A listener that does not return must not hold the others or the record.
func TestStuckListenerHoldsNeitherTheRecordNorTheOthers(t *testing.T) {
	s := New(Agent)
	saved := make(chan Owner, 1)
	s.Persist(func(snap Snapshot) { saved <- snap.Owner })
	release := make(chan struct{})
	s.OnChange(func(Snapshot) { <-release })
	other := make(chan struct{})
	s.OnChange(func(Snapshot) { close(other) })

	done := make(chan struct{})
	go func() {
		s.Set(Human)
		close(done)
	}()
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("the other listener did not run")
	}
	select {
	case owner := <-saved:
		if owner != Human {
			t.Fatalf("recorded %s", owner)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the change was not recorded")
	}
	if !s.Is(Human) {
		t.Fatal("the owner did not change at once")
	}
	select {
	case <-done:
		t.Fatal("Set returned before every listener reacted")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
}

func TestPersistRunsBeforeListeners(t *testing.T) {
	s := New(Agent)
	var order []string
	var mu sync.Mutex
	add := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}
	s.Persist(func(Snapshot) { add("persist") })
	s.OnChange(func(Snapshot) { add("listener") })
	s.Set(Paused)
	if strings.Join(order, ",") != "persist,listener" {
		t.Fatalf("order = %v", order)
	}
}

// The agent must not act before what the human held is released, and a waiter
// must not wake up before it either.
func TestAgentGetsControlOnlyOnceListenersReacted(t *testing.T) {
	s := New(Human)
	release := make(chan struct{})
	reacted := make(chan struct{})
	s.OnChange(func(snap Snapshot) {
		if snap.Owner == Agent {
			close(reacted)
			<-release
		}
	})
	seq := s.Get().Seq
	woken := make(chan Snapshot, 1)
	go func() {
		snap, _ := s.Wait(context.Background(), seq)
		woken <- snap
	}()
	go s.Set(Agent)
	<-reacted

	if got := s.ForAgent(); got.Owner != Human {
		t.Fatalf("the agent sees %s while the keys are being released", got.Owner)
	}
	if !s.Is(Agent) {
		t.Fatalf("the owner is %s", s.Get().Owner)
	}
	select {
	case snap := <-woken:
		t.Fatalf("Wait returned %+v before the listener finished", snap)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if snap := <-woken; snap.Owner != Agent {
		t.Fatalf("Wait returned %+v", snap)
	}
	if got := s.ForAgent(); got.Owner != Agent {
		t.Fatalf("the agent sees %s once settled", got.Owner)
	}
}

// Away from the agent, the change applies at once, even with a listener stuck.
func TestAgentLosesControlAtOnce(t *testing.T) {
	s := New(Agent)
	release := make(chan struct{})
	defer close(release)
	reacted := make(chan struct{})
	s.OnChange(func(Snapshot) {
		close(reacted)
		<-release
	})
	go s.Set(Human)
	<-reacted
	if got := s.ForAgent(); got.Owner != Human {
		t.Fatalf("the agent sees %s", got.Owner)
	}
}

func TestSeqStrictlyGrowsUnderConcurrentChanges(t *testing.T) {
	s := New(Agent)
	var mu sync.Mutex
	seen := map[uint64]bool{}
	s.OnChange(func(snap Snapshot) {
		mu.Lock()
		defer mu.Unlock()
		if seen[snap.Seq] {
			t.Errorf("seq %d seen twice", snap.Seq)
		}
		seen[snap.Seq] = true
	})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 100 {
				switch (i + j) % 3 {
				case 0:
					s.Set(Human)
				case 1:
					s.RequestHuman("help")
				default:
					s.Set(Agent)
				}
			}
		})
	}
	wg.Wait()
	if got, want := s.ForAgent().Seq, s.Get().Seq; s.Is(Agent) && got != want {
		t.Fatalf("settled at seq %d, current %d", got, want)
	}
}

func TestLongRequestIsShortened(t *testing.T) {
	long := strings.Repeat("é", MaxRequest+100)
	for name, got := range map[string]string{
		"request": New(Agent).RequestHuman(long).Request,
		"restore": Restore(Snapshot{Owner: Paused, Request: long}).Get().Request,
	} {
		if utf8.RuneCountInString(got) > MaxRequest+20 || !strings.HasSuffix(got, "(shortened)") || !utf8.ValidString(got) {
			t.Errorf("%s: %d characters: %q", name, utf8.RuneCountInString(got), got)
		}
	}
	short := strings.Repeat("a", MaxRequest)
	if got := New(Agent).RequestHuman(short).Request; got != short {
		t.Errorf("a reason within the limit became %q", got)
	}
}
