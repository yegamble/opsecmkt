package market

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func init() {
	registerAction("/admin/operator-key", actionSpec{Roles: []string{"admin"}, Run: operatorKeyAction})
	registerAction("/admin/canary", actionSpec{Roles: []string{"admin"}, Run: canaryAction})
	registerLoader("canary", func(ctx context.Context, a *App, r *http.Request, d *PageData) error {
		v, err := loadCanary(ctx, a.db, d.Settings)
		if err != nil {
			return err
		}
		d.Canary = v
		ev := auditExportStatus()
		d.AuditExport = &AuditExportView{Available: ev.Available, PublicKey: ev.PublicKey, Error: ev.Error}
		return nil
	})
	registerLoader("admin", func(ctx context.Context, a *App, r *http.Request, d *PageData) error {
		v, err := loadCanary(ctx, a.db, d.Settings)
		if err != nil {
			return err
		}
		d.Canary = v
		return nil
	})
	registerPreview("canary", func(d *PageData) {
		d.Canary = previewCanary()
		d.AuditExport = &AuditExportView{Error: "Read-only preview: signed audit export needs PostgreSQL and AUDIT_SIGNING_KEY."}
	})
	registerPreview("admin", func(d *PageData) {
		d.Canary = previewCanary()
		d.AuditExport = &AuditExportView{Error: "Read-only preview: signed audit export needs PostgreSQL and AUDIT_SIGNING_KEY."}
	})
}

func setSetting(c *actionCtx, key, value string) error {
	_, err := c.Tx.ExecContext(c.Ctx(), "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

// operatorKeyAction stores the operator's armored PGP public key used to verify the canary.
func operatorKeyAction(c *actionCtx) (actionResult, error) {
	armored := strings.TrimSpace(c.Form.Get("operator_key"))
	if armored == "" || len(armored) > 65536 {
		return actionResult{}, fail(400, "Paste the operator's armored PGP public key")
	}
	_, fp, err := parsePublicKey(armored)
	if err != nil {
		return actionResult{}, fail(400, "Operator key rejected: "+err.Error())
	}
	if err = setSetting(c, settingOperatorKey, armored); err != nil {
		return actionResult{}, err
	}
	if err = setSetting(c, settingOperatorFingerprint, fp); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/admin?saved=1#transparency", Audit: "Set operator PGP key " + fp + " for canary verification"}, nil
}

// canaryAction stores an operator-clearsigned statement verbatim, only if it verifies against the operator key.
func canaryAction(c *actionCtx) (actionResult, error) {
	text := strings.TrimSpace(c.Form.Get("canary"))
	if text == "" {
		return actionResult{}, fail(400, "Paste the clearsigned canary statement")
	}
	var key string
	err := c.Tx.QueryRowContext(c.Ctx(), "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingOperatorKey).Scan(&key)
	if err != nil || strings.TrimSpace(key) == "" {
		return actionResult{}, fail(409, "Configure the operator PGP key before publishing a canary")
	}
	_, signedAt, fp, err := verifyCanary(key, text)
	if err != nil {
		return actionResult{}, fail(400, "Canary rejected: "+err.Error())
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), "INSERT INTO canary(id,clearsigned,posted) VALUES(1,$1,now()) ON CONFLICT(id) DO UPDATE SET clearsigned=excluded.clearsigned, posted=excluded.posted", text); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/canary", Audit: "Published warrant canary signed " + formatUTC(signedAt) + " by operator key " + fp}, nil
}

// previewCanary signs a clearly labelled sample with a throwaway key generated when the preview starts, then runs
// the real verification, so the preview shows the verified layout without inventing a signature.
var previewCanary = sync.OnceValue(func() *CanaryView {
	unavailable := &CanaryView{}
	e, err := openpgp.NewEntity("Preview operator", "throwaway preview key", "preview@example.invalid", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		return unavailable
	}
	var pub, signed bytes.Buffer
	w, err := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil || e.Serialize(w) != nil || w.Close() != nil {
		return unavailable
	}
	sw, err := clearsign.Encode(&signed, e.PrivateKey, nil)
	if err != nil {
		return unavailable
	}
	io.WriteString(sw, "PREVIEW SAMPLE - not a real warrant canary.\n\nThis text was signed by a throwaway key generated when this read-only preview server started.\nA real canary is written and signed offline by the operator.\n")
	if sw.Close() != nil {
		return unavailable
	}
	return canaryView(pub.String(), signed.String(), time.Now())
})
