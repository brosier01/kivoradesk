package desktop

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Screenshots taken at once must not share a file: this scrot writes in two steps.
func TestParallelScreenshotsKeepTheirFile(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\nfor p; do :; done\n: >\"$p\"\nsleep 0.02\nprintf BEGIN >>\"$p\"\nsleep 0.02\nprintf -- -END >>\"$p\"\n"
	if err := os.WriteFile(filepath.Join(bin, "scrot"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	dir := t.TempDir()
	d := &Desktop{Display: ":99", Width: 1280, Height: 800, ShotDir: dir}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				png, err := d.Screenshot(context.Background())
				if err != nil || string(png) != "BEGIN-END" {
					t.Errorf("screenshot = %q, %v", png, err)
				}
			}
		}()
	}
	wg.Wait()
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("%d files left behind", len(left))
	}
}

func TestSplitText(t *testing.T) {
	got := SplitText("abcdef é€ ok\nab\tcdefgh\n\n", 4)
	want := []Segment{
		{"abcd", true},
		{"ef é", false},
		{"€ ok", false},
		{"\nab\t", true},
		{"cdef", true},
		{"gh\n\n", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitText = %+v, want %+v", got, want)
	}
	if segs := SplitText("", 4); len(segs) != 0 {
		t.Fatalf("SplitText(\"\") = %+v, want nothing", segs)
	}
}

func TestCheckPoint(t *testing.T) {
	d := &Desktop{Width: 1280, Height: 800}
	for _, p := range [][2]int{{0, 0}, {1279, 799}} {
		if err := d.CheckPoint(p[0], p[1]); err != nil {
			t.Errorf("CheckPoint%v: %v", p, err)
		}
	}
	for _, p := range [][2]int{{-1, 0}, {1280, 10}, {10, 800}} {
		if err := d.CheckPoint(p[0], p[1]); err == nil {
			t.Errorf("CheckPoint%v accepted a point off screen", p)
		}
	}
}

func TestRequestLogCountsServedRequests(t *testing.T) {
	l := newRequestLog()
	ctx := context.Background()
	l.Write([]byte("Using UTF8_STRING.\nWaiting for selection requests, Control-C to quit\n"))
	l.Write([]byte("  Waiting for selection request number 1\n  Waiting for sel"))
	if n := l.served(); n != 0 {
		t.Fatalf("served = %d before any answer, want 0", n)
	}
	if l.waitAbove(ctx, 0, 20*time.Millisecond) {
		t.Fatal("waitAbove returned true with no request served")
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		l.Write([]byte("ection request number 2\n"))
	}()
	if !l.waitAbove(ctx, 0, time.Second) || l.served() != 1 {
		t.Fatalf("served = %d after one answer split across writes, want 1", l.served())
	}
}
