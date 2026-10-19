package operator

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"opsecmkt/internal/brandingconfig"
	"opsecmkt/internal/onionidentity"
)

type onionFlags struct{ address, publicKey, secretKey, prefix *string }

func addOnionFlags(f *flag.FlagSet, role string) onionFlags {
	return onionFlags{
		address:   f.String(role+"onion-address", "", "import existing v3 onion address with both Tor public and secret key files"),
		publicKey: f.String(role+"onion-public-key", "", "Tor hs_ed25519_public_key file; requires matching address and secret key"),
		secretKey: f.String(role+"onion-secret-key", "", "private 0600 Tor hs_ed25519_secret_key file; requires matching address and public key"),
		prefix:    f.String(role+"onion-prefix", "", "optional lowercase base32 prefix, at most 6 characters; otherwise generate a random identity"),
	}
}

func (f onionFlags) requested() bool {
	return *f.address != "" || *f.publicKey != "" || *f.secretKey != "" || *f.prefix != ""
}

func initializeOnionIdentity(ctx context.Context, stage, role string, enabled bool, f onionFlags) (string, string, error) {
	if !enabled {
		if f.requested() {
			return "", "", fmt.Errorf("%sonion flags require the corresponding managed Tor image", role)
		}
		return "", "", nil
	}
	imports := *f.address != "" || *f.publicKey != "" || *f.secretKey != ""
	if imports && (*f.address == "" || *f.publicKey == "" || *f.secretKey == "" || *f.prefix != "") {
		return "", "", errors.New("onion identity imports require address, public key and secret key together and cannot use a prefix")
	}
	if len(*f.prefix) > 6 || strings.Trim(*f.prefix, "abcdefghijklmnopqrstuvwxyz234567") != "" {
		return "", "", errors.New("onion prefix must contain at most 6 lowercase base32 characters")
	}
	var identity onionidentity.Identity
	var err error
	if imports {
		if err := onionidentity.ValidateAddress(*f.address); err != nil {
			return "", "", err
		}
		secret, readErr := privateFile(*f.secretKey, 96)
		if readErr != nil {
			return "", "", errors.New("onion secret key import requires a private regular Tor key file")
		}
		public, readErr := readImport(*f.publicKey, false)
		if readErr != nil {
			return "", "", errors.New("onion public key import requires a regular Tor public key file")
		}
		identity, err = onionidentity.Parse(secret, public, *f.address)
	} else {
		identity, err = onionidentity.Generate(ctx, *f.prefix, 0)
	}
	if err != nil {
		return "", "", err
	}
	dir := "onion-public"
	if role == "admin-" {
		dir = "onion-admin"
	}
	realStage, err := filepath.EvalSymlinks(stage)
	if err != nil {
		return "", "", err
	}
	if err := identity.WriteNew(filepath.Join(realStage, dir)); err != nil {
		return "", "", err
	}
	return dir, identity.Address, nil
}

func readManagedOnion(dir, relative, address string) (onionidentity.Identity, error) {
	var zero onionidentity.Identity
	path, err := managedPath(dir, relative)
	if err != nil {
		return zero, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return zero, errors.New("managed onion identity requires a private real directory")
	}
	secret, err := privateFile(filepath.Join(path, "hs_ed25519_secret_key"), 96)
	if err != nil {
		return zero, err
	}
	public, err := privateFile(filepath.Join(path, "hs_ed25519_public_key"), 64)
	if err != nil {
		return zero, err
	}
	hostname, err := privateFile(filepath.Join(path, "hostname"), 63)
	if err != nil {
		return zero, err
	}
	identity, err := onionidentity.Parse(secret, public, string(hostname))
	if err != nil || identity.Address != address || (string(hostname) != identity.Address && string(hostname) != identity.Address+"\n") {
		return zero, errors.New("managed onion address and Tor keys do not match")
	}
	return identity, nil
}

func validateOnionIdentities(dir string, c Config) error {
	for _, role := range []struct {
		dir, address, expected string
		enabled                bool
	}{{c.OnionDir, c.OnionAddress, "onion-public", c.TorImage != ""}, {c.AdminOnionDir, c.AdminOnionAddress, "onion-admin", c.AdminTorImage != ""}} {
		if role.dir == "" && role.address == "" {
			continue // Legacy installations must adopt/export their existing volume explicitly.
		}
		if !role.enabled || role.dir != role.expected || role.address == "" {
			return errors.New("managed onion metadata must name the matching enabled Tor role and identity directory")
		}
		if _, err := readManagedOnion(dir, role.dir, role.address); err != nil {
			return err
		}
	}
	if c.OnionAddress != "" && c.OnionAddress == c.AdminOnionAddress {
		return errors.New("public and administrator onion services require separate identities")
	}
	// Every user's encrypted PGP sign-in code lists OFFICIAL_ADDRESSES, so the private administrator onion never may.
	if c.AdminOnionAddress != "" {
		official, err := brandingconfig.ParseOfficialAddresses(c.Settings["OFFICIAL_ADDRESSES"], c.Mode == "dev")
		if err != nil {
			return err
		}
		for _, origin := range official {
			if u, err := url.Parse(origin); err != nil || strings.EqualFold(u.Hostname(), c.AdminOnionAddress) {
				return errors.New("OFFICIAL_ADDRESSES must not name the private administrator onion: every user's PGP sign-in code lists those addresses")
			}
		}
	}
	return nil
}

func printOnionPlan(out io.Writer, c Config) error {
	for _, role := range []struct {
		label, address, dir string
		enabled             bool
	}{{"development public", c.OnionAddress, c.OnionDir, c.TorImage != ""}, {"private administrator", c.AdminOnionAddress, c.AdminOnionDir, c.AdminTorImage != ""}} {
		if role.address != "" {
			if _, err := fmt.Fprintf(out, "%s onion: %s\nBack up %s/ offline with its hostname and both Tor key files; keep secret keys private.\n", role.label, role.address, role.dir); err != nil {
				return err
			}
		} else if role.enabled {
			if _, err := fmt.Fprintf(out, "%s onion: legacy volume identity; export and verify its existing native Tor keys offline before migration. Empty or partial legacy volumes refuse startup.\n", role.label); err != nil {
				return err
			}
		}
	}
	return nil
}
