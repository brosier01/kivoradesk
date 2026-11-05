// Command kivora-scenarios measures how reliably complete workflows run on
// KivoraDesk (goal: at least 90 % over 30 integration scenarios).
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
// browser restart, fault injection); run it on the server, in the project's
// directory.
package main

import (
	"context"
	"encoding/json"
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

type scenario struct {
	name string
	host bool // needs the server's admin commands
	run  func(r *runner) error
}

type runner struct {
	ctx      context.Context
	cs       *mcp.ClientSession
	web      string
	token    string
	adminCmd []string
}

type result struct {
	Name     string  `json:"name"`
	Runs     int     `json:"runs"`
	Passed   int     `json:"passed"`
	Rate     float64 `json:"rate"`
	P50Sec   float64 `json:"p50_seconds"`
	LastFail string  `json:"last_failure,omitempty"`
}

func main() {
	mcpCmd := flag.String("mcp", "", "command starting the MCP server (over SSH)")
	web := flag.String("web", "http://localhost:6080", "web gateway URL (through the tunnel)")
	token := flag.String("token", os.Getenv("KIVORA_WEB_TOKEN"), "web gateway session (kivora-web session)")
	admin := flag.String("admin", "", "command that runs the server's admin commands (enables host scenarios)")
	runs := flag.Int("runs", 3, "runs per scenario")
	only := flag.String("only", "", "run scenarios whose name matches this regexp")
	jsonOut := flag.Bool("json", false, "print results as JSON")
	flag.Parse()
	if *mcpCmd == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "usage: kivora-scenarios -mcp COMMAND -token TOKEN [-web URL] [-admin COMMAND] [-runs N]")
		os.Exit(2)
	}

	ctx := context.Background()
	argv := strings.Fields(*mcpCmd)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "kivora-scenarios"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: exec.Command(argv[0], argv[1:]...)}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer cs.Close()
	r := &runner{ctx: ctx, cs: cs, web: *web, token: *token, adminCmd: strings.Fields(*admin)}

	var filter *regexp.Regexp
	if *only != "" {
		filter = regexp.MustCompile(*only)
	}
	var results []result
	total, passed := 0, 0
	for _, sc := range scenarios {
		if (sc.host && len(r.adminCmd) == 0) || (filter != nil && !filter.MatchString(sc.name)) {
			continue
		}
		res := result{Name: sc.name}
		var durations []float64
		for i := 0; i < *runs; i++ {
			if err := r.reset(); err != nil {
				res.LastFail = "reset: " + err.Error()
				res.Runs++
				continue
			}
			start := time.Now()
			err := r.timed(3*time.Minute, sc.run)
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
		results = append(results, res)
		if !*jsonOut {
			fmt.Fprintf(os.Stderr, "%-28s %d/%d\n", sc.name, res.Passed, res.Runs)
		}
	}

	rate := 100 * float64(passed) / float64(max(total, 1))
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"scenarios": results, "runs": total, "passed": passed, "rate": rate})
	} else {
		fmt.Printf("\n%-28s %5s %7s %8s  %s\n", "scenario", "runs", "rate", "p50", "last failure")
		for _, res := range results {
			fmt.Printf("%-28s %5d %6.1f%% %7.1fs  %s\n", res.Name, res.Runs, res.Rate, res.P50Sec, truncate(res.LastFail, 70))
		}
		fmt.Printf("\n%d scenarios, %d runs, %d passed: %.1f %%\n", len(results), total, passed, rate)
	}
	if rate < 90 {
		os.Exit(1)
	}
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

func (r *runner) setOwner(owner string) error {
	req, _ := http.NewRequestWithContext(r.ctx, "POST", r.web+"/api/control", strings.NewReader(fmt.Sprintf(`{"owner":%q}`, owner)))
	req.Header.Set("Cookie", "kivora_session="+r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("set owner: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("set owner %s: %s", owner, resp.Status)
	}
	return nil
}

// admin runs one of the server's admin commands.
func (r *runner) admin(stdin string, args ...string) (string, error) {
	argv := append(append([]string(nil), r.adminCmd...), args...)
	cmd := exec.CommandContext(r.ctx, argv[0], argv[1:]...)
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
