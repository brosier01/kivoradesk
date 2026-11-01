package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogWritesDailyFilesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "audit-2020-01-01.jsonl")
	recent := filepath.Join(dir, "audit-"+time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")+".jsonl")
	os.WriteFile(old, []byte("{}\n"), 0o600)
	os.WriteFile(recent, []byte("{}\n"), 0o600)

	l := &Log{Dir: dir, Retention: 90 * 24 * time.Hour}
	if err := l.Append([]byte("{\"tool\":\"x\"}\n")); err != nil {
		t.Fatal(err)
	}
	l.Close()

	if _, err := os.Stat(old); err == nil {
		t.Error("file older than the retention was kept")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent file was deleted")
	}
	path := filepath.Join(dir, "audit-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	today, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(today), `"tool":"x"`) {
		t.Fatalf("today's log = %q, %v", today, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
}

func TestClientReportsAnAbsentOrFailingHelper(t *testing.T) {
	c := &Client{Prefix: []string{"/nonexistent/helper"}}
	if err := c.Append([]byte("x\n")); err == nil {
		t.Fatal("no error without a helper")
	}

	c = &Client{Prefix: []string{"sh", "-c", "read line; exit 1"}}
	defer c.Close()
	if err := c.Append([]byte("x\n")); err == nil {
		t.Fatal("no error when the helper stops without confirming")
	}
}

func TestClientWaitsForConfirmation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "lines")
	c := &Client{Prefix: []string{"sh", "-c", `while read -r line; do echo "$line" >>"$0"; echo ok; done`, file}}
	for _, line := range []string{"one\n", "two\n"} {
		if err := c.Append([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	if b, err := os.ReadFile(file); err != nil || string(b) != "one\ntwo\n" {
		t.Fatalf("helper got %q, %v", b, err)
	}
}
