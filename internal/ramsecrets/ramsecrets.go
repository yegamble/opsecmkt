// Package ramsecrets packages offline installation files for encryption and
// unlocks administrator-decrypted bundles into verified Linux tmpfs storage.
// Private OpenPGP decryption keys are never runtime inputs. It starts no services
// and implements no stop, wipe, lock, reboot or disk-erasure command.
package ramsecrets

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const maxBundle = 1 << 20
const maxFile = 64 << 10
const maxFiles = 64
const help = `usage: opsecmkt secrets COMMAND [flags]
  pack --dir OFFLINE_INSTALL_DIR --recipient-public-key FILE --out FILE.pgp
      Encrypt operator.json, referenced secrets, public administrator client keys
      managed onion identities and optional installation.json.
      Excludes retention, Docker volumes and generated runtime files.
  mount [--dir /run/opsecmkt-secrets]
      Linux root only: create a fresh tmpfs mount, mode0700, size16MiB,
      nosuid,nodev,noexec. Refuse nonempty or unverified existing destinations.
  unlock --dir /run/opsecmkt-secrets/INSTANCE
      Linux root only: read the administrator-decrypted JSON bundle from stdin.
      Require a verified hardened tmpfs parent; never overwrite an instance.
Bounds:64files,64KiB each,1MiB complete plaintext JSON. Directories0700; files0600.
Keep retention outside tmpfs. Only secrets/ is assigned to the configured runtime
UID/GID for Docker bind mounts; controller metadata stays root-owned.
No private recovery key, password or decrypted bundle is accepted as a flag.
RAM storage does not prevent swap, core dumps, snapshots, Docker metadata copies
or live-root access. No services start and no storage is wiped by these commands.
`

// Bundle is the bounded plaintext format carried inside an encrypted PGP message.
// Data uses JSON's standard base64 encoding for []byte; paths are slash-separated.
type Bundle struct {
	Version   int    `json:"version"`
	SourceDir string `json:"source_dir,omitempty"`
	Files     []File `json:"files"`
}
type File struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

type platform struct {
	verify  func(string) error
	mount   func(string) error
	euid    func() int
	chown   func(string, int, int) error
	isTmpfs func(string) (bool, error)
	publish func(string, string) error
}

// Run handles pack, mount and unlock. stdin is used only for the plaintext JSON
// stream produced by an administrator's offline age/GPG decryption command.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	return run(ctx, args, in, out, errOut, platform{verify: Verify, mount: mountMemory, euid: os.Geteuid, chown: os.Chown, isTmpfs: IsTmpfs, publish: publishMemory})
}

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, p platform) error {
	if out == nil {
		out = io.Discard
	}
	if in == nil {
		in = bytes.NewReader(nil)
	}
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(out, help)
		return err
	}
	command := args[0]
	f := flag.NewFlagSet("secrets "+command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	defaultDir := ""
	if command == "mount" {
		defaultDir = "/run/opsecmkt-secrets"
	}
	dir := f.String("dir", defaultDir, "installation or RAM mount directory")
	var recipient, output *string
	switch command {
	case "pack":
		recipient = f.String("recipient-public-key", "", "one armored OpenPGP encryption public key")
		output = f.String("out", "", "new ciphertext output, mode0600; never overwrite")
	case "mount", "unlock":
	default:
		return errors.New("secrets: unsupported command; run secrets help")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(out, help)
			return err
		}
		return errors.New("secrets: invalid command flags; run secrets help")
	}
	if f.NArg() != 0 || *dir == "" {
		return errors.New("secrets: --dir is required and positional arguments are refused")
	}
	if ctx.Err() != nil {
		return errors.New("secrets: operation cancelled")
	}
	switch command {
	case "pack":
		if *recipient == "" || *output == "" {
			return errors.New("secrets pack: --recipient-public-key and --out are required")
		}
		data, err := collect(*dir)
		if err != nil {
			return err
		}
		public, err := readRegular(*recipient, false)
		if err != nil {
			return errors.New("secrets pack: recipient must be a regular armored public-key file of at most64KiB")
		}
		encrypted, err := encrypt(public, data)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errors.New("secrets: operation cancelled")
		}
		if err := writeCiphertext(*output, encrypted); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "Encrypted secrets bundle created; private recovery keys remain offline.")
		return err
	case "mount":
		if p.euid() != 0 {
			return errors.New("secrets mount: Linux root privileges are required")
		}
		if err := p.mount(*dir); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Hardened RAM mount ready; no secrets loaded and no services started.")
		return err
	case "unlock":
		if p.euid() != 0 {
			return errors.New("secrets unlock: Linux root privileges are required")
		}
		if err := unlock(ctx, *dir, in, p); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Secrets unlocked in verified RAM storage; no services started.")
		return err
	}
	return nil
}

func encrypt(public, plain []byte) (encrypted []byte, err error) {
	defer func() {
		if recover() != nil {
			encrypted = nil
			err = errors.New("secrets pack: invalid recipient public key")
		}
	}()
	text := strings.TrimSpace(string(public))
	if strings.Contains(text, "PRIVATE KEY") || strings.Count(text, "-----BEGIN PGP PUBLIC KEY BLOCK-----") != 1 || !strings.HasPrefix(text, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || !strings.HasSuffix(text, "-----END PGP PUBLIC KEY BLOCK-----") {
		return nil, errors.New("secrets pack: import exactly one public OpenPGP encryption key; private keys are refused")
	}
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(text))
	if err != nil || len(entities) != 1 {
		return nil, errors.New("secrets pack: invalid recipient public key")
	}
	entity := entities[0]
	if entity.PrivateKey != nil {
		return nil, errors.New("secrets pack: private recovery keys must remain offline")
	}
	for _, sub := range entity.Subkeys {
		if sub.PrivateKey != nil {
			return nil, errors.New("secrets pack: private recovery subkeys must remain offline")
		}
	}
	if _, ok := entity.EncryptionKey(time.Now()); !ok {
		return nil, errors.New("secrets pack: recipient needs a currently usable encryption key")
	}
	var buffer bytes.Buffer
	armored, err := armor.Encode(&buffer, "PGP MESSAGE", nil)
	if err != nil {
		return nil, errors.New("secrets pack: encryption failed")
	}
	writer, err := openpgp.Encrypt(armored, entities, nil, nil, &packet.Config{DefaultCipher: packet.CipherAES256, DefaultHash: crypto.SHA256})
	if err != nil {
		return nil, errors.New("secrets pack: encryption failed")
	}
	if _, err = writer.Write(plain); err != nil {
		return nil, errors.New("secrets pack: encryption failed")
	}
	if err = writer.Close(); err != nil {
		return nil, errors.New("secrets pack: encryption failed")
	}
	if err = armored.Close(); err != nil {
		return nil, errors.New("secrets pack: encryption failed")
	}
	return buffer.Bytes(), nil
}

func writeCiphertext(path string, data []byte) error {
	// Stage only ciphertext, never the decrypted bundle, on the output filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".opsecmkt-bundle-*")
	if err != nil {
		return errors.New("secrets pack: cannot stage ciphertext output")
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		return errors.New("secrets pack: cannot write ciphertext output")
	}
	if err = os.Link(tmp.Name(), path); err != nil {
		return errors.New("secrets pack: output exists or cannot be created; nothing replaced")
	}
	return nil
}
