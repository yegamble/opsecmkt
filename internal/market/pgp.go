package market

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// OpenPGP helpers shared by the PGP identity (P2) and transparency (P6) packages. The server only
// verifies and encrypts to public keys; it never holds user private keys.

// parsePublicKey accepts exactly one armored, unrevoked public key and returns it with its uppercase hex fingerprint.
func parsePublicKey(armored string) (*openpgp.Entity, string, error) {
	e, fp, _, err := parseCanonicalPublicKey(armored)
	return e, fp, err
}

// parseCanonicalPublicKey is parsePublicKey that also returns the canonical form of the key (A-79): its
// decoded packets re-armored unchanged, without armor headers (Version:, Comment: ...), CRLF line endings
// or any text around the block. The same key always has the same canonical form. It keeps only the packets
// publishablePackets allows (A-110): a key saved with a photo ID or another packet type before such keys were
// refused is shown and re-saved without them. If the kept packets are not exactly one key with the same
// fingerprint (a skipped packet sat between a user ID or subkey and its signature), the key is refused, so it
// is reported as unusable rather than shown to others in a form their PGP software cannot import.
func parseCanonicalPublicKey(armored string) (*openpgp.Entity, string, string, error) {
	e, fp, data, err := decodePublicKey(armored)
	if err != nil {
		return nil, "", "", err
	}
	if data, _, _, err = publishablePackets(data); err != nil {
		return nil, "", "", errors.New("the public key could not be parsed")
	}
	list, err := openpgp.ReadKeyRing(bytes.NewReader(data))
	if err != nil || len(list) != 1 || !bytes.Equal(list[0].PrimaryKey.Fingerprint, e.PrimaryKey.Fingerprint) {
		return nil, "", "", errors.New("the public key could not be parsed")
	}
	if !readableUserIDs(list[0]) { // the key as stored; dropped packets can leave a user ID with only its revocation
		return nil, "", "", errUnreadableUserIDs
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err == nil {
		if _, err = w.Write(data); err == nil {
			err = w.Close()
		}
	}
	if err != nil {
		return nil, "", "", err
	}
	return e, fp, strings.TrimSpace(buf.String()), nil
}

// decodePublicKey accepts exactly one armored, unrevoked public key and returns it with its uppercase hex
// fingerprint and its decoded packets.
func decodePublicKey(armored string) (*openpgp.Entity, string, []byte, error) {
	if strings.Count(armored, "-----BEGIN PGP") != 1 {
		return nil, "", nil, errors.New("paste exactly one armored public key block")
	}
	block, err := armor.Decode(strings.NewReader(strings.TrimSpace(armored)))
	if err != nil {
		return nil, "", nil, errors.New("not an ASCII-armored OpenPGP key")
	}
	if block.Type != openpgp.PublicKeyType {
		return nil, "", nil, errors.New("paste an armored public key block only; never upload a private key")
	}
	data, err := io.ReadAll(block.Body)
	if err != nil {
		return nil, "", nil, errors.New("the public key could not be parsed")
	}
	list, err := openpgp.ReadKeyRing(bytes.NewReader(data))
	if err != nil {
		return nil, "", nil, errors.New("the public key could not be parsed")
	}
	if len(list) != 1 {
		return nil, "", nil, errors.New("paste exactly one public key")
	}
	e := list[0]
	if e.PrivateKey != nil {
		return nil, "", nil, errors.New("private key material rejected; paste only your public key")
	}
	if e.Revoked(time.Now()) {
		return nil, "", nil, errors.New("this key is revoked")
	}
	if !readableUserIDs(e) {
		return nil, "", nil, errUnreadableUserIDs
	}
	return e, strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)), data, nil
}

var errUnreadableUserIDs = errors.New("a revoked user ID in the key has only its revocation, without its self-signature, while another revoked user ID keeps its self-signature, so the key cannot be read safely; export the full key again with gpg --armor --export")

