package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"opsecmkt/internal/ramsecrets"

	"github.com/ProtonMail/go-crypto/openpgp"
)

var recoveryCheckPersistent = checkRecoveryPersistent

const recoveryLimit = 2 << 20
const recoveryHelp = `usage: opsecmkt deploy recovery COMMAND [flags]
  export --dir DIR --recipient-public-key FILE --out NEW_DIR
  restore --dir NEW_DIR [--old-host-fenced]
Restore reads only an offline-decrypted recovery JSON document from stdin.
Production restore requires explicit --old-host-fenced attestation, verified
persistent encrypted storage, and later explicit deploy storage bind.
This recovers application configuration and managed identities only; database,
retention data, wallets, Docker volumes and host boot readiness are excluded.
No services start, no keys rotate, and existing destinations are refused.
`
const recoveryInstructions = `APPLICATION CONFIGURATION RECOVERY ONLY

private.pgp contains an encrypted recovery JSON document for offline decryption.
Keep the private OpenPGP recovery key offline. Never copy it to the app host.
deployment.json and manifest.json contain public metadata and ciphertext hashes.
Verify their hashes against a trusted copy before use. Hashes detect corruption;
they do not authenticate an archive independently of its trusted provenance.

Decrypt on a trusted offline machine, then transfer the plaintext only as a
protected stream to: opsecmkt deploy recovery restore --dir NEW_DIR
For production add --old-host-fenced only after ensuring the old host cannot
run the application or its Tor identities. Never run duplicate onion identities.
The plaintext must be the complete exported recovery document, not a secrets
pack document. No plaintext file or private decryption key flag is accepted.

EXCLUDED: PostgreSQL data/backups, retention content and signing material,
Monero wallet state/keys/backups, blockchain data, Docker volumes, upload/scanner
state, dependency worker state, host units, storage proof and reboot readiness.
Restore preserves project, external endpoints and persistent path references.
It does not create or recover that external state. Inspect every path and mount,
restore the excluded data using the corresponding service runbook, reconcile
payment recovery state, and fence the old host before enabling any service.

All production storage bindings are cleared. Run deploy storage bind explicitly
on the replacement host after checking the preserved paths and excluded state.
Review deploy plan and reinstall host boot/reboot guards on the replacement host;
no old-host readiness proof is adopted. Then follow deploy install instructions.
Do not claim full disaster recovery from this configuration-only archive.
`

type recoveryDeployment struct {
	Version           int    `json:"version"`
	Scope             string `json:"scope"`
	Mode              string `json:"mode"`
	Project           string `json:"project"`
	AppImage          string `json:"app_image"`
	DockerContext     string `json:"docker_context"`
	RetentionDir      string `json:"retention_dir"`
	OnionAddress      string `json:"onion_address,omitempty"`
	AdminOnionAddress string `json:"admin_onion_address,omitempty"`
}
type recoveryDocument struct {
	Version    int                `json:"version"`
	Deployment recoveryDeployment `json:"deployment"`
	Bundle     json.RawMessage    `json:"bundle"`
	SHA256     map[string]string  `json:"sha256"`
}
type recoveryManifest struct {
	Version int               `json:"version"`
	Scope   string            `json:"scope"`
	SHA256  map[string]string `json:"sha256"`
}

func recoveryMetadata(c Config) recoveryDeployment {
	return recoveryDeployment{Version: 1, Scope: "application-configuration-only", Mode: c.Mode, Project: c.Project, AppImage: c.AppImage, DockerContext: c.DockerContext, RetentionDir: c.RetentionDir, OnionAddress: c.OnionAddress, AdminOnionAddress: c.AdminOnionAddress}
}
func recoveryHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func recoveryJSON(value any) ([]byte, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	return append(b, '\n'), err
}
func recoveryDecode(data []byte, value any) error {
	if err := recoveryUniqueJSON(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("recovery: invalid or unsupported JSON document")
	}
	return nil
}

