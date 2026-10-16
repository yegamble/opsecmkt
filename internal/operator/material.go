package operator

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

func readImport(path string, private bool) ([]byte, error) {
	if private {
		return privateFile(path, 64<<10)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("import must name a regular, non-symlink file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open imported public file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return nil, errors.New("imported public file exceeds 64 KiB or cannot be read")
	}
	return b, nil
}
func readPublicKey(path string) ([]byte, error) {
	b, err := readImport(path, false)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(b), "PRIVATE KEY") {
		return nil, errors.New("only public OpenPGP keys may be imported; keep private decryption keys offline")
	}
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(string(b)))
	if err != nil || len(entities) != 1 {
		return nil, errors.New("public-key import requires one armored OpenPGP public key")
	}
	entity := entities[0]
	if entity.PrivateKey != nil {
		return nil, errors.New("private OpenPGP key packets are refused")
	}
	for _, subkey := range entity.Subkeys {
		if subkey.PrivateKey != nil {
			return nil, errors.New("private OpenPGP subkey packets are refused")
		}
	}
	if _, ok := entity.EncryptionKey(time.Now()); !ok {
		return nil, errors.New("public OpenPGP key needs a currently usable encryption subkey")
	}
	return b, nil
}
func validateMaterial(dir string, c Config) error {
	if c.AdminAuthDir != "" {
		path, err := managedPath(dir, c.AdminAuthDir)
		if err != nil {
			return err
		}
		if _, err := readAdminAuth(path); err != nil {
			return err
		}
	}
	for _, key := range []string{"APP_SECRET_KEY", "SETUP_TOKEN", "AUDIT_SIGNING_KEY", "MONERO_WALLET_SECRET"} {
		if c.SecretFiles[key] == "" {
			continue
		}
		value, err := secretValue(dir, c, key)
		if err != nil {
			return err
		}
		if key == "AUDIT_SIGNING_KEY" {
			b, err := hex.DecodeString(value)
			if err != nil || len(b) != 32 {
				return errors.New("AUDIT_SIGNING_KEY must be a 64-hex-character Ed25519 seed")
			}
		} else {
			if len(value) < 32 {
				return fmt.Errorf("%s must contain at least 32 bytes", key)
			}
			different := map[rune]bool{}
			for _, r := range value {
				different[r] = true
			}
			if len(different) < 8 {
				return fmt.Errorf("%s has too little character variety", key)
			}
		}
	}
	app, _ := secretValue(dir, c, "APP_SECRET_KEY")
	setup, _ := secretValue(dir, c, "SETUP_TOKEN")
	monero, _ := secretValue(dir, c, "MONERO_WALLET_SECRET")
	if app == setup || monero != "" && (monero == app || monero == setup) {
		return errors.New("application, setup and Monero wallet secrets must be distinct")
	}
	for key, path := range c.PublicFiles {
		full, err := managedPath(dir, path)
		if err != nil {
			return err
		}
		if key == "DEPENDENCY_REPORT_PUBLIC_KEY" {
			b, err := privateFile(full, 1024)
			if err != nil {
				return err
			}
			if err := validateWorkerPublicKey(b); err != nil {
				return err
			}
		} else if _, err := readPublicKey(full); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{"UPLOAD_CERT", "UPLOAD_KEY"}, {"TLS_CERT", "TLS_KEY"}, {"MONERO_RPC_CERT", "MONERO_RPC_KEY"}} {
		cert, key := c.TLSFiles[pair[0]], c.TLSFiles[pair[1]]
		if cert == "" && key == "" {
			continue
		}
		if cert == "" || key == "" {
			return errors.New("TLS certificate and private key must be supplied together")
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
			return errors.New("TLS certificate and private key do not form a valid pair")
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
			return errors.New("TLS leaf certificate is invalid, expired or not yet valid")
		}
		if leaf.IsCA {
			return errors.New("CA signing keys must stay offline; import a leaf TLS certificate and key")
		}
	}
	for _, key := range []string{"UPLOAD_CA", "MONERO_RPC_CA", "DATABASE_CA"} {
		if path := c.TLSFiles[key]; path != "" {
			file, err := managedPath(dir, path)
			if err != nil {
				return err
			}
			b, err := privateFile(file, 64<<10)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "PRIVATE KEY") || !x509.NewCertPool().AppendCertsFromPEM(b) {
				return errors.New("CA imports require public PEM certificates without private keys")
			}
		}
	}
	return nil
}

func validateWorkerPublicKey(b []byte) error {
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != 32 {
		return errors.New("dependency worker public key must hold 64 hexadecimal characters")
	}
	return nil
}
