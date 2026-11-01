package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		`curl -H "Authorization: Bearer abcdef1234567890" https://api.example.com`:                "abcdef1234567890",
		`mysql --password=hunter2 -u root`:                                                        "hunter2",
		`{"command":"export API_KEY=s3cr3tvalue && run"}`:                                         "s3cr3tvalue",
		`git clone https://bob:pa55word@git.example.com/repo.git`:                                 "pa55word",
		`echo AKIAIOSFODNN7EXAMPLE`:                                                               "AKIAIOSFODNN7EXAMPLE",
		`token ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789`:                                          "ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
		`{"command":"curl -d '{\"password\": \"swordfish\"}'"}`:                                   "swordfish",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----": "b3BlbnNzaC1rZXk",
	}
	for in, secret := range cases {
		out := redact(in)
		if strings.Contains(out, secret) {
			t.Errorf("redact(%q) = %q, secret still visible", in, out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("redact(%q) = %q, no redaction marker", in, out)
		}
	}
	for _, clean := range []string{`ls -la /workspace`, `{"url":"https://example.com/?q=tokens+in+python"}`, `curl https://example.com`} {
		if out := redact(clean); out != clean {
			t.Errorf("redact(%q) = %q, want it unchanged", clean, out)
		}
	}
}

func TestAuditWritesDailyFilesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "audit-2020-01-01.jsonl")
	recent := filepath.Join(dir, "audit-"+time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")+".jsonl")
	os.WriteFile(old, []byte("{}\n"), 0o600)
	os.WriteFile(recent, []byte("{}\n"), 0o600)

	a, err := openAudit(dir, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a.record("abcd1234", "shell_run", map[string]string{"command": "export TOKEN=topsecret"}, nil, time.Millisecond)
	a.Close()

	if _, err := os.Stat(old); err == nil {
		t.Error("file older than the retention was kept")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent file was deleted")
	}
	today, err := os.ReadFile(filepath.Join(dir, "audit-"+time.Now().UTC().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(today), `"tool":"shell_run"`) || strings.Contains(string(today), "topsecret") {
		t.Fatalf("today's log = %s", today)
	}
}
