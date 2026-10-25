package market

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"opsecmkt/internal/moderation"
)

const moderationActionDomain = "opsecmkt-moderation-action-v1\n"
const moderationActionTokenLimit = 4096

// ModerationActionToken authorizes a review annotation only. It cannot authorize
// a listing change, account restriction, order transition or wallet operation.
type ModerationActionToken struct {
	Version      int    `json:"version"`
	Audience     string `json:"audience"`
	SuggestionID string `json:"suggestion_id"`
	ContentHash  string `json:"content_hash"`
	Action       string `json:"action"`
	ExpiresAt    string `json:"expires_at"`
	Nonce        string `json:"nonce"`
}

var errModerationToken = errors.New("invalid moderation action token")

// moderationTokenObject rejects ambiguous JSON before decoding typed values.
func moderationTokenObject(data []byte, names ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errModerationToken
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	m := make(map[string]json.RawMessage, len(names))
	for d.More() {
		t, err = d.Token()
		name, ok := t.(string)
		if err != nil || !ok || !allowed[name] || m[name] != nil {
			return nil, errModerationToken
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errModerationToken
		}
		m[name] = value
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') || d.Decode(new(any)) != io.EOF || len(m) != len(names) {
		return nil, errModerationToken
	}
	return m, nil
}

func moderationHex64(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

// VerifyModerationActionToken authenticates the exact token bytes, with a domain
// separator and installation audience. The offline private key is never loaded.
func VerifyModerationActionToken(data []byte, public ed25519.PublicKey, audience string, now time.Time) (ModerationActionToken, error) {
	var token ModerationActionToken
	if len(data) == 0 || len(data) > moderationActionTokenLimit || len(public) != ed25519.PublicKeySize || !moderationHex64(audience) {
		return token, errModerationToken
	}
	env, err := moderationTokenObject(data, "token", "signature")
	if err != nil {
		return token, errModerationToken
	}
	if _, err = moderationTokenObject(env["token"], "version", "audience", "suggestion_id", "content_hash", "action", "expires_at", "nonce"); err != nil {
		return token, errModerationToken
	}
	var signature string
	if json.Unmarshal(env["signature"], &signature) != nil || json.Unmarshal(env["token"], &token) != nil {
		return ModerationActionToken{}, errModerationToken
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(public, append([]byte(moderationActionDomain), env["token"]...), sig) {
		return ModerationActionToken{}, errModerationToken
	}
	expires, err := time.Parse(time.RFC3339, token.ExpiresAt)
	if token.Version != 1 || token.Audience != audience || !moderationHex64(token.SuggestionID) || !moderationHex64(token.ContentHash) || !moderationHex64(token.Nonce) || err != nil || !strings.HasSuffix(token.ExpiresAt, "Z") || !expires.After(now) || expires.After(now.Add(7*24*time.Hour)) {
		return ModerationActionToken{}, errModerationToken
	}
	switch token.Action {
	case "accept", "dismiss", "hold":
		return token, nil
	}
	return ModerationActionToken{}, errModerationToken
}

func moderationActionPublicKey() (ed25519.PublicKey, string, error) {
	audience := os.Getenv("MODERATION_ACTION_AUDIENCE")
	if !moderationHex64(audience) {
		return nil, "", errModerationToken
	}
	f, err := os.Open(os.Getenv("MODERATION_ACTION_PUBLIC_KEY"))
	if err != nil {
		return nil, "", errModerationToken
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", errModerationToken
	}
	b, err := io.ReadAll(io.LimitReader(f, 66))
	key := strings.TrimSuffix(string(b), "\n")
	if err != nil || !moderationHex64(key) {
		return nil, "", errModerationToken
	}
	public, _ := hex.DecodeString(key)
	return ed25519.PublicKey(public), audience, nil
}

func init() {
	registerPage("admin/moderation/offline", pageSpec{})
	registerLoader("admin/moderation/offline", func(ctx context.Context, a *App, r *http.Request, d *PageData) error {
		if !privateAdminIngress(r.Context()) {
			return sql.ErrNoRows
		}
		if receipt := r.URL.Query().Get("receipt"); moderationHex64(receipt) && a.db != nil {
			var exists bool
			if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM moderation_action_receipts WHERE nonce=$1)", receipt).Scan(&exists); err != nil {
				return err
			}
			if exists {
				d.Notice = "Signed review recorded. Marketplace content and funds were unchanged."
			}
		}
		return nil
	})
	registerAction("/admin/moderation/offline", actionSpec{Public: true, Run: moderationOfflineAction})
}

func moderationOfflineAction(c *actionCtx) (actionResult, error) {
	if !privateAdminIngress(c.Ctx()) {
		return actionResult{}, fail(http.StatusNotFound, "Unknown action")
	}
	for name, values := range c.Form {
		if (name != "csrf" && name != "signed_token") || len(values) != 1 {
			return actionResult{}, fail(400, "Provide one signed review token.")
		}
	}
	public, audience, err := moderationActionPublicKey()
	if err != nil {
		return actionResult{}, fail(503, "Offline review is unavailable. Ask the operator to check its public verification key and audience.")
	}
	raw := []byte(c.Form.Get("signed_token"))
	token, err := VerifyModerationActionToken(raw, public, audience, time.Now().UTC())
	if err != nil {
		return actionResult{}, fail(400, "The signed review token is invalid or expired.")
	}
	var contentHash, disposition string
	var expires time.Time
	err = c.Tx.QueryRowContext(c.Ctx(), `SELECT content_hash,disposition,expires_at FROM moderation_suggestions WHERE id=$1 FOR UPDATE`, token.SuggestionID).Scan(&contentHash, &disposition, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return actionResult{}, fail(404, "Review item unavailable.")
	}
	if err != nil {
		return actionResult{}, err
	}
	until, _ := time.Parse(time.RFC3339, token.ExpiresAt)
	if contentHash != token.ContentHash || !expires.After(time.Now()) || until.After(expires) || (token.Action == "accept" && disposition == string(moderation.Hold)) {
		return actionResult{}, fail(409, "The review item has changed or expired. Nothing was applied.")
	}
	current, err := ModerationSuggestionCurrent(c.Ctx(), c.Tx, token.SuggestionID)
	if err != nil {
		return actionResult{}, err
	}
	if !current {
		return actionResult{}, fail(409, "The content has changed. Review the latest suggestion.")
	}
	keyHash := sha256.Sum256(public)
	tokenHash := sha256.Sum256(append([]byte(moderationActionDomain), raw...))
	res, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO moderation_action_receipts(nonce,token_hash,suggestion_id,signer_fingerprint,action) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, token.Nonce, hex.EncodeToString(tokenHash[:]), token.SuggestionID, hex.EncodeToString(keyHash[:]), token.Action)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, fail(409, "This signed review token was already used. Nothing was applied.")
	}
	res, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO moderation_reviews(suggestion_id,action) VALUES($1,$2) ON CONFLICT DO NOTHING`, token.SuggestionID, token.Action)
	if err != nil {
		return actionResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return actionResult{}, fail(409, "This item already has a review. Nothing was applied.")
	}
	return actionResult{Redirect: "/admin/moderation/offline?receipt=" + token.Nonce, Audit: "Offline-signed moderation review " + token.Action + "; suggestion " + token.SuggestionID + "; signer " + hex.EncodeToString(keyHash[:])}, nil
}
