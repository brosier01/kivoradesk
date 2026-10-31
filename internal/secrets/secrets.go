// Package secrets stores credentials that the agent may fill into web pages
// without ever reading them.
//
// Each secret is bound to the origins (scheme, host and port) where it may
// be filled. The store is a JSON file readable only by the desktop account;
// the human manages it from the server, and the agent only sees names and
// origins.
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

// Secret is one stored credential.
type Secret struct {
	Origins []string `json:"origins"`
	Value   string   `json:"value"`
}

// Info is what the agent may know about a secret.
type Info struct {
	Name    string   `json:"name"`
	Origins []string `json:"origins"`
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

// Set creates or replaces a secret.
func (s *Store) Set(name string, origins []string, value string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid name %q (letters, digits, . _ -; up to 64 characters)", name)
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
	m[name] = Secret{Origins: norm, Value: value}
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

// List returns names and origins, sorted by name. Values are not included.
func (s *Store) List() ([]Info, error) {
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	infos := []Info{}
	for name, sec := range m {
		infos = append(infos, Info{Name: name, Origins: sec.Origins})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

// ValueFor returns the value of a secret and the matching origin if pageURL
// is on one of the secret's origins.
func (s *Store) ValueFor(name, pageURL string) (value, origin string, err error) {
	m, err := s.load()
	if err != nil {
		return "", "", err
	}
	sec, ok := m[name]
	if !ok {
		return "", "", fmt.Errorf("no secret named %q; ask the human to add it", name)
	}
	origin, err = NormalizeOrigin(pageURL)
	if err != nil || !slices.Contains(sec.Origins, origin) {
		return "", "", fmt.Errorf("secret %q may only be used on %s, not on this page (%s)",
			name, strings.Join(sec.Origins, ", "), pageURL)
	}
	return sec.Value, origin, nil
}