// runRecovery handles the deploy recovery subcommands. Decryption is offline;
// stdin is the only plaintext recovery input and no process is started here.
func runRecovery(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(out, recoveryHelp)
		return err
	}
	if args[0] != "export" && args[0] != "restore" {
		return errors.New("recovery: unsupported command; run deploy recovery help")
	}
	f := flag.NewFlagSet("deploy recovery "+args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dirFlag := f.String("dir", "", "installation directory")
	var recipient, destination string
	var fenced bool
	if args[0] == "export" {
		f.StringVar(&recipient, "recipient-public-key", "", "offline public encryption key")
		f.StringVar(&destination, "out", "", "new private archive directory")
	} else {
		f.BoolVar(&fenced, "old-host-fenced", false, "attest the old host cannot run application or onion identities")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(out, recoveryHelp)
			return err
		}
		return errors.New("recovery: invalid flags; plaintext and private keys are never accepted as flags")
	}
	if f.NArg() != 0 || *dirFlag == "" {
		return errors.New("recovery: --dir is required and positional inputs are refused")
	}
	dir, err := installationPath(*dirFlag)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("recovery: operation cancelled")
	}
	if args[0] == "export" {
		if recipient == "" || destination == "" {
			return errors.New("recovery export: --recipient-public-key and --out are required")
		}
		return exportRecovery(ctx, dir, recipient, destination, out)
	}
	return restoreRecovery(ctx, dir, in, fenced, out)
}

