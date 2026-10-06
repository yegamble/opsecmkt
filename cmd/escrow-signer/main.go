// Command escrow-signer is the market's dedicated signer for per-order (shape (b)) Bitcoin escrows (A-454, ruling
// P4-R17; docs/operator-guide.md, "Escrow signer"). It holds a fresh seed of its own in a mode-0400 file, listens only
// on a Unix socket that the app's container mounts, and signs one escrow's PSBT per request through Bitcoin Core's
// descriptorprocesspsbt under its own whitelisted RPC user, with SIGHASH_ALL hard-coded, after refusing anything that
// is not exactly one 2-of-3 escrow holding its key for the requested order index (sign.go). It never returns or logs
// key material, and the seed never appears in its arguments, environment, logs or errors.
//
//	escrow-signer init    [-seed FILE] [-bytes 32|64]       make a new seed file (refuses one that exists)
//	escrow-signer serve   [-seed FILE] [-socket PATH]       sign on the socket (ESCROW_SIGNER_RPC_URL: the signer's RPC user)
//	escrow-signer info    [-seed FILE]                      print the protocol version, account tpub and fingerprint
//	escrow-signer backup  [-seed FILE] -key OPERATOR.asc    write the seed PGP-encrypted to the operator's key (stdout)
//	escrow-signer restore [-seed FILE] [-fingerprint HEX]   read a decrypted seed from stdin into a new seed file
//
// FILE defaults to ESCROW_SIGNER_SEED_FILE, else /var/lib/escrow-signer/seed; PATH to ESCROW_SIGNER_SOCKET, else
// /run/escrow-signer/signer.sock.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const (
	defaultSeedFile = "/var/lib/escrow-signer/seed"
	defaultSocket   = "/run/escrow-signer/signer.sock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

const usage = `usage: escrow-signer init|serve|info|backup|restore [flags]  (docs/operator-guide.md, "Escrow signer")`

// run is the command line: 0 on success, 1 on a failure, 2 on a usage error. Messages name files and fixed reasons,
// never key material.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd := args[0]
	fl := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fl.SetOutput(stderr)
	seedFile := defaultSeedFile
	if v := getenv("ESCROW_SIGNER_SEED_FILE"); v != "" {
		seedFile = v
	}
	fl.StringVar(&seedFile, "seed", seedFile, "the seed file")
	var socket, keyFile, fingerprint *string
	var size *int
	switch cmd {
	case "init":
		size = fl.Int("bytes", seedLen, "seed length in bytes: 32 or 64")
	case "serve":
		def := defaultSocket
		if v := getenv("ESCROW_SIGNER_SOCKET"); v != "" {
			def = v
		}
		socket = fl.String("socket", def, "the Unix socket path")
	case "info":
	case "backup":
		keyFile = fl.String("key", "", "the operator's OpenPGP public key file (armored or binary)")
	case "restore":
		fingerprint = fl.String("fingerprint", "", "refuse unless the restored seed's account fingerprint is this (8 hex digits)")
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	switch cmd {
	case "init":
		err = cmdInit(seedFile, *size, stdout)
	case "serve":
		err = cmdServe(ctx, seedFile, *socket, getenv("ESCROW_SIGNER_RPC_URL"), stderr)
	case "info":
		var k *signerKeys
		if k, err = loadKeys(seedFile); err == nil {
			printInfo(stdout, k)
		}
	case "backup":
		err = cmdBackup(seedFile, *keyFile, stdout, stderr)
	case "restore":
		err = cmdRestore(seedFile, *fingerprint, stdin, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "escrow-signer %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func printInfo(w io.Writer, k *signerKeys) {
	fmt.Fprintf(w, "escrow signer protocol version %d\naccount (m/48h/1h/0h/2h, rooted): %s\naccount fingerprint: %s\n", protocolVersion, k.accountTpub(), k.fingerprint())
}

// readSeed reads a seed file: a regular file no group or other user can read, of exactly 32 or 64 bytes.
func readSeed(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("seed file %s: %w", path, errors.Unwrap(err))
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("seed file %s is not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("seed file %s can be read by its group or others (mode %04o); it must be 0400", path, st.Mode().Perm())
	}
	if st.Size() != 32 && st.Size() != 64 {
		return nil, fmt.Errorf("seed file %s: %w", path, errSeedSize)
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("seed file %s cannot be read", path)
	}
	if len(seed) != 32 && len(seed) != 64 {
		return nil, fmt.Errorf("seed file %s: %w", path, errSeedSize)
	}
	return seed, nil
}

func loadKeys(path string) (*signerKeys, error) {
	seed, err := readSeed(path)
	if err != nil {
		return nil, err
	}
	return keysFromSeed(seed)
}

// writeSeed creates path (mode 0400, its directory 0700 when missing) and writes seed; an existing file is refused,
// never replaced.
func writeSeed(path string, seed []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("seed directory for %s cannot be created", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("seed file %s already exists; it is never overwritten", path)
	}
	if err != nil {
		return fmt.Errorf("seed file %s cannot be created", path)
	}
	_, err = f.Write(seed)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("seed file %s could not be written", path)
	}
	return nil
}

// cmdInit makes a new seed of size random bytes (crypto/rand) and prints the account it gives.
func cmdInit(path string, size int, stdout io.Writer) error {
	if size != 32 && size != 64 {
		return errSeedSize
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("seed file %s already exists; it is never overwritten", path)
	}
	seed := make([]byte, size)
	var keys *signerKeys
	for {
		if _, err := rand.Read(seed); err != nil {
			return errors.New("no randomness available")
		}
		var err error
		if keys, err = keysFromSeed(seed); err == nil {
			break
		}
	}
	if err := writeSeed(path, seed); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "new seed written to %s (mode 0400). Back it up now: escrow-signer backup -key <operator key file>\n", path)
	printInfo(stdout, keys)
	return nil
}

