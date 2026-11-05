package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
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

// fakeDisk replaces statfs: free gives the bytes free outside the reserve at
// each call, counted from 1. It returns the number of calls.
func fakeDisk(t *testing.T, free func(calls int) uint64) *int {
	t.Helper()
	calls := 0
	statfs = func(fd int, st *unix.Statfs_t) error {
		calls++
		st.Bsize = 1
		st.Bavail = free(calls)
		return nil
	}
	t.Cleanup(func() { statfs = unix.Fstatfs })
	return &calls
}

func TestCreateRefusesAFullDisk(t *testing.T) {
	root := t.TempDir()
	fakeDisk(t, func(int) uint64 { return minFree })
	if _, err := Create(root, "downloads", "a.txt", strings.NewReader("x"), 10); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Create = %v, want ErrDiskFull", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "downloads")); len(entries) != 0 {
		t.Fatalf("file created on a full disk: %v", entries)
	}
}

func TestCreateStopsWhenTheDiskFills(t *testing.T) {
	root := t.TempDir()
	// Room for two checks, then the disk is full.
	calls := fakeDisk(t, func(n int) uint64 {
		if n <= 2 {
			return 100 << 20
		}
		return minFree / 2
	})
	big := strings.NewReader(strings.Repeat("x", 3*checkEvery))
	if _, err := Create(root, "", "big", big, 1<<30); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Create = %v, want ErrDiskFull", err)
	}
	if *calls != 3 {
		t.Fatalf("free space checked %d times, want 3", *calls)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("partial file left behind: %v", entries)
	}
}

func TestCreateNeverWritesPastTheFreeSpace(t *testing.T) {
	root := t.TempDir()
	// 1000 bytes above the floor, used up by the write.
	disk := func(n int) uint64 {
		if n == 1 {
			return minFree + 1000
		}
		return minFree
	}
	fakeDisk(t, disk)
	if _, err := Create(root, "", "a", strings.NewReader(strings.Repeat("x", 1000)), 1<<20); err != nil {
		t.Fatalf("Create of 1000 bytes: %v", err)
	}
	fakeDisk(t, disk)
	if _, err := Create(root, "", "b", strings.NewReader(strings.Repeat("x", 1001)), 1<<20); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("Create of 1001 bytes = %v, want ErrDiskFull", err)
	}
}

func TestCreateWritesWithRoom(t *testing.T) {
	root := t.TempDir()
	calls := fakeDisk(t, func(int) uint64 { return 1 << 30 })
	data := strings.Repeat("y", checkEvery+10)
	rel, err := Create(root, "downloads", "ok.bin", strings.NewReader(data), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, rel)); string(b) != data {
		t.Fatalf("wrote %d bytes, want %d", len(b), len(data))
	}
	// Once before and once after checkEvery bytes.
	if *calls != 2 {
		t.Fatalf("free space checked %d times, want 2", *calls)
	}
}
