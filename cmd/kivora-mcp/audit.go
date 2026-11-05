package main

import (
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"sync"
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

// auditEntry is one tool call. Its args and err are already cleaned of the stored
// credentials.
type auditEntry struct {
	trace, tool string
	args        string // as JSON
	err         string // empty if the call worked
	notes       map[string]any
	took        time.Duration
}

// withheld stands for the arguments of a call that could not be cleaned.
const withheld = `"[withheld: could not be checked for stored credentials]"`

// maxAuditInput is what is cleaned of a call's arguments: far more than the log
// keeps, so that a value cut at the end of it is not kept.
const maxAuditInput = 64 << 10

// auditArgs returns the arguments of a call as JSON, up to maxAuditInput.
func auditArgs(in any) string {
	args, _ := json.Marshal(in)
	return cutText(string(args), maxAuditInput)
}

// record returns an error if the entry could not be stored.
func (a *auditLog) record(e auditEntry) error {
	entry := map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "trace": e.trace, "tool": e.tool,
		"args": truncate(redact(e.args), 2000), "ok": e.err == "", "ms": e.took.Milliseconds(),
	}
	for k, v := range e.notes {
		entry[k] = v
	}
	if e.err != "" {
		entry["error"] = truncate(redact(e.err), 2000)
	}
	line, _ := json.Marshal(entry)
	return a.sink.Append(append(line, '\n'))
}

type notesKey struct{}

// auditNotes is what a tool adds to its entry in the log.
type auditNotes struct {
	mu     sync.Mutex
	fields map[string]any
}

// noteAudit adds a field to the audit entry of the current call.
func noteAudit(ctx context.Context, key string, value any) {
	if n, ok := ctx.Value(notesKey{}).(*auditNotes); ok {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.fields == nil {
			n.fields = make(map[string]any)
		}
		n.fields[key] = value
	}
}

func (n *auditNotes) all() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return maps.Clone(n.fields)
}

// A value after an option or a key: in quotes, which JSON may have escaped, or up
// to the next separator.
const secretValue = `(?:\\"[^"]*\\"|"[^"]*"|'[^']*'|[^\s"'&\\,;]+)`

const secretName = `(?:password|passwd|pwd|passphrase|secret|token|api[_-]?key|access[_-]?key|auth)[\w-]*`

// secretPatterns match credentials that commonly appear in commands and URLs.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), "[REDACTED PRIVATE KEY]"},
	// Headers, whatever the scheme: Authorization: Token x, Cookie: a=b; c=d
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|cookie|set-cookie|x-api-key|x-auth-token)(\\?["']?\s*:\s*)[^"'\\\n]+`), "$1$2[REDACTED]"},
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 [REDACTED]"},
	{regexp.MustCompile(`(?i)(` + secretName + `)(\\?["']?\s*[:=]\s*)` + secretValue), "$1$2[REDACTED]"},
	// Options followed by their value: --password x, --token x
	{regexp.MustCompile(`(?i)(--?` + secretName + `)(\s+)` + secretValue), "$1$2[REDACTED]"},
	// Options named differently by a few well-known commands.
	{regexp.MustCompile(`\b((?:mysql|mysqldump|mysqladmin|mariadb|sshpass)\b[^|;&\n]*?\s-p\s*)[^\s"'\\]+`), "$1[REDACTED]"},
	{regexp.MustCompile(`\b(openssl\b[^|;&\n]*?\s-k\s+)[^\s"'\\]+`), "$1[REDACTED]"},
	{regexp.MustCompile(`(-pass\s+pass:)[^\s"'\\]+`), "$1[REDACTED]"},
	{regexp.MustCompile(`\b(echo\s+)\S+(\s*\|\s*sudo\s+-S)`), "$1[REDACTED]$2"},
	// user:password as an option, as in curl -u or wget --user.
	{regexp.MustCompile(`(\s(?:-u|--user)[\s=]*[^\s:"'\\]+:)[^\s"'\\]+`), "$1[REDACTED]"},
	// One-time codes and sessions in a URL.
	{regexp.MustCompile(`(?i)([?&](?:code|otp|pin|sid|session|sessionid)=)[^&\s"'\\]+`), "$1[REDACTED]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), "[REDACTED JWT]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED AWS KEY]"},
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-[A-Za-z0-9_-]{20,}|xox[abpr]-[A-Za-z0-9-]{10,})`), "[REDACTED TOKEN]"},
	// user:password@ in the URL of any scheme.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]*):[^/\s@]+@`), "$1:[REDACTED]@"},
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
