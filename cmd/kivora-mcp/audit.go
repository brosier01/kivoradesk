package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// auditLog appends one JSON line per tool call to a file per day
// (audit-YYYY-MM-DD.jsonl), and deletes files older than the retention.
type auditLog struct {
	dir       string
	retention time.Duration

	mu   sync.Mutex
	day  string
	file *os.File
}

func openAudit(dir string, retention time.Duration) (*auditLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &auditLog{dir: dir, retention: retention}
	a.prune(time.Now())
	return a, nil
}

func (a *auditLog) record(trace, tool string, in any, err error, d time.Duration) {
	args, _ := json.Marshal(in)
	entry := map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "trace": trace, "tool": tool,
		"args": truncate(redact(string(args)), 2000), "ok": err == nil, "ms": d.Milliseconds(),
	}
	if err != nil {
		entry["error"] = truncate(redact(err.Error()), 2000)
	}
	line, _ := json.Marshal(entry)

	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().UTC()
	if day := now.Format("2006-01-02"); day != a.day {
		if a.file != nil {
			a.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(a.dir, "audit-"+day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			a.file, a.day = nil, ""
			return
		}
		a.file, a.day = f, day
		a.prune(now)
	}
	a.file.Write(append(line, '\n'))
}

// prune deletes daily files older than the retention.
func (a *auditLog) prune(now time.Time) {
	files, _ := filepath.Glob(filepath.Join(a.dir, "audit-*.jsonl"))
	for _, f := range files {
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "audit-"), ".jsonl"))
		if err == nil && now.Sub(day) > a.retention {
			os.Remove(f)
		}
	}
}

func (a *auditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	return a.file.Close()
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
