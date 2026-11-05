package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// helperTimeout bounds one call to the vault helper.
const helperTimeout = 10 * time.Second

// MaxScrub bounds the text the vault helper cleans at once.
const MaxScrub = 32 << 20

// Client is the desktop account's side of the store: it reaches it through
// the helper that runs as the vault account.
type Client struct {
	// Prefix runs the helper as the vault account, for example
	// ["doas", "-u", "kivora-vault", "/usr/local/bin/kivora-vault"].
	Prefix []string
}

// List returns what the agent may know about the stored secrets.
func (c *Client) List(ctx context.Context) ([]Info, error) {
	out, err := c.helper(ctx, "list", nil)
	if err != nil {
		return nil, err
	}
	var infos []Info
	return infos, json.Unmarshal(out, &infos)
}

// ValueFor returns a grant for a secret if pageURL is on one of its origins.
func (c *Client) ValueFor(ctx context.Context, name, pageURL string) (Grant, error) {
	req, err := json.Marshal(Request{Name: name, URL: pageURL})
	if err != nil {
		return Grant{}, err
	}
	out, err := c.helper(ctx, "value", req)
	if err != nil {
		return Grant{}, err
	}
	var g Grant
	if err := json.Unmarshal(out, &g); err != nil {
		return Grant{}, errors.New("the vault helper returned an invalid answer") // never echo its output
	}
	return g, nil
}

// Scrub returns each text without the stored values and their encodings, in one
// call to the helper. There is no result on failure: nothing unchecked goes out.
func (c *Client) Scrub(ctx context.Context, texts []string) ([]string, error) {
	const separator = "\x00" // a stored value cannot hold it
	joined := make([]string, len(texts))
	for i, text := range texts {
		joined[i] = strings.ReplaceAll(text, separator, "\uFFFD")
	}
	input := strings.Join(joined, separator)
	if len(input) > MaxScrub {
		return nil, fmt.Errorf("%d bytes to check, over the limit of %d", len(input), MaxScrub)
	}
	out, err := c.helper(ctx, "scrub", []byte(input))
	if err != nil {
		return nil, err
	}
	cleaned := strings.Split(string(out), separator)
	if len(cleaned) != len(texts) {
		return nil, errors.New("the vault helper returned an invalid answer")
	}
	return cleaned, nil
}

// helper runs one command of the vault. It runs under another account: this
// one cannot kill it, so a helper that hangs is left behind rather than waited
// for.
func (c *Client) helper(ctx context.Context, command string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	argv := append(append([]string{}, c.Prefix...), command)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case <-ctx.Done():
		return nil, errors.New("the vault helper does not answer")
	case err := <-done:
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return nil, errors.New(msg)
			}
			return nil, fmt.Errorf("vault helper: %w", err)
		}
		return stdout.Bytes(), nil
	}
}