// cmdBackup writes the seed encrypted to the one OpenPGP key in keyFile, ASCII-armored, to stdout.
func cmdBackup(seedPath, keyFile string, stdout, stderr io.Writer) error {
	if keyFile == "" {
		return errors.New("-key names the operator's OpenPGP public key file")
	}
	seed, err := readSeed(seedPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("operator key file %s cannot be read", keyFile)
	}
	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(raw))
	if err != nil {
		ring, err = openpgp.ReadKeyRing(bytes.NewReader(raw))
	}
	if err != nil || len(ring) != 1 {
		return fmt.Errorf("operator key file %s must hold exactly one OpenPGP public key", keyFile)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return err
	}
	pt, err := openpgp.Encrypt(w, ring, nil, &openpgp.FileHints{IsBinary: true}, &packet.Config{DefaultCipher: packet.CipherAES256})
	if err != nil {
		return fmt.Errorf("the operator key in %s cannot encrypt (expired, revoked, or no encryption subkey)", keyFile)
	}
	if _, err = pt.Write(seed); err == nil {
		if err = pt.Close(); err == nil {
			err = w.Close()
		}
	}
	if err != nil {
		return errors.New("encryption failed")
	}
	buf.WriteByte('\n')
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "seed encrypted to OpenPGP key %X\n", ring[0].PrimaryKey.Fingerprint)
	return nil
}

// cmdRestore reads a decrypted seed (exactly 32 or 64 bytes) from stdin into a new seed file, after checking its
// account fingerprint when one is expected, and prints the account.
func cmdRestore(path, want string, stdin io.Reader, stdout io.Writer) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("seed file %s already exists; it is never overwritten", path)
	}
	seed, err := io.ReadAll(io.LimitReader(stdin, 65))
	if err != nil {
		return errors.New("the seed could not be read from stdin")
	}
	keys, err := keysFromSeed(seed)
	if err != nil {
		return fmt.Errorf("stdin: %w (pipe the decrypted backup straight in: gpg --decrypt seed-backup.asc | escrow-signer restore)", err)
	}
	if want != "" && want != keys.fingerprint() {
		return fmt.Errorf("the restored seed's account fingerprint is %s, not %s; nothing was written", keys.fingerprint(), want)
	}
	if err := writeSeed(path, seed); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "seed restored to %s (mode 0400)\n", path)
	printInfo(stdout, keys)
	return nil
}

// cmdServe signs on the socket until ctx ends.
func cmdServe(ctx context.Context, seedPath, socket, rpcURL string, stderr io.Writer) error {
	c, err := newCore(rpcURL)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(socket) {
		return errors.New("-socket (ESCROW_SIGNER_SOCKET) must be an absolute path")
	}
	keys, err := loadKeys(seedPath)
	if err != nil {
		return err
	}
	logger := log.New(stderr, "escrow-signer: ", log.LstdFlags|log.LUTC)
	syscall.Umask(0o117) // the socket is created 0660 (listenSocket sets it again)
	ln, err := listenSocket(socket)
	if err != nil {
		return fmt.Errorf("socket %s: %v", socket, err)
	}
	logger.Printf("listening on %s (protocol version %d, account fingerprint %s)", socket, protocolVersion, keys.fingerprint())
	serveSocket(ctx, ln, newSigner(keys, c, logger))
	logger.Print("stopped")
	return nil
}
