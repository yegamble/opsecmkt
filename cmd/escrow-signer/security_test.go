package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

type blockingSignerRPC struct {
	started  chan context.Context
	finished chan struct{}
}

func (b *blockingSignerRPC) call(ctx context.Context, _ string, _ []any, _ any) error {
	b.started <- ctx
	<-ctx.Done()
	b.finished <- struct{}{}
	return errCoreDown
}

func TestSignerConnectionBound(t *testing.T) {
	for _, closeListener := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "listener-close"}[closeListener], func(t *testing.T) {
			ln, err := listenSocket(testSocketPath(t))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer ln.Close()
			blocked := &blockingSignerRPC{make(chan context.Context, maxSignerConns+1), make(chan struct{}, maxSignerConns+1)}
			s := newSigner(&signerKeys{}, blocked, log.New(io.Discard, "", 0))
			done := make(chan struct{})
			go func() { serveSocket(ctx, ln, s); close(done) }()
			t.Cleanup(func() {
				cancel()
				ln.Close()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("socket server did not finish during cleanup")
				}
			})
			for i := 0; i < maxSignerConns; i++ {
				c, err := net.DialTimeout("unix", ln.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if _, err := io.WriteString(c, `{"version":1,"op":"sign","psbt":"cHNidP8=","index":1}`+"\n"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-blocked.started:
				case <-time.After(2 * time.Second):
					t.Fatal("admitted request did not reach Core")
				}
			}
			// All slots are doing RPC work. An extra connection is closed before it can dispatch a request.
			extra, err := net.DialTimeout("unix", ln.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer extra.Close()
			extra.SetDeadline(time.Now().Add(2 * time.Second))
			io.WriteString(extra, `{"version":1,"op":"sign","psbt":"cHNidP8=","index":1}`+"\n")
			var one [1]byte
			if _, err := extra.Read(one[:]); err == nil {
				t.Fatal("excess connection remained open")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("excess connection was left waiting instead of rejected")
			}
			select {
			case <-blocked.started:
				t.Fatal("Core work exceeded the connection bound")
			default:
			}
			if closeListener {
				ln.Close()
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown did not close connections and cancel pending RPCs")
			}
			if len(blocked.finished) != maxSignerConns {
				t.Fatal("shutdown returned before all Core calls ended")
			}
		})
	}
}

func TestSignerConnectionSlotReleased(t *testing.T) {
	sock, _ := startServer(t)
	var clients []net.Conn
	for i := 0; i < maxSignerConns; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		io.WriteString(c, `{"version":1,"op":"info"}`+"\n")
		if _, err := bufio.NewReader(c).ReadBytes('\n'); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	clients[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", sock, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(deadline)
		io.WriteString(c, `{"version":1,"op":"info"}`+"\n")
		line, err := bufio.NewReader(c).ReadBytes('\n')
		c.Close()
		if err == nil && bytes.Contains(line, []byte(`"tpub"`)) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("closing an admitted connection did not free its slot")
}

// Limit a real pipe's write deadline so the test exercises blocked replies without waiting two minutes.
type shortWriteDeadlineConn struct{ net.Conn }

func (c shortWriteDeadlineConn) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		return c.Conn.SetWriteDeadline(t)
	}
	return c.Conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
}

func TestSignerReplyWriteDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	done := make(chan struct{})
	go func() { serveConn(context.Background(), shortWriteDeadlineConn{server}, &signer{}); close(done) }()
	client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(client, "not-json\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a client that never reads a reply kept the connection handler blocked")
	}
}

