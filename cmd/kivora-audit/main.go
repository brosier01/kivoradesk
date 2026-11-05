// Command kivora-audit writes the audit log of the MCP server. It runs as the
// kivora-audit account, so that the desktop account can add entries but not
// change or delete the ones already there:
//
//	kivora-audit record    one JSON object per line on stdin, "ok" on stdout for each
//
// Entries arrive redacted. Files are kept 90 days in /var/lib/kivora-audit. The
// account that sends them decides what they say, not when they were recorded,
// nor the bytes a human later reads in a terminal.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/brosier01/kivoradesk/internal/audit"
)

const (
	logDir    = "/var/lib/kivora-audit"
	retention = 90 * 24 * time.Hour
	maxLine   = 1 << 20
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "record" {
		fmt.Fprintln(os.Stderr, "usage: kivora-audit record")
		os.Exit(2)
	}
	if err := record(&audit.Log{Dir: logDir, Retention: retention}, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "kivora-audit:", err)
		os.Exit(1)
	}
}

// record stops at the first failure: the client then reports it to the agent's
// side, instead of believing the entry was written.
func record(log *audit.Log, in io.Reader, out io.Writer) error {
	defer log.Close()
	r := bufio.NewScanner(in)
	r.Buffer(make([]byte, 64<<10), maxLine)
	for r.Scan() {
		line, err := entry(r.Bytes(), time.Now().UTC())
		if err != nil {
			return err
		}
		if err := log.Append(line); err != nil {
			return err
		}
		if _, err := io.WriteString(out, "ok\n"); err != nil {
			return err
		}
	}
	return r.Err()
}

// entry checks one line and returns what to store: the same object on one line,
// with the time this account received it first. In valid JSON, control characters
// are escaped, so a line cannot hold terminal sequences.
func entry(line []byte, now time.Time) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, line); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	object := compact.Bytes()
	if len(object) == 0 || object[0] != '{' {
		return nil, errors.New("not a JSON object")
	}
	out := fmt.Appendf(nil, `{"recorded":%q`, now.Format(time.RFC3339Nano))
	if len(object) > 2 {
		out = append(out, ',')
	}
	out = append(out, object[1:]...)
	return append(out, '\n'), nil
}