func recoveryRealPath(path string) error {
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("recovery: directory ancestors must exist without symlinks")
		}
	}
	return nil
}
func recoveryNewPath(path string) error {
	if err := recoveryRealPath(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return errors.New("recovery: destination exists or cannot be inspected; nothing overwritten")
	}
	return nil
}
func exportRecovery(ctx context.Context, dir, recipient, destination string, out io.Writer) error {
	if err := recoveryRealPath(dir); err != nil {
		return err
	}
	target, err := installationPath(destination)
	if err != nil {
		return err
	}
	if target == dir || strings.HasPrefix(target, dir+string(filepath.Separator)) {
		return errors.New("recovery export: archive must be outside the installation")
	}
	if err := recoveryNewPath(target); err != nil {
		return err
	}
	unlock, err := lockInstallation(dir)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := loadConfig(dir)
	if err != nil {
		return err
	}
	if err := validateRecoveryMaterial(dir, c); err != nil {
		return err
	}
	if c.AdminTorImage != "" && c.AdminOnionDir == "" || c.TorImage != "" && c.OnionDir == "" {
		return errors.New("recovery export: legacy Tor volume identity is not managed; adopt and verify the existing volume identity before export")
	}
	plain, err := ramsecrets.CollectInstallation(dir)
	if err != nil {
		return err
	}
	bundle, err := ramsecrets.ParseInstallation(plain)
	if err != nil {
		return err
	}
	hashes := make(map[string]string, len(bundle.Files))
	for _, file := range bundle.Files {
		if file.Path == "operator.json" {
			var parsed Config
			if err := recoveryDecode(file.Data, &parsed); err != nil {
				return err
			}
		}
		if file.Path == "installation.json" {
			var parsed receipt
			if err := recoveryDecode(file.Data, &parsed); err != nil {
				return err
			}
		}
		hashes[file.Path] = recoveryHash(file.Data)
	}
	doc := recoveryDocument{Version: 1, Deployment: recoveryMetadata(c), Bundle: plain, SHA256: hashes}
	encoded, err := json.Marshal(doc)
	if err != nil || len(encoded) > recoveryLimit {
		return errors.New("recovery: document exceeds size limit")
	}
	public, err := readImport(recipient, false)
	if err != nil {
		return errors.New("recovery export: recipient must be a regular public key file of at most 64KiB")
	}
	encrypted, err := ramsecrets.EncryptInstallation(public, encoded)
	if err != nil {
		return err
	}
	deployment, err := recoveryJSON(doc.Deployment)
	if err != nil {
		return err
	}
	files := map[string][]byte{"private.pgp": encrypted, "deployment.json": deployment, "RECOVERY.txt": []byte(recoveryInstructions)}
	archiveHashes := map[string]string{}
	for name, data := range files {
		archiveHashes[name] = recoveryHash(data)
	}
	manifest, err := recoveryJSON(recoveryManifest{Version: 1, Scope: "application-configuration-only", SHA256: archiveHashes})
	if err != nil {
		return err
	}
	files["manifest.json"] = manifest
	if err := recoveryWriteDirectory(ctx, target, files, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Encrypted application configuration archive created; retained data is excluded. Keep the recovery key offline.")
	return err
}

func restoreRecovery(ctx context.Context, dir string, in io.Reader, fenced bool, out io.Writer) error {
	if err := recoveryNewPath(dir); err != nil {
		return err
	}
	if err := recoveryCheckPersistent(filepath.Dir(dir)); err != nil {
		return err
	}
	if in == nil {
		return errors.New("recovery restore: offline-decrypted document is required on stdin")
	}
	data, err := io.ReadAll(io.LimitReader(in, recoveryLimit+1))
	if err != nil || len(data) == 0 || len(data) > recoveryLimit {
		return errors.New("recovery restore: stdin document is empty, unreadable or exceeds 2MiB")
	}
	var doc recoveryDocument
	if err := recoveryDecode(data, &doc); err != nil {
		return err
	}
	if doc.Version != 1 {
		return errors.New("recovery restore: unsupported format version")
	}
	bundle, err := ramsecrets.ParseInstallation(doc.Bundle)
	if err != nil {
		return err
	}
	if len(doc.SHA256) != len(bundle.Files) {
		return errors.New("recovery restore: file hash inventory mismatch")
	}
	var c Config
	files := make(map[string][]byte, len(bundle.Files))
	for _, file := range bundle.Files {
		if doc.SHA256[file.Path] != recoveryHash(file.Data) {
			return errors.New("recovery restore: file hash mismatch")
		}
		if file.Path == "installation.json" {
			var r receipt
			if err := recoveryDecode(file.Data, &r); err != nil {
				return err
			}
		}
		files[file.Path] = file.Data
		if file.Path == "operator.json" {
			if err := recoveryDecode(file.Data, &c); err != nil {
				return err
			}
		}
	}
	if doc.Deployment != recoveryMetadata(c) {
		return errors.New("recovery restore: deployment metadata does not match configuration")
	}
	if c.Mode == "prod" {
		if !fenced {
			return errors.New("recovery restore: production requires --old-host-fenced after fencing the old application host and onion identities")
		}
		if _, err := productionStorageProbe(ctx, dir, c, nil); err != nil {
			return err
		}
	}
	if os.Geteuid() != 0 && (c.RuntimeUID != os.Getuid() || c.RuntimeGID != os.Getgid()) {
		return errors.New("recovery restore: root is required to preserve another runtime UID/GID")
	}
	// Storage identities and boot readiness belong to the old host. Preserve
	// endpoint/path references, but force an explicit storage bind before install.
	c.StorageBindings = nil
	files["operator.json"], err = recoveryJSON(c)
	if err != nil {
		return err
	}
	if err := recoveryWriteDirectory(ctx, dir, files, &c); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Application configuration restored; no services started. Retained data is excluded. Review preserved paths and explicitly bind production storage before install.")
	return err
}

func recoveryWriteDirectory(ctx context.Context, target string, files map[string][]byte, c *Config) error {
	stage, err := os.MkdirTemp(filepath.Dir(target), ".opsecmkt-recovery-")
	if err != nil {
		return errors.New("recovery: cannot create private staging directory")
	}
	defer os.RemoveAll(stage)
	for name, data := range files {
		parent := filepath.Dir(filepath.Join(stage, filepath.FromSlash(name)))
		if err := os.MkdirAll(parent, 0700); err != nil {
			return errors.New("recovery: cannot create private managed directory")
		}
		if err := writeExclusive(filepath.Join(stage, filepath.FromSlash(name)), data, 0600); err != nil {
			return err
		}
	}
	if c != nil {
		if err := validateRecoveryConfig(stage, *c); err != nil {
			return err
		}
		if err := validateRecoveryMaterial(stage, *c); err != nil {
			return err
		}
		if err := checkReceipt(stage, *c); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := filepath.Walk(filepath.Join(stage, "secrets"), func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				return os.Chown(path, c.RuntimeUID, c.RuntimeGID)
			}); err != nil {
				return errors.New("recovery: cannot preserve runtime secret ownership")
			}
		}
	}
	if ctx.Err() != nil {
		return errors.New("recovery: operation cancelled")
	}
	// Sync each managed directory before publishing its entries as one unit.
	if err := filepath.Walk(stage, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return syncDirectory(path)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := publishRecovery(stage, target); err != nil {
		return errors.New("recovery: destination appeared or publication failed; nothing overwritten")
	}

	if err := syncDirectory(target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

// Reject ambiguous JSON before decoding security-sensitive configuration. The
// token walk also bounds nesting independently of encoding/json's larger limit.
func recoveryUniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("recovery: excessive JSON nesting")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("recovery: malformed JSON")
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delimiter != '{' && delimiter != '[' {
			return errors.New("recovery: malformed JSON")
		}
		keys := map[string]bool{}
		for d.More() {
			if delimiter == '{' {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || keys[name] {
					return errors.New("recovery: duplicate or malformed JSON key")
				}
				keys[name] = true
			}
			if err := walk(depth + 1); err != nil {
				return err
			}
		}
		close, err := d.Token()
		if err != nil || delimiter == '{' && close != json.Delim('}') || delimiter == '[' && close != json.Delim(']') {
			return errors.New("recovery: malformed JSON")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("recovery: trailing JSON data")
	}
	return nil
}

// Recovery preserves configured public keys and certificates across expiry.
// Deployment retains its current encryption-usability and TLS validity gates.
func validateRecoveryMaterial(dir string, c Config) error {
	originalPublic := c.PublicFiles
	c.PublicFiles = map[string]string{}
	for key, path := range originalPublic {
		if key == "DEPENDENCY_REPORT_PUBLIC_KEY" {
			c.PublicFiles[key] = path
			continue
		}
		full, err := managedPath(dir, path)
		if err != nil {
			return err
		}
		data, err := privateFile(full, 64<<10)
		if err != nil {
			return err
		}
		if err := validateRecoveryPublicKey(data); err != nil {
			return err
		}
	}
	original := c.TLSFiles
	c.TLSFiles = map[string]string{}
	for _, key := range []string{"UPLOAD_CA", "MONERO_RPC_CA", "DATABASE_CA"} {
		if original[key] != "" {
			c.TLSFiles[key] = original[key]
		}
	}
	if err := validateMaterial(dir, c); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"UPLOAD_CERT", "UPLOAD_KEY"}, {"TLS_CERT", "TLS_KEY"}, {"MONERO_RPC_CERT", "MONERO_RPC_KEY"}} {
		cert, key := original[pair[0]], original[pair[1]]
		if cert == "" && key == "" {
			continue
		}
		if cert == "" || key == "" {
			return errors.New("recovery: TLS certificate and key must be supplied together")
		}
		certPath, err := managedPath(dir, cert)
		if err != nil {
			return err
		}
		keyPath, err := managedPath(dir, key)
		if err != nil {
			return err
		}
		certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return errors.New("recovery: TLS certificate and key do not form a valid pair")
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil || leaf.IsCA {
			return errors.New("recovery: TLS leaf must be valid and CA signing keys must remain offline")
		}
	}
	return nil
}

