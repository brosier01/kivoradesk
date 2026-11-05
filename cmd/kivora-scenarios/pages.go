package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// pageServer serves the pages of the boundary scenarios from this process, which
// runs on the server. The VM reaches it like any private service, through kivora
// allow, at two addresses: to a browser, these are two sites, in two processes.
type pageServer struct {
	only string // addresses given with -pages

	once sync.Once
	err  error
	srv  *http.Server
	port int
	// Addresses the VM reached, each allowed for this run.
	hosts []string

	mu      sync.Mutex
	reports []string
}

// ensure starts the server and opens it to the VM, once. A VM that cannot reach
// this computer means the scenarios cannot conclude.
func (p *pageServer) ensure(r *runner) error {
	p.once.Do(func() { p.err = p.start(r) })
	return p.err
}

func (p *pageServer) start(r *runner) error {
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return err
	}
	p.port = ln.Addr().(*net.TCPAddr).Port
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 10 * time.Second}
	go p.srv.Serve(ln)

	candidates := strings.FieldsFunc(p.only, func(c rune) bool { return c == ',' })
	if len(candidates) == 0 {
		if candidates, err = localAddresses(); err != nil {
			return err
		}
	}
	for _, host := range candidates {
		entry := fmt.Sprintf("%s:%d", host, p.port)
		if _, err := r.admin("", "allow", "add", entry, "kivora-scenarios"); err != nil {
			return fmt.Errorf("allow %s: %w", entry, err)
		}
		if _, err := r.admin("", "ssh", fmt.Sprintf("nc -z -w3 %s %d", host, p.port)); err != nil {
			r.admin("", "allow", "remove", entry)
			continue
		}
		p.hosts = append(p.hosts, host)
		if len(p.hosts) == 2 {
			return nil
		}
	}
	return skip("the VM reached %d of the addresses %v of this computer, two are needed: see -pages", len(p.hosts), candidates)
}

// localAddresses lists the IPv4 addresses of this computer, loopback aside.
func localAddresses() ([]string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var hosts []string
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if v4 := n.IP.To4(); v4 != nil && !v4.IsLoopback() {
				hosts = append(hosts, v4.String())
			}
		}
	}
	return hosts, nil
}

// close removes what start allowed.
func (p *pageServer) close(r *runner) {
	if p.srv == nil {
		return
	}
	p.srv.Close()
	for _, host := range p.hosts {
		r.admin("", "allow", "remove", fmt.Sprintf("%s:%d", host, p.port))
	}
}

// url gives the address of a page on one of the two sites.
func (p *pageServer) url(site int, path string) string {
	return fmt.Sprintf("http://%s:%d%s", p.hosts[site], p.port, path)
}

// origin gives the origin of one of the two sites.
func (p *pageServer) origin(site int) string { return p.url(site, "") }

// waitReport waits until a page has reported something containing text, and
// returns it.
func (p *pageServer) waitReport(text string, d time.Duration) (string, bool) {
	deadline := time.Now().Add(d)
	for {
		if got, ok := p.report(text); ok {
			return got, true
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// report returns what a page has reported containing text.
func (p *pageServer) report(text string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, got := range p.reports {
		if strings.Contains(got, text) {
			return got, true
		}
	}
	return "", false
}

func (p *pageServer) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/report" {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<16))
		p.mu.Lock()
		p.reports = append(p.reports, string(body))
		p.mu.Unlock()
		return
	}
	var page string
	switch req.URL.Path {
	case "/plain":
		page = `<title>Plain page</title><body>Plain page`
	case "/link":
		page = `<title>Links</title><body><a href="` + req.URL.Query().Get("to") + `">next</a>`
	case "/poser":
		page = poserPage
	case "/bank":
		page = bankPage
	case "/alert":
		page = `<title>Dialog</title><script>alert("kivora")</script><body>after the dialog`
	case "/busy":
		page = `<title>Busy</title><script>for(;;);</script><body>never`
	case "/secret":
		page = secretPage
	default:
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, page)
}

const (
	posedText = "FAKE BANK CONTENT"
	realText  = "REAL BANK CONTENT"

	// Hidden frames of this page answer as if they were the page on screen, and its
	// field reports what reaches it. It keeps the focus for itself.
	poserPage = `<title>Article</title><script>
const poser = 'Object.defineProperty(document,"visibilityState",{get:()=>"visible"});document.title="My bank";document.body.innerText="` + posedText + `";';
for (let i = 0; i < 60; i++) {
	const f = document.createElement('iframe');
	document.documentElement.append(f);
	const w = f.contentWindow;
	w.document.write('<body></body>');
	w.document.close();
	w.eval(poser);
	f.style.display = 'none';
}
</script><body>Text of the article <input id=steal autofocus name=steal><script>
const i = document.getElementById('steal');
setInterval(() => { if (document.activeElement !== i) i.focus(); }, 200);
i.addEventListener('input', () => fetch('/report', {method: 'POST', body: 'HOSTILE:' + i.value}));
</script>`

	bankPage = `<title>Real bank</title><body>` + realText + ` <input id=q name=q><script>
const i = document.getElementById('q');
i.addEventListener('input', () => fetch('/report', {method: 'POST', body: 'BANK:' + i.value}));
</script>`

	// What a site can do with a value filled in its form: show it under many
	// encodings, in its text, its title and its address. It reports what it showed:
	// the scenario looks for exactly that.
	secretPage = `<title>Sign in</title><body><input type=password id=pw name=pw>
<button id=go type=button>Send</button><pre id=out></pre><script>
document.getElementById('go').onclick = () => {
	const v = document.getElementById('pw').value;
	const bytes = new TextEncoder().encode(v);
	const uri = encodeURIComponent(v);
	const lines = [
		'ECHO-RAW=' + v,
		'ECHO-URI=' + uri,
		'ECHO-FORM=' + new URLSearchParams({a: v}).toString().slice(2),
		'ECHO-LOWER=' + uri.replace(/%[0-9A-F]{2}/g, m => m.toLowerCase()),
		'ECHO-DOUBLE=' + encodeURIComponent(uri),
		'ECHO-BASE64=' + btoa(String.fromCharCode(...bytes)),
		'ECHO-HEX=' + Array.from(bytes, b => b.toString(16).padStart(2, '0')).join(''),
		'ECHO-UPPER=' + v.toUpperCase(),
		'ECHO-JSON=' + JSON.stringify(v).slice(1, -1),
		'ECHO-HTML=' + v.replace(/&/g, '&amp;').replace(/"/g, '&quot;'),
		'ECHO-NUMERIC=' + v.replace(/[&"]/g, c => '&#' + c.charCodeAt(0) + ';'),
	];
	document.getElementById('out').textContent = lines.join('\n');
	document.title = 'T-' + v;
	history.replaceState(null, '', '?pw=' + uri);
	fetch('/report', {method: 'POST', body: lines.join('\n')});
};
</script>`
)
