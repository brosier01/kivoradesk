// Command kivora-probe measures, from inside the VM, whether native X11 input
// lands exactly where it is aimed. It loads an oracle page in Chromium over
// CDP, drives the real mouse and keyboard with xdotool, and compares what the
// page received with what was sent.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"time"

	"github.com/brosier01/kivoradesk/internal/cdp"
)

// oracleHTML records every pointer event on a full-page pad, in screen
// coordinates, and exposes a textarea for keyboard checks.
const oracleHTML = `<!doctype html><html><head><style>
html,body{margin:0;height:100%;overflow:hidden;font:16px sans-serif}
#pad{position:fixed;inset:0;background:#f4f4f4}
#text{position:fixed;left:10px;bottom:10px;width:400px;height:40px}
</style></head><body><div id="pad"></div><textarea id="text"></textarea><script>
window.events = [];
for (const t of ["click", "dblclick", "contextmenu", "wheel"]) {
  pad.addEventListener(t, e => {
    events.push({type: e.type, x: e.screenX, y: e.screenY, button: e.button, dy: e.deltaY || 0});
    e.preventDefault();
  }, {passive: false});
}
// A native context menu would swallow the following actions.
document.addEventListener("contextmenu", e => e.preventDefault());
</script></body></html>`

// Screen area that the random targets are drawn from: below the browser
// toolbar (about 87px high at 1280x800) and above the textarea.
const (
	minX, maxX = 10, 1270
	minY, maxY = 100, 720
)

type event struct {
	Type   string  `json:"type"`
	X      int     `json:"x"`
	Y      int     `json:"y"`
	Button int     `json:"button"`
	DY     float64 `json:"dy"`
}

type result struct {
	Name     string   `json:"name"`
	Attempts int      `json:"attempts"`
	Passed   int      `json:"passed"`
	P50MS    float64  `json:"p50_ms"`
	P95MS    float64  `json:"p95_ms"`
	Failures []string `json:"failures,omitempty"`
}

func (r *result) rate() float64 { return 100 * float64(r.Passed) / float64(max(r.Attempts, 1)) }

type probe struct {
	ctx     context.Context
	page    *cdp.Page
	timeout time.Duration

	typeDelay string // xdotool --delay for typing, in ms
}

func main() {
	endpoint := flag.String("cdp", "http://127.0.0.1:9222", "Chromium DevTools HTTP endpoint")
	n := flag.Int("n", 1000, "attempts per pointer test")
	nType := flag.Int("n-type", 100, "attempts per keyboard test")
	typeDelay := flag.Int("type-delay", 8, "xdotool delay between typed characters, in milliseconds")
	timeout := flag.Duration("timeout", 2*time.Second, "time allowed for one action to be observed")
	seed := flag.Uint64("seed", 1, "random seed, for reproducible runs")
	jsonOut := flag.Bool("json", false, "print results as JSON")
	flag.Parse()

	if err := run(*endpoint, *n, *nType, *typeDelay, *timeout, *seed, *jsonOut); err != nil {
		fmt.Fprintln(os.Stderr, "kivora-probe:", err)
		os.Exit(2)
	}
}

func run(endpoint string, n, nType, typeDelay int, timeout time.Duration, seed uint64, jsonOut bool) error {
	ctx := context.Background()
	wsURL, err := cdp.BrowserURL(ctx, endpoint)
	if err != nil {
		return err
	}
	conn, err := cdp.Dial(ctx, wsURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	url := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(oracleHTML))
	page, err := conn.NewPage(ctx, url)
	if err != nil {
		return fmt.Errorf("open oracle page: %w", err)
	}
	defer page.Close(ctx)

	p := &probe{ctx: ctx, page: page, timeout: timeout, typeDelay: strconv.Itoa(typeDelay)}
	if err := p.waitTrue(`document.readyState === "complete" && typeof events !== "undefined"`); err != nil {
		return fmt.Errorf("load oracle page: %w", err)
	}
	rng := rand.New(rand.NewPCG(seed, seed))
	ascii := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .,;:-_/@#()[]{}?!'\"")
	accents := []rune("éèàçùâêîôûëïüœÉÀÇ€")

	results := []*result{
		p.pointer("click", n, rng, []string{"click", "1"}, func(evs []event, x, y int) error {
			return expectOne(evs, "click", x, y, 0)
		}),
		p.pointer("double_click", n/10, rng, []string{"click", "--repeat", "2", "--delay", "40", "1"}, func(evs []event, x, y int) error {
			return expectOne(evs, "dblclick", x, y, 0)
		}),
		p.pointer("right_click", n/10, rng, []string{"click", "3"}, func(evs []event, x, y int) error {
			return expectOne(evs, "contextmenu", x, y, 2)
		}),
		p.pointer("scroll_down", n/10, rng, []string{"click", "5"}, func(evs []event, x, y int) error {
			if err := expectOne(evs, "wheel", x, y, 0); err != nil {
				return err
			}
			if evs[0].DY <= 0 {
				return fmt.Errorf("wheel deltaY=%v, want > 0", evs[0].DY)
			}
			return nil
		}),
		p.typing("type_ascii", nType, rng, ascii),
		p.typing("type_accents", nType, rng, accents),
		p.hotkey("hotkey_select_all_delete", nType/4),
		p.screenshot("screenshot_scrot", 20),
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}
	fmt.Printf("%-26s %8s %8s %8s %9s %9s\n", "test", "attempts", "passed", "rate", "p50", "p95")
	for _, r := range results {
		fmt.Printf("%-26s %8d %8d %7.2f%% %7.1fms %7.1fms\n", r.Name, r.Attempts, r.Passed, r.rate(), r.P50MS, r.P95MS)
		for _, f := range r.Failures {
			fmt.Printf("    - %s\n", f)
		}
	}
	return nil
}

