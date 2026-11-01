// Package secrets stores credentials that the agent may fill into web pages
// without ever reading them.
//
// Each secret is bound to the origins (scheme, host and port) where it may
// be filled, and to the kind of field it may go into. The store is a JSON file
// readable only by the vault account (cmd/kivora-vault), not by the desktop
// account, whose browser the agent drives. The human manages it from the
// server; the agent only sees names, origins and field kinds.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Field kinds say which inputs may receive a secret.
const (
	// FieldPassword is for password inputs only, which browsers neither echo
	// nor let anyone copy. It is the default.
	FieldPassword = "password"
	// FieldText also allows inputs declared as a login name, an email address
	// or a one-time code. Sites may display what these contain.
	FieldText = "text"
)

// Secret is one stored credential.
type Secret struct {
	Origins []string `json:"origins"`
	Value   string   `json:"value"`
	Field   string   `json:"field,omitempty"` // empty means FieldPassword
}

// Info is what the agent may know about a secret.
type Info struct {
	Name    string   `json:"name"`
	Origins []string `json:"origins"`
	Field   string   `json:"field"`
}

// Request asks for the value of a secret on behalf of a page.
type Request struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Grant is a value handed out for one fill.
type Grant struct {
	Value  string `json:"value"`
	Origin string `json:"origin"`
	Field  string `json:"field"`
}

// Store is a secrets file.
type Store struct {
	Path string
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// NormalizeOrigin reduces a URL or origin to scheme://host[:port], dropping
// default ports. Only https origins are accepted, plus http on localhost.
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid origin %q (expected e.g. https://accounts.example.com)", raw)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	switch {
	case u.Scheme == "https":
		if port == "443" {
			port = ""
		}
	case u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1"):
		if port == "80" {
			port = ""
		}
	default:
		return "", fmt.Errorf("origin %q must use https", raw)
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

func (s *Store) load() (map[string]Secret, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Secret{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Secret{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.Path, err)
	}
	return m, nil
}

func (s *Store) save(m map[string]Secret) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

func (sec Secret) field() string {
	if sec.Field == "" {
		return FieldPassword
	}
	return sec.Field
}

// Set creates or replaces a secret. An empty field means FieldPassword.
func (s *Store) Set(name, field string, origins []string, value string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid name %q (letters, digits, . _ -; up to 64 characters)", name)
	}
	if field == "" {
		field = FieldPassword
	}
	if field != FieldPassword && field != FieldText {
		return fmt.Errorf("invalid field kind %q (want %s or %s)", field, FieldPassword, FieldText)
	}
	if value == "" {
		return errors.New("empty value")
	}
	if len(origins) == 0 {
		return errors.New("at least one origin is required")
	}
	var norm []string
	for _, o := range origins {
		n, err := NormalizeOrigin(o)
		if err != nil {
			return err
		}
		if !slices.Contains(norm, n) {
			norm = append(norm, n)
		}
	}
	m, err := s.load()
	if err != nil {
		return err
	}
	m[name] = Secret{Origins: norm, Value: value, Field: field}
	return s.save(m)
}

// Delete removes a secret.
func (s *Store) Delete(name string) error {
	m, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return fmt.Errorf("no secret named %q", name)
	}
	delete(m, name)
	return s.save(m)
}

// List returns names, origins and field kinds, sorted by name. Values are not
// included.
func (s *Store) List() ([]Info, error) {
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	infos := []Info{}
	for name, sec := range m {
		infos = append(infos, Info{Name: name, Origins: sec.Origins, Field: sec.field()})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

// ValueFor returns a grant for a secret if pageURL is on one of its origins.
func (s *Store) ValueFor(name, pageURL string) (Grant, error) {
	m, err := s.load()
	if err != nil {
		return Grant{}, err
	}
	sec, ok := m[name]
	if !ok {
		return Grant{}, fmt.Errorf("no secret named %q; ask the human to add it", name)
	}
	origin, err := NormalizeOrigin(pageURL)
	if err != nil || !slices.Contains(sec.Origins, origin) {
		return Grant{}, fmt.Errorf("secret %q may only be used on %s, not on this page",
			name, strings.Join(sec.Origins, ", "))
	}
	return Grant{Value: sec.Value, Origin: origin, Field: sec.field()}, nil
}
