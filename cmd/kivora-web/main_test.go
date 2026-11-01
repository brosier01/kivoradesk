package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/brosier01/kivoradesk/internal/control"
)

func TestControlStateAcrossRestarts(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "control.json")

	if got := loadControl(path, log).Get().Owner; got != control.Agent {
		t.Fatalf("first start: owner = %s, want agent", got)
	}

	state := control.New(control.Agent)
	save := func() {
		t.Helper()
		if err := saveControl(path, state.Get()); err != nil {
			t.Fatal(err)
		}
	}
	state.Set(control.Human)
	save()
	if got := loadControl(path, log).Get().Owner; got != control.Paused {
		t.Fatalf("restart during a takeover: owner = %s, want paused", got)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v, %v", st, err)
	}

	state.Set(control.Agent)
	save()
	if got := loadControl(path, log).Get().Owner; got != control.Agent {
		t.Fatalf("restart with the agent in control: owner = %s, want agent", got)
	}

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadControl(path, log).Get().Owner; got != control.Paused {
		t.Fatalf("unreadable state: owner = %s, want paused", got)
	}
}
