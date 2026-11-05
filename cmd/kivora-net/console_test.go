package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleStaysSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	// Far more than the limit: an endless stream from the VM.
	const limit = 100_000
	keepConsole(strings.NewReader(strings.Repeat("x", 3_000_000)+"the end"), path, limit)
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) > limit {
		t.Errorf("console.log has %d bytes", len(current))
	}
	if !strings.HasSuffix(string(current), "the end") {
		t.Error("the latest output is not in console.log")
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) > limit {
		t.Errorf("console.log.1 has %d bytes", len(old))
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 2 {
		t.Errorf("files: %v", entries)
	}
}

func TestConsoleKeepsThePreviousBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	keepConsole(strings.NewReader("first boot\n"), path, 10_000)
	keepConsole(strings.NewReader("second boot\n"), path, 10_000)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first boot\nsecond boot\n" {
		t.Errorf("got %q", got)
	}
}

func TestConsoleReadsEvenWhenTheFileFails(t *testing.T) {
	r := strings.NewReader(strings.Repeat("x", 100_000))
	keepConsole(r, filepath.Join(t.TempDir(), "missing", "console.log"), 10_000)
	if r.Len() != 0 {
		t.Errorf("%d bytes left unread: the VM's console would block", r.Len())
	}
}
