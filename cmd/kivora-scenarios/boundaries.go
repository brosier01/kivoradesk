package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	scenarios = append(scenarios, boundaries...)
}

// Boundaries a web page, a shell command or a laptop key must not cross. Each one
// tells the promise from the mere absence of the attack: what it attacks must be
// seen in place, otherwise the scenario fails or says skip.
var boundaries = []scenario{
	{name: "hidden_tab_cannot_pose", host: true, run: hiddenTabCannotPose},
	{name: "page_cannot_hold_the_browser", host: true, run: pageCannotHoldTheBrowser},
	{name: "fifo_cannot_hold_the_session", run: fifoCannotHoldTheSession},
	{name: "shell_leaves_nothing_behind", run: shellLeavesNothingBehind},
	{name: "shell_cannot_nest_namespaces", run: shellCannotNestNamespaces},
	{name: "secret_stays_hidden", host: true, run: secretStaysHidden},
	{name: "shell_text_stays_out_of_syslog", host: true, run: shellTextStaysOutOfSyslog},
	{name: "laptop_key_is_limited", run: laptopKeyIsLimited},

	{name: "backup_retention", host: true, destructive: true, timeout: 20 * time.Minute, run: backupRetention},
	{name: "recover_keeps_the_human_in_control", host: true, destructive: true, timeout: 20 * time.Minute, run: recoverKeepsTheHuman},
	{name: "damaged_data_disk_is_repaired", host: true, destructive: true, timeout: 30 * time.Minute, run: damagedDiskIsRepaired},
}

type tab struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

func (r *runner) tabs() ([]tab, error) {
	out, err := r.call("browser_tabs", nil)
	if err != nil {
		return nil, err
	}
	var tabs []tab
	if err := json.Unmarshal([]byte(jsonList(out)), &tabs); err != nil {
		return nil, fmt.Errorf("browser_tabs: %w: %s", err, oneLine(out))
	}
	return tabs, nil
}

// leaveOnePlainTab closes the tabs a scenario opened, hostile ones included.
func (r *runner) leaveOnePlainTab(p *pageServer) {
	tabs, err := r.tabs()
	if err != nil {
		return
	}
	for i, t := range tabs {
		if i > 0 {
			r.call("browser_close_tab", map[string]any{"id": t.ID})
		}
	}
	r.navigate(p.url(0, "/plain"))
}

// A page in a background tab must neither answer for the visible tab nor receive
// what is typed in it. The world of an earlier page must be gone, which is why
// the browser restarts on the profile in use, and why the hostile page comes after
// a click and not a navigation of the tool.
func hiddenTabCannotPose(r *runner) error {
	p := r.pages
	if err := p.ensure(r); err != nil {
		return err
	}
	defer r.leaveOnePlainTab(p)
	if _, err := r.admin("", "ssh", "doas rc-service -q kivora-browser restart"); err != nil {
		return err
	}
	if _, err := r.navigate(p.url(0, "/link?to="+p.url(1, "/poser"))); err != nil {
		return err
	}
	page, err := r.page(0)
	if err != nil {
		return err
	}
	link, err := element(page, `"next"`)
	if err != nil {
		return err
	}
	if _, err := r.call("browser_click", map[string]any{"element": link}); err != nil {
		return err
	}
	if _, err := r.call("browser_new_tab", map[string]any{"url": p.url(1, "/bank")}); err != nil {
		return err
	}

	seen, err := r.page(0)
	if err != nil {
		return err
	}
	if strings.Contains(seen, posedText) {
		return fmt.Errorf("a background tab answered for the visible one: %s", truncate(oneLine(seen), 200))
	}
	if err := contains(seen, realText); err != nil {
		return err
	}
	tabs, err := r.tabs()
	if err != nil {
		return err
	}
	var active []string
	for _, t := range tabs {
		if t.Active {
			active = append(active, t.URL)
		}
	}
	if len(active) != 1 || !strings.Contains(active[0], "/bank") {
		return fmt.Errorf("the visible tab is the bank, and browser_tabs marks active: %v", active)
	}

	field, err := element(seen, `"q"`)
	if err != nil {
		return err
	}
	text := "typed-" + unique()
	if _, err := r.call("browser_type", map[string]any{"element": field, "text": text}); err != nil {
		return err
	}
	_, reached := p.waitReport("BANK:"+text, 5*time.Second)
	if got, ok := p.report("HOSTILE:" + text); ok {
		return fmt.Errorf("text typed for the visible tab reached the hidden page: %s", got)
	}
	if !reached {
		return fmt.Errorf("text typed for the visible tab never reached it")
	}
	return nil
}

