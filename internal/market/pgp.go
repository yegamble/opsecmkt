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
	if list, err := openpgp.ReadKeyRing(bytes.NewReader(data)); err != nil || len(list) != 1 || !bytes.Equal(list[0].PrimaryKey.Fingerprint, e.PrimaryKey.Fingerprint) {
		return nil, "", "", errors.New("the public key could not be parsed")
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
	return e, strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)), data, nil
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
// whether a user "can receive encrypted messages": contact keys (contactKey) and physical orders (payAction, the
// order, product and checkout pages) use it.
func canReceiveEncrypted(key *openpgp.Entity, now time.Time) bool {
	if key == nil {
		return false
	}
	_, ok := key.EncryptionKey(now)
	return ok
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
	sig, _, err := openpgp.VerifyDetachedSignature(openpgp.EntityList{key}, bytes.NewReader(b.Bytes), b.ArmoredSignature.Body, nil)
	if err != nil {
		return "", time.Time{}, errors.New("signature does not verify with the configured key")
	}
	return string(b.Plaintext), sig.CreationTime, nil
}

// verifyDetached checks an armored detached signature over signed and returns the signature time.
func verifyDetached(signed, armoredSig string, key *openpgp.Entity) (time.Time, error) {
	block, err := armor.Decode(strings.NewReader(strings.TrimSpace(armoredSig)))
	if err != nil || block.Type != openpgp.SignatureType {
		return time.Time{}, errors.New("not an armored OpenPGP signature")
	}
	sig, _, err := openpgp.VerifyDetachedSignature(openpgp.EntityList{key}, strings.NewReader(signed), block.Body, nil)
	if err != nil {
		return time.Time{}, errors.New("signature does not verify with the configured key")
	}
	return sig.CreationTime, nil
}

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
