// Command verify-audit checks an OPSEC Market signed audit export against a pinned Ed25519 public key.
//
//	verify-audit -pub <64 hex chars> export.jsonl export.sig
//
// It exits 0 only when the signature over the exact export bytes verifies with the pinned key (not the key
// named inside the .sig file) and every line is a well-formed event in ascending id order. It uses only the Go
// standard library. A valid signature proves the export came from the holder of the signing key; it does not
// prove the server's history was never altered before the export was produced.
package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify-audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pub := fs.String("pub", "", "pinned Ed25519 public key (64 hex characters) published on the market's /canary page")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: verify-audit -pub <hex> export.jsonl export.sig") }
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *pub == "" || fs.NArg() != 2 {
		fs.Usage()
		return 1
	}
	export, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "FAIL:", err)
		return 1
	}
	sig, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, "FAIL:", err)
		return 1
	}
	events, last, err := verify(strings.TrimSpace(*pub), export, sig)
	if err != nil {
		fmt.Fprintln(stderr, "FAIL:", err)
		return 1
	}
	sum := sha256.Sum256(export)
	fmt.Fprintf(stdout, "OK: %d events (last id %d), sha256 %x, signed by the pinned key\n", events, last, sum)
	return 0
}

func verify(pinned string, export, sigFile []byte) (events int, lastID int64, err error) {
	pub, err := hex.DecodeString(pinned)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return 0, 0, errors.New("-pub must be 64 hexadecimal characters")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sigFile)), "\n") {
		k, v, ok := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return 0, 0, fmt.Errorf("malformed signature file line %q", line)
		}
		if _, dup := fields[k]; dup {
			return 0, 0, fmt.Errorf("signature file repeats %q", k)
		}
		fields[k] = v
	}
	if fields["algorithm"] != "ed25519" {
		return 0, 0, fmt.Errorf("unsupported algorithm %q", fields["algorithm"])
	}
	if !strings.EqualFold(fields["public_key"], pinned) {
		return 0, 0, errors.New("the signature file names a different public key than the pinned one")
	}
	sum := sha256.Sum256(export)
	if !strings.EqualFold(fields["sha256"], hex.EncodeToString(sum[:])) {
		return 0, 0, errors.New("export sha256 does not match the signature file")
	}
	signature, err := hex.DecodeString(fields["signature"])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return 0, 0, errors.New("malformed signature")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), export, signature) {
		return 0, 0, errors.New("signature does not verify with the pinned public key")
	}
	sc := bufio.NewScanner(bytes.NewReader(export))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e struct {
			ID      *int64  `json:"id"`
			Action  *string `json:"action"`
			Created *string `json:"created"`
		}
		if err = json.Unmarshal(sc.Bytes(), &e); err != nil || e.ID == nil || e.Action == nil || e.Created == nil {
			return 0, 0, fmt.Errorf("line %d is not an audit event", events+1)
		}
		if *e.ID <= lastID {
			return 0, 0, fmt.Errorf("line %d: ids are not strictly ascending", events+1)
		}
		lastID = *e.ID
		events++
	}
	if err = sc.Err(); err != nil {
		return 0, 0, err
	}
	if len(export) > 0 && export[len(export)-1] != '\n' {
		return 0, 0, errors.New("export does not end with a newline")
	}
	return events, lastID, nil
}