// A page must not hold the browser tools, and what they lock, for the two minutes
// of a call. Its dialog or its busy loop concern its own tab.
func pageCannotHoldTheBrowser(r *runner) error {
	p := r.pages
	if err := p.ensure(r); err != nil {
		return err
	}
	defer r.leaveOnePlainTab(p)
	const limit = 45 * time.Second
	for _, path := range []string{"/alert", "/busy"} {
		if _, err := r.call("browser_new_tab", map[string]any{"url": p.url(0, "/plain")}); err != nil {
			return err
		}
		start := time.Now()
		if _, _, err := r.try("browser_navigate", map[string]any{"url": p.url(0, path)}); err != nil {
			return err
		}
		if took := time.Since(start); took > limit {
			return fmt.Errorf("%s held browser_navigate for %s", path, took.Round(time.Second))
		}
		start = time.Now()
		if _, err := r.call("shell_run", map[string]any{"command": "true"}); err != nil {
			return err
		}
		if took := time.Since(start); took > 15*time.Second {
			return fmt.Errorf("%s held the other tools for %s", path, took.Round(time.Second))
		}
		tabs, err := r.tabs()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		closed := false
		for _, t := range tabs {
			if strings.Contains(t.URL, path) {
				if _, err := r.call("browser_close_tab", map[string]any{"id": t.ID}); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				closed = true
			}
		}
		if !closed {
			return fmt.Errorf("%s is not in the list of tabs: %+v", path, tabs)
		}
	}
	if _, err := r.navigate(p.url(0, "/plain")); err != nil {
		return err
	}
	page, err := r.page(0)
	if err != nil {
		return err
	}
	return contains(page, "Plain page")
}

// A FIFO left in /workspace must not hold files_read or files_write beyond their
// limit, nor the actions behind them. In a session of its own: a failure here must
// not freeze the rest of the suite.
func fifoCannotHoldTheSession(r *runner) error {
	name := "scenario-fifo-" + unique()
	if _, err := r.call("shell_run", map[string]any{"command": "mkfifo " + name}); err != nil {
		return err
	}
	// A helper still blocked on it would stay for ever: open it from both sides.
	defer r.call("shell_run", map[string]any{"command": fmt.Sprintf(`timeout 3 sh -c 'cat %[1]s >/dev/null & echo >%[1]s'; rm -f %[1]s`, name)})
	return r.timed(100*time.Second, func(r *runner) error {
		return r.session(func(s *runner) error {
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{"files_read", map[string]any{"path": name}},
				{"files_write", map[string]any{"path": name, "content": "x"}},
			} {
				start := time.Now()
				_, isErr, err := s.tryWithin(30*time.Second, call.tool, call.args)
				if err != nil {
					return fmt.Errorf("%s on a FIFO, after %s: %w", call.tool, time.Since(start).Round(time.Second), err)
				}
				if !isErr {
					return fmt.Errorf("%s accepted a FIFO", call.tool)
				}
			}
			out, _, err := s.tryWithin(30*time.Second, "shell_run", map[string]any{"command": "echo still-free"})
			if err != nil {
				return fmt.Errorf("the session is stuck after the FIFO: %w", err)
			}
			return contains(out, "still-free")
		})
	})
}

// tryWithin runs a tool with a limit of its own.
func (r *runner) tryWithin(d time.Duration, name string, args map[string]any) (out string, isErr bool, err error) {
	err = r.timed(d, func(sub *runner) error {
		var callErr error
		out, isErr, callErr = sub.try(name, args)
		return callErr
	})
	return out, isErr, err
}

