// Command kivora-bench measures the resources and speed of a KivoraDesk VM:
// boot time, disk, memory and CPU at idle, memory with real sites open, and
// the latency of the MCP tools. It runs on the server, next to the admin
// scripts, and drives the MCP server over SSH like a laptop.
//
//	kivora-bench -scripts scripts/vm -label baseline -out baseline.json
//	kivora-bench -compare baseline.json candidate.json
//
// Compare two runs to judge a change (A/B): run the benchmark, apply the
// change, run it again.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed memory.sh
var memoryScript string

type report struct {
	Label    string     `json:"label"`
	Date     string     `json:"date"`
	Version  string     `json:"version"`
	Chromium string     `json:"chromium"`
	Boot     *boot      `json:"boot,omitempty"`
	Disk     disk       `json:"disk"`
	Idle     memory     `json:"idle"`
	CPUIdle  float64    `json:"idle_cpu_percent"`
	Sites    []string   `json:"sites"`
	Loaded   memory     `json:"loaded"`
	Latency  []latency  `json:"latency"`
	settings benchFlags `json:"-"`
}

type boot struct {
	SSHSeconds     float64 `json:"ssh_seconds"`     // reboot command to SSH answering
	MCPSeconds     float64 `json:"mcp_seconds"`     // reboot command to a working tool call
	ServicesUptime float64 `json:"services_uptime"` // VM uptime when the first KivoraDesk service started
	BrowserUptime  float64 `json:"browser_uptime"`  // VM uptime when DevTools answered
}

type disk struct {
	RootUsedMiB float64 `json:"root_used_mib"`
	Packages    int     `json:"packages"`
}

type memory struct {
	UsedMiB float64            `json:"used_mib"` // MemTotal - MemAvailable
	PSSMiB  map[string]float64 `json:"pss_mib"`  // proportional set size by component
}

type latency struct {
	Name  string  `json:"name"`
	Runs  int     `json:"runs"`
	P50ms float64 `json:"p50_ms"`
	P95ms float64 `json:"p95_ms"`
}

type benchFlags struct {
	scripts string
	mcpCmd  string
	runs    int
	settle  time.Duration
	reboot  bool
	sites   []string
}

func main() {
	var f benchFlags
	label := flag.String("label", "run", "name of this run")
	out := flag.String("out", "", "write the report as JSON to this file")
	compare := flag.Bool("compare", false, "compare two JSON reports given as arguments")
	sites := flag.String("sites", "https://en.wikipedia.org/wiki/Linux,https://github.com/torvalds/linux,https://www.bbc.com/news", "comma-separated sites opened for the loaded measurement")
	flag.StringVar(&f.scripts, "scripts", "scripts/vm", "directory of the server's admin scripts")
	flag.StringVar(&f.mcpCmd, "mcp", "", "command starting the MCP server (default: ssh to the agent account)")
	flag.IntVar(&f.runs, "runs", 20, "runs per latency measurement")
	flag.DurationVar(&f.settle, "settle", time.Minute, "wait before measuring idle resources")
	flag.BoolVar(&f.reboot, "reboot", true, "reboot the VM first and measure the boot")
	flag.Parse()

	if *compare {
		if flag.NArg() != 2 {
			fail("usage: kivora-bench -compare A.json B.json")
		}
		a, b := load(flag.Arg(0)), load(flag.Arg(1))
		printComparison(a, b)
		return
	}
	f.sites = strings.Split(*sites, ",")
	if f.mcpCmd == "" {
		key := os.ExpandEnv("$HOME/.local/share/kivoradesk/ssh/id_ed25519")
		// Like scripts/vm: the VM gets new host keys when it is rebuilt.
		f.mcpCmd = "ssh -i " + key + " -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null" +
			" -o LogLevel=ERROR -T agent@192.168.122.50 /usr/local/bin/kivora-mcp"
	}

	r := &report{Label: *label, Date: time.Now().UTC().Format(time.RFC3339), Sites: f.sites, settings: f}
	if err := r.run(); err != nil {
		fail(err.Error())
	}
	printReport(r)
	if *out != "" {
		data, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			fail(err.Error())
		}
	}
}

