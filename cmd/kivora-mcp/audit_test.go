package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brosier01/kivoradesk/internal/audit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

type failingSink struct{}

func (failingSink) Append([]byte) error { return errors.New("disk full") }

func TestAuditRedactsAndStores(t *testing.T) {
	dir := t.TempDir()
	a := &auditLog{sink: &audit.Log{Dir: dir, Retention: 48 * time.Hour}}
	if err := a.record("abcd1234", "shell_run", map[string]string{"command": "export TOKEN=topsecret"}, nil, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	today, err := os.ReadFile(filepath.Join(dir, "audit-"+time.Now().UTC().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(today), `"tool":"shell_run"`) || strings.Contains(string(today), "topsecret") {
		t.Fatalf("today's log = %s", today)
	}
}

// A call that could not be logged says so, whether it worked or not.
func TestToolResultsReportAuditFailures(t *testing.T) {
	s := &server{audit: &auditLog{sink: failingSink{}}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	add(srv, s, free, &mcp.Tool{Name: "ok", Description: "ok"}, func(context.Context, none) ([]mcp.Content, error) {
		return text("done"), nil
	})
	add(srv, s, free, &mcp.Tool{Name: "bad", Description: "bad"}, func(context.Context, none) ([]mcp.Content, error) {
		return nil, errors.New("failed")
	})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, name := range []string{"ok", "bad"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			b.WriteString(c.(*mcp.TextContent).Text)
		}
		if !strings.Contains(b.String(), "not in the audit log") {
			t.Errorf("%s returned %q", name, b.String())
		}
	}
}
