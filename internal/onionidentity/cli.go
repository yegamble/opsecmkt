package onionidentity

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

const usage = `keys generate onion --out-dir DIR [--prefix PREFIX] [--workers N]
keys import onion --address ADDRESS.onion --public-key FILE --secret-key FILE --out-dir DIR
Native Tor v3 service identity files are written privately to a new directory.
Prefix: lowercase a-z or 2-7, at most 6 characters. Workers: 1..64, 0 uses at most 8.
Vanity search completion time is unpredictable; cancel with Ctrl-C. Imported keys
must belong to the expected complete address. No runtime Tor service is changed.
`

// Run handles generate/import after the outer "keys ... onion" dispatch.
// Output contains only a completed public address or help, never private keys.
func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	if args[0] != "generate" && args[0] != "import" {
		return errors.New("onion identity: use generate or import")
	}
	f := flag.NewFlagSet("keys "+args[0]+" onion", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("out-dir", "", "new private Tor identity directory")
	var prefix, expected, publicPath, secretPath string
	var workers int
	if args[0] == "generate" {
		f.StringVar(&prefix, "prefix", "", "optional lowercase vanity prefix, up to six characters")
		f.IntVar(&workers, "workers", 0, "parallel search workers; zero uses at most eight")
	} else {
		f.StringVar(&expected, "address", "", "expected complete v3 address")
		f.StringVar(&publicPath, "public-key", "", "native Tor public file")
		f.StringVar(&secretPath, "secret-key", "", "private native Tor expanded secret file")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(out, usage)
			return err
		}
		return errors.New("onion identity: invalid flags; use --help")
	}
	if f.NArg() != 0 || *dir == "" {
		return errors.New("onion identity: --out-dir is required and positional arguments are refused")
	}
	// A long vanity search must not begin for a destination that is already
	// unusable. WriteNew still performs the exclusive creation after searching.
	if _, err := preflightNewDirectory(*dir); err != nil {
		return err
	}
	var id Identity
	var err error
	if args[0] == "generate" {
		id, err = Generate(ctx, prefix, workers)
	} else {
		if expected == "" || publicPath == "" || secretPath == "" {
			return errors.New("onion identity: import requires --address, --public-key and --secret-key")
		}
		if err = ValidateAddress(expected); err != nil {
			return err
		}
		secret, readErr := readInputFile(secretPath, 96, true)
		if readErr != nil {
			return readErr
		}
		public, readErr := readInputFile(publicPath, 64, false)
		if readErr != nil {
			return readErr
		}
		id, err = Parse(secret, public, expected)
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := id.WriteNew(*dir); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, id.Address)
	return err
}
