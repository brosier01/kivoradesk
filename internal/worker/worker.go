// Package worker runs the agent's shell commands and file operations under a
// separate, unprivileged account.
//
// The worker account has no access to the display, the browser's DevTools
// endpoint or the control API, so code the agent runs cannot drive the
// desktop behind the input lock. The desktop account invokes this package
// through `doas -u worker kivora-mcp worker ...`; the helper side (Main) runs
// as the worker.
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
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Root is the only directory the file operations accept.
const Root = "/workspace"

// shellProxy is the only way out of the VM for shell commands, to the sites the
// human allowed (cmd/kivora-proxy).
const shellProxy = "http://127.0.0.1:3128"

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
	_, err := r.helper(ctx, time.Minute, data, "write", "--", path)
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

func (r *Runner) helper(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(append([]string{}, r.Prefix...), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
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
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: worker exec|read|write|list|delete ...")
		return 2
	}
	var err error
	switch args[0] {
	case "exec":
		err = mainExec(args[1:])
	case "read":
		err = mainRead(args[1:])
	case "write":
		err = mainWrite(args[1:])
	case "list":
		err = mainList(args[1:])
	case "delete":
		err = mainDelete(args[1:])
	default:
		err = fmt.Errorf("unknown worker command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func mainExec(args []string) error {
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
	res := runAt(Root, fl.Arg(0), *timeout, *maxOutput, true)
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

// isolate gives the command a PID namespace of its own: when the shell ends,
// the kernel kills everything left in it, whatever a process did to detach
// itself. Nothing the agent runs outlives its call. The user namespace is what
// allows an unprivileged account to create it; the account stays the same.
func isolate(attr *syscall.SysProcAttr) {
	uid, gid := os.Getuid(), os.Getgid()
	attr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID
	attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
	attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
	attr.GidMappingsEnableSetgroups = false
	// If the helper is killed, the shell goes too, and with it the namespace.
	// The signal is tied to the thread that starts the command.
	attr.Pdeathsig = syscall.SIGKILL
	runtime.LockOSThread()
}

// runAt executes command in dir, in its own process group, so that a timeout
// kills everything it started, not just the shell.
func runAt(dir, command string, timeout time.Duration, maxOutput int, isolated bool) ExecResult {
	home, _ := os.UserHomeDir()
	cmd := exec.Command("/bin/sh", "-c", command)
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

	res := ExecResult{ExitCode: -1}
	if err := cmd.Start(); err != nil {
		res.Output = err.Error()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
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

func mainRead(args []string) error {
	fl := flag.NewFlagSet("read", flag.ContinueOnError)
	max := fl.Int64("max", 1<<20, "read at most this many bytes")
	p, err := onePath("read", args, fl)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.IsDir() {
		return fmt.Errorf("%s is a directory", p)
	}
	_, err = io.Copy(os.Stdout, io.LimitReader(f, *max))
	return err
}

func mainWrite(args []string) error {
	p, err := onePath("write", args, flag.NewFlagSet("write", flag.ContinueOnError))
	if err != nil {
		return err
	}
	if p == Root {
		return errors.New("cannot write the workspace itself")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o770); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<20))
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o660)
}

func mainList(args []string) error {
	p, err := onePath("list", args, flag.NewFlagSet("list", flag.ContinueOnError))
	if err != nil {
		return err
	}
	dirents, err := os.ReadDir(p)
	if err != nil {
		return err
	}
	entries := []Entry{}
	for _, d := range dirents {
		info, err := d.Info()
		if err != nil {
			continue
		}
		entries = append(entries, Entry{Name: d.Name(), Dir: d.IsDir(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return json.NewEncoder(os.Stdout).Encode(entries)
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
