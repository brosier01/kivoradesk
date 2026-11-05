package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const (
	// A sign-in link works once, shortly after it is made: it goes through a
	// terminal and stays in the browser's history.
	loginCodeTTL = 5 * time.Minute
	sessionTTL   = 30 * 24 * time.Hour

	codesDir    = "codes"
	sessionsDir = "sessions"
)

// sessionStore keeps sign-in codes and browser sessions as files named after
// the hash of their secret, so that reading the directory does not give access.
// Files let the admin commands and the running gateway share them, and removing
// a file is what makes a code work only once.
type sessionStore struct {
	dir string
}

func hashName(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// issue creates a secret of the given kind (codesDir or sessionsDir).
func (s *sessionStore) issue(kind string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)
	dir := filepath.Join(s.dir, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, hashName(secret)), nil, 0o600); err != nil {
		return "", err
	}
	return secret, nil
}

// pending reports whether code is a valid sign-in code, without using it up.
func (s *sessionStore) pending(code string) bool {
	if code == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(s.dir, codesDir, hashName(code)))
	return err == nil && time.Since(st.ModTime()) <= loginCodeTTL
}

// redeem reports whether code is a valid sign-in code, and uses it up.
func (s *sessionStore) redeem(code string) bool {
	if code == "" {
		return false
	}
	path := filepath.Join(s.dir, codesDir, hashName(code))
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	// Only one request can remove the file.
	if err := os.Remove(path); err != nil {
		return false
	}
	return time.Since(st.ModTime()) <= loginCodeTTL
}

// valid reports whether secret is a current browser session.
func (s *sessionStore) valid(secret string) bool {
	if secret == "" {
		return false
	}
	path := filepath.Join(s.dir, sessionsDir, hashName(secret))
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(st.ModTime()) > sessionTTL {
		os.Remove(path)
		return false
	}
	return true
}

// revoke ends one session.
func (s *sessionStore) revoke(secret string) {
	os.Remove(filepath.Join(s.dir, sessionsDir, hashName(secret)))
}

// signOutAll ends every session and cancels every code, at once.
func (s *sessionStore) signOutAll() error {
	return errors.Join(os.RemoveAll(filepath.Join(s.dir, sessionsDir)), os.RemoveAll(filepath.Join(s.dir, codesDir)))
}

// prune deletes what has expired.
func (s *sessionStore) prune() {
	for kind, ttl := range map[string]time.Duration{codesDir: loginCodeTTL, sessionsDir: sessionTTL} {
		files, _ := filepath.Glob(filepath.Join(s.dir, kind, "*"))
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && time.Since(st.ModTime()) > ttl {
				os.Remove(f)
			}
		}
	}
}
