package market

import (
	"net/url"
)

func escrowFeeNonceForm(bps, address, nonce string) url.Values {
	return url.Values{
		"fee_bps": {bps}, "fee_address": {address}, "password": {testPassword}, formNonceField: {nonce},
	}
}

type escrowFeeNonceEffects struct{ receipts, audits, settings int }

func escrowFeeEffects(x *escrowEnv, adminID string) escrowFeeNonceEffects {
	return escrowFeeNonceEffects{
		agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/admin/escrow-fee'"),
		agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed the Bitcoin escrow market fee%'", adminID),
		agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key IN ($1,$2)", escrowFeeBpsSetting, escrowFeeAddrSetting),
	}
}
