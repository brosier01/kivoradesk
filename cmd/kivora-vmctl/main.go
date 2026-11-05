// kivora-vmctl asks QEMU and the VM's guest agent for what the admin commands
// need, over their sockets in the container.
//
//	kivora-vmctl -qmp QMP.sock status
//	kivora-vmctl -qga QGA.sock agent
//	kivora-vmctl -qmp QMP.sock quit
//	kivora-vmctl -qmp QMP.sock -qga QGA.sock backup DEVICE MOUNTPOINT TARGET
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	backupJob  = "kivora-backup"
	backupNode = "kivora-backup"

	qmpTimeout = 30 * time.Second
	// The guest agent is in the VM: what it sends is not trusted, and this is far
	// more than its answers need.
	maxAgentOutput = 1 << 20
	thawAttempts   = 3
)

// Variables for the tests.
var (
	agentTimeout  = 10 * time.Second
	freezeTimeout = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	qmpPath := flag.String("qmp", "", "QEMU's control socket")
	qgaPath := flag.String("qga", "", "the guest agent's socket")
	flag.Parse()
	if err := run(ctx, *qmpPath, *qgaPath, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "kivora-vmctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, qmpPath, qgaPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kivora-vmctl status | agent | quit | backup DEVICE MOUNTPOINT TARGET")
	}
	switch {
	case args[0] == "agent" && len(args) == 1:
		a, err := dialAgent(qgaPath)
		if err != nil {
			return err
		}
		defer a.close()
		return a.call("guest-ping", nil, agentTimeout)
	case args[0] == "status" && len(args) == 1:
		q, err := dialQMP(qmpPath)
		if err != nil {
			return err
		}
		defer q.close()
		var status struct {
			Status string `json:"status"`
		}
		answer, err := q.call("query-status", nil)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(answer, &status); err != nil {
			return err
		}
		fmt.Println(status.Status)
		return nil
	case args[0] == "quit" && len(args) == 1:
		q, err := dialQMP(qmpPath)
		if err != nil {
			return err
		}
		defer q.close()
		_, err = q.call("quit", nil)
		return err
	case args[0] == "backup" && len(args) == 4:
		q, err := dialQMP(qmpPath)
		if err != nil {
			return err
		}
		defer q.close()
		// Without the agent, the copy is what a power cut would leave.
		a, err := dialAgent(qgaPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: the guest agent does not answer: the copy is crash-consistent:", err)
			a = nil
		} else {
			defer a.close()
		}
		return backup(ctx, q, a, args[1], args[2], args[3])
	}
	return errors.New("usage: kivora-vmctl status | agent | quit | backup DEVICE MOUNTPOINT TARGET")
}

// backup copies a disk of the running VM into TARGET, an existing qcow2 file, as
// it was at one moment: QEMU starts the copy while the guest's filesystem is
// frozen, then keeps the VM running while it copies.
func backup(ctx context.Context, q *qmpClient, a *guestAgent, device, mountpoint, target string) error {
	if err := q.clearBackup(); err != nil {
		return err
	}
	if a != nil {
		a.call("guest-fsfreeze-thaw", nil, agentTimeout) // an interrupted run may have left it frozen
	}
	node := map[string]any{
		"driver":        "qcow2",
		"node-name":     backupNode,
		"detect-zeroes": "on", // keeps the copy of an empty disk small
		"file":          map[string]any{"driver": "file", "filename": target},
	}
	if _, err := q.call("blockdev-add", node); err != nil {
		return fmt.Errorf("opening the copy: %w", err)
	}
	defer q.call("blockdev-del", map[string]any{"node-name": backupNode})

	if a != nil {
		if err := a.call("guest-fsfreeze-freeze-list", map[string]any{"mountpoints": []string{mountpoint}}, freezeTimeout); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not freeze", mountpoint+": the copy is crash-consistent:", err)
		}
	}
	_, startErr := q.call("blockdev-backup", map[string]any{
		"job-id": backupJob,
		"device": device,
		"target": backupNode,
		"sync":   "full",
	})
	if a != nil {
		// A disk left frozen stops the VM's programs: insist.
		var err error
		for range thawAttempts {
			if err = a.call("guest-fsfreeze-thaw", nil, agentTimeout); err == nil {
				break
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not thaw", mountpoint+", which may stay frozen until the VM restarts:", err)
		}
	}
	if startErr != nil {
		return fmt.Errorf("starting the copy: %w", startErr)
	}
	return q.waitJob(ctx, backupJob)
}

// qmpClient speaks QEMU's control protocol. Its socket is out of the VM's reach.
type qmpClient struct {
	conn   net.Conn
	dec    *json.Decoder
	events []qmpMessage
}

type qmpMessage struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Desc string `json:"desc"`
	} `json:"error"`
	Event string `json:"event"`
	Data  struct {
		Device string `json:"device"`
		Error  string `json:"error"`
	} `json:"data"`
	QMP json.RawMessage `json:"QMP"`
}

