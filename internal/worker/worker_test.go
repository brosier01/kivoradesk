package worker

import (
	"strings"
	"testing"
	"time"
)

func TestResolveStaysInWorkspace(t *testing.T) {
	ok := map[string]string{
		"notes.md":                   "/workspace/notes.md",
		"/workspace/a/../b.txt":      "/workspace/b.txt",
		"/workspace":                 "/workspace",
		"downloads/report (1).pdf":   "/workspace/downloads/report (1).pdf",
		"./downloads/../notes/x.txt": "/workspace/notes/x.txt",
	}
	for in, want := range ok {
		if got, err := resolve(in); err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "/workspace2/x", "/home/agent/.Xauthority"} {
		if got, err := resolve(bad); err == nil {
			t.Errorf("resolve(%q) = %q, want an error", bad, got)
		}
	}
}

func TestCappedKeepsHeadAndTail(t *testing.T) {
	c := &capped{max: 10}
	c.Write([]byte("0123456789"))
	c.Write([]byte("abcdefghij"))
	out, truncated := c.String()
	if !truncated || !strings.HasPrefix(out, "01234") || !strings.HasSuffix(out, "fghij") || !strings.Contains(out, "10 bytes omitted") {
		t.Fatalf("String() = %q, %v", out, truncated)
	}

	small := &capped{max: 10}
	small.Write([]byte("abc"))
	if out, truncated := small.String(); out != "abc" || truncated {
		t.Fatalf("String() = %q, %v; want abc, false", out, truncated)
	}
}

func TestRunKillsTheWholeProcessGroupOnTimeout(t *testing.T) {
	start := time.Now()
	res := runIn(t.TempDir(), "sleep 30 & sleep 30; echo never", 300*time.Millisecond)
	if !res.TimedOut || time.Since(start) > 5*time.Second || strings.Contains(res.Output, "never") {
		t.Fatalf("run = %+v after %v", res, time.Since(start))
	}
}

func TestRunReportsExitCodeAndOutput(t *testing.T) {
	res := runIn(t.TempDir(), "echo hello; echo oops >&2; exit 3", 5*time.Second)
	if res.ExitCode != 3 || res.TimedOut || !strings.Contains(res.Output, "hello") || !strings.Contains(res.Output, "oops") {
		t.Fatalf("run = %+v", res)
	}
}

// runIn runs with the workspace replaced by dir, for tests on any machine.
func runIn(dir, command string, timeout time.Duration) ExecResult {
	return runAt(dir, command, timeout, 1<<10)
}
