package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeOrigin(t *testing.T) {
	ok := map[string]string{
		"https://Accounts.Example.com/login?x=1": "https://accounts.example.com",
		"https://example.com:443":                "https://example.com",
		"https://example.com:8443/path":          "https://example.com:8443",
		"http://localhost:3000":                  "http://localhost:3000",
	}
	for in, want := range ok {
		if got, err := NormalizeOrigin(in); err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://example.com", "example.com", "ftp://example.com", "javascript:alert(1)", ""} {
		if got, err := NormalizeOrigin(bad); err == nil {
			t.Errorf("NormalizeOrigin(%q) = %q, want an error", bad, got)
		}
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