// readableUserIDs reports whether the OpenPGP library can choose e's primary user ID (A-266). go-crypto v1.4.1's
// PrimaryIdentity, which every expiry, encryption and signature check calls, dereferences a nil self-signature when a
// user ID that has only its revocation has as many revocations as a user ID that keeps its self-signature, and panics
// depending on map order. A revoked user ID with only its revocation is otherwise safe: gpg --export-options
// export-minimal writes one that way, so it is accepted.
func readableUserIDs(e *openpgp.Entity) bool {
	signed := map[int]bool{} // revocation counts of user IDs that have a self-signature
	for _, id := range e.Identities {
		if id.SelfSignature != nil {
			signed[len(id.Revocations)] = true
		}
	}
	for _, id := range e.Identities {
		if id.SelfSignature == nil && signed[len(id.Revocations)] {
			return false
		}
	}
	return true
}

// OpenPGP packet tags (RFC 9580 section 5).
const (
	signaturePacketTag     = 2
	publicKeyPacketTag     = 6
	trustPacketTag         = 12
	userIDPacketTag        = 13
	publicSubkeyPacketTag  = 14
	userAttributePacketTag = 17
)

// publishablePackets returns decoded key packets keeping only public key, public subkey, user ID and signature
// packets, each byte for byte (A-110). Everything else is removed: user attribute packets, where GnuPG stores a
// photo ID (an image, usually of the owner's face), and every other packet type - a compressed packet or a
// non-critical private one can carry a photo ID that OpenPGP software still imports - including keyring trust
// packets. Signatures that follow a removed packet certify it and go too, except direct-key signatures and key
// revocations. It also returns how many user attribute packets and other packets it removed.
func publishablePackets(data []byte) ([]byte, int, int, error) {
	r := bytes.NewReader(data)
	packets := packet.NewOpaqueReader(r)
	kept, photos, other, skipping := make([]byte, 0, len(data)), 0, 0, false
	for {
		start := len(data) - r.Len()
		p, err := packets.Next()
		if err == io.EOF {
			return kept, photos, other, nil
		}
		if err != nil {
			return nil, 0, 0, err
		}
		switch p.Tag {
		case publicKeyPacketTag, userIDPacketTag, publicSubkeyPacketTag:
			skipping = false
		case signaturePacketTag:
			if skipping && !wholeKeySignature(p) {
				continue
			}
		case userAttributePacketTag:
			photos, skipping = photos+1, true
			continue
		case trustPacketTag: // follows a signature in a keyring and certifies nothing
			other++
			continue
		default:
			other, skipping = other+1, true
			continue
		}
		kept = append(kept, data[start:len(data)-r.Len()]...)
	}
}

// wholeKeySignature reports whether a signature packet is a direct-key signature or key revocation. OpenPGP
// readers apply these to the whole key wherever they appear (openpgp.ReadEntity), so they are kept.
func wholeKeySignature(p *packet.OpaquePacket) bool {
	parsed, err := p.Parse()
	sig, ok := parsed.(*packet.Signature)
	return err == nil && ok && (sig.SigType == packet.SigTypeDirectSignature || sig.SigType == packet.SigTypeKeyRevocation)
}

// keyExtraPackets returns the fingerprint of a pasted public key, how many photo IDs (user attribute packets)
// it carries and how many other packets publishablePackets would remove; a key that does not parse reports none.
func keyExtraPackets(armored string) (fp string, photos, other int) {
	_, fp, data, err := decodePublicKey(armored)
	if err != nil {
		return "", 0, 0
	}
	_, photos, other, _ = publishablePackets(data)
	return fp, photos, other
}

// canReceiveEncrypted reports whether anything can be encrypted to key now (A-68, A-83): the key is neither
// expired nor revoked and has an encryption-capable subkey (or primary key) that is neither expired nor revoked,
// the key openpgp.Encrypt would use. No key (none saved, or one that does not parse) cannot. It is the one test of
// whether a user "can receive encrypted messages": contact keys (contactKey) and physical orders (the escrow checkouts,
// the order, product and checkout pages) use it.
func canReceiveEncrypted(key *openpgp.Entity, now time.Time) bool {
	if key == nil {
		return false
	}
	_, ok := key.EncryptionKey(now)
	return ok
}

