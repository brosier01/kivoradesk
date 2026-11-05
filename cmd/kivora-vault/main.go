// Command kivora-vault owns the credentials store. It runs as the kivora-vault
// account, so that the store stays out of reach of the desktop account, whose
// browser the agent drives:
//
//	kivora-vault list                              names, sites and field kinds
//	kivora-vault value                             one value; the request is JSON on stdin
//	kivora-vault set [-field KIND] NAME ORIGIN...  the value is on stdin
//	kivora-vault delete NAME
//
// The desktop account may only run list and value (see the doas rules). The
// human runs the others from the server, with "kivora secret".
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/brosier01/kivoradesk/internal/secrets"
)

const storePath = "/var/lib/kivora-vault/secrets.json"

const usage = "usage: kivora-vault list | value | set [-field KIND] NAME ORIGIN... | delete NAME"

// maxInput bounds a value or a request read from stdin.
const maxInput = 64 << 10

func main() {
	if err := run(storePath, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path string, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	store := &secrets.Store{Path: path}
	switch args[0] {
	case "list":
		infos, err := store.List()
		if err != nil {
			return err
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(infos)
	case "value":
		var req secrets.Request
		if err := json.NewDecoder(io.LimitReader(stdin, maxInput)).Decode(&req); err != nil {
			return errors.New("invalid request")
		}
		grant, err := store.ValueFor(req.Name, req.URL)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(grant)
	case "set":
		fl := flag.NewFlagSet("set", flag.ContinueOnError)
		field := fl.String("field", secrets.FieldPassword, "kind of field the secret may go into: password or text")
		if err := fl.Parse(args[1:]); err != nil {
			return err
		}
		if fl.NArg() < 2 {
			return errors.New(usage)
		}
		value, err := io.ReadAll(io.LimitReader(stdin, maxInput))
		if err != nil {
			return err
		}
		name := fl.Arg(0)
		if err := store.Set(name, *field, fl.Args()[1:], strings.TrimRight(string(value), "\r\n")); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "secret %s saved\n", name)
		return nil
	case "delete":
		if len(args) != 2 {
			return errors.New(usage)
		}
		if err := store.Delete(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "secret %s deleted\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown command %q; %s", args[0], usage)
}
