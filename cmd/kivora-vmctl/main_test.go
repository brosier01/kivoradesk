package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVM plays QEMU and the guest agent, and records every command in order.
type fakeVM struct {
	t        *testing.T
	mu       sync.Mutex
	commands []string
	qmp, qga string

	jobError   string // reported when the copy ends
	jobRunning bool   // the copy never ends by itself
	addError   string // blockdev-add fails
	freezeFail bool
	freezeHang bool // the freeze gets no answer until later
	leftover   bool // an earlier run left its job in QEMU
	vmStatus   string
	events     chan map[string]any
}

func newFakeVM(t *testing.T) *fakeVM {
	t.Helper()
	dir := t.TempDir()
	v := &fakeVM{t: t, qmp: filepath.Join(dir, "qmp"), qga: filepath.Join(dir, "qga"), events: make(chan map[string]any, 8)}
	return v
}

func (v *fakeVM) record(command string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.commands = append(v.commands, command)
}

func (v *fakeVM) seen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.commands...)
}

func (v *fakeVM) listen(path string, serve func(net.Conn)) {
	l, err := net.Listen("unix", path)
	if err != nil {
		v.t.Fatal(err)
	}
	v.t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
}

func (v *fakeVM) serveQMP(c net.Conn) {
	defer c.Close()
	enc := json.NewEncoder(c)
	dec := json.NewDecoder(c)
	enc.Encode(map[string]any{"QMP": map[string]any{}})
	var mu sync.Mutex
	send := func(m map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(m)
	}
	ok := map[string]any{"return": map[string]any{}}
	for {
		var r struct {
			Execute   string         `json:"execute"`
			Arguments map[string]any `json:"arguments"`
		}
		if dec.Decode(&r) != nil {
			return
		}
		if r.Execute != "qmp_capabilities" {
			v.record("qmp " + r.Execute)
		}
		switch r.Execute {
		case "query-block-jobs":
			if v.leftover {
				v.leftover = false
				send(map[string]any{"return": []map[string]any{{"device": backupJob}}})
			} else {
				send(map[string]any{"return": []map[string]any{}})
			}
		case "query-status":
			status := "running"
			if v.vmStatus != "" {
				status = v.vmStatus
			}
			send(map[string]any{"return": map[string]any{"status": status}})
		case "query-block":
			send(map[string]any{"return": []map[string]any{
				{"device": "sys"},
				{"device": "data", "inserted": map[string]any{"image": map[string]any{"virtual-size": 1 << 34, "actual-size": 1 << 20}}},
			}})
		case "blockdev-add":
			if v.addError != "" {
				send(map[string]any{"error": map[string]any{"desc": v.addError}})
				continue
			}
			send(ok)
		case "blockdev-backup":
			send(ok)
			if !v.jobRunning {
				data := map[string]any{"device": backupJob}
				if v.jobError != "" {
					data["error"] = v.jobError
				}
				// Another device's job must not end the wait.
				send(map[string]any{"event": "BLOCK_JOB_COMPLETED", "data": map[string]any{"device": "other", "error": "not ours"}})
				send(map[string]any{"event": "BLOCK_JOB_COMPLETED", "data": data})
			}
		case "block-job-cancel":
			send(ok)
			send(map[string]any{"event": "BLOCK_JOB_CANCELLED", "data": map[string]any{"device": backupJob}})
		default:
			send(ok)
		}
	}
}

