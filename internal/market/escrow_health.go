package market

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func safeEscrowHealthChain(chain string) string {
	switch chain {
	case "regtest", "signet", "testnet4", "main", "testnet3":
		return chain
	default:
		return "unknown"
	}
}

func escrowHealthView(ctx context.Context, db *sql.DB) (*EscrowHealthView, error) {
	v := &EscrowHealthView{}
	var lastPass sql.NullTime
	var height, keypool sql.NullInt64
	var pinMatch sql.NullBool
	var updatedAt time.Time
	err := db.QueryRowContext(ctx, `SELECT enabled,disabled_class,last_pass,chain,height,pin_match,keypool_high_water,highest_market_index,updated_at
		FROM bitcoin_escrow_health WHERE singleton`).Scan(&v.Enabled, &v.DisabledClass, &lastPass, &v.Chain, &height, &pinMatch, &keypool, &v.HighestMarketIndex, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.Chain = safeEscrowHealthChain(v.Chain)
	if lastPass.Valid {
		v.LastPass = lastPass.Time.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	v.StatusUpdated = updatedAt.UTC().Format("2006-01-02 15:04:05 UTC")
	if height.Valid {
		v.Height = fmt.Sprint(height.Int64)
	}
	if pinMatch.Valid {
		if pinMatch.Bool {
			v.PinMatch = "matches"
		} else {
			v.PinMatch = "does not match"
		}
	}
	if keypool.Valid {
		v.KeypoolHighWater = fmt.Sprint(keypool.Int64)
	}
	return v, nil
}
