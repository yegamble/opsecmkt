// Package secretfile loads a fixed set of one-line runtime secrets from protected files.
package secretfile

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

var names = []string{"DATABASE_URL", "APP_SECRET_KEY", "SETUP_TOKEN", "MONERO_WALLET_SECRET", "ESCROW_XMR_WALLET_RPC_URL", "ESCROW_XMR_DAEMON_URL", "AUDIT_SIGNING_KEY"}

// LoadEnvironment resolves NAME_FILE for the supported runtime secrets. It validates
// all files before changing the environment; errors never contain file contents.
// PEM/TLS configuration already takes file paths and does not use this mechanism.
func LoadEnvironment() error {
	type value struct {
		name, body, previous string
		existed              bool
	}
	var pending []value
	for _, name := range names {
		path := os.Getenv(name + "_FILE")
		if path == "" {
			continue
		}
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s and %s_FILE cannot both be set", name, name)
		}
		body, err := read(path)
		if err != nil {
			return fmt.Errorf("%s_FILE: %s", name, err)
		}
		previous, existed := os.LookupEnv(name)
		pending = append(pending, value{name, string(body), previous, existed})
	}
	for i, v := range pending {
		if err := os.Setenv(v.name, v.body); err != nil {
			for _, old := range pending[:i] {
				if old.existed {
					_ = os.Setenv(old.name, old.previous)
				} else {
					_ = os.Unsetenv(old.name)
				}
			}
			return fmt.Errorf("%s_FILE: cannot set environment", v.name)
		}
	}
	return nil
}

func read(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("requires a readable regular file, without a symlink")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("secret file must not be accessible to group or others (use mode 0600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open secret file")
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) || actual.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("secret file changed or is not private")
	}
	body, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(body) > 65536 {
		return nil, fmt.Errorf("secret file must be readable and at most 64 KiB")
	}
	body = bytes.TrimSuffix(body, []byte{'\n'})
	if len(body) == 0 || bytes.ContainsAny(body, "\x00\r\n") {
		return nil, fmt.Errorf("secret file must hold one nonempty line without NUL bytes")
	}
	return body, nil
}
