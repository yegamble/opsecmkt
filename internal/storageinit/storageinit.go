// Package storageinit prepares one explicitly selected fresh Linux data device.
// It never converts a running root filesystem, starts services, stores unlock
// keys or configures automatic unlock. Passphrases stay with native cryptsetup.
package storageinit

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"opsecmkt/internal/storageguard"
)

const help = `usage: opsecmkt deploy storage COMMAND [flags]
  prepare --device /dev/disk/by-id/ID --confirm-device /dev/REAL_DEVICE
          --name opsecmkt-state --mount /srv/opsecmkt-encrypted
      Linux root and interactive terminal only. Refuse known signatures,
      active use and partitioned devices, then prompt in native cryptsetup
      to create LUKS2 and ext4. Signature probes cannot prove data absence.
      DESTRUCTIVE: use only a newly allocated, empty data device.
  unlock --device /dev/disk/by-id/ID --expected-luks-uuid UUID
         --name opsecmkt-state --mount /srv/opsecmkt-encrypted
      Verify the existing LUKS2 identity before native passphrase entry and
      mount its existing ext4 filesystem. Never format or overwrite data.
  verify --path DIR [--path DIR ...] [--expected-luks-uuid UUID]
      Prove persistent paths are backed entirely by active Linux LUKS devices.
      With a pin, each path must have exactly that one LUKS UUID.
  bind --dir INSTALL_DIR
      After a reviewed host migration, bind preserved production state to its
      verified encryption identities. Requires the local Docker daemon.
  check-bound --dir INSTALL_DIR
      Read-only production boot gate using recorded identities; needs no Docker.
  boot-unit --dir INSTALL_DIR [--binary /usr/local/bin/opsecmkt]
      Print the read-only systemd startup guard for an existing bound instance.
Passphrases are never flags, environment variables, receipts or key files.
Device and mount arguments must be absolute; destination parents must already
exist, be root-owned and not writable by other users. The destination must be
empty and private. No root migration, package installation, automatic boot
unlock, /etc/fstab edit, service start, disk erasure or recovery-key export.
Keep an independently verified encrypted backup and unlock key off the host.
`

type options struct {
	device, confirmation, name, mount, expected string
	prepare                                     bool
}

type console struct {
	in          io.Reader
	out, errOut io.Writer
}

// volume holds the original block-device descriptor throughout preparation.
// A failed operation closes only a mapping or mount created by that operation.
type volume interface {
	Close() error
	Format(context.Context, console) error
	Identity(context.Context) (string, error)
	Open(context.Context, console) error
	MakeFilesystem(context.Context) error
	CheckFilesystem(context.Context) error
	Mount(context.Context) error
	WriteReceipt(string) error
	Cleanup(context.Context) error
}

type platform struct {
	newVolume func(context.Context, options, console) (volume, error)
	inspect   func(string) (storageguard.Evidence, error)
}

// Run dispatches the explicit storage commands. It is not called implicitly by
// setup or boot: setup should run the read-only verification gate instead.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	return run(ctx, args, console{in: in, out: out, errOut: errOut}, platform{newVolume: openSystemVolume, inspect: storageguard.Inspect})
}

