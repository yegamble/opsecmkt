// Package keymaterial implements offline key generation and validated imports.
// It never prints private material or replaces existing files.
package keymaterial

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"opsecmkt/internal/onionidentity"
)

const usage = `keys generate secret --out FILE
keys generate audit --out FILE [--public-out FILE]
keys import secret|audit --in FILE --out FILE [--public-out FILE for audit]
keys generate pgp --name NAME --public-out FILE --private-out FILE
keys import pgp --in FILE --public-out FILE [--private-out FILE]
keys generate tls-ca --name NAME --cert-out FILE --key-out FILE [--days N]
keys generate tls-server --name NAME --host DNS,IP --ca-cert FILE --ca-key FILE --cert-out FILE --key-out FILE [--days N]
keys generate tls-client --name NAME --ca-cert FILE --ca-key FILE --cert-out FILE --key-out FILE [--days N]
keys import tls --cert-in FILE --key-in FILE --cert-out FILE --key-out FILE
keys generate tor-client --public-out FILE.auth --private-out FILE.key [--onion ADDRESS]
keys import tor-client --in FILE.key --public-out FILE.auth --private-out FILE.auth_private --onion ADDRESS
keys generate onion --out-dir DIR [--prefix PREFIX] [--workers N]
keys import onion --address ADDRESS --public-key FILE --secret-key FILE --out-dir DIR
Without --onion, Tor generation writes a raw private Base32 key; bind it later with import tor-client after the admin onion exists. Both Tor authorization files use mode 0600.
Private outputs use mode 0600; existing files are never replaced. Run pgp generation/import on an offline machine and give the runtime only the public recovery key. TLS CA private keys stay on the issuing machine.
`

type options struct {
	in, out, public, private, name, hosts, caCert, caKey, certIn, keyIn, certOut, keyOut, onion string
	days                                                                                        int
}

// Run executes a key command; args excludes the leading "keys" subcommand.
func Run(args []string, out io.Writer) error {
	return RunContext(context.Background(), args, out)
}

// RunContext permits cancellation of an optional vanity search before publication.
func RunContext(ctx context.Context, args []string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		_, err := io.WriteString(out, usage)
		return err
	}
	if len(args) < 2 || (args[0] != "generate" && args[0] != "import") {
		return errors.New("keys: expected generate or import and a key type; use keys help")
	}
	action, kind := args[0], args[1]
	if kind == "onion" {
		return onionidentity.Run(ctx, append([]string{action}, args[2:]...), out)
	}
	o, err := parseOptions(action, kind, args[2:])
	if err != nil {
		return err
	}
	var outputs []outputFile
	var fingerprint string
	switch kind {
	case "secret", "audit":
		var raw []byte
		if action == "generate" {
			raw = make([]byte, 32)
			if _, err = rand.Read(raw); err != nil {
				return errors.New("keys: random key generation failed")
			}
		} else {
			input, err := readFile(o.in, true)
			if err != nil {
				return err
			}
			input = bytes.TrimSuffix(input, []byte{'\n'})
			raw, err = hex.DecodeString(string(input))
			if err != nil || len(raw) != 32 {
				return errors.New("keys: imported secret must contain exactly 64 hexadecimal characters")
			}
		}
		outputs = append(outputs, outputFile{o.out, []byte(hex.EncodeToString(raw) + "\n"), 0600})
		if kind == "audit" && o.public != "" {
			public := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)
			outputs = append(outputs, outputFile{o.public, []byte(hex.EncodeToString(public) + "\n"), 0644})
		}
	case "pgp":
		outputs, fingerprint, err = pgpOutputs(action, o)
	case "tor-client":
		outputs, err = torClientOutputs(action, o)
	case "tls-ca", "tls-server", "tls-client", "tls":
		outputs, err = tlsOutputs(action, kind, o)
	default:
		return errors.New("keys: unsupported key type; use keys help")
	}
	if err != nil {
		return err
	}
	if err = writeFiles(outputs); err != nil {
		return err
	}
	for _, file := range outputs {
		if _, err = fmt.Fprintf(out, "Created %s\n", file.path); err != nil {
			return err
		}
	}
	if fingerprint != "" {
		_, err = fmt.Fprintf(out, "Recovery public key fingerprint: %s\nKeep the private recovery key offline; the runtime takes only its public key.\n", fingerprint)
	}
	return err
}

