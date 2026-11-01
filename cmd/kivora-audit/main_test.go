package main

import (
	"bytes"
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
	if err != nil || string(b) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("log = %q, %v", b, err)
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