func armoredKey(t *testing.T, kind string, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := armor.Encode(&out, kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestSignerBackupPublicKeyValidation(t *testing.T) {
	operator, err := openpgp.NewEntity("Operator", "", "operator@example.invalid", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var public, private, bare, mixed, signingOnly bytes.Buffer
	if err := operator.Serialize(&public); err != nil {
		t.Fatal(err)
	}
	if err := operator.SerializePrivateWithoutSigning(&private, nil); err != nil {
		t.Fatal(err)
	}
	if err := operator.PrimaryKey.Serialize(&bare); err != nil {
		t.Fatal(err)
	}
	noSubkeys := *operator
	noSubkeys.Subkeys = nil
	if err := noSubkeys.Serialize(&signingOnly); err != nil {
		t.Fatal(err)
	}
	mixed.Write(signingOnly.Bytes())
	if err := operator.Subkeys[0].PrivateKey.Serialize(&mixed); err != nil {
		t.Fatal(err)
	}
	if err := operator.Subkeys[0].Sig.Serialize(&mixed); err != nil {
		t.Fatal(err)
	}
	pubArmor := armoredKey(t, openpgp.PublicKeyType, public.Bytes())
	dir := t.TempDir()
	seedPath, keyPath := filepath.Join(dir, "seed"), filepath.Join(dir, "operator.asc")
	if err := writeSeed(seedPath, testSeed); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"private binary":                 private.Bytes(),
		"private armor":                  armoredKey(t, openpgp.PrivateKeyType, private.Bytes()),
		"secret packets in public armor": armoredKey(t, openpgp.PublicKeyType, private.Bytes()),
		"private subkey binary":          mixed.Bytes(),
		"private subkey armor":           armoredKey(t, openpgp.PublicKeyType, mixed.Bytes()),
		"two binary entities":            append(bytes.Clone(public.Bytes()), public.Bytes()...),
		"two armored entities":           append(bytes.Clone(pubArmor), pubArmor...),
		"valid then private":             append(bytes.Clone(public.Bytes()), private.Bytes()...),
		"private then valid":             append(bytes.Clone(private.Bytes()), public.Bytes()...),
		"malformed then valid":           append(bytes.Clone(bare.Bytes()), public.Bytes()...),
		"valid then malformed":           append(bytes.Clone(public.Bytes()), bare.Bytes()...),
		"unsupported secret packet":      append(bytes.Clone(public.Bytes()), 0xc5, 1, 0xff),
		"identityless v4":                bare.Bytes(),
		"truncated":                      public.Bytes()[:len(public.Bytes())-1],
		"garbage":                        []byte("not an OpenPGP key"),
		"signing only":                   signingOnly.Bytes(),
		"oversized":                      append(bytes.Clone(pubArmor), bytes.Repeat([]byte{' '}, maxOperatorKey+1-len(pubArmor))...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if err := cmdBackup(seedPath, keyPath, &out, &errOut); err == nil || out.Len() != 0 || errOut.Len() != 0 {
				t.Fatalf("invalid recipient produced output or succeeded: %v", err)
			} else if name == "oversized" && !strings.Contains(err.Error(), "64 KiB") {
				t.Fatalf("oversize input was not rejected by its size: %v", err)
			}
		})
	}
	for name, raw := range map[string][]byte{
		"binary": public.Bytes(), "armored": pubArmor,
		"exact size limit": append(bytes.Clone(pubArmor), bytes.Repeat([]byte{' '}, maxOperatorKey-len(pubArmor))...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if err := cmdBackup(seedPath, keyPath, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			block, err := armor.Decode(&out)
			if err != nil {
				t.Fatal(err)
			}
			message, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{operator}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := io.ReadAll(message.UnverifiedBody)
			if err != nil || !bytes.Equal(plain, testSeed) {
				t.Fatal("valid backup did not decrypt to the original seed")
			}
		})
	}
}

func TestSignerBackupV6WithoutIdentity(t *testing.T) {
	operator, err := openpgp.NewEntity("Operator", "", "operator@example.invalid", &packet.Config{V6Keys: true, Algorithm: packet.PubKeyAlgoEd25519})
	if err != nil {
		t.Fatal(err)
	}
	operator.Identities = nil
	var public bytes.Buffer
	if err := operator.Serialize(&public); err != nil {
		t.Fatal(err)
	}
	entity, err := readOperatorKey(public.Bytes())
	if err != nil || entity.PrimaryIdentity() != nil {
		t.Fatalf("valid identityless v6 key was refused: %v", err)
	}
	var encrypted bytes.Buffer
	w, err := openpgp.Encrypt(&encrypted, openpgp.EntityList{entity}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(testSeed)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	message, err := openpgp.ReadMessage(&encrypted, openpgp.EntityList{operator}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(message.UnverifiedBody)
	if err != nil || !bytes.Equal(plain, testSeed) {
		t.Fatal("v6 backup did not decrypt to the original seed")
	}
}

func TestSignerIntermediateKeyWipe(t *testing.T) {
	parent, err := masterKey(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.hardenedChild(48)
	if err != nil {
		t.Fatal(err)
	}
	defer child.wipe()
	childBefore := child.serialize(tprvVersion)
	words := parent.key.Bits()
	parent.wipe()
	for _, word := range words[:cap(words)] {
		if word != 0 {
			t.Fatal("temporary key backing buffer retained key words")
		}
	}
	if parent.key != nil || parent.chain != [32]byte{} || child.serialize(tprvVersion) != childBefore {
		t.Fatal("wiping the parent did not clear it independently of the child")
	}
	input := bytes.Clone(testSeed)
	keys, err := keysFromSeed(input)
	if err != nil || !bytes.Equal(input, testSeed) || keys.accountTprv == "" || keys.chain == [32]byte{} {
		t.Fatal("temporary clearing corrupted caller data or active signer key")
	}
}
