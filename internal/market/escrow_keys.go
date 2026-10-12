package market

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"opsecmkt/internal/btc"
)

// Escrow keys (WR-escrow ES-3, A-246 Phase 1). A party registers one extended public key with its key origin,
// "[fingerprint/path]tpub…", parsed here strictly before any RPC sees it: base58check and the BIP32 serialization
// are decoded in Go (stdlib only). Child keys of escrows are derived by Bitcoin Core (utxoupdatepsbt), never here; the
// shape-(b) per-order derivation (escrow_bip32.go, no product caller yet) is cross-checked against Core.

// The base58check, curve and extended-key helpers live in internal/btc (A-454: shared with cmd/escrow-signer).
var (
	base58CheckDecode = btc.Base58CheckDecode
	base58CheckEncode = btc.Base58CheckEncode
	doubleSHA256      = btc.DoubleSHA256
	onCurve           = btc.OnCurve
	secpN             = btc.N // the secp256k1 group order
)

// Extended-key version bytes (BIP32 and SLIP-132). A market accepts its own network class's plain and P2WSH multisig
// public versions (keyNet): tpub and Vpub on test networks (regtest, signet, testnet4), xpub and Zpub on mainnet
// (A-460). Every private version is "exposed".
var (
	privateVersions = map[uint32]bool{0x0488ade4: true, 0x04358394: true, 0x049d7878: true, 0x04b2430c: true, 0x044a4e28: true, 0x045f18bc: true, 0x0295b005: true, 0x02aa7a99: true, 0x024285b5: true, 0x02575048: true}
	privatePrefixes = []string{"xprv", "tprv", "yprv", "zprv", "uprv", "vprv", "Yprv", "Zprv", "Uprv", "Vprv"}
)

// The test networks' plain and multisig public versions, the default class.
const (
	versionTpub = btc.VersionTpub
	versionVpub = btc.VersionVpub
)

// keyNet is a network class's extended-key version bytes and BIP44 coin type (A-460): the plain public version Bitcoin
// Core's descriptors use, the SLIP-132 P2WSH multisig version wallets show, the private version, the single-signature
// and nested-SegWit multisig public versions (refused), and the coin type of the BIP48 account path.
type keyNet struct {
	main                 bool
	pub, msig, prv, coin uint32
	singleSig            [2]uint32
	nested               uint32
}

var (
	keyNetTest = keyNet{pub: btc.VersionTpub, msig: btc.VersionVpub, prv: btc.VersionTprv, coin: 1, singleSig: [2]uint32{0x044a5262, 0x045f1cf6}, nested: 0x024289ef}             // upub, vpub; Upub
	keyNetMain = keyNet{main: true, pub: btc.VersionXpub, msig: btc.VersionZpub, prv: btc.VersionXprv, coin: 0, singleSig: [2]uint32{0x049d7cb2, 0x04b24746}, nested: 0x0295b43f} // ypub, zpub; Ypub
)

// keyNet is the market's key class: mainnet's on a mainnet market (a.live).
func (a *App) keyNet() keyNet { return keyNetFor(a.live) }

// keyNet is the escrow node's key class: mainnet's when the node is on chain "main" (as startEscrow read it).
func (e *escrowBTC) keyNet() keyNet { return keyNetFor(e.network == "main") }

// keyNetFor is the key class of a mainnet market (main) or a test-network one.
func keyNetFor(main bool) keyNet {
	if main {
		return keyNetMain
	}
	return keyNetTest
}

// other is the other network class.
func (n keyNet) other() keyNet { return keyNetFor(!n.main) }

// isPublic reports a public version of class n: plain, multisig, single-signature or nested.
func (n keyNet) isPublic(v uint32) bool {
	return v == n.pub || v == n.msig || v == n.singleSig[0] || v == n.singleSig[1] || v == n.nested
}

// keyRefusal is a refused key expression. Its message is fixed text for the account page: it never repeats what
// was pasted (a pasted private key must not come back in a response or a log). msg is the test-network text, main the
// mainnet market's where it differs (keyRefusalText).
type keyRefusal struct{ msg, main string }

func (e *keyRefusal) Error() string { return e.msg }

// keyRefusalText is err's text for a market of class main: a keyRefusal's mainnet text on a mainnet market.
func keyRefusalText(err error, main bool) string {
	var kr *keyRefusal
	if main && errors.As(err, &kr) && kr.main != "" {
		return kr.main
	}
	return err.Error()
}

