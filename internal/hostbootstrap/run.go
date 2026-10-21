package hostbootstrap

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const help = `usage: opsecmkt host COMMAND [flags]
  plan --config host.json --out NEW_DIR
      Validate inventory and export rules, security defaults and instructions.
  packages --config host.json
      Ubuntu 24.04 root: install distribution Docker/Compose, nftables and tools.
      System package installation can start dependency daemons; starts no app.
  apply --config host.json --operator-access-verified
      Ubuntu 24.04 root: check static network and existing hardened SSH, arm
      five-minute rollback, atomically apply only the owned ingress table.
  confirm --config host.json
      Confirm from a NEW approved named-operator SSH session within five minutes.
      Preserve SSH_CONNECTION through sudo. Otherwise the previous policy returns.
  rollback
      Root: immediately restore the previous owned ingress policy while pending.
Supported: dedicated static-address application hosts only, IPv4 and IPv6.
SSH must already use verified nonroot public-key access (see bootstrap guide).
No provider firewall, remote worker, disk formatting, egress guarantee or app start.
`

// Run handles explicit host provisioning; ordinary server startup never calls it.
func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, e := io.WriteString(out, help)
		return e
	}
	command := args[0]
	f := flag.NewFlagSet("host "+command, flag.ContinueOnError)
	f.SetOutput(errOut)
	if command == "rollback" {
		if len(args) != 1 {
			return errors.New("host rollback takes no flags")
		}
		return rollbackHost(ctx, out)
	}
	if command != "plan" && command != "packages" && command != "apply" && command != "confirm" {
		return errors.New("unknown host command; run host --help")
	}
	config := f.String("config", "", "versioned nonsecret host inventory")
	var output *string
	var verified *bool
	if command == "plan" {
		output = f.String("out", "", "new private artifact directory")
	}
	if command == "apply" {
		verified = f.Bool("operator-access-verified", false, "attest fresh named operator SSH/sudo and recovery console tested")
	}
	if e := f.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 || *config == "" {
		return errors.New("host requires --config and no positional arguments")
	}
	b, e := readConfig(*config)
	if e != nil {
		return e
	}
	c, e := decodeConfig(b)
	if e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	switch command {
	case "plan":
		if *output == "" {
			return errors.New("host plan requires --out NEW_DIR")
		}
		return plan(c, *output, out)
	case "packages":
		return installPackages(ctx, c, out)
	case "apply":
		if !*verified {
			return errors.New("first test named operator SSH/sudo and recovery console, then pass --operator-access-verified")
		}
		return applyHost(ctx, c, out)
	case "confirm":
		return confirmHost(ctx, c, out)
	}
	return nil
}
func readConfig(path string) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("host inventory must be a regular non-symlink file of at most 64 KiB")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(info, actual) {
		return nil, errors.New("host inventory changed while reading")
	}
	return io.ReadAll(io.LimitReader(f, (64<<10)+1))
}
func plan(c Config, dir string, out io.Writer) error {
	parent, e := filepath.Abs(filepath.Dir(dir))
	if e != nil {
		return e
	}
	resolved, e := filepath.EvalSymlinks(parent)
	if e != nil || resolved != parent {
		return errors.New("plan output parent must exist without symlinks")
	}
	if e = os.Mkdir(dir, 0700); e != nil {
		return errors.New("plan output must be a new directory")
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	root, e := os.OpenRoot(dir)
	if e != nil {
		return e
	}
	defer root.Close()
	b, e := marshal(c)
	if e != nil {
		return e
	}
	for name, data := range map[string][]byte{"host.json": b, "ingress.nft": []byte(renderRules(c)), "security-defaults.conf": []byte(sysctlDefaults), "BOOTSTRAP.txt": []byte(planInstructions)} {
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	complete = true
	_, e = fmt.Fprintln(out, "Host plan exported. Review host.json; apply validates the actual host before changes. No host settings changed.")
	return e
}

const sysctlDefaults = "fs.suid_dumpable = 0\nkernel.kptr_restrict = 2\nnet.ipv4.conf.all.accept_redirects = 0\nnet.ipv4.conf.default.accept_redirects = 0\nnet.ipv4.conf.all.send_redirects = 0\nnet.ipv4.conf.default.send_redirects = 0\nnet.ipv6.conf.all.accept_redirects = 0\nnet.ipv6.conf.default.accept_redirects = 0\n"
const planInstructions = `This is a nonsecret application-host ingress recipe; keep operational addresses private.
Prepare Ubuntu 24.04 with encrypted persistent storage before transferring secrets.
Test named nonroot public-key SSH, sudo and an independent recovery console.
Follow docs/install/server-bootstrap.md for the supported static-network profile.
Optional: sudo ./opsecmkt host packages --config host.json
sudo --preserve-env=SSH_CONNECTION ./opsecmkt host apply --config host.json --operator-access-verified
Within five minutes, open a NEW SSH connection from an approved source and run:
sudo --preserve-env=SSH_CONNECTION ./opsecmkt host confirm --config host.json
Without confirmation, prior ingress policy is restored. A pending boot rolls back.
Provider firewall, managed DB source rules and remote services are separate.
No public web listener is permitted; Tor gateway and private admin remain separate.
Repeat independent allowed/denied-source, IPv6 and gateway/Tor checks after changes.
Export encrypted application configuration with deploy recovery export separately.
`
