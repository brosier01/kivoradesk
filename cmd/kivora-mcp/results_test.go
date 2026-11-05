package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brosier01/kivoradesk/internal/audit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serve connects a client to a server holding only s.
func serve(t *testing.T, s *server, register func(*mcp.Server)) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	register(srv)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// Results, errors and the log go through the vault's cleaning, whatever the tool,
// for every stored value: nothing needs to have been filled in this session.
func TestResultsAndTheLogAreCleaned(t *testing.T) {
	dir := t.TempDir()
	s := &server{audit: &auditLog{sink: &audit.Log{Dir: dir, Retention: 48 * time.Hour}}, clean: cleanerOf("hunter2-pass")}
	cs := serve(t, s, func(srv *mcp.Server) {
		type in struct {
			Text string `json:"text"`
		}
		add(srv, s, free, &mcp.Tool{Name: "ok", Description: "ok"}, func(context.Context, in) ([]mcp.Content, error) {
			return text("page says hunter2%%2Dpass and hunter2-pass"), nil
		})
		add(srv, s, free, &mcp.Tool{Name: "bad", Description: "bad"}, func(context.Context, in) ([]mcp.Content, error) {
			return nil, errors.New("failed on HUNTER2-PASS")
		})
	})
	for _, name := range []string{"ok", "bad"} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"text": "typed hunter2-pass"}})
		if err != nil {
			t.Fatal(err)
		}
		got := resultText(res)
		if strings.Contains(strings.ToLower(got), "hunter2") || !strings.Contains(got, "[secret hidden]") {
			t.Errorf("%s returned %q", name, got)
		}
	}
	log, err := os.ReadFile(filepath.Join(dir, "audit-"+time.Now().UTC().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(log)), "hunter2") || !strings.Contains(string(log), "[secret hidden]") {
		t.Fatalf("the log holds a value: %s", log)
	}
}

// Without a working vault, nothing that could hold a value goes out.
func TestResultsAreWithheldWithoutTheVault(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	s := &server{audit: &auditLog{sink: &audit.Log{Dir: dir, Retention: 48 * time.Hour}},
		clean: func(context.Context, []string) ([]string, error) {
			calls++
			return nil, errors.New("the vault helper does not answer")
		}}
	cs := serve(t, s, func(srv *mcp.Server) {
		type in struct {
			Text string `json:"text"`
		}
		add(srv, s, free, &mcp.Tool{Name: "ok", Description: "ok"}, func(context.Context, in) ([]mcp.Content, error) {
			return text("page says hunter2-pass"), nil
		})
	})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ok", Arguments: map[string]any{"text": "typed hunter2-pass"}})
	if err != nil {
		t.Fatal(err)
	}
	got := resultText(res)
	if !res.IsError || strings.Contains(got, "hunter2") || !strings.Contains(got, "withheld") {
		t.Fatalf("result = %q, error %v", got, res.IsError)
	}
	if calls != 1 {
		t.Fatalf("%d calls to the vault for one result", calls)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "audit-"+time.Now().UTC().Format("2006-01-02")+".jsonl"))
	if strings.Contains(string(log), "hunter2") || !strings.Contains(string(log), "withheld") {
		t.Fatalf("log = %s", log)
	}
}

// One call to the vault for all there is to clean in a result.
func TestOneVaultCallPerResult(t *testing.T) {
	var inputs [][]string
	s := &server{audit: &auditLog{sink: &audit.Log{Dir: t.TempDir(), Retention: 48 * time.Hour}},
		clean: func(_ context.Context, texts []string) ([]string, error) {
			inputs = append(inputs, texts)
			return texts, nil
		}}
	cs := serve(t, s, func(srv *mcp.Server) {
		add(srv, s, free, &mcp.Tool{Name: "two", Description: "two"}, func(context.Context, none) ([]mcp.Content, error) {
			return append(text("first"), text("second")...), nil
		})
	})
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "two"}); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || len(inputs[0]) != 4 {
		t.Fatalf("texts sent to the vault: %q", inputs)
	}
}

// A long argument is not kept whole, and a value cut at the end of what is cleaned
// is past what the log keeps.
func TestAuditArgsAreBounded(t *testing.T) {
	got := auditArgs(map[string]string{"content": strings.Repeat("x", 3*maxAuditInput)})
	if len(got) != maxAuditInput {
		t.Fatalf("%d bytes", len(got))
	}
	if !json.Valid([]byte(auditArgs(map[string]string{"text": "ok"}))) {
		t.Fatal("short arguments are not left whole")
	}
}

// A tool that stays stuck past its limit does not hold its answer: its helpers run
// under accounts that cannot be killed from here.
func TestBoundedAnswersWhenAToolDoesNotStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	_, err := bounded(ctx, func() ([]mcp.Content, error) {
		<-release
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("bounded = %v", err)
	}
	if d := time.Since(start); d > stopGrace+2*time.Second {
		t.Fatalf("took %v", d)
	}
}
