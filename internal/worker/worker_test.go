package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMain lets this test binary stand for kivora-mcp: it runs the helper when
// asked to, as the commands below ask it.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "shmget" {
		// A shared memory segment that nothing holds, left behind on purpose.
		if _, err := unix.SysvShmGet(unix.IPC_PRIVATE, 4096, unix.IPC_CREAT|0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		if root := os.Getenv("WORKER_TEST_ROOT"); root != "" {
			Root = root
		}
		os.Exit(Main(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// helperRunner runs the real helper on root, under this account.
func helperRunner(t *testing.T, root string) *Runner {
	t.Helper()
	t.Setenv("WORKER_TEST_ROOT", root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Prefix: []string{self, "worker"}}
}

// skipWithoutIsolation skips where the system refuses unprivileged namespaces or
// what they allow to tests, as Ubuntu does by default. The VM has them: kivora
// doctor and the integration tests check them there.
func skipWithoutIsolation(t *testing.T, res ExecResult) {
	t.Helper()
	if res.ExitCode == -1 || strings.Contains(res.Output, "isolate the command") {
		t.Skipf("namespaces are not available here: %s", res.Output)
	}
}

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
	return runAt(dir, command, timeout, 1<<10, false, nil)
}

func TestIsolatedCommandLeavesNothingBehind(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "alive")
	res := runAt(t.TempDir(), "(setsid sh -c 'sleep 2; echo late >"+marker+"' >/dev/null 2>&1 &) ; id -u; echo done", 5*time.Second, 1<<10, true, nil)
	skipWithoutIsolation(t, res)
	if res.ExitCode != 0 || !strings.Contains(res.Output, "done") || !strings.Contains(res.Output, strconv.Itoa(os.Getuid())) {
		t.Fatalf("run = %+v", res)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a detached process outlived the command")
	}
}

func TestIsolatedCommandLeavesNoMemoryOrFiles(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("kivora-test-%d", os.Getpid())
	first := self + " shmget && echo left >/dev/shm/" + name + " && echo left >/tmp/" + name + " && echo left >/var/tmp/" + name + " && grep -c . /proc/sysvipc/shm"
	res := runAt(t.TempDir(), first, 5*time.Second, 1<<10, true, nil)
	skipWithoutIsolation(t, res)
	if res.ExitCode != 0 || !strings.Contains(res.Output, "2") {
		t.Fatalf("first call = %+v", res)
	}
	second := "grep -c . /proc/sysvipc/shm; ls /dev/shm/" + name + " /tmp/" + name + " /var/tmp/" + name + " 2>/dev/null | wc -l"
	res = runAt(t.TempDir(), second, 5*time.Second, 1<<10, true, nil)
	if lines := strings.Fields(res.Output); len(lines) != 2 || lines[0] != "1" || lines[1] != "0" {
		t.Fatalf("the second call found what the first one left: %+v", res)
	}
	for _, dir := range []string{"/dev/shm", "/tmp", "/var/tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("a file reached the real %s", dir)
		}
	}
}

func TestIsolatedCommandHasNothingLeftToGain(t *testing.T) {
	res := runAt(t.TempDir(), "grep -E '^(CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs)' /proc/self/status; unshare -Ur true && echo USERNS; unshare -n true && echo NETNS", 5*time.Second, 1<<12, true, nil)
	skipWithoutIsolation(t, res)
	for _, line := range []string{"CapPrm:\t0000000000000000", "CapEff:\t0000000000000000", "CapBnd:\t0000000000000000", "CapAmb:\t0000000000000000", "NoNewPrivs:\t1"} {
		if !strings.Contains(res.Output, strings.ReplaceAll(line, "\\t", "\t")) {
			t.Errorf("no %q in %s", line, res.Output)
		}
	}
	for _, escaped := range []string{"USERNS", "NETNS"} {
		if strings.Contains(res.Output, escaped+"\n") {
			t.Errorf("the command created a namespace: %s", res.Output)
		}
	}
}

// The signal is what stops the command when the helper dies, and the namespaces
// are what the tests above depend on.
func TestIsolateAsksForAllItNeeds(t *testing.T) {
	var attr syscall.SysProcAttr
	isolate(&attr)
	defer runtimeUnlock()
	want := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWNS)
	if attr.Cloneflags != want {
		t.Errorf("Cloneflags = %#x, want %#x", attr.Cloneflags, want)
	}
	if attr.Pdeathsig != syscall.SIGKILL {
		t.Errorf("Pdeathsig = %v", attr.Pdeathsig)
	}
	if attr.UidMappings[0].ContainerID != os.Getuid() {
		t.Errorf("the account changes in the namespace: %+v", attr.UidMappings)
	}
}

