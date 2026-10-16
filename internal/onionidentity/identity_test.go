package onionidentity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func vectorIdentity(t *testing.T) Identity {
	t.Helper()
	seed, err := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	if err != nil {
		t.Fatal(err)
	}
	var value [32]byte
	copy(value[:], seed)
	return identityFromSeed(value)
}

func privateTemp(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTorExpandedSecretMatchesRFC8032PublicKey(t *testing.T) {
	id := vectorIdentity(t)
	wantPublic, _ := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	if !bytes.Equal(id.PublicFile()[32:], wantPublic) {
		t.Fatal("expanded secret did not derive RFC8032 public key")
	}
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	wantExpanded := sha512.Sum512(seed)
	wantExpanded[0] &= 248
	wantExpanded[31] &= 63
	wantExpanded[31] |= 64
	if !bytes.Equal(id.SecretFile()[32:], wantExpanded[:]) {
		t.Fatal("Tor expanded secret format changed")
	}
	if bytes.Equal(id.SecretFile()[32:], ed25519.NewKeyFromSeed(seed)) {
		t.Fatal("Tor expanded secret incorrectly uses Go seed/public format")
	}
	if len(id.SecretFile()) != 96 || len(id.PublicFile()) != 64 || len(id.HostnameFile()) != 63 {
		t.Fatal("native file lengths changed")
	}
	if err := ValidateAddress(id.Address); err != nil {
		t.Fatal(err)
	}
	// Independently assemble the specification's checksum input and encoding.
	checksum := sha3.Sum256(append(append([]byte(".onion checksum"), wantPublic...), 3))
	encoded := append(append(append([]byte{}, wantPublic...), checksum[:2]...), 3)
	want := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(encoded)) + ".onion"
	if id.Address != want {
		t.Fatal("onion checksum address differs from specification")
	}
}

func TestParseRequiresMatchingNativeKeypairAndAddress(t *testing.T) {
	id := vectorIdentity(t)
	for _, hostname := range []string{"", id.Address, id.Address + "\n"} {
		got, err := Parse(id.SecretFile(), id.PublicFile(), hostname)
		if err != nil || got.Address != id.Address {
			t.Fatalf("valid identity rejected: %v", err)
		}
	}
	if _, err := Parse(id.SecretFile(), nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		secret, public []byte
		hostname       string
	}{
		{"truncated secret", id.SecretFile()[:95], id.PublicFile(), id.Address},
		{"long secret", append(id.SecretFile(), 0), id.PublicFile(), id.Address},
		{"bad secret header", append([]byte("?"), id.SecretFile()[1:]...), id.PublicFile(), id.Address},
		{"bad padding", append(id.SecretFile()[:31], append([]byte{1}, id.SecretFile()[32:]...)...), id.PublicFile(), id.Address},
		{"truncated public", id.SecretFile(), id.PublicFile()[:63], id.Address},
		{"empty public", id.SecretFile(), []byte{}, id.Address},
		{"bad public header", id.SecretFile(), append([]byte("?"), id.PublicFile()[1:]...), id.Address},
		{"mismatched public", id.SecretFile(), append(id.PublicFile()[:63], id.PublicFile()[63]^1), id.Address},
		{"bad checksum", id.SecretFile(), id.PublicFile(), "a" + id.Address[1:]},
		{"extra newline", id.SecretFile(), id.PublicFile(), id.Address + "\n\n"},
		{"CRLF", id.SecretFile(), id.PublicFile(), id.Address + "\r\n"},
		{"uppercase", id.SecretFile(), id.PublicFile(), strings.ToUpper(id.Address)},
		{"zero scalar", append([]byte(secretHeader), make([]byte, 64)...), nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.secret, tc.public, tc.hostname); err == nil {
				t.Fatal("invalid import accepted")
			}
		})
	}
	other, err := Generate(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(id.SecretFile(), id.PublicFile(), other.Address); err == nil {
		t.Fatal("different valid address accepted")
	}
	// Native expanded scalars must be used as stored, never clamped on import.
	expanded := id.SecretFile()
	expanded[32] ^= 1
	if _, err := Parse(expanded, id.PublicFile(), id.Address); err == nil {
		t.Fatal("import silently re-clamped a changed scalar")
	}
}

func TestValidatePublishedTorAddressesAndRejectCorruption(t *testing.T) {
	for _, value := range []string{
		"pg6mmjiyjmcrsslvykfwnntlaru7p5svn6y2ymmju6nubxndf4pscryd.onion",
		"sp3k262uwy4r2k3ycr5awluarykdpag6a7y33jxop4cs2lu5uz5sseqd.onion",
		"xa4r2iadxm55fbnqgwwi5mymqdcofiu3w6rpbtqn7b2dyn7mgwj64jyd.onion",
	} {
		if err := ValidateAddress(value); err != nil {
			t.Fatal(err)
		}
		if err := ValidateAddress(value[:52] + "aaaa.onion"); err == nil {
			t.Fatal("checksum/version corruption accepted")
		}
	}
}

func TestGenerateShortVanityAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := Generate(ctx, "a", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id.Address, "a") {
		t.Fatal("wrong vanity prefix")
	}
	if _, err := Parse(id.SecretFile(), id.PublicFile(), id.Address); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"abcdefg", "A", "1", "8", "a.onion", "a\n", "é"} {
		if _, err := Generate(ctx, prefix, 1); err == nil {
			t.Fatal("unsafe prefix accepted")
		}
	}
	for _, workers := range []int{-1, 65} {
		if _, err := Generate(ctx, "", workers); err == nil {
			t.Fatal("unbounded worker count accepted")
		}
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := Generate(canceled, "a", 2); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancellation ignored")
	}
	active, stop := context.WithCancel(context.Background())
	defer stop()
	if _, err := generate(active, "a", 1, func() (Identity, error) { stop(); return Identity{Address: "b"}, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("active cancellation ignored")
	}
}

func TestPrivateDirectoryRoundtripAndNoOverwrite(t *testing.T) {
	id := vectorIdentity(t)
	dir := filepath.Join(privateTemp(t), "identity")
	if err := id.WriteNew(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", SecretFilename, PublicFilename, HostnameFilename} {
		info, err := os.Stat(filepath.Join(dir, name))
		want := os.FileMode(0600)
		if name == "" {
			want = 0700
		}
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("output permissions are not private")
		}
	}
	got, err := Import(dir, id.Address)
	if err != nil || got.Address != id.Address {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if err := id.WriteNew(dir); err == nil {
		t.Fatal("existing directory overwritten")
	}
	if err := os.Remove(filepath.Join(dir, PublicFilename)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, HostnameFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err != nil {
		t.Fatal("optional public files not derived")
	}
	id.Address = ""
	if err := id.WriteNew(filepath.Join(privateTemp(t), "invalid")); err == nil {
		t.Fatal("mutated address was published")
	}
}

func TestReadRejectsUnsafeFilesAndSymlinks(t *testing.T) {
	id := vectorIdentity(t)
	for _, tc := range []string{"secret symlink", "public symlink", "hostname symlink", "directory symlink", "ancestor symlink", "public writable", "secret public", "directory public", "bad hostname"} {
		t.Run(tc, func(t *testing.T) {
			base := privateTemp(t)
			dir := filepath.Join(base, "id")
			if err := id.WriteNew(dir); err != nil {
				t.Fatal(err)
			}
			readDir := dir
			switch tc {
			case "secret symlink", "public symlink", "hostname symlink":
				name := SecretFilename
				if tc == "public symlink" {
					name = PublicFilename
				}
				if tc == "hostname symlink" {
					name = HostnameFilename
				}
				path := filepath.Join(dir, name)
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(name+".real", path); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				readDir = filepath.Join(base, "alias")
				if err := os.Symlink(dir, readDir); err != nil {
					t.Fatal(err)
				}
			case "ancestor symlink":
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(base, alias); err != nil {
					t.Fatal(err)
				}
				readDir = filepath.Join(alias, "id")
			case "public writable":
				os.Chmod(filepath.Join(dir, PublicFilename), 0666)
			case "secret public":
				os.Chmod(filepath.Join(dir, SecretFilename), 0644)
			case "directory public":
				os.Chmod(dir, 0755)
			case "bad hostname":
				os.WriteFile(filepath.Join(dir, HostnameFilename), []byte("bad.onion\n"), 0600)
			}
			if _, err := Read(readDir); err == nil {
				t.Fatal("unsafe identity imported")
			}
		})
	}
}

func TestCLIImportChecksAddressWithoutLeakingSecret(t *testing.T) {
	id := vectorIdentity(t)
	base := privateTemp(t)
	source := filepath.Join(base, "source")
	if err := id.WriteNew(source); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	destination := filepath.Join(base, "import")
	args := []string{"import", "--address", id.Address, "--public-key", filepath.Join(source, PublicFilename), "--secret-key", filepath.Join(source, SecretFilename), "--out-dir", destination}
	if err := Run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != id.Address+"\n" {
		t.Fatal("CLI output contains unexpected information")
	}
	if err := Run(context.Background(), args, &out); err == nil {
		t.Fatal("CLI overwrote existing identity")
	}
	if bytes.Contains(out.Bytes(), id.SecretFile()[32:]) {
		t.Fatal("secret leaked to CLI output")
	}
	if err := Run(context.Background(), []string{"generate", "--out-dir", filepath.Join(base, "generated"), "--prefix", "a", "--workers", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"generate", "--out-dir", filepath.Join(base, "invalid"), "--unknown-secret", "sensitive-input"},
		{"import", "--out-dir", filepath.Join(base, "invalid")},
		{"generate", "--prefix", "a"},
	} {
		err := Run(context.Background(), args, &out)
		if err == nil || strings.Contains(err.Error(), "sensitive-input") {
			t.Fatal("invalid CLI arguments accepted or echoed")
		}
	}
}

func TestCLIRefusesUnusableOutputBeforeStartingSearch(t *testing.T) {
	base := privateTemp(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, path := range []string{base, filepath.Join(base, "missing-parent", "new")} {
		err := Run(canceled, []string{"generate", "--prefix", "a", "--out-dir", path}, io.Discard)
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatal("search started before checking output destination")
		}
	}
}