var (
	errKeyExposed    = &keyRefusal{"This is a private key or recovery words, not a public key: this key is exposed, replace it. Anyone who has seen it can spend from that wallet, so move any coins it holds to a new wallet. Then paste only the new wallet's public key (tpub) with its origin. Nothing was saved.", "This is a private key or recovery words, not a public key: this key is exposed, replace it. Anyone who has seen it can spend from that wallet, so move any coins it holds to a new wallet. Then paste only the new wallet's public key (xpub) with its origin. Nothing was saved."}
	errKeyLong       = &keyRefusal{"The key is too long. Paste one key with its origin, for example [d34db33f/48h/1h/0h/2h]tpub…", "The key is too long. Paste one key with its origin, for example [d34db33f/48h/0h/0h/2h]xpub…"}
	errKeySpace      = &keyRefusal{"Paste the key as one line, with no spaces or line breaks inside it.", ""}
	errKeyComma      = &keyRefusal{"Paste exactly one key. A comma means more than one key, and an escrow built from them could let one party spend alone.", ""}
	errKeyDescriptor = &keyRefusal{"Paste only the key with its origin, [fingerprint/path]tpub…, not a whole descriptor or its #checksum.", "Paste only the key with its origin, [fingerprint/path]xpub…, not a whole descriptor or its #checksum."}
	errKeyOrigin     = &keyRefusal{"The key needs its origin in front: [fingerprint/path]tpub…. Bitcoin Core's listdescriptors shows both together.", "The key needs its origin in front: [fingerprint/path]xpub…. Bitcoin Core's listdescriptors shows both together."}
	errKeyOriginForm = &keyRefusal{"The origin must be [, the wallet's 8-character fingerprint, the derivation path from it (at most 10 steps) and ], for example [d34db33f/48h/1h/0h/2h].", "The origin must be [, the wallet's 8-character fingerprint, the derivation path from it (at most 10 steps) and ], for example [d34db33f/48h/0h/0h/2h]."}
	errKeyDepth      = &keyRefusal{"The origin path does not belong to this key. Copy the key and its origin together from your wallet.", ""}
	errKeySuffix     = &keyRefusal{"Nothing may follow the key except /0/*.", ""}
	errKeyInvalid    = &keyRefusal{"This is not a valid extended public key. Check that it was copied completely.", ""}
	errKeyMainnet    = &keyRefusal{"This is a mainnet key (xpub, zpub or similar). Escrow runs on test networks only on this market: export a test-network key (tpub).", ""}
	errKeyTestnet    = &keyRefusal{"This is a test-network key (tpub, vpub or similar). This market runs on Bitcoin mainnet: export a mainnet key (xpub) from a wallet used only for this market.", ""}
	errKeySingleSig  = &keyRefusal{"This key is marked for single-signature use (a upub or vpub). Export a plain tpub, or a multisig Vpub, from a wallet used only for this market.", "This key is marked for single-signature use (a ypub or zpub). Export a plain xpub, or a multisig Zpub, from a wallet used only for this market."}
	errKeyNested     = &keyRefusal{"This key is marked for nested SegWit multisig (a Upub). Export the native SegWit multisig key (m/48h/1h/0h/2h) as a tpub or Vpub.", "This key is marked for nested SegWit multisig (a Ypub). Export the native SegWit multisig key (m/48h/0h/0h/2h) as an xpub or Zpub."}
	errKeyVersion    = &keyRefusal{"This is not a Bitcoin test-network extended public key.", "This is not a Bitcoin mainnet extended public key."}
)

// escrowKey is a parsed key expression: the origin (fingerprint and path) and the account key, stored with its class's
// plain version bytes (a tpub, or an xpub on a mainnet market).
type escrowKey struct {
	Fingerprint string   // 8 lower-case hex digits
	Path        []uint32 // BIP32 steps from the fingerprint's master key; hardened steps have bit 31 set
	PathText    string   // canonical: "/48h/1h/0h/2h"
	XPub        string   // the key re-encoded with its class's plain version bytes (tpub or xpub)
	ChainCode   [32]byte
	PubKey      [33]byte
	BIP48       bool // m/48h/<coin>h/<account>h/2h: the standard P2WSH multisig account of its class (coin 1 or 0)
}