func run(ctx context.Context, args []string, streams console, p platform) error {
	if streams.out == nil {
		streams.out = io.Discard
	}
	if streams.errOut == nil {
		streams.errOut = io.Discard
	}
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(streams.out, help)
		return err
	}
	f := flag.NewFlagSet("deploy storage "+args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var cfg options
	var paths pathFlags
	f.StringVar(&cfg.expected, "expected-luks-uuid", "", "expected canonical LUKS UUID")
	switch args[0] {
	case "prepare", "unlock":
		cfg.prepare = args[0] == "prepare"
		f.StringVar(&cfg.device, "device", "", "absolute data block-device path, preferably /dev/disk/by-id")
		f.StringVar(&cfg.name, "name", "", "new mapper name")
		f.StringVar(&cfg.mount, "mount", "", "private empty mount destination")
		if cfg.prepare {
			f.StringVar(&cfg.confirmation, "confirm-device", "", "exact resolved block-device path to format")
		}
	case "verify":
		f.Var(&paths, "path", "persistent directory to verify; repeatable")
	default:
		return errors.New("storage: unsupported command; run deploy storage help")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(streams.out, help)
			return err
		}
		return errors.New("storage: invalid flags; run deploy storage help")
	}
	if f.NArg() != 0 {
		return errors.New("storage: positional arguments are refused")
	}
	if cfg.expected != "" && !validUUID(cfg.expected) {
		return errors.New("storage: --expected-luks-uuid must be a canonical lowercase UUID")
	}
	if ctx.Err() != nil {
		return errors.New("storage: operation cancelled")
	}
	if args[0] == "verify" {
		if len(paths) == 0 {
			return errors.New("storage verify: at least one --path is required")
		}
		for _, path := range paths {
			evidence, err := p.inspect(path)
			if err != nil {
				return fmt.Errorf("storage verify: %w", err)
			}
			if err := pinIdentity(evidence.LUKSUUIDs, cfg.expected); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(streams.out, "Verified encrypted persistent path %s; LUKS UUIDs: %s\n", evidence.Path, strings.Join(evidence.LUKSUUIDs, ",")); err != nil {
				return err
			}
		}
		return nil
	}
	if cfg.device == "" || cfg.name == "" || cfg.mount == "" || cfg.prepare && cfg.confirmation == "" {
		return errors.New("storage: --device, --name, --mount and (for prepare) --confirm-device are required")
	}
	if !cfg.prepare && cfg.expected == "" {
		return errors.New("storage unlock: --expected-luks-uuid is required")
	}
	if cfg.prepare && cfg.expected != "" {
		return errors.New("storage prepare: an existing UUID pin is not accepted when creating a new volume")
	}
	v, err := p.newVolume(ctx, cfg, streams)
	if err != nil {
		return err
	}
	defer v.Close()
	completed := false
	defer func() {
		if !completed {
			// Cancellation must not skip closing a mapping created by this call.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			if v.Cleanup(cleanupCtx) != nil {
				fmt.Fprintln(streams.errOut, "storage: cleanup incomplete; inspect the selected mount and mapping before proceeding")
			}
		}
	}()
	if cfg.prepare {
		if err := v.Format(ctx, streams); err != nil {
			return err
		}
	}
	uuid, err := v.Identity(ctx)
	if err != nil {
		return err
	}
	if !validUUID(uuid) {
		return errors.New("storage: cryptsetup returned an invalid LUKS identity")
	}
	if cfg.expected != "" && uuid != cfg.expected {
		return errors.New("storage unlock: selected LUKS UUID does not match the required pin")
	}
	if err := v.Open(ctx, streams); err != nil {
		return err
	}
	if cfg.prepare {
		if err := v.MakeFilesystem(ctx); err != nil {
			return err
		}
	} else if err := v.CheckFilesystem(ctx); err != nil {
		return err
	}
	if err := v.Mount(ctx); err != nil {
		return err
	}
	evidence, err := p.inspect(cfg.mount)
	if err != nil {
		return fmt.Errorf("storage: mounted-volume verification failed: %w", err)
	}
	if err := pinIdentity(evidence.LUKSUUIDs, uuid); err != nil {
		return err
	}
	if cfg.prepare {
		if err := v.WriteReceipt(uuid); err != nil {
			return err
		}
	}
	completed = true
	_, err = fmt.Fprintf(streams.out, "Encrypted storage mounted at %s; LUKS UUID: %s\nKeep the unlock key off-host. No services were started.\n", cfg.mount, uuid)
	return err
}

type pathFlags []string

func (p *pathFlags) String() string { return strings.Join(*p, ",") }
func (p *pathFlags) Set(value string) error {
	if value == "" {
		return errors.New("empty persistent path")
	}
	*p = append(*p, value)
	return nil
}

func pinIdentity(uuids []string, expected string) error {
	if len(uuids) == 0 {
		return errors.New("storage: no proven LUKS identity backs the selected path")
	}
	if expected != "" && (len(uuids) != 1 || uuids[0] != expected) {
		return errors.New("storage: mounted path does not match exactly the expected LUKS UUID")
	}
	return nil
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
