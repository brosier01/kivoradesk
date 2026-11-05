package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brosier01/kivoradesk/internal/secrets"
)

func TestVaultCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	vault := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := run(path, args, strings.NewReader(stdin), &out)
		return out.String(), err
	}

	if _, err := vault("hunter2\n", "set", "-field", "text", "login", "https://a.example"); err != nil {
		t.Fatal(err)
	}
	out, err := vault("", "list")
	if err != nil || !strings.Contains(out, "login") || !strings.Contains(out, `"text"`) || strings.Contains(out, "hunter2") {
		t.Fatalf("list = %q, %v", out, err)
	}

	out, err = vault(`{"name": "login", "url": "https://a.example/signin"}`, "value")
	var g secrets.Grant
	if err != nil || json.Unmarshal([]byte(out), &g) != nil || g.Value != "hunter2" || g.Origin != "https://a.example" || g.Field != "text" {
		t.Fatalf("value = %q, %v", out, err)
	}
	if out, err := vault(`{"name": "login", "url": "https://b.example/"}`, "value"); err == nil || strings.Contains(out+err.Error(), "hunter2") {
		t.Fatalf("value elsewhere = %q, %v", out, err)
	}
	if _, err := vault("not json", "value"); err == nil {
		t.Fatal("a malformed request was accepted")
	}

	if _, err := vault("", "delete", "login"); err != nil {
		t.Fatal(err)
	}
	if _, err := vault(`{"name": "login", "url": "https://a.example/"}`, "value"); err == nil {
		t.Fatal("deleted secret still usable")
	}
	if _, err := vault("", "nonsense"); err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestScrubHidesEveryStoredValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	vault := func(stdin string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run(path, args, strings.NewReader(stdin), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := vault("no store yet", "scrub"); got != "no store yet" {
		t.Fatalf("scrub = %q", got)
	}
	vault("hunter2-a", "set", "one", "https://a.example")
	vault("hunter2-b", "set", "-field", "text", "two", "https://b.example")
	got := vault("first hunter2-a then hunter2-b, and nothing else", "scrub")
	if want := "first [secret hidden] then [secret hidden], and nothing else"; got != want {
		t.Fatalf("scrub = %q, want %q", got, want)
	}
	if err := run(path, []string{"scrub"}, strings.NewReader(strings.Repeat("x", secrets.MaxScrub+1)), &bytes.Buffer{}); err == nil {
		t.Fatal("a text over the limit was accepted")
	}
}

// A value is stored whole or not at all.
func TestSetRefusesWhatItCannotKeep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	set := func(value string) error {
		return run(path, []string{"set", "s", "https://a.example"}, strings.NewReader(value), &bytes.Buffer{})
	}
	if err := set(strings.Repeat("x", maxInput+1)); err == nil || !strings.Contains(err.Error(), "longer") {
		t.Fatalf("a value over the limit: %v", err)
	}
	if err := set("caf\xe9"); err == nil {
		t.Fatal("a value that is not UTF-8 was accepted")
	}
	if err := set(strings.Repeat("x", maxInput)); err != nil {
		t.Fatalf("a value at the limit: %v", err)
	}
}