// Expression is the canonical key expression Bitcoin Core accepts in a descriptor.
func (k escrowKey) Expression() string { return "[" + k.Fingerprint + k.PathText + "]" + k.XPub }

// KeyMaterial identifies the key whatever its version bytes, depth or origin metadata: chain code and public key.
// Cross-account reuse is refused on its HMAC (user_escrow_keys.xpub_hmac).
func (k escrowKey) KeyMaterial() string {
	return hex.EncodeToString(k.ChainCode[:]) + hex.EncodeToString(k.PubKey[:])
}

const (
	escrowKeyMaxLen     = 300 // origin (at most 10 steps) + 111-character key + "/0/*"
	escrowKeyScanMaxLen = 64 << 10
	escrowKeyMaxDepth   = 10
)

// parseEscrowKey applies ES-3 to one pasted key expression. Private material anywhere in the input is refused
// first ("this key is exposed, replace it"), then the syntax a descriptor could hide a second key in (whitespace,
// commas, parentheses, '#'), then the origin, the key and its version bytes, which must be a public version of the
// market's class n (a key of the other class is refused, naming it). Outer whitespace is trimmed.
func parseEscrowKey(in string, n keyNet) (escrowKey, error) {
	if len(in) > escrowKeyScanMaxLen {
		return escrowKey{}, errKeyLong
	}
	if containsPrivateMaterial(in) {
		return escrowKey{}, errKeyExposed
	}
	s := strings.TrimSpace(in)
	if len(s) > escrowKeyMaxLen {
		return escrowKey{}, errKeyLong
	}
	switch {
	case strings.ContainsFunc(s, isSpaceByte):
		return escrowKey{}, errKeySpace
	case strings.Contains(s, ","):
		return escrowKey{}, errKeyComma
	case strings.ContainsAny(s, "()#"):
		return escrowKey{}, errKeyDescriptor
	case !strings.HasPrefix(s, "["):
		return escrowKey{}, errKeyOrigin
	}
	end := strings.IndexByte(s, ']')
	if end < 0 || strings.Count(s, "[") != 1 || strings.Count(s, "]") != 1 {
		return escrowKey{}, errKeyOriginForm
	}
	origin, rest := s[1:end], s[end+1:]
	if len(origin) < 9 || !isLowerHexFold(origin[:8]) {
		return escrowKey{}, errKeyOriginForm
	}
	path, err := parseOriginPath(origin[8:])
	if err != nil || len(path) == 0 {
		return escrowKey{}, errKeyOriginForm
	}
	keyText := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		if rest[i:] != "/0/*" {
			return escrowKey{}, errKeySuffix
		}
		keyText = rest[:i]
	}
	raw, err := base58CheckDecode(keyText)
	if err != nil || len(raw) != 78 {
		return escrowKey{}, errKeyInvalid
	}
	switch version := binary.BigEndian.Uint32(raw[:4]); {
	case version == n.pub || version == n.msig:
	case privateVersions[version]:
		return escrowKey{}, errKeyExposed
	case n.other().isPublic(version) && n.main:
		return escrowKey{}, errKeyTestnet
	case n.other().isPublic(version):
		return escrowKey{}, errKeyMainnet
	case version == n.singleSig[0] || version == n.singleSig[1]:
		return escrowKey{}, errKeySingleSig
	case version == n.nested:
		return escrowKey{}, errKeyNested
	default:
		return escrowKey{}, errKeyVersion
	}
	depth, child := int(raw[4]), binary.BigEndian.Uint32(raw[9:13])
	if depth != len(path) || path[len(path)-1] != child {
		return escrowKey{}, errKeyDepth
	}
	if raw[45] != 0x02 && raw[45] != 0x03 || !onCurve(raw[45:78]) {
		return escrowKey{}, errKeyInvalid
	}
	k := escrowKey{Fingerprint: strings.ToLower(origin[:8]), Path: path, PathText: formatOriginPath(path), BIP48: isBIP48Path(path, n.coin)}
	copy(k.ChainCode[:], raw[13:45])
	copy(k.PubKey[:], raw[45:78])
	binary.BigEndian.PutUint32(raw[:4], n.pub)
	k.XPub = base58CheckEncode(raw)
	return k, nil
}