func dialQMP(path string) (*qmpClient, error) {
	conn, err := net.DialTimeout("unix", path, qmpTimeout)
	if err != nil {
		return nil, err
	}
	q := &qmpClient{conn: conn, dec: json.NewDecoder(conn)}
	conn.SetDeadline(time.Now().Add(qmpTimeout))
	var greeting qmpMessage
	if err := q.dec.Decode(&greeting); err != nil {
		conn.Close()
		return nil, fmt.Errorf("QEMU's greeting: %w", err)
	}
	if _, err := q.call("qmp_capabilities", nil); err != nil {
		conn.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmpClient) close() { q.conn.Close() }

// resetDecoder lets the next read work after a timeout, which a Decoder keeps
// returning otherwise.
func (q *qmpClient) resetDecoder() {
	q.dec = json.NewDecoder(io.MultiReader(q.dec.Buffered(), q.conn))
}

func (q *qmpClient) call(command string, args any) (json.RawMessage, error) {
	q.conn.SetDeadline(time.Now().Add(qmpTimeout))
	request := map[string]any{"execute": command}
	if args != nil {
		request["arguments"] = args
	}
	if err := json.NewEncoder(q.conn).Encode(request); err != nil {
		return nil, err
	}
	for {
		var m qmpMessage
		if err := q.dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("%s: %w", command, err)
		}
		switch {
		case m.Event != "":
			q.events = append(q.events, m)
		case m.Error != nil:
			return nil, fmt.Errorf("%s: %s", command, m.Error.Desc)
		case m.Return != nil:
			return m.Return, nil
		}
	}
}

// clearBackup removes what a run that was killed may have left in QEMU.
func (q *qmpClient) clearBackup() error {
	answer, err := q.call("query-block-jobs", nil)
	if err != nil {
		return err
	}
	var jobs []struct {
		Device string `json:"device"`
	}
	if err := json.Unmarshal(answer, &jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Device == backupJob {
			if _, err := q.call("block-job-cancel", map[string]any{"device": backupJob, "force": true}); err != nil {
				return err
			}
			if err := q.waitJob(context.Background(), backupJob); err != nil && !errors.Is(err, errJobCancelled) {
				return err
			}
		}
	}
	q.call("blockdev-del", map[string]any{"node-name": backupNode}) // fails when there is nothing to remove
	return nil
}

var errJobCancelled = errors.New("the copy was cancelled")

// waitJob waits for the end of a block job, and cancels it if ctx ends first.
func (q *qmpClient) waitJob(ctx context.Context, job string) error {
	q.conn.SetDeadline(time.Time{})
	stop := context.AfterFunc(ctx, func() { q.conn.SetReadDeadline(time.Now()) })
	defer stop()
	for {
		m, err := q.nextEvent()
		if err != nil {
			if ctx.Err() == nil {
				return fmt.Errorf("waiting for the copy: %w", err)
			}
			q.resetDecoder()
			if _, cancelErr := q.call("block-job-cancel", map[string]any{"device": job, "force": true}); cancelErr != nil {
				return fmt.Errorf("%w; cancelling the copy: %v", ctx.Err(), cancelErr)
			}
			if err := q.waitJob(context.Background(), job); err != nil && !errors.Is(err, errJobCancelled) {
				return err
			}
			return ctx.Err()
		}
		if m.Data.Device != job {
			continue
		}
		switch m.Event {
		case "BLOCK_JOB_COMPLETED":
			if m.Data.Error != "" {
				return fmt.Errorf("the copy failed: %s", m.Data.Error)
			}
			return nil
		case "BLOCK_JOB_CANCELLED":
			return errJobCancelled
		}
	}
}

func (q *qmpClient) nextEvent() (qmpMessage, error) {
	if len(q.events) > 0 {
		m := q.events[0]
		q.events = q.events[1:]
		return m, nil
	}
	var m qmpMessage
	if err := q.dec.Decode(&m); err != nil {
		return m, err
	}
	return m, nil
}

// guestAgent speaks to the agent in the VM.
type guestAgent struct {
	conn   net.Conn
	input  io.Reader
	dec    *json.Decoder
	failed bool
}

func dialAgent(path string) (*guestAgent, error) {
	conn, err := net.DialTimeout("unix", path, agentTimeout)
	if err != nil {
		return nil, err
	}
	input := io.LimitReader(conn, maxAgentOutput)
	a := &guestAgent{conn: conn, input: input, dec: json.NewDecoder(input)}
	if err := a.sync(); err != nil {
		conn.Close()
		return nil, err
	}
	return a, nil
}

func (a *guestAgent) close() { a.conn.Close() }

// sync skips what is left unread on the channel: the late answer to a command
// that timed out, or to an earlier client.
func (a *guestAgent) sync() error {
	if a.failed {
		// A Decoder keeps returning a timeout otherwise.
		a.dec = json.NewDecoder(io.MultiReader(a.dec.Buffered(), a.input))
		a.failed = false
	}
	id := time.Now().UnixNano() & (1<<31 - 1)
	a.conn.SetDeadline(time.Now().Add(agentTimeout))
	request := map[string]any{"execute": "guest-sync", "arguments": map[string]any{"id": id}}
	if err := json.NewEncoder(a.conn).Encode(request); err != nil {
		return err
	}
	for {
		var m struct {
			Return *int64 `json:"return"`
		}
		if err := a.dec.Decode(&m); err != nil {
			a.failed = true
			return fmt.Errorf("guest agent: %w", err)
		}
		if m.Return != nil && *m.Return == id {
			return nil
		}
	}
}

func (a *guestAgent) call(command string, args any, timeout time.Duration) error {
	if err := a.sync(); err != nil {
		return err
	}
	a.conn.SetDeadline(time.Now().Add(timeout))
	request := map[string]any{"execute": command}
	if args != nil {
		request["arguments"] = args
	}
	if err := json.NewEncoder(a.conn).Encode(request); err != nil {
		return err
	}
	var m struct {
		Error *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err := a.dec.Decode(&m); err != nil {
		a.failed = true
		return fmt.Errorf("%s: %w", command, err)
	}
	if m.Error != nil {
		return fmt.Errorf("%s: %s", command, m.Error.Desc)
	}
	return nil
}
