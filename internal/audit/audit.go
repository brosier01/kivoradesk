// Package audit keeps the log of the agent's tool calls out of its reach.
//
// The MCP server runs as the desktop account, whose browser the agent drives.
// A helper running as the kivora-audit account (cmd/kivora-audit) owns the
// files: the desktop account can add entries through it, not change or delete
// the ones already there.
package audit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// ackTimeout is how long the helper may take to confirm an entry.
	ackTimeout = 5 * time.Second
	// stopGrace is how long the helper may take to stop once its input closes: it
	// runs under another account, which this one cannot signal.
	stopGrace = 5 * time.Second
)

// Log appends lines to one file per day (audit-YYYY-MM-DD.jsonl) and deletes
// the files older than the retention.
type Log struct {
	Dir       string
	Retention time.Duration

	mu   sync.Mutex
	day  string
	file *os.File
}

// Append adds one line, which must end with a newline.
func (l *Log) Append(line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().UTC()
	if day := now.Format("2006-01-02"); day != l.day {
		if l.file != nil {
			l.file.Close()
			l.file = nil
		}
		if err := os.MkdirAll(l.Dir, 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(l.Dir, "audit-"+day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			l.day = ""
			return err
		}
		l.file, l.day = f, day
		l.prune(now)
	}
	_, err := l.file.Write(line)
	return err
}

func (l *Log) prune(now time.Time) {
	files, _ := filepath.Glob(filepath.Join(l.Dir, "audit-*.jsonl"))
	for _, f := range files {
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "audit-"), ".jsonl"))
		if err == nil && now.Sub(day) > l.Retention {
			os.Remove(f)
		}
	}
}

// Close closes the current file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file, l.day = nil, ""
	return err
}

// Client sends lines to the helper, which confirms each one. It starts the
// helper when needed, and again if it stopped.
type Client struct {
	// Prefix runs the helper as the audit account, for example ["doas", "-u",
	// "kivora-audit", "/usr/local/bin/kivora-audit", "record"].
	Prefix []string

	mu  sync.Mutex
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// Append sends one line and waits for the helper to confirm it.
func (c *Client) Append(line []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.send(line)
	if err != nil {
		c.stop()
		err = c.send(line) // the helper may simply have stopped
	}
	if err != nil {
		c.stop()
	}
	return err
}

func (c *Client) send(line []byte) error {
	if c.cmd == nil {
		if err := c.start(); err != nil {
			return err
		}
	}
	if _, err := c.in.Write(line); err != nil {
		return err
	}
	out := c.out
	ack := make(chan error, 1)
	go func() {
		_, err := out.ReadString('\n')
		ack <- err
	}()
	select {
	case err := <-ack:
		return err
	case <-time.After(ackTimeout):
		return errors.New("the audit helper does not answer")
	}
}

func (c *Client) start() error {
	cmd := exec.Command(c.Prefix[0], c.Prefix[1:]...)
	cmd.WaitDelay = stopGrace
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the audit helper: %w", err)
	}
	c.cmd, c.in, c.out = cmd, in, bufio.NewReader(out)
	return nil
}

func (c *Client) stop() {
	if c.cmd == nil {
		return
	}
	c.in.Close()
	c.cmd.Process.Kill()
	waitBriefly(c.cmd)
	c.cmd, c.in, c.out = nil, nil, nil
}

// waitBriefly leaves a helper behind rather than hold every call of the agent.
func waitBriefly(cmd *exec.Cmd) {
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopGrace):
	}
}

// Close stops the helper.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		c.in.Close()
		waitBriefly(c.cmd)
		c.cmd, c.in, c.out = nil, nil, nil
	}
	return nil
}
