// Package desktop drives the X11 desktop with native input: xdotool for the
// mouse and keyboard, scrot for screenshots and xclip for the clipboard.
//
// Coordinates are in the real screen space (the fixed Xvfb geometry), which
// is also the size of every screenshot, so no scaling is ever involved.
package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Desktop is one X display.
type Desktop struct {
	Display string
	Width   int
	Height  int
	// ShotDir is a directory private to the desktop user, so that nobody can
	// redirect a screenshot file through a symlink.
	ShotDir string
}

// Window is a top-level window.
type Window struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// CheckPoint returns an error if (x, y) is outside the screen.
func (d *Desktop) CheckPoint(x, y int) error {
	if x < 0 || y < 0 || x >= d.Width || y >= d.Height {
		return fmt.Errorf("point (%d, %d) is outside the %dx%d screen", x, y, d.Width, d.Height)
	}
	return nil
}

// Screenshot returns a PNG of the whole screen, mouse pointer included.
func (d *Desktop) Screenshot(ctx context.Context) ([]byte, error) {
	if err := os.MkdirAll(d.ShotDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(d.ShotDir, "screenshot.png")
	if _, err := d.run(ctx, "scrot", "--overwrite", "--pointer", path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Move moves the pointer.
func (d *Desktop) Move(ctx context.Context, x, y int) error {
	_, err := d.xdotool(ctx, "mousemove", "--sync", itoa(x), itoa(y))
	return err
}

// Click clicks button (1 left, 2 middle, 3 right) count times at (x, y).
func (d *Desktop) Click(ctx context.Context, x, y, button, count int) error {
	_, err := d.xdotool(ctx, "mousemove", "--sync", itoa(x), itoa(y),
		"click", "--repeat", itoa(count), "--delay", "60", itoa(button))
	return err
}

// Drag presses the left button at (x1, y1), moves to (x2, y2) and releases.
func (d *Desktop) Drag(ctx context.Context, x1, y1, x2, y2 int) error {
	_, err := d.xdotool(ctx,
		"mousemove", "--sync", itoa(x1), itoa(y1), "mousedown", "1",
		"sleep", "0.1", "mousemove", "--sync", itoa((x1+x2)/2), itoa((y1+y2)/2),
		"sleep", "0.1", "mousemove", "--sync", itoa(x2), itoa(y2),
		"sleep", "0.1", "mouseup", "1")
	return err
}

// Scroll turns the wheel amount notches in direction (up, down, left, right)
// with the pointer at (x, y).
func (d *Desktop) Scroll(ctx context.Context, x, y int, direction string, amount int) error {
	button, ok := map[string]string{"up": "4", "down": "5", "left": "6", "right": "7"}[direction]
	if !ok {
		return fmt.Errorf("unknown scroll direction %q (want up, down, left or right)", direction)
	}
	_, err := d.xdotool(ctx, "mousemove", "--sync", itoa(x), itoa(y),
		"click", "--repeat", itoa(amount), "--delay", "40", button)
	return err
}

// Key presses a key or chord in xdotool syntax ("Return", "ctrl+l"),
// repeat times.
func (d *Desktop) Key(ctx context.Context, keys string, repeat int) error {
	if strings.TrimSpace(keys) == "" || strings.HasPrefix(keys, "-") {
		return fmt.Errorf("invalid key %q", keys)
	}
	_, err := d.xdotool(ctx, "key", "--repeat", itoa(repeat), "--delay", "40", keys)
	return err
}

// TypeASCII types printable ASCII text, including newlines and tabs.
func (d *Desktop) TypeASCII(ctx context.Context, text string) error {
	_, err := d.xdotool(ctx, "type", "--delay", "12", "--", text)
	return err
}

// Paste puts text on the clipboard and pastes it with ctrl+v. Used for
// characters outside the keyboard layout, which xdotool types unreliably.
//
// xclip serves the selection in the foreground and is stopped once the
// target has fetched it: a selection owner left behind stalls x11vnc, which
// then ignores the human's mouse and keyboard.
func (d *Desktop) Paste(ctx context.Context, text string) error {
	requests := newRequestLog()
	owner := exec.Command("xclip", "-selection", "clipboard", "-in", "-verbose")
	owner.Env = d.env()
	owner.Stdin = strings.NewReader(text)
	owner.Stderr = requests
	if err := owner.Start(); err != nil {
		return fmt.Errorf("xclip: %w", err)
	}
	defer func() {
		owner.Process.Kill()
		owner.Wait()
	}()

	// Paste only once xclip owns the selection, or the previous content
	// would be pasted.
	deadline := time.Now().Add(2 * time.Second)
	for {
		out, err := d.run(ctx, "xclip", "-selection", "clipboard", "-out")
		if err == nil && out == text {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("clipboard not ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	requests.waitAbove(ctx, 0, 500*time.Millisecond) // the check above
	base := requests.served()
	if _, err := d.xdotool(ctx, "key", "ctrl+v"); err != nil {
		return err
	}

	// Keep the selection until the target has fetched the text and had time
	// to insert it: Chromium drops a paste when the clipboard changes before
	// the paste is done, which can take more than 250 ms on a busy host. A
	// window that does not take text never asks: give up after 2 s.
	end := time.Now().Add(3 * time.Second)
	if requests.waitAbove(ctx, base, 2*time.Second) {
		for n := requests.served(); time.Now().Before(end) && requests.waitAbove(ctx, n, pasteSettle); n = requests.served() {
		}
	}
	return nil
}

// pasteSettle is how long the paste target must stop asking for the
// selection before xclip is stopped.
const pasteSettle = 750 * time.Millisecond

// requestLog counts the selection requests xclip has served, from its
// verbose output, which announces each request it waits for.
type requestLog struct {
	mu      sync.Mutex
	partial []byte
	waits   int
	changed chan struct{}
}

func newRequestLog() *requestLog {
	return &requestLog{changed: make(chan struct{}, 1)}
}

func (l *requestLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		if bytes.Contains(l.partial[:i], []byte("Waiting for selection request ")) {
			l.waits++
			select {
			case l.changed <- struct{}{}:
			default:
			}
		}
		l.partial = l.partial[i+1:]
	}
	return len(p), nil
}

// served returns the number of requests answered: xclip announces the next
// one after each answer.
func (l *requestLog) served() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.waits-1, 0)
}

// waitAbove waits until more than n requests have been served, and reports
// whether that happened within d.
func (l *requestLog) waitAbove(ctx context.Context, n int, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for l.served() <= n {
		select {
		case <-l.changed:
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// Windows lists visible top-level windows that have a title. Untitled ones
// are toolkit or window manager internals that cannot be focused.
func (d *Desktop) Windows(ctx context.Context) ([]Window, error) {
	out, err := d.xdotool(ctx, "search", "--onlyvisible", "--name", "")
	if err != nil {
		if strings.TrimSpace(out) == "" {
			return nil, nil // no window matches
		}
		return nil, err
	}
	var wins []Window
	for _, id := range strings.Fields(out) {
		w, err := d.window(ctx, id)
		if err != nil || w.Name == "" || w.Width <= 1 || w.Height <= 1 {
			continue // closed meanwhile, or a helper window
		}
		wins = append(wins, w)
	}
	return wins, nil
}

// ActiveWindow returns the focused window.
func (d *Desktop) ActiveWindow(ctx context.Context) (Window, error) {
	out, err := d.xdotool(ctx, "getactivewindow")
	if err != nil {
		return Window{}, err
	}
	return d.window(ctx, strings.TrimSpace(out))
}

// Focus raises and focuses a window, and checks that it became active.
// (xdotool's own --sync waits 15 seconds for windows the window manager
// refuses to activate, then reports success.)
func (d *Desktop) Focus(ctx context.Context, id string) error {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return fmt.Errorf("invalid window id %q", id)
	}
	if _, err := d.xdotool(ctx, "windowactivate", id); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		out, err := d.xdotool(ctx, "getactivewindow")
		if err == nil && strings.TrimSpace(out) == id {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("window %s could not be focused; pick one from computer_windows", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *Desktop) window(ctx context.Context, id string) (Window, error) {
	w := Window{ID: id}
	name, err := d.xdotool(ctx, "getwindowname", id)
	if err != nil {
		return w, err
	}
	w.Name = strings.TrimSpace(name)
	geo, err := d.xdotool(ctx, "getwindowgeometry", "--shell", id)
	if err != nil {
		return w, err
	}
	for _, line := range strings.Split(geo, "\n") {
		k, v, _ := strings.Cut(line, "=")
		n, _ := strconv.Atoi(v)
		switch k {
		case "X":
			w.X = n
		case "Y":
			w.Y = n
		case "WIDTH":
			w.Width = n
		case "HEIGHT":
			w.Height = n
		}
	}
	return w, nil
}

func (d *Desktop) xdotool(ctx context.Context, args ...string) (string, error) {
	return d.run(ctx, "xdotool", args...)
}

func (d *Desktop) run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = d.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return stdout.String(), fmt.Errorf("%s timed out", name)
		}
		return stdout.String(), fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (d *Desktop) env() []string {
	return append(os.Environ(), "DISPLAY="+d.Display)
}

func itoa(n int) string { return strconv.Itoa(n) }

// Segment is a run of text typed one way: through the keyboard (ASCII only)
// or through the clipboard.
type Segment struct {
	Text  string
	ASCII bool
}

// SplitText cuts text into segments of at most chunk runes, so that control
// can be checked between segments. ASCII runs are typed; a piece of a line
// holding any other character is pasted whole, as each paste waits for the
// target. Newlines and tabs are always typed: pasted, they would not submit
// a form or move the focus.
func SplitText(text string, chunk int) []Segment {
	var segs []Segment
	add := func(piece []rune, ascii bool) {
		if n := len(segs); n > 0 && ascii && segs[n-1].ASCII && utf8.RuneCountInString(segs[n-1].Text)+len(piece) <= chunk {
			segs[n-1].Text += string(piece)
			return
		}
		segs = append(segs, Segment{Text: string(piece), ASCII: ascii})
	}
	var line []rune
	flushLine := func() {
		for len(line) > 0 {
			piece := line[:min(chunk, len(line))]
			ascii := true
			for _, r := range piece {
				if r < 0x20 || r >= 0x7f {
					ascii = false
				}
			}
			add(piece, ascii)
			line = line[len(piece):]
		}
	}
	for _, r := range text {
		if r == '\n' || r == '\t' {
			flushLine()
			add([]rune{r}, true)
			continue
		}
		line = append(line, r)
	}
	flushLine()
	return segs
}
