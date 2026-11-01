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
