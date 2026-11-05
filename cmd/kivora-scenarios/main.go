// Command kivora-scenarios measures how reliably complete workflows run on
// KivoraDesk (goal: at least 90 % over its integration scenarios), and checks the
// boundaries a web page, a shell command or a laptop key must not cross.
//
// It drives the real MCP server over SSH, like a laptop, with scripted tool
// sequences: no model is involved, so the result measures the computer, not
// an agent's reasoning. Each scenario runs several times; the report gives
// the success rate per scenario and overall.
//
//	kivora-scenarios -mcp "ssh -T kivoradesk /usr/local/bin/kivora-mcp" \
//	  -web http://localhost:6080 -token TOKEN \
//	  -admin "docker compose exec -T kivoradesk kivora" -runs 3
//
// -admin enables the scenarios that need the server's admin commands (secrets,
// browser restart, fault injection, pages served from this process); run it on the
// server, in the project's directory. -destructive adds those that damage the data
// disk, restart the VM or replace its data: use a disposable instance.
//
// A scenario that cannot conclude says skip: it counts neither as passed nor as
// failed.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultTimeout = 3 * time.Minute

type scenario struct {
	name        string
	host        bool          // needs the server's admin commands
	destructive bool          // restarts the VM or touches its data: once, and only with -destructive
	timeout     time.Duration // defaultTimeout if zero
	run         func(r *runner) error
}

type runner struct {
	ctx      context.Context
	cs       *mcp.ClientSession
	mcpCmd   string
	sshCmd   string
	web      string
	token    string
	adminCmd []string
	pages    *pageServer
}

type result struct {
	Name     string  `json:"name"`
	Runs     int     `json:"runs"`
	Passed   int     `json:"passed"`
	Skipped  int     `json:"skipped,omitempty"`
	Rate     float64 `json:"rate"`
	P50Sec   float64 `json:"p50_seconds"`
	LastFail string  `json:"last_failure,omitempty"`
	LastSkip string  `json:"last_skip,omitempty"`
}

// skipError says that a scenario could not conclude.
type skipError struct{ reason string }

func (e skipError) Error() string { return e.reason }

func skip(format string, args ...any) error { return skipError{fmt.Sprintf(format, args...)} }

func main() {
	mcpCmd := flag.String("mcp", "", "command starting the MCP server (over SSH)")
	sshCmd := flag.String("ssh", "", "SSH command of the laptop, without a remote command (default: taken from -mcp)")
	web := flag.String("web", "http://localhost:6080", "web gateway URL (through the tunnel)")
	token := flag.String("token", os.Getenv("KIVORA_WEB_TOKEN"), "web gateway session (kivora-web session)")
	admin := flag.String("admin", "", "command that runs the server's admin commands (enables host scenarios)")
	runs := flag.Int("runs", 3, "runs per scenario")
	only := flag.String("only", "", "run scenarios whose name matches this regexp")
	destructive := flag.Bool("destructive", false, "also run the scenarios that restart the VM or damage its data disk, on a disposable instance")
	pageHosts := flag.String("pages", "", "comma-separated addresses of this computer where the VM can reach the test pages (default: try them all)")
	jsonOut := flag.Bool("json", false, "print results as JSON")
	flag.Parse()
	if *mcpCmd == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "usage: kivora-scenarios -mcp COMMAND -token TOKEN [-web URL] [-admin COMMAND] [-runs N]")
		os.Exit(2)
	}

	r := &runner{
		ctx:      context.Background(),
		mcpCmd:   *mcpCmd,
		sshCmd:   *sshCmd,
		web:      *web,
		token:    *token,
		adminCmd: strings.Fields(*admin),
		pages:    &pageServer{only: *pageHosts},
	}
	if err := r.reconnect(); err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}

	var filter *regexp.Regexp
	if *only != "" {
		filter = regexp.MustCompile(*only)
	}
	var results []result
	total, passed, skipped := 0, 0, 0
	for _, sc := range scenarios {
		if (sc.host && len(r.adminCmd) == 0) || (sc.destructive && !*destructive) || (filter != nil && !filter.MatchString(sc.name)) {
			continue
		}
		res := result{Name: sc.name}
		var durations []float64
		times := *runs
		if sc.destructive {
			times = 1
		}
		for i := 0; i < times; i++ {
			if err := r.reset(); err != nil {
				res.LastFail = "reset: " + err.Error()
				res.Runs++
				continue
			}
			start := time.Now()
			err := r.timed(cmp.Or(sc.timeout, defaultTimeout), sc.run)
			if sc.destructive {
				// The VM restarted: this session and its tunnel are gone.
				if err := r.reconnect(); err != nil && !*jsonOut {
					fmt.Fprintf(os.Stderr, "%s: reconnect: %s\n", sc.name, oneLine(err.Error()))
				}
			}
			var s skipError
			if errors.As(err, &s) {
				res.Skipped++
				res.LastSkip = oneLine(s.reason)
				if !*jsonOut {
					fmt.Fprintf(os.Stderr, "SKIP %s #%d: %s\n", sc.name, i+1, res.LastSkip)
				}
				continue
			}
			res.Runs++
			if err != nil {
				res.LastFail = oneLine(err.Error())
				if !*jsonOut {
					fmt.Fprintf(os.Stderr, "FAIL %s #%d: %s\n", sc.name, i+1, res.LastFail)
				}
				continue
			}
			res.Passed++
			durations = append(durations, time.Since(start).Seconds())
		}
		res.Rate = 100 * float64(res.Passed) / float64(max(res.Runs, 1))
		if len(durations) > 0 {
			sort.Float64s(durations)
			res.P50Sec = durations[len(durations)/2]
		}
		total += res.Runs
		passed += res.Passed
		skipped += res.Skipped
		results = append(results, res)
		if !*jsonOut {
			fmt.Fprintf(os.Stderr, "%-36s %d/%d", sc.name, res.Passed, res.Runs)
			if res.Skipped > 0 {
				fmt.Fprintf(os.Stderr, " (%d skipped)", res.Skipped)
			}
			fmt.Fprintln(os.Stderr)
		}
	}
	r.pages.close(r)
	r.cs.Close()

	rate := 100 * float64(passed) / float64(max(total, 1))
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"scenarios": results, "runs": total, "passed": passed, "skipped": skipped, "rate": rate})
	} else {
		fmt.Printf("\n%-36s %5s %7s %8s  %s\n", "scenario", "runs", "rate", "p50", "last failure or skip")
		for _, res := range results {
			note := res.LastFail
			if note == "" && res.LastSkip != "" {
				note = "skip: " + res.LastSkip
			}
			fmt.Printf("%-36s %5d %6.1f%% %7.1fs  %s\n", res.Name, res.Runs, res.Rate, res.P50Sec, truncate(note, 70))
		}
		fmt.Printf("\n%d scenarios, %d runs, %d passed, %d skipped: %.1f %%\n", len(results), total, passed, skipped, rate)
	}
	if rate < 90 {
		os.Exit(1)
	}
}