func parseOptions(action, kind string, args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	add := func(key string, target *string) { fs.StringVar(target, key, "", "") }
	switch kind {
	case "secret", "audit":
		add("out", &o.out)
		if action == "import" {
			add("in", &o.in)
		}
		if kind == "audit" {
			add("public-out", &o.public)
		}
	case "pgp":
		add("public-out", &o.public)
		add("private-out", &o.private)
		if action == "generate" {
			add("name", &o.name)
		} else {
			add("in", &o.in)
		}
	case "tor-client":
		add("public-out", &o.public)
		add("private-out", &o.private)
		add("onion", &o.onion)
		if action == "import" {
			add("in", &o.in)
		}
	case "tls-ca", "tls-server", "tls-client":
		if action != "generate" {
			return o, errors.New("keys: import a TLS certificate/key pair with import tls")
		}
		add("name", &o.name)
		add("cert-out", &o.certOut)
		add("key-out", &o.keyOut)
		days := 365
		if kind == "tls-ca" {
			days = 3650
		}
		fs.IntVar(&o.days, "days", days, "")
		if kind != "tls-ca" {
			add("ca-cert", &o.caCert)
			add("ca-key", &o.caKey)
		}
		if kind == "tls-server" {
			add("host", &o.hosts)
		}
	case "tls":
		if action != "import" {
			return o, errors.New("keys: generate tls-ca, tls-server or tls-client")
		}
		add("cert-in", &o.certIn)
		add("key-in", &o.keyIn)
		add("cert-out", &o.certOut)
		add("key-out", &o.keyOut)
	default:
		return o, errors.New("keys: unsupported key type; use keys help")
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return o, errors.New("keys: invalid command flags; use keys help")
	}
	missing := false
	switch kind {
	case "secret", "audit":
		missing = o.out == "" || (action == "import" && o.in == "")
	case "pgp":
		missing = o.public == "" || (action == "generate" && (o.private == "" || o.name == "")) || (action == "import" && o.in == "")
	case "tor-client":
		missing = o.public == "" || o.private == "" || action == "import" && (o.in == "" || o.onion == "")
	case "tls":
		missing = o.certIn == "" || o.keyIn == "" || o.certOut == "" || o.keyOut == ""
	default:
		missing = o.name == "" || o.certOut == "" || o.keyOut == "" || (kind != "tls-ca" && (o.caCert == "" || o.caKey == "")) || (kind == "tls-server" && o.hosts == "")
	}
	if missing {
		return o, errors.New("keys: missing required key input or output; use keys help")
	}
	if strings.ContainsAny(o.name, "\r\n\x00") || len(o.name) > 128 {
		return o, errors.New("keys: name must be at most 128 bytes without control line separators")
	}
	if strings.HasPrefix(kind, "tls-") && (o.days < 1 || o.days > 3650) {
		return o, errors.New("keys: certificate validity must be between 1 and 3650 days")
	}
	return o, nil
}

func pgpOutputs(action string, o options) (files []outputFile, fingerprint string, err error) {
	defer func() {
		if recover() != nil {
			files, fingerprint, err = nil, "", errors.New("keys: malformed OpenPGP key")
		}
	}()
	var entity *openpgp.Entity
	var private []byte
	if action == "generate" {
		var err error
		entity, err = openpgp.NewEntity(o.name, "", "", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
		if err != nil {
			return nil, "", errors.New("keys: OpenPGP recovery key generation failed")
		}
		private, err = armorEntity(entity, true)
		if err != nil {
			return nil, "", err
		}
	} else {
		input, err := readFile(o.in, false)
		if err != nil {
			return nil, "", err
		}
		text := strings.TrimSpace(string(input))
		if strings.Count(text, "-----BEGIN PGP ") != 1 || strings.Count(text, "-----END PGP ") != 1 {
			return nil, "", errors.New("keys: import requires exactly one armored OpenPGP key")
		}
		block, err := armor.Decode(strings.NewReader(text))
		if err != nil || (block.Type != openpgp.PublicKeyType && block.Type != openpgp.PrivateKeyType) {
			return nil, "", errors.New("keys: import requires an armored OpenPGP key")
		}
		end := "-----END " + block.Type + "-----"
		if !strings.HasPrefix(text, "-----BEGIN "+block.Type+"-----") || !strings.HasSuffix(text, end) {
			return nil, "", errors.New("keys: unexpected data outside OpenPGP key")
		}
		ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(text))
		if err != nil || len(ring) != 1 {
			return nil, "", errors.New("keys: invalid OpenPGP key")
		}
		entity = ring[0]
		hasPrivate := entity.PrivateKey != nil
		for _, subkey := range entity.Subkeys {
			hasPrivate = hasPrivate || subkey.PrivateKey != nil
		}
		if block.Type == openpgp.PrivateKeyType || hasPrivate {
			if o.private == "" {
				return nil, "", errors.New("keys: private OpenPGP input requires an explicit --private-out on an offline machine")
			}
			protected, err := readFile(o.in, true)
			if err != nil {
				return nil, "", err
			}
			if !bytes.Equal(input, protected) {
				return nil, "", errors.New("keys: OpenPGP input changed during import")
			}
			private = append([]byte(text), '\n')
		} else if o.private != "" {
			return nil, "", errors.New("keys: public OpenPGP input cannot produce a private key")
		}
	}
	if _, ok := entity.EncryptionKey(time.Now()); !ok {
		return nil, "", errors.New("keys: OpenPGP key has no currently usable encryption key")
	}
	public, err := armorEntity(entity, false)
	if err != nil {
		return nil, "", err
	}
	files = []outputFile{{o.public, public, 0644}}
	if len(private) > 0 {
		files = append(files, outputFile{o.private, private, 0600})
	}
	return files, strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint)), nil
}

func armorEntity(e *openpgp.Entity, private bool) ([]byte, error) {
	var buffer bytes.Buffer
	kind := openpgp.PublicKeyType
	if private {
		kind = openpgp.PrivateKeyType
	}
	w, err := armor.Encode(&buffer, kind, nil)
	if err != nil {
		return nil, errors.New("keys: OpenPGP serialization failed")
	}
	if private {
		err = e.SerializePrivate(w, nil)
	} else {
		err = e.Serialize(w)
	}
	if err != nil {
		return nil, errors.New("keys: OpenPGP serialization failed")
	}
	if err = w.Close(); err != nil {
		return nil, errors.New("keys: OpenPGP serialization failed")
	}
	return buffer.Bytes(), nil
}
