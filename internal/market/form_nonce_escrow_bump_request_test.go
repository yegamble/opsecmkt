package market

import (
	"net/url"
	"strconv"

	"testing"
	"time"
)

func bumpNonceForm(order string, sid int64, rate, nonce string) url.Values {
	return url.Values{
		"order_id": {order}, "settlement_id": {strconv.FormatInt(sid, 10)},
		"fee_rate": {rate}, "password": {testPassword}, formNonceField: {nonce},
	}
}

type bumpNonceEffects struct {
	receipts, bumps, audits, events, notices int
}

func bumpEffects(t *testing.T, x *escrowEnv, order string, sid int64) bumpNonceEffects {
	t.Helper()
	return bumpNonceEffects{
		agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/escrow/bump'"),
		agInt(x.testEnv, "SELECT count(*) FROM settlement_bumps WHERE settlement_id=$1", sid),
		agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1", x.vendor),
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order),
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1", x.buyer),
	}
}

func bumpCreatedUpdated(t *testing.T, x *escrowEnv, id int64) (time.Time, time.Time) {
	t.Helper()
	var created, updated time.Time
	if err := x.DB.QueryRow("SELECT created,updated FROM settlement_bumps WHERE id=$1", id).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	return created, updated
}