// keyExpiry returns when the self-signature sig makes pk expire; zero means it never expires.
func keyExpiry(pk *packet.PublicKey, sig *packet.Signature) time.Time {
	if sig == nil || sig.KeyLifetimeSecs == nil || *sig.KeyLifetimeSecs == 0 {
		return time.Time{}
	}
	return pk.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second)
}

// primaryKeyExpiry returns when key's primary key expires (zero: never, or no self-signature).
func primaryKeyExpiry(key *openpgp.Entity) time.Time {
	sig, _ := key.PrimarySelfSignature()
	return keyExpiry(key.PrimaryKey, sig)
}

// encryptionKey describes the key encryptTo uses (A-155): Expires is its own expiry, Until when encryption to it
// stops, the earlier of that and the primary key's expiry (zero: never).
type encryptionKey struct {
	Key            *packet.PublicKey
	Expires, Until time.Time
}

// encryptionSubkey returns the key encryptTo selects now (canReceiveEncrypted). When none is usable now, it returns
// the unrevoked encryption-capable key whose encryption stopped last because it or the primary key expired, so an
// expired key still says which subkey expired and when. ok is false when neither exists: no key, a revoked key, one
// that is signing only, or one unusable for another reason (a revoked encryption subkey or user ID).
func encryptionSubkey(key *openpgp.Entity, now time.Time) (encryptionKey, bool) {
	if key == nil {
		return encryptionKey{}, false
	}
	primarySig, _ := key.PrimarySelfSignature()
	if primarySig == nil {
		return encryptionKey{}, false
	}
	primary := keyExpiry(key.PrimaryKey, primarySig)
	describe := func(pk *packet.PublicKey, sig *packet.Signature) encryptionKey {
		k := encryptionKey{Key: pk, Expires: keyExpiry(pk, sig)}
		k.Until = k.Expires
		if !primary.IsZero() && (k.Until.IsZero() || primary.Before(k.Until)) {
			k.Until = primary
		}
		return k
	}
	if k, ok := key.EncryptionKey(now); ok {
		return describe(k.PublicKey, k.SelfSignature), true
	}
	if key.Revoked(now) {
		return encryptionKey{}, false
	}
	var last encryptionKey
	found := false
	consider := func(k encryptionKey) {
		if !k.Until.IsZero() && !k.Until.After(now) && (!found || k.Until.After(last.Until)) {
			last, found = k, true
		}
	}
	for _, s := range key.Subkeys {
		if s.Sig.FlagsValid && s.Sig.FlagEncryptCommunications && s.PublicKey.PubKeyAlgo.CanEncrypt() && !s.Revoked(now) {
			consider(describe(s.PublicKey, s.Sig))
		}
	}
	if !found && primarySig.FlagsValid && primarySig.FlagEncryptCommunications && key.PrimaryKey.PubKeyAlgo.CanEncrypt() {
		consider(describe(key.PrimaryKey, primarySig))
	}
	return last, found
}

// revocationSignatures returns the body of every key, subkey and user ID revocation signature in an armored key.
func revocationSignatures(armored string) (map[string]bool, error) {
	_, _, data, err := decodePublicKey(armored)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	packets := packet.NewOpaqueReader(bytes.NewReader(data))
	for {
		p, err := packets.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if p.Tag != signaturePacketTag {
			continue
		}
		parsed, err := p.Parse()
		if sig, ok := parsed.(*packet.Signature); err == nil && ok && (sig.SigType == packet.SigTypeKeyRevocation ||
			sig.SigType == packet.SigTypeSubkeyRevocation || sig.SigType == packet.SigTypeCertificationRevocation) {
			out[string(p.Contents)] = true
		}
	}
}