// pointer moves to random points and runs one xdotool pointer action at each.
func (p *probe) pointer(name string, n int, rng *rand.Rand, action []string, check func([]event, int, int) error) *result {
	r := &result{Name: name}
	var lat []time.Duration
	for range n {
		x, y := minX+rng.IntN(maxX-minX), minY+rng.IntN(maxY-minY)
		r.Attempts++
		start := time.Now()
		err := p.eval(`events = []`, nil)
		if err == nil {
			args := append([]string{"mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y)}, action...)
			err = xdotool(args...)
		}
		var evs []event
		if err == nil {
			evs, err = p.waitEvents()
		}
		if err == nil {
			err = check(evs, x, y)
		}
		if err != nil {
			r.fail(fmt.Sprintf("(%d,%d): %v", x, y, err))
			continue
		}
		lat = append(lat, time.Since(start))
		r.Passed++
	}
	r.setLatency(lat)
	return r
}

// typing types random strings into the textarea and compares its value.
func (p *probe) typing(name string, n int, rng *rand.Rand, charset []rune) *result {
	r := &result{Name: name}
	var lat []time.Duration
	for range n {
		want := randomString(rng, charset, 8+rng.IntN(24))
		r.Attempts++
		start := time.Now()
		got, err := p.typeAndRead(want)
		if err != nil {
			r.fail(fmt.Sprintf("%q: %v", want, err))
			continue
		}
		if got != want {
			r.fail(fmt.Sprintf("typed %q, got %q", want, got))
			continue
		}
		lat = append(lat, time.Since(start))
		r.Passed++
	}
	r.setLatency(lat)
	return r
}

// hotkey checks that ctrl+a followed by BackSpace empties the textarea.
func (p *probe) hotkey(name string, n int) *result {
	r := &result{Name: name}
	var lat []time.Duration
	for range n {
		r.Attempts++
		if _, err := p.typeAndRead("hotkey check"); err != nil {
			r.fail(err.Error())
			continue
		}
		start := time.Now()
		err := xdotool("key", "--delay", "20", "ctrl+a", "BackSpace")
		if err == nil {
			err = p.waitTrue(`text.value === ""`)
		}
		if err != nil {
			r.fail(err.Error())
			continue
		}
		lat = append(lat, time.Since(start))
		r.Passed++
	}
	r.setLatency(lat)
	return r
}

// screenshot times a full-screen capture with scrot.
func (p *probe) screenshot(name string, n int) *result {
	r := &result{Name: name}
	var lat []time.Duration
	for range n {
		r.Attempts++
		start := time.Now()
		if out, err := exec.Command("scrot", "-o", "/tmp/kivora-probe.png").CombinedOutput(); err != nil {
			r.fail(fmt.Sprintf("%v: %s", err, out))
			continue
		}
		lat = append(lat, time.Since(start))
		r.Passed++
	}
	r.setLatency(lat)
	return r
}

func (p *probe) typeAndRead(s string) (string, error) {
	if err := p.eval(`text.value = ""; text.focus()`, nil); err != nil {
		return "", err
	}
	if err := xdotool("type", "--delay", p.typeDelay, "--", s); err != nil {
		return "", err
	}
	want, _ := json.Marshal(len([]rune(s)))
	_ = p.waitTrue(`[...text.value].length >= ` + string(want))
	var got string
	err := p.eval(`text.value`, &got)
	return got, err
}

// waitEvents polls the oracle page until it has recorded events, then waits a
// short settle time so that duplicates or late events are caught too.
func (p *probe) waitEvents() ([]event, error) {
	if err := p.waitTrue(`events.length > 0`); err != nil {
		return nil, err
	}
	time.Sleep(60 * time.Millisecond)
	var evs []event
	err := p.eval(`events`, &evs)
	return evs, err
}

func (p *probe) waitTrue(js string) error {
	deadline := time.Now().Add(p.timeout)
	for {
		var ok bool
		if err := p.eval(js, &ok); err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the page to observe the action")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *probe) eval(js string, out any) error {
	return p.page.Eval(p.ctx, js, out)
}

func (r *result) fail(msg string) {
	if len(r.Failures) < 5 {
		r.Failures = append(r.Failures, msg)
	}
}

func (r *result) setLatency(lat []time.Duration) {
	if len(lat) == 0 {
		return
	}
	slices.Sort(lat)
	ms := func(q float64) float64 { return float64(lat[int(q*float64(len(lat)-1))].Microseconds()) / 1000 }
	r.P50MS, r.P95MS = ms(0.50), ms(0.95)
}

// expectOne checks that exactly one event of type typ was recorded at (x, y),
// ignoring the click events that precede a dblclick.
func expectOne(evs []event, typ string, x, y, button int) error {
	var hits []event
	for _, e := range evs {
		if e.Type == typ {
			hits = append(hits, e)
		} else if !(typ == "dblclick" && e.Type == "click") {
			return fmt.Errorf("unexpected %s event", e.Type)
		}
	}
	if len(hits) != 1 {
		return fmt.Errorf("got %d %s events, want 1", len(hits), typ)
	}
	e := hits[0]
	if e.X != x || e.Y != y {
		return fmt.Errorf("%s landed at (%d,%d)", typ, e.X, e.Y)
	}
	if e.Button != button {
		return fmt.Errorf("%s button=%d, want %d", typ, e.Button, button)
	}
	return nil
}

func randomString(rng *rand.Rand, charset []rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = charset[rng.IntN(len(charset))]
	}
	return string(out)
}

func xdotool(args ...string) error {
	if out, err := exec.Command("xdotool", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("xdotool %v: %v: %s", args, err, out)
	}
	return nil
}