// shmProgram is a small x86-64 Linux program that leaves a segment of System V
// shared memory of 1 MiB behind, and exits. The VM has no tool that creates one.
func shmProgram() []byte {
	const base, headers = 0x400000, 64 + 56
	code := []byte{
		0xb8, 0x1d, 0x00, 0x00, 0x00, // mov eax, 29 (shmget)
		0x31, 0xff, // xor edi, edi (IPC_PRIVATE)
		0xbe, 0x00, 0x00, 0x10, 0x00, // mov esi, 1 MiB
		0xba, 0x80, 0x03, 0x00, 0x00, // mov edx, IPC_CREAT|0600
		0x0f, 0x05, // syscall
		0xb8, 0x3c, 0x00, 0x00, 0x00, // mov eax, 60 (exit)
		0x31, 0xff, // xor edi, edi
		0x0f, 0x05, // syscall
	}
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	put := func(values ...any) {
		for _, v := range values {
			binary.Write(&b, binary.LittleEndian, v)
		}
	}
	size := uint64(headers + len(code))
	put(uint16(2), uint16(62), uint32(1), uint64(base+headers), uint64(64), uint64(0), uint32(0))
	put(uint16(64), uint16(56), uint16(1), uint16(64), uint16(0), uint16(0))
	put(uint32(1), uint32(5), uint64(0), uint64(base), uint64(base), size, size, uint64(0x1000))
	b.Write(code)
	return b.Bytes()
}

// What a shell command leaves in shared memory or in a temporary directory must
// end with its call, like its processes.
func shellLeavesNothingBehind(r *runner) error {
	id := unique()
	prog := "scenario-shm-" + id
	places := []string{"/dev/shm/scenario-" + id, "/tmp/scenario-" + id, "/var/tmp/scenario-" + id}
	var first strings.Builder
	fmt.Fprintf(&first, "echo %s | base64 -d >%s && chmod 755 %s && ./%s; echo \"shm-rc=$?\"; rm -f %s\n",
		base64.StdEncoding.EncodeToString(shmProgram()), prog, prog, prog, prog)
	first.WriteString("echo \"shm-count=$(tail -n +2 /proc/sysvipc/shm | wc -l)\"\n")
	for _, place := range places {
		fmt.Fprintf(&first, "echo x >%s 2>/dev/null && [ -f %s ] && echo written=%s\n", place, place, place)
	}
	out, err := r.call("shell_run", map[string]any{"command": first.String()})
	if err != nil {
		return err
	}
	defer r.call("shell_run", map[string]any{"command": "awk 'NR>1{print $2}' /proc/sysvipc/shm | xargs -r -n1 ipcrm -m 2>/dev/null; rm -f " + strings.Join(places, " ")})
	if !strings.Contains(out, "shm-rc=0") || strings.Contains(out, "shm-count=0") {
		return skip("no shared memory segment could be left to look for: %s", truncate(oneLine(out), 150))
	}
	for _, place := range places[:2] {
		if err := contains(out, "written="+place); err != nil {
			return err
		}
	}

	var second strings.Builder
	second.WriteString("echo \"shm-count=$(tail -n +2 /proc/sysvipc/shm | wc -l)\"\n")
	for _, place := range places {
		fmt.Fprintf(&second, "[ -e %s ] && echo left=%s\n", place, place)
	}
	second.WriteString("echo end\n")
	if out, err = r.call("shell_run", map[string]any{"command": second.String()}); err != nil {
		return err
	}
	if err := contains(out, "end"); err != nil {
		return err
	}
	if strings.Contains(out, "left=") || !strings.Contains(out, "shm-count=0") {
		return fmt.Errorf("a shell command left something behind: %s", truncate(oneLine(out), 300))
	}
	return nil
}

// A shell command must not get kernel features through namespaces of its own.
func shellCannotNestNamespaces(r *runner) error {
	out, err := r.call("shell_run", map[string]any{"command": `command -v unshare >/dev/null || echo NO-UNSHARE
unshare -Urn true 2>/dev/null && echo NESTED-USER
unshare -n true 2>/dev/null && echo NESTED-NET
unshare -m true 2>/dev/null && echo NESTED-MOUNT
echo "io-uring=$(cat /proc/sys/kernel/io_uring_disabled 2>/dev/null)"
echo end`})
	if err != nil {
		return err
	}
	if strings.Contains(out, "NO-UNSHARE") {
		return skip("no unshare command in the VM")
	}
	if strings.Contains(out, "NESTED-") {
		return fmt.Errorf("a shell command got namespaces of its own: %s", truncate(oneLine(out), 200))
	}
	return contains(out, "io-uring=2", "end")
}

