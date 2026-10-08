package market

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// escrowHealthObservation contains only allowlisted node metadata. It intentionally excludes RPC replies and errors.
type escrowHealthObservation struct {
	chain    string
	height   int64
	known    bool
	pinMatch bool
	pinKnown bool
}

func safeEscrowHealthChain(chain string) string {
	switch chain {
	case "regtest", "signet", "testnet4", "main", "testnet3":
		return chain
	default:
		return "unknown"
	}
}

func (a *App) observeEscrowNode(node bitcoinNode, pinMatch *bool) {
	previous := a.escrowHealthObservation.Load()
	obs := escrowHealthObservation{chain: safeEscrowHealthChain(node.Chain), height: node.Blocks, known: true}
	if previous != nil {
		obs.pinMatch, obs.pinKnown = previous.pinMatch, previous.pinKnown
	}
	if pinMatch != nil {
		obs.pinMatch, obs.pinKnown = *pinMatch, true
	}
	a.escrowHealthObservation.Store(&obs)
}

func escrowHealthDisabledClass(state string) string {
	switch state {
	case "off":
		return "off"
	case escrowLogUnchecked:
		return "node_unreachable"
	case escrowLogRefused, errEscrowChain.Error():
		return "unsupported_chain"
	case escrowLogChain:
		return "chain_pin_mismatch"
	case escrowLogPooledKey:
		return "pooled_key_access"
	case escrowLogArbiter:
		return "arbiter_wallet_unreadable"
	case escrowLogFingerprint:
		return "arbiter_fingerprint_mismatch"
	case escrowLogArbiterKey:
		return "arbiter_key_mismatch"
	case escrowLogArbiterUse:
		return "arbiter_wallet_not_dedicated"
	case escrowLogArbiterRead, escrowLogArbiterWait:
		return "arbiter_wallet_history_unreadable"
	case escrowLogWatch:
		return "watch_wallet_unreadable"
	case errEscrowFeeAddress.Error():
		return "fee_address_invalid"
	default:
		return "check_failed"
	}
}

func (a *App) saveEscrowHealthAttempt(ctx context.Context, enabled bool, state string) {
	class := ""
	if !enabled {
		class = escrowHealthDisabledClass(state)
	}
	obs := a.escrowHealthObservation.Load()
	chain, varHeight, pinMatch := "", any(nil), any(nil)
	observationKnown := obs != nil
	if obs != nil && obs.known {
		chain, varHeight = obs.chain, obs.height
	}
	if obs != nil && obs.pinKnown {
		pinMatch = obs.pinMatch
	}
	_, err := a.db.ExecContext(ctx, `UPDATE bitcoin_escrow_health SET enabled=$1,
		disabled_class=CASE WHEN $1 THEN disabled_class ELSE $2 END,
		chain=COALESCE(NULLIF($3,''),chain), height=COALESCE($4,height),
		pin_match=CASE WHEN $6 THEN $5 ELSE pin_match END, updated_at=now()
		WHERE singleton`, enabled, class, chain, varHeight, pinMatch, observationKnown)
	if err != nil {
		log.Printf("escrow health: status not saved (%s)", errorCause(err))
	}
}

// saveEscrowHealthPass records a completed pass and the current allocated market-index high-water.
func (a *App) saveEscrowHealthPass(ctx context.Context, node bitcoinNode) {
	chain := safeEscrowHealthChain(node.Chain)
	index, err := highestEscrowMarketIndex(ctx, a.db)
	if err != nil {
		log.Printf("escrow health: market index not read (%s)", errorCause(err))
		return
	}
	_, err = a.db.ExecContext(ctx, `UPDATE bitcoin_escrow_health SET enabled=true, last_pass=now(), chain=$1,
		height=$2, pin_match=true, highest_market_index=GREATEST(highest_market_index,$3), updated_at=now()
		WHERE singleton`, chain, node.Blocks, index)
	if err != nil {
		log.Printf("escrow health: pass not saved (%s)", errorCause(err))
	}
}

func (a *App) saveEscrowHealthObservation(ctx context.Context, node bitcoinNode, pinMatch *bool) {
	chain := safeEscrowHealthChain(node.Chain)
	var pin any
	if pinMatch != nil {
		pin = *pinMatch
	}
	_, err := a.db.ExecContext(ctx, `UPDATE bitcoin_escrow_health SET chain=$1,height=$2,pin_match=COALESCE($3,pin_match),
		enabled=CASE WHEN $3=false THEN false ELSE enabled END,
		disabled_class=CASE WHEN $3=false THEN 'chain_pin_mismatch' ELSE disabled_class END,updated_at=now() WHERE singleton`,
		chain, node.Blocks, pin)
	if err != nil {
		log.Printf("escrow health: node observation not saved (%s)", errorCause(err))
	}
}

func (a *App) saveEscrowHealthUnavailable(ctx context.Context, class string) {
	if class != "node_unreachable" && class != "chain_pin_mismatch" {
		class = "check_failed"
	}
	_, err := a.db.ExecContext(ctx, `UPDATE bitcoin_escrow_health SET enabled=false,disabled_class=$1,updated_at=now() WHERE singleton`, class)
	if err != nil {
		log.Printf("escrow health: unavailable state not saved (%s)", errorCause(err))
	}
}

func highestEscrowMarketIndex(ctx context.Context, db *sql.DB) (int64, error) {
	var index int64
	err := db.QueryRowContext(ctx, `SELECT GREATEST(COALESCE((SELECT max(market_index) FROM escrows WHERE coin='BTC'),0),
		(SELECT last_value FROM escrow_market_index))`).Scan(&index)
	return index, err
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

func (a *App) noteEscrowKeypoolHighWater(ctx context.Context, index int64) {
	if index < 0 {
		return
	}
	_, err := a.db.ExecContext(ctx, `UPDATE bitcoin_escrow_health SET keypool_high_water=GREATEST(COALESCE(keypool_high_water,0),$1),
		updated_at=now() WHERE singleton`, index)
	if err != nil {
		log.Printf("escrow health: keypool high-water not saved (%s)", errorCause(err))
	}
}

func escrowHealthContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
