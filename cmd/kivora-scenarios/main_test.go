package main

import (
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestShmProgramIsAValidProgram(t *testing.T) {
	data := shmProgram()
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if f.Machine != elf.EM_X86_64 || f.Type != elf.ET_EXEC || f.Class != elf.ELFCLASS64 {
		t.Fatalf("machine %v, type %v, class %v", f.Machine, f.Type, f.Class)
	}
	if len(f.Progs) != 1 {
		t.Fatalf("%d program headers", len(f.Progs))
	}
	p := f.Progs[0]
	if p.Type != elf.PT_LOAD || p.Flags&elf.PF_X == 0 {
		t.Fatalf("segment %+v", p)
	}
	if f.Entry < p.Vaddr || f.Entry >= p.Vaddr+p.Filesz || p.Filesz != uint64(len(data)) {
		t.Fatalf("entry %#x outside the segment %+v, file of %d bytes", f.Entry, p, len(data))
	}
	code := data[f.Entry-p.Vaddr:]
	if len(code) == 0 || code[0] != 0xb8 || code[1] != 29 {
		t.Fatalf("code starts with % x", code[:2])
	}
}

func TestSSHParts(t *testing.T) {
	for _, c := range []struct {
		command string
		opts    []string
		host    string
		remote  []string
		ok      bool
	}{
		{"ssh -o ClearAllForwardings=yes -T kivoradesk /usr/local/bin/kivora-mcp",
			[]string{"-o", "ClearAllForwardings=yes", "-T"}, "kivoradesk", []string{"/usr/local/bin/kivora-mcp"}, true},
		{"ssh -F /tmp/config -p 2222 -T host /usr/local/bin/kivora-mcp",
			[]string{"-F", "/tmp/config", "-p", "2222", "-T"}, "host", []string{"/usr/local/bin/kivora-mcp"}, true},
		{"ssh host", nil, "host", []string{}, true},
		{"/usr/bin/ssh -oBatchMode=yes host a b", []string{"-oBatchMode=yes"}, "host", []string{"a", "b"}, true},
		{"sh -c ssh", nil, "", nil, false},
		{"ssh -F", nil, "", nil, false},
		{"ssh -T", nil, "", nil, false},
	} {
		opts, host, remote, ok := sshParts(strings.Fields(c.command))
		if ok != c.ok || host != c.host || !slices.Equal(opts, c.opts) || !slices.Equal(remote, c.remote) {
			t.Errorf("%q: got %q %q %q %v", c.command, opts, host, remote, ok)
		}
	}
}

func TestCopyOptsSpeakScp(t *testing.T) {
	got := copyOpts([]string{"-F", "cfg", "-p", "22", "-T", "-l", "agent", "-o", "A=b", "-oC=d", "-i", "key", "-N"})
	want := []string{"-F", "cfg", "-P", "22", "-o", "User=agent", "-o", "A=b", "-oC=d", "-i", "key"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWithEnvGoesAfterExec(t *testing.T) {
	admin := []string{"docker", "compose", "exec", "-T", "kivoradesk", "kivora"}
	got, ok := withEnv(admin, "A=1")
	want := []string{"docker", "compose", "exec", "-e", "A=1", "-T", "kivoradesk", "kivora"}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("got %q %v", got, ok)
	}
	if admin[3] != "-T" {
		t.Fatalf("the command changed: %q", admin)
	}
	if _, ok := withEnv([]string{"ssh", "server", "kivora"}, "A=1"); ok {
		t.Fatal("a command without exec cannot take a setting")
	}
}

func TestAdminEnvSaysSkipWithoutExec(t *testing.T) {
	r := &runner{ctx: context.Background(), adminCmd: []string{"ssh", "server", "kivora"}}
	_, err := r.adminEnv("A=1", "", "backup")
	var s skipError
	if !errors.As(err, &s) {
		t.Fatalf("got %v", err)
	}
}

func TestLaptopSaysSkipWithoutSSH(t *testing.T) {
	for _, command := range []string{"kivora-mcp", "ssh host /bin/other", "ssh host"} {
		r := &runner{mcpCmd: command}
		var s skipError
		if _, err := r.laptop(); !errors.As(err, &s) {
			t.Errorf("%q: got %v", command, err)
		}
	}
	r := &runner{mcpCmd: "ssh -T host /usr/local/bin/kivora-mcp"}
	if l, err := r.laptop(); err != nil || l.host != "host" {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestScenarioNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, sc := range scenarios {
		if seen[sc.name] {
			t.Errorf("%s twice", sc.name)
		}
		seen[sc.name] = true
		if sc.destructive && !sc.host {
			t.Errorf("%s is destructive without admin commands", sc.name)
		}
	}
}

func TestPagesKeepWhatTheyReport(t *testing.T) {
	p := &pageServer{}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	defer srv.Close()
	for _, path := range []string{"/plain", "/poser", "/bank", "/alert", "/busy", "/secret", "/link?to=/plain"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("%s: %s, %s", path, resp.Status, resp.Header.Get("Content-Type"))
		}
	}
	if resp, err := http.Get(srv.URL + "/other"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("/other: %v, %v", resp, err)
	}
	if _, ok := p.report("BANK:x"); ok {
		t.Fatal("a report from nowhere")
	}
	resp, err := http.Post(srv.URL+"/report", "text/plain", strings.NewReader("BANK:x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, ok := p.waitReport("BANK:x", time.Second); !ok || got != "BANK:x" {
		t.Fatalf("got %q, %v", got, ok)
	}
	if _, ok := p.waitReport("HOSTILE:x", 10*time.Millisecond); ok {
		t.Fatal("found what no page reported")
	}
}