// revocationsKept reports whether key, a new version of saved, keeps every revocation OpenPGP software applies in saved
// (A-261): the primary key's, each revoked subkey's on every copy of that subkey (by fingerprint) in key, and each
// revoked user ID's. revocationSignatures' byte comparison alone cannot tell: a subkey or user ID revocation moved
// before the first user ID keeps its bytes but is ignored (go-crypto's ReadEntity), so encryptTo would select a
// revoked subkey again. A revoked subkey or user ID left out of key is not kept either: a later version could add it
// back unrevoked with nothing left to compare it with.
func revocationsKept(saved, key *openpgp.Entity) bool {
	if !signaturesIn(saved.Revocations, key.Revocations) {
		return false
	}
	for _, s := range saved.Subkeys {
		if len(s.Revocations) == 0 {
			continue
		}
		found := false
		for _, k := range key.Subkeys {
			if bytes.Equal(k.PublicKey.Fingerprint, s.PublicKey.Fingerprint) {
				if !signaturesIn(s.Revocations, k.Revocations) {
					return false
				}
				found = true
			}
		}
		if !found {
			return false
		}
	}
	for name, id := range saved.Identities {
		if len(id.Revocations) == 0 {
			continue
		}
		if other, ok := key.Identities[name]; !ok || !signaturesIn(id.Revocations, other.Revocations) {
			return false
		}
	}
	return true
}

// signaturesIn reports whether every signature in want is also in have, compared as serialized packets.
func signaturesIn(want, have []*packet.Signature) bool {
	in := map[string]bool{}
	for _, sig := range have {
		if b, err := signatureBytes(sig); err == nil {
			in[b] = true
		}
	}
	for _, sig := range want {
		if b, err := signatureBytes(sig); err != nil || !in[b] {
			return false
		}
	}
	return true
}

func signatureBytes(sig *packet.Signature) (string, error) {
	var buf bytes.Buffer
	c := *sig // Serialize records the subpackets it writes; the parsed key stays as it was
	err := c.Serialize(&buf)
	return buf.String(), err
}

// entityKeyIDs lists the primary and subkey IDs, for matching encrypted-message recipients.
func entityKeyIDs(e *openpgp.Entity) []uint64 {
	ids := []uint64{e.PrimaryKey.KeyId}
	for _, s := range e.Subkeys {
		ids = append(ids, s.PublicKey.KeyId)
	}
	return ids
}

// verifyClearsigned checks a cleartext-signed message against key and returns the signed text and signature time.
func verifyClearsigned(armored string, key *openpgp.Entity) (string, time.Time, error) {
	b, _ := clearsign.Decode([]byte(strings.TrimSpace(armored)))
	if b == nil {
		return "", time.Time{}, errors.New("not a clearsigned OpenPGP message")
	}
	sig, err := verifySignature(key, bytes.NewReader(b.Bytes), b.ArmoredSignature.Body)
	if err != nil {
		return "", time.Time{}, err
	}
	return string(b.Plaintext), sig.CreationTime, nil
}

// verifyDetached checks an armored detached signature over signed and returns the signature time.
func verifyDetached(signed, armoredSig string, key *openpgp.Entity) (time.Time, error) {
	block, err := armor.Decode(strings.NewReader(strings.TrimSpace(armoredSig)))
	if err != nil || block.Type != openpgp.SignatureType {
		return time.Time{}, errors.New("not an armored OpenPGP signature")
	}
	sig, err := verifySignature(key, strings.NewReader(signed), block.Body)
	if err != nil {
		return time.Time{}, err
	}
	return sig.CreationTime, nil
}

// verifySignature checks a detached signature over signed with key. go-crypto v1.4.1 dereferences a nil primary
// self-signature when a subkey made the signature and no user ID of the key has a self-signature (every one revoked
// and exported with only its revocation, A-266); that panic is answered as a signature that does not verify.
func verifySignature(key *openpgp.Entity, signed, signature io.Reader) (sig *packet.Signature, err error) {
	defer func() {
		if recover() != nil {
			sig, err = nil, errBadSignature
		}
	}()
	if sig, _, err = openpgp.VerifyDetachedSignature(openpgp.EntityList{key}, signed, signature, nil); err != nil {
		return nil, errBadSignature
	}
	return sig, nil
}

var errBadSignature = errors.New("signature does not verify with the configured key")

