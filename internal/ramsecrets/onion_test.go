package ramsecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"opsecmkt/internal/onionidentity"
)

func onionBundle(t *testing.T) (string, Bundle) {
	t.Helper()
	dir, _ := fixture(t)
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := onionidentity.Generate(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.WriteNew(filepath.Join(dir, "onion-public")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "operator.json")
	var fields map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["onion_dir"], fields["onion_address"] = "onion-public", identity.Address
	data, _ = json.Marshal(fields)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	data, err = collect(dir)
	if err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	return dir, bundle
}

func TestBundlePreservesPrivateOnionIdentityAcrossUnlock(t *testing.T) {
	dir, bundle := onionBundle(t)
	body, _ := json.Marshal(bundle)
	target := filepath.Join(t.TempDir(), "runtime")
	if _, err := runFake(t, fakePlatform(), body, "unlock", "--dir", target); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hostname", "hs_ed25519_public_key", "hs_ed25519_secret_key"} {
		before, _ := os.ReadFile(filepath.Join(dir, "onion-public", name))
		path := filepath.Join(target, "onion-public", name)
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("onion identity changed during unlock")
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("onion material is not private")
		}
	}
	info, err := os.Stat(filepath.Join(target, "onion-public"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("onion directory is not private")
	}
}

func TestBundleRejectsUnboundOrMismatchedOnionMaterial(t *testing.T) {
	_, original := onionBundle(t)
	for _, test := range []struct {
		name   string
		mutate func(*Bundle)
	}{
		{"missing", func(b *Bundle) {
			for i, f := range b.Files {
				if f.Path == "onion-public/hostname" {
					b.Files = append(b.Files[:i], b.Files[i+1:]...)
					break
				}
			}
		}},
		{"unreferenced", func(b *Bundle) {
			for i := range b.Files {
				if b.Files[i].Path == "operator.json" {
					var fields map[string]any
					json.Unmarshal(b.Files[i].Data, &fields)
					delete(fields, "onion_dir")
					delete(fields, "onion_address")
					b.Files[i].Data, _ = json.Marshal(fields)
				}
			}
		}},
		{"mismatched-key", func(b *Bundle) {
			for i := range b.Files {
				if b.Files[i].Path == "onion-public/hs_ed25519_public_key" {
					b.Files[i].Data[len(b.Files[i].Data)-1] ^= 1
				}
			}
		}},
		{"extra", func(b *Bundle) {
			b.Files = append(b.Files, File{Path: "onion-public/extra", Data: []byte("unexpected")})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := original
			b.Files = make([]File, len(original.Files))
			for i, f := range original.Files {
				b.Files[i] = File{Path: f.Path, Data: append([]byte(nil), f.Data...)}
			}
			test.mutate(&b)
			if _, err := validate(b); err == nil {
				t.Fatal("accepted invalid onion identity bundle")
			}
		})
	}
}