func (v *fakeVM) serveQGA(c net.Conn) {
	defer c.Close()
	enc := json.NewEncoder(c)
	dec := json.NewDecoder(c)
	for {
		var r struct {
			Execute   string `json:"execute"`
			Arguments struct {
				ID int64 `json:"id"`
			} `json:"arguments"`
		}
		if dec.Decode(&r) != nil {
			return
		}
		if r.Execute == "guest-sync" {
			enc.Encode(map[string]any{"return": 12345}) // left by an earlier client
			enc.Encode(map[string]any{"return": r.Arguments.ID})
			continue
		}
		v.record("qga " + r.Execute)
		if r.Execute == "guest-shutdown" {
			continue // no answer when it works
		}
		if r.Execute == "guest-fsfreeze-freeze-list" && v.freezeHang {
			continue
		}
		if r.Execute == "guest-fsfreeze-freeze-list" && v.freezeFail {
			enc.Encode(map[string]any{"error": map[string]any{"desc": "no such mountpoint"}})
			continue
		}
		enc.Encode(map[string]any{"return": 1})
	}
}

func (v *fakeVM) start() {
	v.listen(v.qmp, v.serveQMP)
	v.listen(v.qga, v.serveQGA)
}

func (v *fakeVM) backup(ctx context.Context) error {
	return run(ctx, v.qmp, v.qga, []string{"backup", "data", "/data", "/x/backup.qcow2"})
}

func TestBackupFreezesOnlyWhileTheCopyStarts(t *testing.T) {
	v := newFakeVM(t)
	v.start()
	if err := v.backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"qmp query-block-jobs",
		"qmp blockdev-del",
		"qga guest-fsfreeze-thaw",
		"qmp blockdev-add",
		"qga guest-fsfreeze-freeze-list",
		"qmp blockdev-backup",
		"qga guest-fsfreeze-thaw",
		"qmp blockdev-del",
	}
	waitFor(t, func() bool { return reflect.DeepEqual(v.seen(), want) }, func() string { return strings.Join(v.seen(), "\n") })
}

func TestBackupReportsAFailedCopy(t *testing.T) {
	v := newFakeVM(t)
	v.jobError = "No space left on device"
	v.start()
	err := v.backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "No space left") {
		t.Fatalf("got %v", err)
	}
}

func TestBackupRefusesWhenTheCopyCannotOpen(t *testing.T) {
	v := newFakeVM(t)
	v.addError = "Permission denied"
	v.start()
	err := v.backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("got %v", err)
	}
	for _, c := range v.seen() {
		if c == "qga guest-fsfreeze-freeze-list" {
			t.Error("froze the guest for nothing")
		}
	}
}

func TestBackupGoesOnWithoutTheAgent(t *testing.T) {
	v := newFakeVM(t)
	v.listen(v.qmp, v.serveQMP) // no guest agent socket
	if err := v.backup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBackupGoesOnWhenFreezeFailsAndStillThaws(t *testing.T) {
	v := newFakeVM(t)
	v.freezeFail = true
	v.start()
	if err := v.backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen := v.seen()
	if len(seen) < 2 || seen[len(seen)-3] != "qmp blockdev-backup" || seen[len(seen)-2] != "qga guest-fsfreeze-thaw" {
		t.Errorf("got %q", seen)
	}
}

func TestThawFollowsAFreezeThatTimedOut(t *testing.T) {
	old := freezeTimeout
	freezeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { freezeTimeout = old })
	v := newFakeVM(t)
	v.freezeHang = true
	v.start()
	if err := v.backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen := v.seen()
	if len(seen) < 3 || seen[len(seen)-2] != "qga guest-fsfreeze-thaw" {
		t.Errorf("got %q", seen)
	}
}

func TestBackupClearsWhatAKilledRunLeft(t *testing.T) {
	v := newFakeVM(t)
	v.leftover = true
	v.start()
	if err := v.backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen := v.seen(); seen[1] != "qmp block-job-cancel" {
		t.Errorf("got %q", seen)
	}
}

func TestInterruptedBackupCancelsTheCopy(t *testing.T) {
	v := newFakeVM(t)
	v.jobRunning = true
	v.start()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- v.backup(ctx) }()
	waitFor(t, func() bool {
		for _, c := range v.seen() {
			if c == "qmp blockdev-backup" {
				return true
			}
		}
		return false
	}, func() string { return strings.Join(v.seen(), "\n") })
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted backup does not stop")
	}
	seen := v.seen()
	if seen[len(seen)-2] != "qmp block-job-cancel" || seen[len(seen)-1] != "qmp blockdev-del" {
		t.Errorf("got %q", seen)
	}
}

