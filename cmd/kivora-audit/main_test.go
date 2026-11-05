package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brosier01/kivoradesk/internal/audit"
)

func TestRecordConfirmsEachLine(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := record(&audit.Log{Dir: dir, Retention: 48 * time.Hour}, strings.NewReader("{\"a\":1}\n{\"a\":2}\n"), &out)
	if err != nil || out.String() != "ok\nok\n" {
		t.Fatalf("record = %q, %v", out.String(), err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "audit-"+time.Now().UTC().Format("2006-01-02")+".jsonl"))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if err != nil || len(lines) != 2 || !strings.HasSuffix(lines[0], `,"a":1}`) || !strings.HasSuffix(lines[1], `,"a":2}`) {
		t.Fatalf("log = %q, %v", b, err)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if recorded, _ := time.Parse(time.RFC3339Nano, first["recorded"].(string)); time.Since(recorded) > time.Minute {
		t.Fatalf("recorded = %v", first["recorded"])
	}
}

func TestEntry(t *testing.T) {
	now := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	const at = `{"recorded":"2026-11-05T01:02:03Z"`
	for in, want := range map[string]string{
		`{"tool":"x"}`:             at + `,"tool":"x"}` + "\n",
		`  { "tool" : "x" }  `:     at + `,"tool":"x"}` + "\n",
		`{}`:                       at + "}\n",
		`{"t":"\u001b]0;x\u0007"}`: at + `,"t":"\u001b]0;x\u0007"}` + "\n",
	} {
		got, err := entry([]byte(in), now)
		if err != nil || string(got) != want {
			t.Errorf("entry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "not json", "\x1b]0;forged\a", `{"t":"` + "\x1b" + `"}`, `[1]`, `"x"`, `1`, `{"a":1} {"b":2}`} {
		if got, err := entry([]byte(bad), now); err == nil {
			t.Errorf("entry(%q) = %q, want an error", bad, got)
		}
	}
}

func TestRecordRefusesWhatIsNotJSON(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := record(&audit.Log{Dir: dir, Retention: 48 * time.Hour}, strings.NewReader("\x1b]0;forged\a not json\n"), &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("record = %q, %v", out.String(), err)
	}
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Fatalf("something was stored: %v", files)
	}
}

func TestRecordFailsWhenItCannotWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	var out bytes.Buffer
	err := record(&audit.Log{Dir: filepath.Join(file, "dir"), Retention: 48 * time.Hour}, strings.NewReader("{}\n"), &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("record = %q, %v", out.String(), err)
	}
}