// The separately operated dependency worker is outside the archive. Validate its
// path and pinned public key, leaving availability to the normal install gate.
func validateRecoveryConfig(dir string, c Config) error {
	if c.DependencyReportDir != "" {
		if !filepath.IsAbs(c.DependencyReportDir) || strings.ContainsAny(c.DependencyReportDir, "\x00\r\n$") {
			return errors.New("recovery: dependency report directory must be an absolute safe path")
		}
		c.DependencyReportDir = dir
	}
	return validate(dir, c)
}

// Check packet structure and private-key absence without selecting a currently
// usable encryption key: archived configured keys may have expired since export.
func validateRecoveryPublicKey(data []byte) error {
	text := strings.TrimSpace(string(data))
	if strings.Contains(text, "PRIVATE KEY") || strings.Count(text, "-----BEGIN PGP PUBLIC KEY BLOCK-----") != 1 || !strings.HasPrefix(text, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || !strings.HasSuffix(text, "-----END PGP PUBLIC KEY BLOCK-----") {
		return errors.New("recovery: configured PGP material must contain exactly one armored public key")
	}
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(text))
	if err != nil || len(entities) != 1 {
		return errors.New("recovery: configured OpenPGP public key is malformed")
	}
	entity := entities[0]
	if entity.PrivateKey != nil {
		return errors.New("recovery: configured PGP private key packets are refused")
	}
	for _, sub := range entity.Subkeys {
		if sub.PrivateKey != nil {
			return errors.New("recovery: configured PGP private subkey packets are refused")
		}
	}
	return nil
}
