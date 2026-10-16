// Package onionidentity creates and validates native Tor v3 service identities.
// Tor stores an expanded Ed25519 scalar and PRF key, not Go's seed/public pair.
package onionidentity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha3"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"runtime"
	"strings"
	"sync"

	"filippo.io/edwards25519"
)

const (
	SecretFilename   = "hs_ed25519_secret_key"
	PublicFilename   = "hs_ed25519_public_key"
	HostnameFilename = "hostname"
	secretHeader     = "== ed25519v1-secret: type0 ==\x00\x00\x00"
	publicHeader     = "== ed25519v1-public: type0 ==\x00\x00\x00"
	MaxPrefixLength  = 6
	MaxWorkers       = 64
)

var onionEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Identity holds the Tor service master identity. File getters return copies;
// callers must keep SecretFile's contents private and off application mounts.
type Identity struct {
	Address string
	secret  [64]byte
	public  [32]byte
}

func (id Identity) SecretFile() []byte   { return append([]byte(secretHeader), id.secret[:]...) }
func (id Identity) PublicFile() []byte   { return append([]byte(publicHeader), id.public[:]...) }
func (id Identity) HostnameFile() []byte { return []byte(id.Address + "\n") }

// Generate searches random independent identities. Prefixes are lowercase Tor
// base32, at most six characters. A caller controls duration through ctx; the
// prefix limit is not a completion-time guarantee. Zero workers uses at most
// eight of the process's available Go execution threads.
func Generate(ctx context.Context, prefix string, workers int) (Identity, error) {
	return generate(ctx, prefix, workers, randomIdentity)
}

func generate(ctx context.Context, prefix string, workers int, next func() (Identity, error)) (Identity, error) {
	if len(prefix) > MaxPrefixLength || strings.IndexFunc(prefix, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7')
	}) >= 0 {
		return Identity{}, errors.New("onion identity: prefix must contain at most six lowercase a-z or 2-7 characters")
	}
	if workers == 0 {
		workers = min(runtime.GOMAXPROCS(0), 8)
	}
	if workers < 1 || workers > MaxWorkers {
		return Identity{}, errors.New("onion identity: workers must be between 1 and 64, or 0 for the default")
	}
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if prefix == "" {
		return next()
	}
	search, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan Identity, 1)
	failures := make(chan error, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for search.Err() == nil {
				id, err := next()
				if err != nil {
					select {
					case failures <- err:
					default:
					}
					cancel()
					return
				}
				if strings.HasPrefix(id.Address, prefix) {
					select {
					case results <- id:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	select {
	case id := <-results:
		cancel()
		return id, nil
	case err := <-failures:
		cancel()
		return Identity{}, err
	case <-ctx.Done():
		cancel()
		return Identity{}, ctx.Err()
	}
}

func randomIdentity() (Identity, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return Identity{}, errors.New("onion identity: secure random generation failed")
	}
	return identityFromSeed(seed), nil
}

func identityFromSeed(seed [32]byte) Identity {
	expanded := sha512.Sum512(seed[:])
	expanded[0] &= 248
	expanded[31] &= 63
	expanded[31] |= 64
	id, _ := identityFromExpanded(expanded)
	return id
}

func identityFromExpanded(expanded [64]byte) (Identity, error) {
	// Reduce the stored scalar as-is: re-clamping an imported expanded key can
	// change its identity. Only newly generated seeds are expanded and clamped.
	var wide [64]byte
	copy(wide[:32], expanded[:32])
	scalar, _ := new(edwards25519.Scalar).SetUniformBytes(wide[:])
	if scalar.Equal(new(edwards25519.Scalar)) == 1 {
		return Identity{}, errors.New("onion identity: invalid zero secret scalar")
	}
	point := new(edwards25519.Point).ScalarBaseMult(scalar)
	id := Identity{secret: expanded}
	copy(id.public[:], point.Bytes())
	id.Address = address(id.public[:])
	return id, nil
}

func address(public []byte) string {
	var checksumInput [47]byte
	copy(checksumInput[:], ".onion checksum")
	copy(checksumInput[15:47], public)
	digest := sha3.Sum256(append(checksumInput[:], byte(3)))
	var encoded [35]byte
	copy(encoded[:32], public)
	copy(encoded[32:34], digest[:2])
	encoded[34] = 3
	return strings.ToLower(onionEncoding.EncodeToString(encoded[:])) + ".onion"
}

// ValidateAddress checks the complete v3 address, including version, checksum
// and canonical non-identity Ed25519 public point. It accepts no whitespace.
func ValidateAddress(value string) error {
	if len(value) != 62 || !strings.HasSuffix(value, ".onion") || value != strings.ToLower(value) {
		return errors.New("onion identity: expected a complete lowercase v3 .onion address")
	}
	decoded, err := onionEncoding.DecodeString(strings.ToUpper(value[:56]))
	if err != nil || len(decoded) != 35 || decoded[34] != 3 || address(decoded[:32]) != value {
		return errors.New("onion identity: invalid v3 address checksum or version")
	}
	point, err := new(edwards25519.Point).SetBytes(decoded[:32])
	if err != nil || !bytes.Equal(point.Bytes(), decoded[:32]) || point.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return errors.New("onion identity: invalid service public key")
	}
	return nil
}

// Parse validates exact native Tor key headers and derives the public identity
// from the expanded secret. A supplied public file or hostname must match. The
// hostname may have the one final newline written by Tor.
func Parse(secretFile, publicFile []byte, hostname string) (Identity, error) {
	if len(secretFile) != 96 || !bytes.Equal(secretFile[:32], []byte(secretHeader)) {
		return Identity{}, errors.New("onion identity: secret file must be exactly 96 bytes with the native Tor v3 header")
	}
	var expanded [64]byte
	copy(expanded[:], secretFile[32:])
	id, err := identityFromExpanded(expanded)
	if err != nil {
		return Identity{}, err
	}
	if publicFile != nil && (len(publicFile) != 64 || !bytes.Equal(publicFile[:32], []byte(publicHeader)) || subtle.ConstantTimeCompare(publicFile[32:], id.public[:]) != 1) {
		return Identity{}, errors.New("onion identity: public key does not match the native Tor expanded secret")
	}
	if hostname != "" {
		hostname = strings.TrimSuffix(hostname, "\n")
		if err := ValidateAddress(hostname); err != nil {
			return Identity{}, err
		}
		if hostname != id.Address {
			return Identity{}, errors.New("onion identity: supplied address does not match the keypair")
		}
	}
	return id, nil
}