// connect starts a new MCP session, like a new client on the laptop.
func (r *runner) connect() (*mcp.ClientSession, error) {
	argv := strings.Fields(r.mcpCmd)
	type connected struct {
		cs  *mcp.ClientSession
		err error
	}
	done := make(chan connected, 1)
	go func() {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "kivora-scenarios"}, nil).
			Connect(context.Background(), &mcp.CommandTransport{Command: exec.Command(argv[0], argv[1:]...)}, nil)
		done <- connected{cs, err}
	}()
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	defer cancel()
	select {
	case c := <-done:
		return c.cs, c.err
	case <-ctx.Done():
		go func() {
			if c := <-done; c.cs != nil {
				c.cs.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// reconnect replaces the session the scenarios share. The SSH of a VM that just
// restarted may need a moment.
func (r *runner) reconnect() error {
	var err error
	for range 3 {
		var cs *mcp.ClientSession
		if cs, err = r.connect(); err == nil {
			if r.cs != nil {
				r.cs.Close()
			}
			r.cs = cs
			return nil
		}
		time.Sleep(10 * time.Second)
	}
	return err
}

// session runs fn on a new MCP session, closed afterwards: what it leaves stuck
// stays out of the shared one.
func (r *runner) session(fn func(*runner) error) error {
	cs, err := r.connect()
	if err != nil {
		return err
	}
	defer cs.Close()
	sub := *r
	sub.cs = cs
	return fn(&sub)
}

// reset gives the agent control and leaves a single tab.
func (r *runner) reset() error {
	if err := r.setOwner("agent"); err != nil {
		return err
	}
	out, err := r.call("browser_tabs", nil)
	if err != nil {
		return err
	}
	var tabs []struct {
		ID     string `json:"id"`
		Active bool   `json:"active"`
	}
	if err := json.Unmarshal([]byte(jsonList(out)), &tabs); err != nil {
		return err
	}
	for i, t := range tabs {
		if i > 0 {
			r.call("browser_close_tab", map[string]any{"id": t.ID})
		}
	}
	return nil
}

func (r *runner) timed(d time.Duration, fn func(*runner) error) error {
	ctx, cancel := context.WithTimeout(r.ctx, d)
	defer cancel()
	sub := *r
	sub.ctx = ctx
	return fn(&sub)
}

// call runs a tool and fails if the tool reports an error.
func (r *runner) call(name string, args map[string]any) (string, error) {
	out, isErr, err := r.try(name, args)
	if err != nil {
		return "", err
	}
	if isErr {
		return out, fmt.Errorf("%s: %s", name, out)
	}
	return out, nil
}

// refused runs a tool that must fail and returns its error text.
func (r *runner) refused(name string, args map[string]any) (string, error) {
	out, isErr, err := r.try(name, args)
	if err != nil {
		return "", err
	}
	if !isErr {
		return out, fmt.Errorf("%s succeeded but should have been refused: %s", name, oneLine(out))
	}
	return out, nil
}

func (r *runner) try(name string, args map[string]any) (string, bool, error) {
	res, err := r.cs.CallTool(r.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String(), res.IsError, nil
}

var webClient = http.Client{Timeout: 15 * time.Second}

// setOwner goes through the tunnel, like the human. A restart of the VM ends the
// tunnel: the server's admin commands reach the gateway without it.
func (r *runner) setOwner(owner string) error {
	err := r.setOwnerWeb(owner)
	if err == nil || len(r.adminCmd) == 0 {
		return err
	}
	return r.setOwnerAdmin(owner)
}

func (r *runner) setOwnerWeb(owner string) error {
	req, _ := http.NewRequestWithContext(r.ctx, "POST", r.web+"/api/control", strings.NewReader(fmt.Sprintf(`{"owner":%q}`, owner)))
	req.Header.Set("Cookie", "kivora_session="+r.token)
	resp, err := webClient.Do(req)
	if err != nil {
		return fmt.Errorf("set owner: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("set owner %s: %s", owner, resp.Status)
	}
	return nil
}

func (r *runner) setOwnerAdmin(owner string) error {
	_, err := r.admin("", "ssh", fmt.Sprintf(`. /etc/conf.d/kivoradesk
session=$(doas -u kivora-web /usr/local/bin/kivora-web session) &&
curl -fsS -m 10 -o /dev/null -H "Cookie: kivora_session=$session" -d '{"owner":%q}' "http://127.0.0.1:$KIVORA_WEB_PORT/api/control"`, owner))
	return err
}

// admin runs one of the server's admin commands.
func (r *runner) admin(stdin string, args ...string) (string, error) {
	return r.run(r.adminCmd, stdin, args...)
}

// adminEnv runs an admin command with a setting of the container changed for this
// command only. It needs the "exec" of docker compose in -admin.
func (r *runner) adminEnv(setting, stdin string, args ...string) (string, error) {
	argv, ok := withEnv(r.adminCmd, setting)
	if !ok {
		return "", skip("cannot set %s through -admin %q", setting, strings.Join(r.adminCmd, " "))
	}
	return r.run(argv, stdin, args...)
}

func withEnv(admin []string, setting string) ([]string, bool) {
	for i, a := range admin {
		if a == "exec" {
			argv := append(append([]string(nil), admin[:i+1]...), "-e", setting)
			return append(argv, admin[i+1:]...), true
		}
	}
	return nil, false
}

func (r *runner) run(command []string, stdin string, args ...string) (string, error) {
	argv := append(append([]string(nil), command...), args...)
	cmd := exec.CommandContext(r.ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %v: %s", args, err, oneLine(string(out)))
	}
	return string(out), nil
}

func (r *runner) page(maxChars int) (string, error) {
	args := map[string]any{}
	if maxChars > 0 {
		args["max_chars"] = maxChars
	}
	return r.call("browser_read_page", args)
}

func (r *runner) navigate(url string) (string, error) {
	return r.call("browser_navigate", map[string]any{"url": url})
}

func contains(out string, wants ...string) error {
	for _, w := range wants {
		if !strings.Contains(out, w) {
			return fmt.Errorf("expected %q in: %s", w, truncate(oneLine(out), 300))
		}
	}
	return nil
}

// element returns the number of the first interactive element whose line
// matches re in a browser_read_page result.
func element(page, re string) (int, error) {
	m := regexp.MustCompile(`\[(\d+)\] [^\n]*` + re).FindStringSubmatch(page)
	if m == nil {
		return 0, fmt.Errorf("no element matching %q", re)
	}
	var n int
	fmt.Sscan(m[1], &n)
	return n, nil
}

// path extracts the first /workspace/... path ending in ext from a tool result.
func path(out, ext string) (string, error) {
	m := regexp.MustCompile(`/workspace/(\S+` + regexp.QuoteMeta(ext) + `)`).FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no %s path in %q", ext, oneLine(out))
	}
	return m[1], nil
}

// jsonList extracts the JSON array of a tool result, from its untrusted wrapper.
func jsonList(out string) string {
	start, end := strings.Index(out, "["), strings.LastIndex(out, "]")
	if start < 0 || end < start {
		return out
	}
	return out[start : end+1]
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
