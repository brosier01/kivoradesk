package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":         "report.pdf",
		"../../etc/passwd":   "passwd",
		"..":                 "file",
		".bashrc":            "bashrc",
		"a\nb.txt":           "a_b.txt",
		`C:\Users\x\doc.pdf`: "doc.pdf",
		"":                   "file",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	for i, want := range []string{"downloads/a.txt", "downloads/a (1).txt", "downloads/a (2).txt"} {
		got, err := Create(root, "downloads", "a.txt", strings.NewReader("x"), 10)
		if err != nil || got != want {
			t.Fatalf("Create #%d = %q, %v; want %q", i, got, err, want)
		}
	}
}

func TestCreateRefusesSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	os.WriteFile(victim, []byte("safe"), 0o600)

	// A symlinked directory must not be followed.
	os.Symlink(outside, filepath.Join(root, "downloads"))
	if _, err := Create(root, "downloads", "x.txt", strings.NewReader("evil"), 10); err == nil {
		t.Fatal("Create followed a symlinked directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("file written outside the workspace")
	}

	// A symlink at the file name must not be followed or replaced.
	os.Remove(filepath.Join(root, "downloads"))
	os.Mkdir(filepath.Join(root, "downloads"), 0o700)
	os.Symlink(victim, filepath.Join(root, "downloads", "x.txt"))
	got, err := Create(root, "downloads", "x.txt", strings.NewReader("evil"), 10)
	if err != nil || got != "downloads/x (1).txt" {
		t.Fatalf("Create = %q, %v; want a new name next to the symlink", got, err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "safe" {
		t.Fatalf("symlink target overwritten: %q", b)
	}
}

func TestCreateEnforcesLimit(t *testing.T) {
	root := t.TempDir()
	if _, err := Create(root, "", "big", strings.NewReader("0123456789"), 5); err == nil {
		t.Fatal("Create accepted a file over the limit")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("partial file left behind: %v", entries)
	}
}