func TestStatusAndAgent(t *testing.T) {
	v := newFakeVM(t)
	v.start()
	if err := run(context.Background(), v.qmp, "", []string{"status"}); err != nil {
		t.Error(err)
	}
	if err := run(context.Background(), "", v.qga, []string{"agent"}); err != nil {
		t.Error(err)
	}
	if err := run(context.Background(), v.qmp, "", []string{"quit"}); err != nil {
		t.Error(err)
	}
}

func TestShutdownUsesBothWaysThatNeedNoNetwork(t *testing.T) {
	v := newFakeVM(t)
	v.start()
	if err := run(context.Background(), v.qmp, v.qga, []string{"shutdown"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		seen := v.seen()
		return slicesContains(seen, "qga guest-shutdown") && slicesContains(seen, "qmp system_powerdown") && !slicesContains(seen, "qmp cont")
	}, func() string { return strings.Join(v.seen(), "\n") })
}

func TestShutdownResumesAPausedVMFirst(t *testing.T) {
	v := newFakeVM(t)
	v.vmStatus = "io-error"
	v.start()
	if err := run(context.Background(), v.qmp, v.qga, []string{"shutdown"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		seen := v.seen()
		return len(seen) > 1 && seen[0] == "qmp query-status" && seen[1] == "qmp cont"
	}, func() string { return strings.Join(v.seen(), "\n") })
}

func TestShutdownWithOneWayLeft(t *testing.T) {
	onlyQMP := newFakeVM(t)
	onlyQMP.listen(onlyQMP.qmp, onlyQMP.serveQMP)
	if err := run(context.Background(), onlyQMP.qmp, onlyQMP.qga, []string{"shutdown"}); err != nil {
		t.Errorf("without the agent: %v", err)
	}
	onlyAgent := newFakeVM(t)
	onlyAgent.listen(onlyAgent.qga, onlyAgent.serveQGA)
	if err := run(context.Background(), onlyAgent.qmp, onlyAgent.qga, []string{"shutdown"}); err != nil {
		t.Errorf("without QEMU's socket: %v", err)
	}
	none := newFakeVM(t)
	if err := run(context.Background(), none.qmp, none.qga, []string{"shutdown"}); err == nil {
		t.Error("nothing to send it to, and no error")
	}
}

func TestContAndSize(t *testing.T) {
	v := newFakeVM(t)
	v.start()
	if err := run(context.Background(), v.qmp, "", []string{"cont"}); err != nil {
		t.Error(err)
	}
	q, err := dialQMP(v.qmp)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	virtual, actual, err := q.diskSize("data")
	if err != nil || virtual != 1<<34 || actual != 1<<20 {
		t.Errorf("got %d, %d, %v", virtual, actual, err)
	}
	if _, _, err := q.diskSize("sys"); err == nil {
		t.Error("a drive without a disk has a size")
	}
	if _, _, err := q.diskSize("nothing"); err == nil {
		t.Error("an unknown disk has a size")
	}
	if !slicesContains(v.seen(), "qmp cont") {
		t.Errorf("got %q", v.seen())
	}
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestAgentOutputIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qga")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// A compromised VM answers with a string that never ends.
		c.Write([]byte(`{"return":"`))
		chunk := make([]byte, 1<<16)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	}()
	if a, err := dialAgent(path); err == nil {
		a.close()
		t.Fatal("trusted an endless answer")
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nonsense"}, {"backup", "data"}, {"status", "extra"}, {"size"}, {"cont", "x"}, {"shutdown", "x"}} {
		if err := run(context.Background(), "", "", args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

func waitFor(t *testing.T, ok func() bool, show func() string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("got:\n%s", show())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