func (r *report) run() error {
	f := r.settings
	if f.reboot {
		b, err := measureBoot(f)
		if err != nil {
			return fmt.Errorf("boot: %w", err)
		}
		r.Boot = b
	}
	r.Version, _ = admin(f, "", "/usr/local/bin/kivora-mcp -version")
	r.Chromium, _ = admin(f, "", "chromium-browser --version")
	progress("settling for %s", f.settle)
	time.Sleep(f.settle)

	var err error
	if r.Disk, err = measureDisk(f); err != nil {
		return fmt.Errorf("disk: %w", err)
	}
	if r.Idle, err = measureMemory(f); err != nil {
		return fmt.Errorf("idle memory: %w", err)
	}
	if r.CPUIdle, err = measureCPU(f, 30*time.Second); err != nil {
		return fmt.Errorf("idle CPU: %w", err)
	}

	ctx := context.Background()
	cs, err := connect(ctx, f.mcpCmd)
	if err != nil {
		return err
	}
	defer cs.Close()
	c := &client{ctx: ctx, cs: cs}
	if r.Latency, err = measureLatency(c, f.runs); err != nil {
		return fmt.Errorf("latency: %w", err)
	}
	if r.Loaded, err = measureLoaded(c, f); err != nil {
		return fmt.Errorf("loaded memory: %w", err)
	}
	return nil
}

