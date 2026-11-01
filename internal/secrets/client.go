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

func (c *Client) helper(ctx context.Context, command string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	argv := append(append([]string{}, c.Prefix...), command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("vault helper: %w", err)
	}
	return stdout.Bytes(), nil
}