// A filled value must not come back to the model, in this session or the next one,
// nor stay in the audit log. The vault only accepts https origins, so a real site
// echoes it. Its plain letters show any form that leaves them readable; the encodings
// that hide them are covered by the tests of the vault.
func secretStaysHidden(r *runner) error {
	mark := "U" + unique()
	value := "Sc~7!a&b/" + mark + "@example.com"
	name := "scenario-echo-" + unique()
	if _, err := r.admin(value, "secret", "set", "--field", "text", name, "https://httpbin.org"); err != nil {
		return err
	}
	defer r.admin("", "secret", "delete", name)

	if _, err := r.navigate("https://httpbin.org/forms/post"); err != nil {
		return err
	}
	page, err := r.page(0)
	if err != nil {
		return err
	}
	field, err := element(page, `"custemail"`)
	if err != nil {
		return err
	}
	var results []string
	out, err := r.call("secrets_fill", map[string]any{"name": name, "element": field})
	if err != nil {
		return err
	}
	results = append(results, out)
	send, err := element(page, `button "Submit order"`)
	if err != nil {
		return err
	}
	if out, err = r.call("browser_click", map[string]any{"element": send}); err != nil {
		return err
	}
	results = append(results, out)

	look := func(s *runner) error {
		page, err := s.page(0)
		if err != nil {
			return err
		}
		// The site shows what it received: the place stays, not the value.
		if err := contains(page, "[secret hidden]"); err != nil {
			return err
		}
		results = append(results, page)
		for _, tool := range []string{"browser_tabs", "computer_get_state", "computer_windows"} {
			out, err := s.call(tool, nil)
			if err != nil {
				return err
			}
			results = append(results, out)
		}
		return nil
	}
	if err := look(r); err != nil {
		return err
	}
	if err := r.session(look); err != nil {
		return fmt.Errorf("next session: %w", err)
	}
	for _, out := range results {
		if strings.Contains(out, mark) {
			return fmt.Errorf("the value reached the model: %s", oneLine(out))
		}
	}

	// Even a model that knows the value leaves it out of the log.
	file := "scenario-echo-" + unique() + ".txt"
	if _, err := r.call("files_write", map[string]any{"path": file, "content": value}); err != nil {
		return err
	}
	defer r.call("files_delete", map[string]any{"path": file})
	logged, err := r.admin("", "ssh", fmt.Sprintf(`doas sh -c 'grep -l -- %q /var/lib/kivora-audit/audit-*.jsonl || true'`, mark))
	if err != nil {
		return err
	}
	if strings.TrimSpace(logged) != "" {
		return fmt.Errorf("the value is in the audit log: %s", oneLine(logged))
	}
	return nil
}

// doas must not copy the text of shell commands to the system log, where the audit
// log's masking does not apply.
func shellTextStaysOutOfSyslog(r *runner) error {
	id := unique()
	if _, err := r.call("shell_run", map[string]any{"command": "true scenario-syslog-" + id}); err != nil {
		return err
	}
	// doas logs this command too: the bracket keeps it from matching itself.
	out, err := r.admin("", "ssh", fmt.Sprintf(`doas sh -c '[ -f /var/log/messages ] || echo NO-LOG; grep -c -- "scenario-syslo[g]-%s" /var/log/messages || true'`, id))
	if err != nil {
		return err
	}
	if strings.Contains(out, "NO-LOG") {
		return skip("no /var/log/messages in the VM")
	}
	if strings.TrimSpace(out) != "0" {
		return fmt.Errorf("the text of a shell command is in the system log: %s", oneLine(out))
	}
	return nil
}

// laptopCommand is the SSH access of the laptop, taken apart.
type laptopCommand struct {
	opts []string
	host string
}

// laptop finds the SSH access of the laptop: -ssh, or -mcp without its command.
func (r *runner) laptop() (laptopCommand, error) {
	command := r.sshCmd
	if command == "" {
		command = r.mcpCmd
	}
	opts, host, remote, ok := sshParts(strings.Fields(command))
	if !ok || (r.sshCmd == "" && (len(remote) != 1 || !strings.HasSuffix(remote[0], "/kivora-mcp"))) {
		return laptopCommand{}, skip("no SSH access found in %q: see -ssh", command)
	}
	return laptopCommand{opts: opts, host: host}, nil
}