// measureBoot reboots the VM through the guest agent and times each stage.
func measureBoot(f benchFlags) (*boot, error) {
	progress("rebooting the VM")
	before := uptime(f)
	start := time.Now()
	reboot := exec.Command("bash", "-c", `source "$1/common.sh" && virsh_ reboot "$VM_NAME" --mode agent >/dev/null`, "_", f.scripts)
	if out, err := reboot.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("reboot: %v: %s", err, out)
	}
	b := &boot{}
	deadline := start.Add(3 * time.Minute)
	for {
		if u := uptime(f); u > 0 && (before == 0 || u < before) {
			b.SSHSeconds = time.Since(start).Seconds()
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("SSH did not come back within 3 minutes")
		}
		time.Sleep(500 * time.Millisecond)
	}
	for {
		out, err := admin(f, "", `curl -fsS -o /dev/null http://127.0.0.1:9222/json/version && cut -d' ' -f1 /proc/uptime`)
		if err == nil {
			b.BrowserUptime, _ = strconv.ParseFloat(strings.TrimSpace(out), 64)
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("DevTools did not answer within 3 minutes")
		}
		time.Sleep(250 * time.Millisecond)
	}
	// OpenRC marks each started service with a symlink created at start.
	if out, err := admin(f, "", `cut -d' ' -f1 /proc/uptime; date +%s.%N; stat -c %Y /run/openrc/started/kivora-firewall`); err == nil {
		if v := strings.Fields(out); len(v) == 3 {
			up, _ := strconv.ParseFloat(v[0], 64)
			now, _ := strconv.ParseFloat(v[1], 64)
			fw, _ := strconv.ParseFloat(v[2], 64)
			b.ServicesUptime = up - (now - fw)
		}
	}
	ctx := context.Background()
	for {
		cs, err := connect(ctx, f.mcpCmd)
		if err == nil {
			c := &client{ctx: ctx, cs: cs}
			_, err = c.call("computer_get_state", nil)
			cs.Close()
			if err == nil {
				b.MCPSeconds = time.Since(start).Seconds()
				return b, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("MCP tools did not work within 3 minutes: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func uptime(f benchFlags) float64 {
	out, err := admin(f, "", "cut -d' ' -f1 /proc/uptime")
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(out), 64)
	return v
}

func measureDisk(f benchFlags) (disk, error) {
	out, err := admin(f, "", `doas du -sxk / | cut -f1; apk info | wc -l`)
	if err != nil {
		return disk{}, err
	}
	v := strings.Fields(out)
	if len(v) != 2 {
		return disk{}, fmt.Errorf("unexpected output %q", out)
	}
	kib, _ := strconv.ParseFloat(v[0], 64)
	pkgs, _ := strconv.Atoi(v[1])
	return disk{RootUsedMiB: kib / 1024, Packages: pkgs}, nil
}

// measureMemory sums the proportional set size of each component, so that
// shared pages are split between the processes that map them.
func measureMemory(f benchFlags) (memory, error) {
	out, err := admin(f, memoryScript, "doas sh -s")
	if err != nil {
		return memory{}, err
	}
	m := memory{PSSMiB: map[string]float64{}}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		v := strings.Fields(line)
		if len(v) != 2 {
			continue
		}
		kib, _ := strconv.ParseFloat(v[1], 64)
		if v[0] == "used" {
			m.UsedMiB = kib / 1024
		} else {
			m.PSSMiB[v[0]] = kib / 1024
		}
	}
	if m.UsedMiB == 0 {
		return m, fmt.Errorf("unexpected output %q", out)
	}
	return m, nil
}

// measureCPU returns the share of the VM's CPU time spent busy over d.
func measureCPU(f benchFlags, d time.Duration) (float64, error) {
	progress("measuring idle CPU for %s", d)
	read := func() ([]float64, error) {
		out, err := admin(f, "", "head -1 /proc/stat")
		if err != nil {
			return nil, err
		}
		var v []float64
		for _, s := range strings.Fields(out)[1:] {
			n, _ := strconv.ParseFloat(s, 64)
			v = append(v, n)
		}
		return v, nil
	}
	a, err := read()
	if err != nil {
		return 0, err
	}
	time.Sleep(d)
	b, err := read()
	if err != nil {
		return 0, err
	}
	var total, idle float64
	for i := range a {
		total += b[i] - a[i]
	}
	idle = (b[3] - a[3]) + (b[4] - a[4]) // idle + iowait
	return 100 * (total - idle) / total, nil
}

func measureLatency(c *client, runs int) ([]latency, error) {
	progress("measuring tool latency (%d runs each)", runs)
	if _, err := c.call("browser_navigate", map[string]any{"url": "https://example.com"}); err != nil {
		return nil, err
	}
	ops := []struct {
		name string
		do   func() error
	}{
		{"computer_get_state", c.do("computer_get_state", nil)},
		{"computer_screenshot", c.do("computer_screenshot", nil)},
		{"computer_click", c.do("computer_click", map[string]any{"x": 1000, "y": 700})},
		{"computer_key", c.do("computer_key", map[string]any{"keys": "Escape"})},
		{"computer_type ascii (20 chars)", c.do("computer_type", map[string]any{"text": "hello world 12345678"})},
		{"computer_type accented (20 chars)", c.do("computer_type", map[string]any{"text": "café crème brûlée ok"})},
		{"browser_navigate example.com", c.do("browser_navigate", map[string]any{"url": "https://example.com"})},
		{"browser_read_page", c.do("browser_read_page", nil)},
		{"shell_run true", c.do("shell_run", map[string]any{"command": "true"})},
		{"files_write + files_read", func() error {
			if _, err := c.call("files_write", map[string]any{"path": "bench/probe.txt", "content": "probe"}); err != nil {
				return err
			}
			_, err := c.call("files_read", map[string]any{"path": "bench/probe.txt"})
			return err
		}},
	}
	var res []latency
	for _, op := range ops {
		var ms []float64
		for i := 0; i < runs; i++ {
			start := time.Now()
			if err := op.do(); err != nil {
				return nil, fmt.Errorf("%s: %w", op.name, err)
			}
			ms = append(ms, float64(time.Since(start).Microseconds())/1000)
		}
		sort.Float64s(ms)
		res = append(res, latency{Name: op.name, Runs: runs, P50ms: ms[len(ms)/2], P95ms: ms[(len(ms)*95+99)/100-1]})
	}
	c.call("files_delete", map[string]any{"path": "bench/probe.txt"})
	c.call("files_delete", map[string]any{"path": "bench"})
	return res, nil
}

// measureLoaded opens real sites in tabs, measures memory, then closes them.
func measureLoaded(c *client, f benchFlags) (memory, error) {
	progress("opening %d sites", len(f.sites))
	for _, site := range f.sites {
		if _, err := c.call("browser_new_tab", map[string]any{"url": site}); err != nil {
			return memory{}, err
		}
	}
	time.Sleep(15 * time.Second)
	m, err := measureMemory(f)
	out, _ := c.call("browser_tabs", nil)
	var tabs []struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(out), &tabs)
	for i, t := range tabs {
		if i > 0 {
			c.call("browser_close_tab", map[string]any{"id": t.ID})
		}
	}
	return m, err
}

// admin runs a shell command on the VM as the admin account.
func admin(f benchFlags, stdin, command string) (string, error) {
	cmd := exec.Command(f.scripts+"/ssh.sh", command)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

type client struct {
	ctx context.Context
	cs  *mcp.ClientSession
}

func connect(ctx context.Context, command string) (*mcp.ClientSession, error) {
	argv := strings.Fields(command)
	return mcp.NewClient(&mcp.Implementation{Name: "kivora-bench"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: exec.Command(argv[0], argv[1:]...)}, nil)
}

func (c *client) call(name string, args map[string]any) (string, error) {
	res, err := c.cs.CallTool(c.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, content := range res.Content {
		if t, ok := content.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("%s: %s", name, b.String())
	}
	return b.String(), nil
}

func (c *client) do(name string, args map[string]any) func() error {
	return func() error {
		_, err := c.call(name, args)
		return err
	}
}

func load(path string) *report {
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err.Error())
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		fail(path + ": " + err.Error())
	}
	return &r
}

func progress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "==> "+format+"\n", args...)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}
