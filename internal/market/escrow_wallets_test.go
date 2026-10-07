package market

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A-450 (ruling P4-R13; flipped by A-456b): the ES-2 paste check on settlement PSBTs signed by real wallets on regtest,
// the files scripts/test-escrow-wallets.sh writes (Electrum 4.8.2 offline, Bitcoin Core 31.1; throwaway keys, public
// data only). Electrum writes a derivation for every key of the multisig, the market's included, which the check
// refused "under the market's key" until A-456b relaxed that refusal for per-order wallets (P4-R2, P4-R13): every
// Electrum signature is now accepted, with that entry in it, and only the party's signatures are taken. Every signature
// rule stays (TestEscrowPasteRefusals).
// The committed files are testdata/escrow-wallets; the script points ESCROW_WALLETS_FIXTURES at the files of its run.

type walletFixture struct {
	File              string `json:"file"`
	Wallet            string `json:"wallet"`
	Shape             string `json:"shape"`
	Variant           string `json:"variant"`
	PartyPubKey       string `json:"party_pubkey"`
	MarketPubKey      string `json:"market_pubkey"`
	MarketFingerprint string `json:"market_fingerprint"`
	TxID              string `json:"txid"`
	Want              string `json:"want"`
}

func TestEscrowWalletFixtures(t *testing.T) {
	dir := os.Getenv("ESCROW_WALLETS_FIXTURES")
	if dir == "" {
		dir = filepath.Join("internal", "market", "testdata", "escrow-wallets") // tests run from the repository root (TestMain)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Entries []walletFixture `json:"entries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	electrum := 0
	for _, f := range manifest.Entries {
		if strings.HasPrefix(f.Wallet, "Electrum") {
			electrum++
			if f.Want != "accepted" {
				t.Errorf("%s: want %q; since A-456b relaxed the derivation refusal (P4-R13) the check accepts every Electrum signature", f.File, f.Want)
			}
		}
		t.Run(f.File, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join(dir, f.File))
			if err != nil {
				t.Fatal(err)
			}
			p, err := decodePSBT(string(text))
			if err != nil || p.TxID() != f.TxID {
				t.Fatalf("not the recorded transaction: %v", err)
			}
			// No private material: no extended private key (xprv or tprv version bytes, or their base58 text).
			for _, v := range [][]byte{{0x04, 0x88, 0xad, 0xe4}, {0x04, 0x35, 0x83, 0x94}, []byte("xprv"), []byte("tprv")} {
				if bytes.Contains(p.Raw, v) || bytes.Contains(data, v) {
					t.Fatal("the fixture or its manifest carries an extended private key")
				}
			}
			party, market := walletKey(t, f.PartyPubKey, ""), walletKey(t, f.MarketPubKey, f.MarketFingerprint)
			sigs, err := checkSettlementPaste(string(text), f.TxID, party, market)
			switch f.Want {
			case "accepted":
				if err != nil {
					t.Fatalf("%s (shape %s, %s) refused: %v", f.Wallet, f.Shape, f.Variant, err)
				}
				t.Logf("%s, shape (%s), %s: accepted by today's ES-2 check", f.Wallet, f.Shape, f.Variant)
				if strings.HasPrefix(f.Wallet, "Electrum") {
					// What A-450 found and P4-R13 relaxed: Electrum writes a derivation for every key, the market's
					// included, and the paste is accepted with it (only the party's signatures are kept).
					for i, in := range p.Inputs {
						if len(in.Derivations) != 3 || !slices.ContainsFunc(in.Derivations, func(d psbtDerivation) bool { return bytes.Equal(d.PubKey, market.PubKey) }) {
							t.Fatalf("input %d: %d derivations; Electrum writes one per key, the market's included", i, len(in.Derivations))
						}
					}
				}
			default:
				t.Fatalf("want %q", f.Want)
			}
			if len(sigs) != len(p.Inputs) {
				t.Fatalf("%d signatures for %d inputs", len(sigs), len(p.Inputs))
			}
			for i, s := range sigs {
				if !bytes.Equal(s.PubKey, party.PubKey) || s.SighashByte() != 0x01 {
					t.Fatalf("input %d: not the party's SIGHASH_ALL signature", i)
				}
			}
		})
	}
	if electrum == 0 {
		t.Fatal("no Electrum fixture")
	}
}

func walletKey(t *testing.T, pub, fp string) escrowKeyRow {
	t.Helper()
	b, err := hex.DecodeString(pub)
	if err != nil || len(b) != 33 {
		t.Fatalf("public key %q", pub)
	}
	return escrowKeyRow{PubKey: b, Fingerprint: fp}
}
