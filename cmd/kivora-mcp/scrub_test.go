package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestScrubberHidesValuesAndEncodings(t *testing.T) {
	var c scrubber
	if got := c.clean("nothing remembered"); got != "nothing remembered" {
		t.Fatalf("clean = %q", got)
	}
	c.remember("login", `me+"a b"@example.com`)
	for _, in := range []string{
		`form: me+"a b"@example.com.`,
		`https://site.example/?q=me%2B%22a+b%22%40example.com`,
		`{"email": "me+\"a b\"@example.com"}`,
		`<b>me+&#34;a b&#34;@example.com</b>`,
	} {
		if got := c.clean(in); strings.Contains(got, "example.com\"") || strings.Contains(got, "a b") || strings.Contains(got, "a+b") || !strings.Contains(got, "[secret login hidden]") {
			t.Errorf("clean(%q) = %q", in, got)
		}
	}

	c.remember("short", "abc")
	if got := c.clean("abcdef"); got != "abcdef" {
		t.Errorf("a value too short was hidden: %q", got)
	}
}

// Tool results and errors go through the scrubber, whatever the tool.
func TestToolResultsAreScrubbed(t *testing.T) {
	audit, err := openAudit(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{audit: audit}
	s.filled.remember("pw", "hunter2")
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	add(srv, s, free, &mcp.Tool{Name: "ok", Description: "ok"}, func(context.Context, none) ([]mcp.Content, error) {
		return text("page says hunter2"), nil
	})
	add(srv, s, free, &mcp.Tool{Name: "bad", Description: "bad"}, func(context.Context, none) ([]mcp.Content, error) {
		return nil, errors.New("failed on hunter2")
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
		got := res.Content[0].(*mcp.TextContent).Text
		if strings.Contains(got, "hunter2") || !strings.Contains(got, "[secret pw hidden]") {
			t.Errorf("%s returned %q", name, got)
		}
	}
}