func isSpaceByte(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0xa0 || r > 0x7f && strings.TrimSpace(string(r)) == ""
}

func isASCIIAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func isLowerHexFold(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i] | 0x20
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// parseOriginPath reads "/48h/1h/0h/2h" (h, H or ' for hardened) into BIP32 steps; at most escrowKeyMaxDepth.
func parseOriginPath(s string) ([]uint32, error) {
	if s == "" {
		return nil, nil
	}
	if s[0] != '/' {
		return nil, errKeyOriginForm
	}
	parts := strings.Split(s[1:], "/")
	if len(parts) > escrowKeyMaxDepth {
		return nil, errKeyOriginForm
	}
	path := make([]uint32, 0, len(parts))
	for _, p := range parts {
		hardened := strings.HasSuffix(p, "h") || strings.HasSuffix(p, "H") || strings.HasSuffix(p, "'")
		if hardened {
			p = p[:len(p)-1]
		}
		if p == "" || len(p) > 10 || strings.TrimLeft(p, "0123456789") != "" || len(p) > 1 && p[0] == '0' {
			return nil, errKeyOriginForm
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil || n >= 1<<31 {
			return nil, errKeyOriginForm
		}
		step := uint32(n)
		if hardened {
			step |= 1 << 31
		}
		path = append(path, step)
	}
	return path, nil
}

func formatOriginPath(path []uint32) string {
	var sb strings.Builder
	for _, step := range path {
		sb.WriteByte('/')
		sb.WriteString(strconv.FormatUint(uint64(step&^(1<<31)), 10))
		if step&(1<<31) != 0 {
			sb.WriteByte('h')
		}
	}
	return sb.String()
}

// isBIP48Path reports m/48h/<coin>h/<account>h/2h: BIP48's P2WSH multisig account with the class's coin type (1 on
// test networks, 0 on mainnet).
func isBIP48Path(path []uint32, coin uint32) bool {
	const h = 1 << 31
	return len(path) == 4 && path[0] == 48|h && path[1] == coin|h && path[2]&h != 0 && path[3] == 2|h
}

// containsPrivateMaterial reports an extended private key (by its prefix or decoded version), a WIF private key or
// a list of recovery words anywhere in s. Base58 runs are checked one by one, so a private descriptor or a pasted
// listdescriptors reply is caught whatever surrounds the key. Recovery words count however they are written down:
// every character outside [A-Za-z0-9] separates words (numbering such as "(1)", "1." or "#1", quotes, bullets, a
// JSON array, hyphens, slashes, underscores, dots), numbers are skipped, and a run of at least 12 words of 3 to 8
// letters (in any case) is recovery words, whatever comes before or after it: a label such as "BIP39 seed:" or a
// trailing "passphrase: …" (A-318). Any other token ends a run. A key expression never qualifies (its extended key is
// one token of 111 characters), nor does a whole public listdescriptors reply (its fingerprints, keys and checksums
// break its short JSON words into runs well under 12).
func containsPrivateMaterial(s string) bool {
	run := 0
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !isASCIIAlnum(r) }) {
		if strings.TrimLeft(w, "0123456789") == "" { // a number ("12" of "12.", "(12)", "#12")
			continue
		}
		if len(w) < 3 || len(w) > 8 || strings.TrimLeft(strings.ToLower(w), "abcdefghijklmnopqrstuvwxyz") != "" {
			run = 0
			continue
		}
		if run++; run >= 12 {
			return true
		}
	}
	for _, run := range strings.FieldsFunc(s, func(r rune) bool { return r > 0x7f || !btc.IsBase58Char(byte(r)) }) {
		for _, p := range privatePrefixes {
			if strings.HasPrefix(run, p) {
				return true
			}
		}
		if len(run) > btc.Base58MaxLen {
			continue
		}
		raw, err := base58CheckDecode(run)
		if err != nil {
			continue
		}
		if len(raw) == 78 && privateVersions[binary.BigEndian.Uint32(raw[:4])] {
			return true
		}
		// WIF: version 0x80 (mainnet) or 0xef (test networks), 32 key bytes, optional 0x01 compression flag. The
		// length is checked first: a checksummed payload can be empty (A-307).
		if (len(raw) == 33 || len(raw) == 34 && raw[33] == 0x01) && (raw[0] == 0x80 || raw[0] == 0xef) {
			return true
		}
	}
	return false
}
