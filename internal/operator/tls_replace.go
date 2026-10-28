package operator

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

const tlsUsage = `Usage: opsecmkt deploy tls replace --dir DIR [--tls-cert FILE --tls-key FILE]
       [--upload-cert FILE --upload-key FILE] [--upload-ca FILE]
Replaces only the named TLS material of a stopped installation, for example a
renewed one-year leaf. Each certificate needs its private key; keys must be 0600.
The pair, serverAuth usage, validity period and CA rules are checked as setup
checks them. Every other key, endpoint, pin and the installation receipt are
kept. Starts nothing; run deploy down first and deploy up afterwards.
`

// runTLSReplace is the explicit, operator-invoked replacement of imported TLS
// material (A-695). It replaces only material the installation already uses.
func runTLSReplace(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, tlsUsage)
		return err
	}
	if args[0] != "replace" {
		return errors.New("unknown tls command; run deploy tls help")
	}
	f := flag.NewFlagSet("deploy tls replace", flag.ContinueOnError)
	f.SetOutput(errOut)
	directory := f.String("dir", "./opsecmkt-deployment", "existing private installation directory")
	sources := map[string]*string{
		"TLS_CERT":    f.String("tls-cert", "", "replacement application HTTPS certificate PEM"),
		"TLS_KEY":     f.String("tls-key", "", "replacement application HTTPS private key PEM; 0600"),
		"UPLOAD_CERT": f.String("upload-cert", "", "replacement upload backend server certificate PEM"),
		"UPLOAD_KEY":  f.String("upload-key", "", "replacement upload backend private key PEM; 0600"),
		"UPLOAD_CA":   f.String("upload-ca", "", "replacement upload backend client CA PEM (public certificates only)"),
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	for _, pair := range [][2]string{{"TLS_CERT", "TLS_KEY"}, {"UPLOAD_CERT", "UPLOAD_KEY"}} {
		if (*sources[pair[0]] == "") != (*sources[pair[1]] == "") {
			return errors.New("a replacement certificate and its private key must be supplied together; nothing was changed")
		}
	}
	var keys, imports []string
	for key, source := range sources {
		if *source != "" {
			keys = append(keys, key)
			if strings.HasSuffix(key, "KEY") {
				imports = append(imports, *source)
			}
		}
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		return errors.New("tls replace needs --tls-cert/--tls-key, --upload-cert/--upload-key or --upload-ca; nothing was changed")
	}
	dir, c, unlock, err := openReplacement(ctx, *directory, imports)
	if err != nil {
		return err
	}
	defer unlock()
	stage := newReplacementStage(dir, c)
	defer stage.discard()
	for _, key := range keys {
		if c.TLSFiles[key] == "" {
			return fmt.Errorf("this installation has no %s to replace; nothing was changed", key)
		}
		b, err := readImport(*sources[key], strings.HasSuffix(key, "KEY"))
		if err != nil {
			return fmt.Errorf("%s import: %w; nothing was changed", key, err)
		}
		if strings.HasSuffix(key, "CERT") {
			if err := checkServerLeafUsage(b); err != nil {
				return fmt.Errorf("%s: %w; nothing was changed", key, err)
			}
		}
		if err := stage.add(stage.candidate.TLSFiles, key, c.TLSFiles[key], b); err != nil {
			return err
		}
	}
	if err := validate(dir, stage.candidate); err != nil {
		return err
	}
	if err := validateMaterial(dir, stage.candidate); err != nil {
		return err
	}
	if err := stage.commit(nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "replaced %s; every other key, endpoint, pin and the installation receipt were kept; nothing was started. Run deploy up to start with the replacement\n", strings.Join(keys, ", "))
	return err
}

// checkServerLeafUsage refuses a leaf that a TLS 1.3 peer would reject as a
// server certificate: both replaced leaves serve TLS (the HTTPS listener and the
// upload backend), so a clientAuth-only or non-signing leaf would only fail
// after the next start.
func checkServerLeafUsage(b []byte) error {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("certificate import must start with a PEM CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("certificate cannot be parsed")
	}
	if len(leaf.ExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return errors.New("leaf certificate lacks the serverAuth extended key usage")
	}
	if leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return errors.New("leaf certificate lacks the digitalSignature key usage TLS 1.3 needs")
	}
	return nil
}
