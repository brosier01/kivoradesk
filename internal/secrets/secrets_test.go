package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeOrigin(t *testing.T) {
	ok := map[string]string{
		"https://Accounts.Example.com/login?x=1": "https://accounts.example.com",
		"https://example.com:443":                "https://example.com",
		"https://example.com:8443/path":          "https://example.com:8443",
		"http://localhost:3000":                  "http://localhost:3000",
		"https://münchen.de/login":               "https://xn--mnchen-3ya.de",
		"https://xn--mnchen-3ya.de":              "https://xn--mnchen-3ya.de",
		"https://my_app.example.com":             "https://my_app.example.com",
		"https://[2001:db8::1]:8443/x":           "https://[2001:db8::1]:8443",
		"https://[2001:db8::1]":                  "https://[2001:db8::1]",
		"https://[2001:DB8::1]:443":              "https://[2001:db8::1]",
		"https://203.0.113.9:8443":               "https://203.0.113.9:8443",
	}
	for in, want := range ok {
		if got, err := NormalizeOrigin(in); err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://example.com", "example.com", "ftp://example.com", "javascript:alert(1)", "",
		"https://", "https://:", "https://:8443", "https://*.example.com", "https://example.com:0", "https://example.com:99999"} {
		if got, err := NormalizeOrigin(bad); err == nil {
			t.Errorf("NormalizeOrigin(%q) = %q, want an error", bad, got)
		}
	}
}

// Two origins must not collapse into one: the port is not a part of the address.
func TestNormalizeOriginKeepsIPv6PortApart(t *testing.T) {
	a, errA := NormalizeOrigin("https://[2001:db8::1]:8443")
	b, errB := NormalizeOrigin("https://[2001:db8::1:8443]")
	if errA != nil || errB != nil || a == b {
		t.Fatalf("%q, %v and %q, %v", a, errA, b, errB)
	}
}

func TestStoreBindsSecretsToOrigins(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state", "secrets.json")}
	if err := s.Set("bank-password", "", []string{"https://bank.example/login"}, "s3cret"); err != nil {
		t.Fatal(err)
	}

	want := Grant{Value: "s3cret", Origin: "https://bank.example", Field: FieldPassword}
	if g, err := s.ValueFor("bank-password", "https://bank.example/accounts?id=4"); err != nil || g != want {
		t.Fatalf("ValueFor on the right origin = %+v, %v", g, err)
	}
	for _, page := range []string{
		"https://evil.example/bank.example",
		"https://bank.example.evil.example/",
		"http://bank.example/",
		"https://bank.example:8443/",
	} {
		if g, err := s.ValueFor("bank-password", page); err == nil {
			t.Errorf("ValueFor on %s returned %q", page, g.Value)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error leaks the value: %v", err)
		}
	}

	infos, err := s.List()
	if err != nil || len(infos) != 1 || infos[0].Name != "bank-password" || infos[0].Origins[0] != "https://bank.example" || infos[0].Field != FieldPassword {
		t.Fatalf("List = %+v, %v", infos, err)
	}
	st, err := os.Stat(s.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store file mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}

	if err := s.Delete("bank-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValueFor("bank-password", "https://bank.example/"); err == nil {
		t.Fatal("deleted secret still usable")
	}
}

// A site with a name in another script is known to the browser by its ASCII form.
func TestSecretOfAnInternationalSite(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "secrets.json")}
	if err := s.Set("idn", "", []string{"https://münchen.de"}, "v"); err != nil {
		t.Fatal(err)
	}
	if g, err := s.ValueFor("idn", "https://xn--mnchen-3ya.de/login"); err != nil || g.Origin != "https://xn--mnchen-3ya.de" {
		t.Fatalf("ValueFor = %+v, %v", g, err)
	}
}

// Commands running at once must not lose each other's changes.
func TestConcurrentChangesAreAllKept(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "secrets.json")}
	var wg sync.WaitGroup
	const n = 16
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := &Store{Path: s.Path}
			if err := store.Set(fmt.Sprintf("s%d", i), "", []string{"https://a.example"}, "v"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	infos, err := s.List()
	if err != nil || len(infos) != n {
		t.Fatalf("%d secrets kept of %d: %v", len(infos), n, err)
	}
	if err := s.Delete("s0"); err != nil {
		t.Fatal(err)
	}
	if infos, _ = s.List(); len(infos) != n-1 {
		t.Fatalf("%d secrets after a delete", len(infos))
	}
}

func TestSetValidates(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "secrets.json")}
	cases := []struct {
		name, field, value string
		origins            []string
	}{
		{"", "", "v", []string{"https://a.example"}},
		{"bad name", "", "v", []string{"https://a.example"}},
		{"ok", "", "", []string{"https://a.example"}},
		{"ok", "", "v", nil},
		{"ok", "", "v", []string{"http://a.example"}},
		{"ok", "textarea", "v", []string{"https://a.example"}},
		{"ok", "", "bad\xff", []string{"https://a.example"}},
		{"ok", "", "nul\x00", []string{"https://a.example"}},
		{"ok", "", "v", []string{"https://*.example.com"}},
	}
	for _, c := range cases {
		if err := s.Set(c.name, c.field, c.origins, c.value); err == nil {
			t.Errorf("Set(%q, %q, %v, %q) succeeded", c.name, c.field, c.origins, c.value)
		}
	}
}

// An entry stored before field kinds existed may only go into password fields.
func TestOlderEntriesDefaultToPasswordFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"old": {"origins": ["https://a.example"], "value": "v"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := (&Store{Path: path}).ValueFor("old", "https://a.example/")
	if err != nil || g.Field != FieldPassword {
		t.Fatalf("ValueFor = %+v, %v", g, err)
	}
}

func TestClientReportsTheHelper(t *testing.T) {
	c := &Client{Prefix: []string{"sh", "-c", `cat >/dev/null; echo '{"value":"v","origin":"https://a.example","field":"text"}'`, "_"}}
	g, err := c.ValueFor(context.Background(), "n", "https://a.example/")
	if err != nil || g.Value != "v" || g.Field != FieldText {
		t.Fatalf("ValueFor = %+v, %v", g, err)
	}
	c.Prefix = []string{"sh", "-c", `cat >/dev/null; echo 'may only be used on https://a.example' >&2; exit 1`, "_"}
	if _, err := c.ValueFor(context.Background(), "n", "https://b.example/"); err == nil || !strings.Contains(err.Error(), "may only be used") {
		t.Fatalf("refusal = %v", err)
	}
}
