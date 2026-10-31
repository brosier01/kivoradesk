package desktop

import (
	"reflect"
	"testing"
)

func TestSplitText(t *testing.T) {
	got := SplitText("abcdef é€ ok\n", 4)
	want := []Segment{
		{"abcd", true},
		{"ef ", true},
		{"é€", false},
		{" ok\n", true},
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
