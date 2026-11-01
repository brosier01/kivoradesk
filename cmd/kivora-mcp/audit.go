package main

import (
	"encoding/json"
	"regexp"
	"time"
)

// auditSink stores one line. In production, it is a helper running as another
// account (internal/audit), which keeps the log out of the agent's reach.
type auditSink interface {
	Append(line []byte) error
}

// auditLog writes one JSON line per tool call.
type auditLog struct {
	sink auditSink
}

// record returns an error if the entry could not be stored.
func (a *auditLog) record(trace, tool string, in any, err error, d time.Duration) error {
	args, _ := json.Marshal(in)
	entry := map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "trace": trace, "tool": tool,
		"args": truncate(redact(string(args)), 2000), "ok": err == nil, "ms": d.Milliseconds(),
	}
	if err != nil {
		entry["error"] = truncate(redact(err.Error()), 2000)
	}
	line, _ := json.Marshal(entry)
	return a.sink.Append(append(line, '\n'))
}

// secretPatterns match credentials that commonly appear in commands and URLs.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), "[REDACTED PRIVATE KEY]"},
	{regexp.MustCompile(`(?i)(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 [REDACTED]"},
	{regexp.MustCompile(`(?i)((?:password|passwd|pwd|passphrase|secret|token|api[_-]?key|access[_-]?key|auth)[\w-]*)(\\?["']?\s*[:=]\s*\\?["']?)[^\s"'&\\,;]+`), "$1$2[REDACTED]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED AWS KEY]"},
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-[A-Za-z0-9_-]{20,}|xox[abpr]-[A-Za-z0-9-]{10,})`), "[REDACTED TOKEN]"},
	{regexp.MustCompile(`(https?://[^/\s:@]+):[^/\s@]+@`), "$1:[REDACTED]@"},
}

// redact masks credentials in s. It is a safety net for the audit log, not a
// guarantee: secrets should go through the secrets tools, never in commands.
func redact(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