// sshParts splits an ssh command line into its options, its host and what it runs.
func sshParts(argv []string) (opts []string, host string, remote []string, ok bool) {
	if len(argv) < 2 || filepath.Base(argv[0]) != "ssh" {
		return nil, "", nil, false
	}
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			return opts, a, argv[i+1:], true
		}
		opts = append(opts, a)
		if len(a) == 2 && strings.Contains("BbcDEeFIiJLlmOoPpQRSWw", a[1:]) {
			i++
			if i == len(argv) {
				return nil, "", nil, false
			}
			opts = append(opts, argv[i])
		}
	}
	return nil, "", nil, false
}

// copyOpts gives the options of ssh that scp and sftp understand.
func copyOpts(opts []string) []string {
	var out []string
	for i := 0; i < len(opts); i++ {
		switch a := opts[i]; a {
		case "-F", "-i", "-J", "-o":
			out = append(out, a, opts[i+1])
			i++
		case "-p":
			out = append(out, "-P", opts[i+1])
			i++
		case "-l":
			out = append(out, "-o", "User="+opts[i+1])
			i++
		default:
			if strings.HasPrefix(a, "-o") && len(a) > 2 {
				out = append(out, a)
			}
		}
	}
	return out
}

// ssh runs a command through the laptop's access. Without one, it asks for a login
// shell, and gives it a command to read.
func (l laptopCommand) ssh(r *runner, args ...string) (string, error) {
	argv := append([]string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ClearAllForwardings=yes"}, l.opts...)
	return r.run(append(argv, l.host), "echo STDIN-SHELL\n", args...)
}

// A key of a laptop serves the MCP server, file transfers and the tunnel to the
// live view: an agent with a shell on the laptop gets nothing more through it.
func laptopKeyIsLimited(r *runner) error {
	l, err := r.laptop()
	if err != nil {
		return err
	}
	mark := "SHELL-" + unique()
	for _, command := range [][]string{
		nil, // a login shell, reading its commands
		{"echo " + mark},
		{"id"},
		{"doas -u kivora-vault /usr/local/bin/kivora-vault list"},
		{"DISPLAY=:1 xdotool getmouselocation"},
		{"/usr/local/bin/kivora-mcp -version"},
		{"/usr/local/bin/kivora-mcp worker exec -- id"},
		{"/usr/local/bin/kivora-mcp; echo " + mark},
		{"scp -f /workspace"},
	} {
		out, err := l.ssh(r, command...)
		if err == nil || strings.Contains(out, mark) || strings.Contains(out, "uid=") || strings.Contains(out, "STDIN-SHELL") {
			return fmt.Errorf("the laptop key ran %q: %s", command, truncate(oneLine(out), 200))
		}
		// What ssh itself exits with when it cannot connect: nothing was refused.
		if strings.Contains(err.Error(), "exit status 255") && strings.Contains(out, "Connection") {
			return fmt.Errorf("the laptop key could not even connect: %w", err)
		}
	}

	// The MCP server.
	if err := r.session(func(s *runner) error {
		out, err := s.call("computer_get_state", nil)
		if err != nil {
			return err
		}
		return contains(out, `"owner"`)
	}); err != nil {
		return fmt.Errorf("MCP through the laptop key: %w", err)
	}

	// File transfers, in /workspace.
	dir, err := os.MkdirTemp("", "kivora-scenarios")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := "scenario-copy-" + unique() + ".txt"
	if _, err := r.call("files_write", map[string]any{"path": file, "content": mark}); err != nil {
		return err
	}
	defer r.call("files_delete", map[string]any{"path": file})
	scp := append([]string{"scp", "-q", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, copyOpts(l.opts)...)
	if _, err := r.run(scp, "", l.host+":/workspace/"+file, filepath.Join(dir, "scp")); err != nil {
		return fmt.Errorf("scp through the laptop key: %w", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "scp")); string(got) != mark {
		return fmt.Errorf("scp brought %q", got)
	}
	sftp := append([]string{"sftp", "-q", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, copyOpts(l.opts)...)
	if _, err := r.run(append(sftp, "-b", "-", l.host), "get "+file+" "+filepath.Join(dir, "sftp")+"\n"); err != nil {
		return fmt.Errorf("sftp through the laptop key: %w", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sftp")); string(got) != mark {
		return fmt.Errorf("sftp brought %q", got)
	}
	// The old protocol runs a command, which a transfer does not need.
	if _, err := r.run(append(scp, "-O"), "", l.host+":/workspace/"+file, filepath.Join(dir, "old")); err == nil {
		return fmt.Errorf("scp -O ran through the laptop key")
	}

	// A tunnel runs no command: it must stay up.
	tunnelArgs := append(append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, l.opts...), "-o", "ClearAllForwardings=yes", "-N", l.host)
	tunnel := exec.CommandContext(r.ctx, "ssh", tunnelArgs...)
	var stderr bytes.Buffer
	tunnel.Stderr = &stderr
	if err := tunnel.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- tunnel.Wait() }()
	select {
	case err := <-exited:
		return fmt.Errorf("ssh -N through the laptop key ended: %v: %s", err, oneLine(stderr.String()))
	case <-time.After(3 * time.Second):
		tunnel.Process.Kill()
		<-exited
	}
	// And the gateway answers through the one the suite uses.
	req, _ := http.NewRequestWithContext(r.ctx, "GET", r.web+"/", nil)
	resp, err := webClient.Do(req)
	if err != nil {
		return fmt.Errorf("the tunnel to the gateway: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("the gateway answered %s without a session", resp.Status)
	}
	return nil
}

// Backup settings that make no sense must be refused, not lose the backups. And a
// restore must keep the backup it restores.
func backupRetention(r *runner) error {
	if _, err := r.admin("", "backup"); err != nil {
		return err
	}
	if _, err := r.admin("", "backup"); err != nil {
		return err
	}
	list := func() ([]string, error) {
		out, err := r.admin("", "restore", "--list")
		if err != nil {
			return nil, err
		}
		var names []string
		for line := range strings.Lines(out) {
			if name := strings.TrimSpace(line); strings.HasSuffix(name, ".gpg") {
				names = append(names, name)
			}
		}
		return names, nil
	}
	before, err := list()
	if err != nil {
		return err
	}
	if len(before) < 2 {
		return fmt.Errorf("two backups made, %d listed", len(before))
	}
	for _, setting := range []string{"0", "-1", "abc"} {
		if out, err := r.adminEnv("KIVORA_BACKUP_KEEP="+setting, "", "backup"); err == nil {
			return fmt.Errorf("KIVORA_BACKUP_KEEP=%s was accepted: %s", setting, truncate(oneLine(out), 150))
		} else if _, ok := err.(skipError); ok {
			return err
		}
		after, err := list()
		if err != nil {
			return err
		}
		if strings.Join(after, " ") != strings.Join(before, " ") {
			return fmt.Errorf("KIVORA_BACKUP_KEEP=%s changed the backups: %v, then %v", setting, before, after)
		}
	}
	oldest := before[len(before)-1]
	if _, err := r.adminEnv("KIVORA_BACKUP_KEEP=1", "", "restore", "--yes", oldest); err != nil {
		return err
	}
	after, err := list()
	if err != nil {
		return err
	}
	if !strings.Contains(strings.Join(after, " "), oldest) {
		return fmt.Errorf("the restore removed the backup it restored: %v", after)
	}
	return nil
}

// refusedForControl checks that an action is refused because the agent does not
// hold the desktop, and not for some other reason.
func refusedForControl(s *runner, name string, args map[string]any) error {
	out, err := s.refused(name, args)
	if err != nil {
		return err
	}
	if strings.Contains(out, "unreachable") || !strings.Contains(out, "control") {
		return fmt.Errorf("%s was refused, not for control: %s", name, truncate(oneLine(out), 200))
	}
	return nil
}

// A rebuild of the system must leave the human in control, and the files in place.
func recoverKeepsTheHuman(r *runner) error {
	file := "scenario-recover-" + unique() + ".txt"
	if _, err := r.call("files_write", map[string]any{"path": file, "content": "still here"}); err != nil {
		return err
	}
	if err := r.setOwner("human"); err != nil {
		return err
	}
	defer r.setOwner("agent")
	if out, err := r.admin("", "recover", "--yes"); err != nil {
		return fmt.Errorf("recover: %w: %s", err, truncate(oneLine(out), 200))
	}
	if err := r.session(func(s *runner) error {
		for _, action := range []struct {
			name string
			args map[string]any
		}{
			{"shell_run", map[string]any{"command": "echo the-agent-acts"}},
			{"computer_click", map[string]any{"x": 10, "y": 10}},
		} {
			if err := refusedForControl(s, action.name, action.args); err != nil {
				return fmt.Errorf("after recover: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := r.setOwner("agent"); err != nil {
		return err
	}
	return r.session(func(s *runner) error {
		defer s.call("files_delete", map[string]any{"path": file})
		out, err := s.call("files_read", map[string]any{"path": file})
		if err != nil {
			return err
		}
		return contains(out, "still here")
	})
}

// A data disk with a damaged superblock must never be formatted by the tools meant
// to save it. kivora recover must keep it, and kivora repair-data bring it back.
func damagedDiskIsRepaired(r *runner) error {
	file := "scenario-repair-" + unique() + ".txt"
	if _, err := r.call("files_write", map[string]any{"path": file, "content": "still here"}); err != nil {
		return err
	}
	// The way back, should this go wrong on the instance: the scenario restores nothing.
	if _, err := r.admin("", "backup"); err != nil {
		return err
	}
	disk, err := r.admin("", "ssh", `for b in /sys/block/vd*; do [ "$(cat $b/serial 2>/dev/null)" = kivora-data ] && echo /dev/${b##*/}; done; true`)
	if err != nil {
		return err
	}
	disk = strings.TrimSpace(disk)
	if !strings.HasPrefix(disk, "/dev/vd") {
		return fmt.Errorf("no data disk found: %q", disk)
	}
	// The connection ends with the VM.
	r.timed(20*time.Second, func(sub *runner) error {
		sub.admin("", "ssh", fmt.Sprintf(`doas sh -c "sync; dd if=/dev/zero of=%s bs=1024 seek=1 count=1 conv=fsync 2>/dev/null; reboot -f"`, disk))
		return nil
	})
	if err := r.waitForAdmin(5 * time.Minute); err != nil {
		return fmt.Errorf("after the damage: %w", err)
	}
	if out, err := r.admin("", "doctor"); err == nil || !strings.Contains(out, "mount /data") {
		return fmt.Errorf("doctor on a damaged disk: %v: %s", err, truncate(oneLine(out), 300))
	}

	// recover answers as it can: what counts is what is left of the disk.
	recoverOut, recoverErr := r.admin("", "recover", "--yes")
	if err := r.waitForAdmin(5 * time.Minute); err != nil {
		return fmt.Errorf("after recover: %w: %v: %s", err, recoverErr, truncate(oneLine(recoverOut), 200))
	}
	if out, err := r.admin("", "repair-data", "--yes"); err != nil {
		return fmt.Errorf("repair-data: %w: %s", err, truncate(oneLine(out), 300))
	}
	if err := r.waitForAdmin(5 * time.Minute); err != nil {
		return fmt.Errorf("after repair-data: %w", err)
	}
	out, err := r.admin("", "ssh", "doas cat /workspace/"+file)
	if err != nil {
		return fmt.Errorf("the file did not come back: %w", err)
	}
	if err := contains(out, "still here"); err != nil {
		return err
	}
	if out, err := r.admin("", "doctor"); err != nil {
		return fmt.Errorf("doctor after the repair: %w: %s", err, truncate(oneLine(out), 300))
	}
	r.admin("", "ssh", "doas rm -f /workspace/"+file)
	return nil
}

// waitForAdmin waits until the VM answers the admin commands.
func (r *runner) waitForAdmin(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		var err error
		r.timed(20*time.Second, func(sub *runner) error {
			_, err = sub.admin("", "ssh", "true")
			return nil
		})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the VM did not answer within %s: %w", d, err)
		}
		time.Sleep(3 * time.Second)
	}
}
