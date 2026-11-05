package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":          "report.pdf",
		"../../etc/passwd":    "passwd",
		"..":                  "file",
		".bashrc":             "bashrc",
		"a\nb.txt":            "a_b.txt",
		`C:\Users\x\doc.pdf`:  "doc.pdf",
		"":                    "file",
		"-rf":                 "rf",
		"--checkpoint=1":      "checkpoint=1",
		" . - name":           "name",
		"report\u202Efdp.exe": "reportfdp.exe",
		"a\u200Db\u2066c":     "abc",
		"bad\xffbyte.txt":     "bad_byte.txt",
		"\x85name":            "_name",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeNameShortensOnACharacter(t *testing.T) {
	got := SanitizeName("a" + strings.Repeat("é", 150) + ".pdf")
	if !utf8.ValidString(got) || !strings.HasSuffix(got, ".pdf") || len(got) > 200 {
		t.Fatalf("SanitizeName = %q (%d bytes)", got, len(got))
	}
	got = SanitizeName(strings.Repeat("x", 250) + "." + strings.Repeat("y", 100))
	if !utf8.ValidString(got) || len(got) != 200 {
		t.Fatalf("a long extension: %q (%d bytes)", got, len(got))
	}
	if got := SanitizeName(strings.Repeat("x", 200)); len(got) != 200 {
		t.Fatalf("at the limit: %d bytes", len(got))
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

// swapDir replaces the directory with a link when the file is being written,
// as the worker could, and then fails like a lost connection.
type swapDir struct{ dir, target string }

func (s swapDir) Read([]byte) (int, error) {
	os.Rename(s.dir, s.dir+".old")
	os.Symlink(s.target, s.dir)
	return 0, errors.New("connection lost")
}

func TestCleanupDoesNotFollowASwappedDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "x.txt")
	os.WriteFile(victim, []byte("safe"), 0o600)
	dir := filepath.Join(root, "downloads")

	if _, err := Create(root, "downloads", "x.txt", swapDir{dir, outside}, 10); err == nil {
		t.Fatal("Create succeeded on a failing reader")
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "safe" {
		t.Fatalf("file outside the workspace changed: %q, %v", b, err)
	}
	if _, err := os.Stat(dir + ".old/x.txt"); err == nil {
		t.Fatal("partial file left behind")
	}
}