// encryptTo returns plaintext encrypted to key as an armored PGP MESSAGE.
func encryptTo(key *openpgp.Entity, plaintext string) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return "", err
	}
	pt, err := openpgp.Encrypt(w, []*openpgp.Entity{key}, nil, nil, nil)
	if err != nil {
		return "", err
	}
	if _, err = io.WriteString(pt, plaintext); err != nil {
		return "", err
	}
	if err = pt.Close(); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// decodeMessageArmor accepts exactly one ASCII-armored PGP MESSAGE block with nothing but whitespace
// around it (LF or CRLF line endings) and returns the decoded bytes of its whole body. Armor headers
// (Version:, Comment: ...) are skipped and the CRC line is ignored; every other line must be base64.
// Unlike armor.Decode it refuses text before the block, inside the data and after the checksum.
func decodeMessageArmor(armored string) ([]byte, error) {
	errArmor := errors.New("not exactly one ASCII-armored PGP MESSAGE block")
	lines := strings.Split(strings.TrimSpace(armored), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if len(lines) < 4 || lines[0] != "-----BEGIN PGP MESSAGE-----" || lines[len(lines)-1] != "-----END PGP MESSAGE-----" {
		return nil, errArmor
	}
	lines = lines[1 : len(lines)-1]
	blank := slices.Index(lines, "")
	if blank < 0 {
		return nil, errArmor
	}
	for _, h := range lines[:blank] {
		if !strings.Contains(h, ":") {
			return nil, errArmor
		}
	}
	data := lines[blank+1:]
	if n := len(data); n > 1 && len(data[n-1]) == 5 && data[n-1][0] == '=' {
		data = data[:n-1] // CRC24 checksum; the canonical re-armor writes a fresh one
	}
	for _, l := range data {
		if l == "" || strings.Trim(l, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") != "" {
			return nil, errArmor
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.Join(data, ""))
	if err != nil || len(decoded) == 0 {
		return nil, errArmor
	}
	return decoded, nil
}

// canonicalMessage re-armors the decoded packets of an armored PGP MESSAGE without armor headers, so
// that nothing but the OpenPGP packets themselves is stored or shown.
func canonicalMessage(armored string) (string, error) {
	data, err := decodeMessageArmor(armored)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return "", err
	}
	if _, err = w.Write(data); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// inspectEncrypted reports whether an armored message is OpenPGP-encrypted, and the recipient key IDs
// of its public-key session packets (0 = hidden recipient). It never decrypts. Encrypted means at
// least one session key packet (PKESK/SKESK) followed by exactly one integrity-protected encrypted-data
// packet (SEIPD or AEAD) that runs to the end of the data; a message whose first other packet is not
// encrypted data (signed-only, literal, compressed) is reported as not encrypted.
func inspectEncrypted(armored string) (recipientKeyIDs []uint64, isEncrypted bool, err error) {
	data, err := decodeMessageArmor(armored)
	if err != nil {
		return nil, false, err
	}
	malformed := errors.New("malformed OpenPGP message")
	r := bytes.NewReader(data)
	keyPackets := 0
	for {
		p, err := packet.Read(r)
		if err == io.EOF {
			return recipientKeyIDs, false, nil
		}
		if _, unsupported := err.(pgperrors.UnsupportedError); err != nil && !unsupported {
			return nil, false, malformed
		}
		var contents io.Reader
		switch p := p.(type) {
		case *packet.EncryptedKey: // an unsupported key algorithm still names its recipient key ID
			recipientKeyIDs = append(recipientKeyIDs, p.KeyId)
			keyPackets++
			continue
		case *packet.SymmetricKeyEncrypted:
			keyPackets++
			continue
		case *packet.SymmetricallyEncrypted:
			if err != nil || !p.IntegrityProtected {
				return nil, false, malformed
			}
			contents = p.Contents
		case *packet.AEADEncrypted:
			if err != nil {
				return nil, false, malformed
			}
			contents = p.Contents
		default:
			if err != nil {
				return nil, false, malformed
			}
			return recipientKeyIDs, false, nil
		}
		// The encrypted data must end exactly where the message ends: no trailing packets or bytes.
		if _, err = io.Copy(io.Discard, contents); err != nil || r.Len() != 0 || keyPackets == 0 {
			return nil, false, malformed
		}
		return recipientKeyIDs, true, nil
	}
}