func TestStopEndsTheCommand(t *testing.T) {
	stop := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(stop) })
	marker := filepath.Join(t.TempDir(), "late")
	start := time.Now()
	res := runAt(t.TempDir(), "sleep 2; echo late >"+marker, 30*time.Second, 1<<10, false, stop)
	if res.TimedOut || time.Since(start) > time.Second {
		t.Fatalf("run = %+v after %v", res, time.Since(start))
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command went on after the stop")
	}
}

func TestRunnerStopsAWriteAndLeavesNothing(t *testing.T) {
	root := t.TempDir()
	r := helperRunner(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// More data announced than sent: the helper waits for it, like one that is
	// stuck, until its input closes.
	_, err := r.helper(ctx, time.Minute, []byte("abc"), "write", "-size", "10", "--", "partial.txt")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "partial.txt")); err == nil {
		t.Fatal("a file was written from partial data")
	}
}

func TestFilesWaitForNothing(t *testing.T) {
	root := t.TempDir()
	r := helperRunner(t, root)
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/ptmx", filepath.Join(root, "device")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Now()
	for _, name := range []string{"fifo", "device"} {
		if _, _, err := r.ReadFile(ctx, name, 100); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("read %s: %v", name, err)
		}
	}
	for _, name := range []string{"fifo", "device"} {
		err := r.WriteFile(ctx, name, []byte("x"))
		if err == nil {
			t.Errorf("write %s succeeded", name)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestFilesRoundTrip(t *testing.T) {
	root := t.TempDir()
	r := helperRunner(t, root)
	ctx := context.Background()
	if err := r.WriteFile(ctx, "sub/notes.txt", []byte("first version")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(ctx, "sub/notes.txt", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(ctx, "empty.txt", nil); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := r.ReadFile(ctx, "sub/notes.txt", 100)
	if err != nil || truncated || string(got) != "second" {
		t.Fatalf("ReadFile = %q, %v, %v", got, truncated, err)
	}
	got, truncated, err = r.ReadFile(ctx, "sub/notes.txt", 3)
	if err != nil || !truncated || string(got) != "sec" {
		t.Fatalf("ReadFile, short = %q, %v, %v", got, truncated, err)
	}
	if _, _, err := r.ReadFile(ctx, "sub", 100); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("ReadFile on a directory: %v", err)
	}
	entries, err := r.List(ctx, "")
	if err != nil || len(entries) != 2 || entries[0].Name != "empty.txt" || !entries[1].Dir {
		t.Fatalf("List = %+v, %v", entries, err)
	}
	if err := r.Delete(ctx, "empty.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRefusesWhatItCannotHold(t *testing.T) {
	if err := (&Runner{}).WriteFile(context.Background(), "big", make([]byte, MaxWrite+1)); err == nil {
		t.Fatal("WriteFile accepted a file over the limit")
	}
	if _, err := readInput(strings.NewReader(strings.Repeat("x", 10)), MaxWrite+1); err == nil {
		t.Fatal("readInput accepted a size over the limit")
	}
	if _, err := readInput(strings.NewReader("abc"), 10); err == nil {
		t.Fatal("readInput accepted an input that ends early")
	}
	if data, err := readInput(strings.NewReader("abc"), -1); err != nil || string(data) != "abc" {
		t.Fatalf("readInput to the end = %q, %v", data, err)
	}
}

func TestListStopsAtItsLimit(t *testing.T) {
	root := t.TempDir()
	for i := range 7 {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d", i)), nil, 0o600)
	}
	entries, err := listDir(root, 5)
	if err != nil || len(entries) != 6 {
		t.Fatalf("listDir = %+v, %v", entries, err)
	}
	last := entries[5]
	if !last.Truncated || !strings.Contains(last.Name, "2 more") || entries[4].Name != "f4" || entries[4].Truncated {
		t.Fatalf("entries = %+v", entries)
	}
	if entries, _ := listDir(root, 7); len(entries) != 7 || entries[6].Truncated {
		t.Fatalf("at the limit: %+v", entries)
	}
}

func TestValidJSON(t *testing.T) {
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "good"), filepath.Join(dir, "bad")
	os.WriteFile(good, []byte(`{"a":[1]}`), 0o600)
	os.WriteFile(bad, []byte(`{"a":`), 0o600)
	if err := mainValidJSON([]string{good}); err != nil {
		t.Error(err)
	}
	if err := mainValidJSON([]string{bad}); err == nil {
		t.Error("broken JSON accepted")
	}
	if err := mainValidJSON([]string{filepath.Join(dir, "absent")}); err == nil {
		t.Error("absent file accepted")
	}
}

func runtimeUnlock() { runtime.UnlockOSThread() }
