package secrets

import (
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
	if err := s.Set("bank-password", []string{"https://bank.example/login"}, "s3cret"); err != nil {
		t.Fatal(err)
	}

	if v, o, err := s.ValueFor("bank-password", "https://bank.example/accounts?id=4"); err != nil || v != "s3cret" || o != "https://bank.example" {
		t.Fatalf("ValueFor on the right origin = %q, %q, %v", v, o, err)
	}
	for _, page := range []string{
		"https://evil.example/bank.example",
		"https://bank.example.evil.example/",
		"http://bank.example/",
		"https://bank.example:8443/",
	} {
		if v, _, err := s.ValueFor("bank-password", page); err == nil {
			t.Errorf("ValueFor on %s returned %q", page, v)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error leaks the value: %v", err)
		}
	}

	infos, err := s.List()
	if err != nil || len(infos) != 1 || infos[0].Name != "bank-password" || infos[0].Origins[0] != "https://bank.example" {
		t.Fatalf("List = %+v, %v", infos, err)
	}
	st, err := os.Stat(s.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store file mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}

	if err := s.Delete("bank-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ValueFor("bank-password", "https://bank.example/"); err == nil {
		t.Fatal("deleted secret still usable")
	}
}

func TestSetValidates(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "secrets.json")}
	cases := []struct {
		name, value string
		origins     []string
	}{
		{"", "v", []string{"https://a.example"}},
		{"bad name", "v", []string{"https://a.example"}},
		{"ok", "", []string{"https://a.example"}},
		{"ok", "v", nil},
		{"ok", "v", []string{"http://a.example"}},
	}
	for _, c := range cases {
		if err := s.Set(c.name, c.origins, c.value); err == nil {
			t.Errorf("Set(%q, %v, %q) succeeded", c.name, c.origins, c.value)
		}
	}
}
