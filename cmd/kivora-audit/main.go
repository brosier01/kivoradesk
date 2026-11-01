// Command kivora-audit writes the audit log of the MCP server. It runs as the
// kivora-audit account, so that the desktop account can add entries but not
// change or delete the ones already there:
//
//	kivora-audit record    one JSON line per entry on stdin, "ok" on stdout for each
//
// Entries arrive redacted. Files are kept 90 days in /var/lib/kivora-audit.
package main

import (
	"bufio"
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
		if err := log.Append(append(r.Bytes(), '\n')); err != nil {
			return err
		}
		if _, err := io.WriteString(out, "ok\n"); err != nil {
			return err
		}
	}
	return r.Err()
}
