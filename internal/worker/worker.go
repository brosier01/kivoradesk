// Package worker runs the agent's shell commands and file operations under a
// separate, unprivileged account.
//
// The worker account has no access to the display, the browser's DevTools
// endpoint or the control API, so code the agent runs cannot drive the
// desktop behind the input lock. The desktop account invokes this package
// through `doas -u worker kivora-mcp worker ...`; the helper side (Main) runs
// as the worker.
//
// That account cannot signal the helper: to stop it, the desktop account closes
// its standard input, and the helper ends itself and what it started.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Root is the only directory the file operations accept. A variable for the tests.
var Root = "/workspace"

// shellProxy is the only way out of the VM for shell commands, to the sites the
// human allowed (cmd/kivora-proxy).
const shellProxy = "http://127.0.0.1:3128"

const (
	// MaxWrite is the largest file WriteFile accepts.
	MaxWrite = 64 << 20
	// maxListed bounds a listing: its result goes into the agent's context.
	maxListed = 5000
	// stopGrace is how long a helper that was told to stop may take to do so.
	stopGrace = 5 * time.Second
	// cancelOnEOF makes the helper stop when its standard input closes.
	cancelOnEOF = "-cancel-on-eof"
)

// ExecResult is the outcome of a shell command.
type ExecResult struct {
	ExitCode  int    `json:"exit_code"`
	TimedOut  bool   `json:"timed_out"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

// Entry is one item of a directory listing.
type Entry struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	// Truncated marks a last entry that stands for the ones not listed.
	Truncated bool `json:"truncated,omitempty"`
}

// Runner is the desktop account's side: it starts the helper as the worker.
type Runner struct {
	// Prefix runs the helper as the worker, for example
	// ["doas", "-u", "worker", "/usr/local/bin/kivora-mcp", "worker"].
	Prefix []string
}

// Exec runs command with /bin/sh in the workspace.
func (r *Runner) Exec(ctx context.Context, command string, timeout time.Duration, maxOutput int) (ExecResult, error) {
	var res ExecResult
	out, err := r.helper(ctx, timeout+10*time.Second, nil, "exec",
		"-timeout", timeout.String(), "-max-output", fmt.Sprint(maxOutput), "--", command)
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(out, &res)
}

// ReadFile returns at most max bytes of a workspace file and whether the
// file was longer.
func (r *Runner) ReadFile(ctx context.Context, path string, max int) ([]byte, bool, error) {
	out, err := r.helper(ctx, time.Minute, nil, "read", "-max", fmt.Sprint(max+1), "--", path)
	if err != nil {
		return nil, false, err
	}
	if len(out) > max {
		return out[:max], true, nil
	}
	return out, false, nil
}

// WriteFile creates or replaces a workspace file, creating parent
// directories.
func (r *Runner) WriteFile(ctx context.Context, path string, data []byte) error {
	if len(data) > MaxWrite {
		return fmt.Errorf("%d bytes is more than the %d a file written this way can hold", len(data), MaxWrite)
	}
	_, err := r.helper(ctx, time.Minute, data, "write", "-size", fmt.Sprint(len(data)), "--", path)
	return err
}

// List lists a workspace directory.
func (r *Runner) List(ctx context.Context, path string) ([]Entry, error) {
	out, err := r.helper(ctx, time.Minute, nil, "list", "--", path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	return entries, json.Unmarshal(out, &entries)
}

// Delete removes a workspace file or empty directory.
func (r *Runner) Delete(ctx context.Context, path string) error {
	_, err := r.helper(ctx, time.Minute, nil, "delete", "--", path)
	return err
}

// helper runs one command of the helper. Its input stays open during the call:
// closing it is the only way to stop a process of another account. A helper that
// still does not stop is left behind, rather than holding the call.
func (r *Runner) helper(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(append(append([]string{}, r.Prefix...), cancelOnEOF), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.WaitDelay = stopGrace
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("worker helper: %w", err)
	}
	if len(stdin) > 0 {
		go in.Write(stdin)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		in.Close()
		select {
		case <-done:
			return nil, fmt.Errorf("worker helper: %w", ctx.Err())
		case <-time.After(stopGrace):
			return nil, fmt.Errorf("worker helper: %w, and it does not stop", ctx.Err())
		}
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("worker helper: %w", err)
	}
	return stdout.Bytes(), nil
}

// Main is the helper's entry point, running as the worker account. It
// prints results on stdout and errors on stderr.
func Main(args []string) int {
	// The desktop account cannot signal this process: it ends with its parent.
	unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0)
	cancelled := make(chan struct{})
	watch := false
	if len(args) > 0 && args[0] == cancelOnEOF {
		watch = true
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: worker exec|read|write|list|delete|valid-json ...")
		return 2
	}
	// The data to write comes first on the input, and then the same rule applies.
	if watch && args[0] != "write" {
		watchInput(cancelled)
		if args[0] != "exec" {
			stopWhen(cancelled)
		}
	}
	var err error
	switch args[0] {
	case "exec":
		err = mainExec(args[1:], cancelled)
	case "init":
		err = mainInit(args[1:])
	case "read":
		err = mainRead(args[1:])
	case "write":
		err = mainWrite(args[1:], watch, cancelled)
	case "list":
		err = mainList(args[1:])
	case "delete":
		err = mainDelete(args[1:])
	case "valid-json":
		err = mainValidJSON(args[1:])
	default:
		err = fmt.Errorf("unknown worker command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// watchInput closes cancelled when the standard input ends.
func watchInput(cancelled chan struct{}) {
	go func() {
		io.Copy(io.Discard, os.Stdin)
		close(cancelled)
	}()
}

// stopWhen ends the helper, whatever it is doing, when cancelled closes.
func stopWhen(cancelled <-chan struct{}) {
	go func() {
		<-cancelled
		fmt.Fprintln(os.Stderr, "cancelled")
		os.Exit(1)
	}()
}

func mainExec(args []string, cancelled <-chan struct{}) error {
	fl := flag.NewFlagSet("exec", flag.ContinueOnError)
	timeout := fl.Duration("timeout", time.Minute, "kill the command after this long")
	maxOutput := fl.Int("max-output", 32<<10, "keep at most this many bytes of output")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 1 {
		return errors.New("exec takes exactly one command string")
	}
	if err := applyLimits(); err != nil {
		return fmt.Errorf("limit the command: %w", err)
	}
	res := runAt(Root, fl.Arg(0), *timeout, *maxOutput, true, cancelled)
	return json.NewEncoder(os.Stdout).Encode(res)
}

// applyLimits bounds what a command and its children can take from the desktop
// sharing this VM. The memory limit is per process: the out-of-memory killer
// also picks them first, before the browser or sshd.
func applyLimits() error {
	for _, l := range []struct {
		resource int
		max      uint64
	}{
		{unix.RLIMIT_NPROC, 512},
		{unix.RLIMIT_AS, 3 << 30},
		{unix.RLIMIT_FSIZE, 8 << 30},
		{unix.RLIMIT_CORE, 0},
	} {
		if err := unix.Setrlimit(l.resource, &unix.Rlimit{Cur: l.max, Max: l.max}); err != nil {
			return err
		}
	}
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, 10); err != nil {
		return err
	}
	return os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0)
}

// isolate starts the command in namespaces of its own. The PID namespace kills
// everything left in it when the shell ends, whatever a process did to detach
// itself: nothing the agent runs outlives its call. With the IPC and mount
// namespaces, neither do shared memory segments nor files in /tmp, /var/tmp and
// /dev/shm. The user namespace is what allows an unprivileged account to create
// them; the account stays the same. mainInit runs inside first, and takes back the
// capabilities it needed.
func isolate(attr *syscall.SysProcAttr) {
	uid, gid := os.Getuid(), os.Getgid()
	attr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWNS
	attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
	attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
	attr.GidMappingsEnableSetgroups = false
	attr.AmbientCaps = []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_RESOURCE, unix.CAP_SETPCAP}
	// If the helper is killed, the shell goes too, and with it the namespaces.
	// The signal is tied to the thread that starts the command.
	attr.Pdeathsig = syscall.SIGKILL
	runtime.LockOSThread()
}

// scratchDirs are the directories a command shares with the other accounts. In
// its namespace they are empty, bounded, and gone with the call.
var scratchDirs = []struct{ path, size string }{
	{"/tmp", "512m"},
	{"/var/tmp", "128m"},
	{"/dev/shm", "256m"},
}

// mainInit runs as the first process of the namespaces of isolate, which it
// prepares before becoming the shell. If anything fails, the command does not run.
func mainInit(args []string) error {
	if len(args) != 2 || args[0] != "--" {
		return errors.New("usage: worker init -- COMMAND")
	}
	// Capabilities and the parent's death signal belong to a thread: the one that
	// becomes the shell.
	runtime.LockOSThread()
	unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0)
	if err := prepareNamespace(); err != nil {
		return fmt.Errorf("isolate the command: %w", err)
	}
	return syscall.Exec("/bin/sh", []string{"/bin/sh", "-c", args[1]}, os.Environ())
}

func prepareNamespace() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("private mounts: %w", err)
	}
	for _, d := range scratchDirs {
		options := "mode=1777,size=" + d.size
		if err := unix.Mount("tmpfs", d.path, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, options); err != nil {
			return fmt.Errorf("mount %s: %w", d.path, err)
		}
	}
	// Each new user namespace would give the command capabilities again, which
	// reach parts of the kernel it has no use for.
	if err := os.WriteFile("/proc/sys/user/max_user_namespaces", []byte("0"), 0); err != nil {
		return fmt.Errorf("user namespaces: %w", err)
	}
	return dropCapabilities()
}

// dropCapabilities leaves the thread with nothing to gain, even through exec.
func dropCapabilities() error {
	for c := uintptr(0); c <= unix.CAP_LAST_CAP; c++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, c, 0, 0, 0); err != nil {
			return fmt.Errorf("bounding set: %w", err)
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("ambient set: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no new privileges: %w", err)
	}
	var none [2]unix.CapUserData
	if err := unix.Capset(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &none[0]); err != nil {
		return fmt.Errorf("capabilities: %w", err)
	}
	return nil
}

// runAt executes command in dir, in its own process group, so that a timeout
// kills everything it started, not just the shell.
func runAt(dir, command string, timeout time.Duration, maxOutput int, isolated bool, stop <-chan struct{}) ExecResult {
	res := ExecResult{ExitCode: -1}
	home, _ := os.UserHomeDir()
	cmd := exec.Command("/bin/sh", "-c", command)
	if isolated {
		self, err := os.Executable()
		if err != nil {
			res.Output = err.Error()
			return res
		}
		cmd = exec.Command(self, "worker", "init", "--", command)
	}
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
		"LANG=C.UTF-8", "TERM=dumb", "SHELL=/bin/sh", "USER=" + os.Getenv("USER"),
		// The firewall leaves the worker no other way out.
		"https_proxy=" + shellProxy, "HTTPS_PROXY=" + shellProxy, "http_proxy=" + shellProxy,
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if isolated {
		isolate(cmd.SysProcAttr)
	}
	buf := &capped{max: maxOutput}
	cmd.Stdout, cmd.Stderr = buf, buf

	if err := cmd.Start(); err != nil {
		res.Output = err.Error()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-stop:
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	case <-time.After(timeout):
		res.TimedOut = true
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	res.Output, res.Truncated = buf.String()
	return res
}

// capped keeps the head and the tail of the output, dropping the middle.
type capped struct {
	max     int
	head    []byte
	tail    []byte
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	half := c.max / 2
	if room := half - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	c.tail = append(c.tail, p...)
	if extra := len(c.tail) - (c.max - half); extra > 0 {
		c.tail = c.tail[extra:]
		c.dropped += extra
	}
	return n, nil
}

// String gives the output. Where the middle was dropped, the note has the form of
// a cut mark of the secrets package: the cleaning of the result hides a stored
// value that the cut leaves in part on either side of it.
func (c *capped) String() (string, bool) {
	if c.dropped == 0 {
		return string(c.head) + string(c.tail), false
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", c.head, c.dropped, c.tail), true
}

// resolve maps a user path to an absolute path inside Root. Relative paths
// are taken from Root. The worker's own permissions are the real boundary;
// this keeps the tools focused on the workspace.
func resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(Root, p)
	}
	p = filepath.Clean(p)
	if p != Root && !strings.HasPrefix(p, Root+"/") {
		return "", fmt.Errorf("%s is outside %s", p, Root)
	}
	return p, nil
}

func onePath(name string, args []string, fl *flag.FlagSet) (string, error) {
	if err := fl.Parse(args); err != nil {
		return "", err
	}
	if fl.NArg() != 1 {
		return "", fmt.Errorf("%s takes exactly one path", name)
	}
	return resolve(fl.Arg(0))
}

// openRegular opens a file without waiting, and refuses what is not a regular
// file. A command can leave a FIFO or a link to a device under any name, and
// opening or reading those would never end.
func openRegular(p string, flags int) (*os.File, error) {
	f, err := os.OpenFile(p, flags|syscall.O_NONBLOCK, 0o660)
	if errors.Is(err, syscall.ENXIO) { // a FIFO nobody reads
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%s is a directory", p)
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	return f, nil
}

func mainRead(args []string) error {
	fl := flag.NewFlagSet("read", flag.ContinueOnError)
	max := fl.Int64("max", 1<<20, "read at most this many bytes")
	p, err := onePath("read", args, fl)
	if err != nil {
		return err
	}
	f, err := openRegular(p, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(os.Stdout, io.LimitReader(f, *max))
	return err
}

// mainWrite reads the whole file before touching anything: an input that ends
// early, as when the caller gives up, writes nothing.
func mainWrite(args []string, watch bool, cancelled chan struct{}) error {
	fl := flag.NewFlagSet("write", flag.ContinueOnError)
	size := fl.Int64("size", -1, "bytes to read from the standard input; -1 to read it to its end")
	p, err := onePath("write", args, fl)
	if err != nil {
		return err
	}
	if p == Root {
		return errors.New("cannot write the workspace itself")
	}
	data, err := readInput(os.Stdin, *size)
	if err != nil {
		return err
	}
	if watch {
		watchInput(cancelled)
		stopWhen(cancelled)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o770); err != nil {
		return err
	}
	f, err := openRegular(p, os.O_WRONLY|os.O_CREATE)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}

func readInput(r io.Reader, size int64) ([]byte, error) {
	if size > MaxWrite {
		return nil, fmt.Errorf("%d bytes is more than the %d a file written this way can hold", size, MaxWrite)
	}
	if size >= 0 {
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("the file was not sent in full: %w", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxWrite+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxWrite {
		return nil, fmt.Errorf("more than the %d bytes a file written this way can hold", MaxWrite)
	}
	return data, nil
}

func mainList(args []string) error {
	p, err := onePath("list", args, flag.NewFlagSet("list", flag.ContinueOnError))
	if err != nil {
		return err
	}
	entries, err := listDir(p, maxListed)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(entries)
}

// listDir returns the first max entries of a directory in name order, and a last
// one that tells how many it left out.
func listDir(p string, max int) ([]Entry, error) {
	dirents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	entries := []Entry{}
	for i, d := range dirents {
		if i == max {
			entries = append(entries, Entry{Name: fmt.Sprintf("[%d more entries not listed]", len(dirents)-max), Truncated: true})
			break
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		entries = append(entries, Entry{Name: d.Name(), Dir: d.IsDir(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	return entries, nil
}

// mainValidJSON checks a file for kivora doctor: Chromium ignores a policy file
// it cannot read, and the VM has no other JSON parser.
func mainValidJSON(args []string) error {
	if len(args) != 1 {
		return errors.New("valid-json takes exactly one path")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("%s is not valid JSON", args[0])
	}
	return nil
}

func mainDelete(args []string) error {
	p, err := onePath("delete", args, flag.NewFlagSet("delete", flag.ContinueOnError))
	if err != nil {
		return err
	}
	if p == Root {
		return errors.New("cannot delete the workspace itself")
	}
	err = os.Remove(p)
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("%s is not empty; delete its content first", p)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s does not exist", p)
	}
	return err
}
