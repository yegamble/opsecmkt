package market

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// An outage is continuous skipped or failing watcher work for one configured payment provider. The receipts
// and notifications commit together so a restarted or competing watcher cannot deliver the same event twice.
// The clock is read after the episode row lock, and tests can age bad_since without waiting ten minutes.
const providerOutageDelay = 10 * time.Minute

var providerEpisodeNames = map[string]string{
	"escrow_BTC": "Bitcoin escrow payment provider",
	"escrow_XMR": "Monero escrow payment provider",
}

func (a *App) observeProviderEpisode(ctx context.Context, provider string, bad bool) (err error) {
	name, ok := providerEpisodeNames[provider]
	if !ok {
		return fmt.Errorf("unknown provider episode key")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO provider_episodes(provider) VALUES($1) ON CONFLICT DO NOTHING`, provider); err != nil {
		return err
	}
	var episode int64
	var badSince sql.NullTime
	var alerted bool
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT episode,bad_since,alerted,clock_timestamp()
		FROM provider_episodes WHERE provider=$1 FOR UPDATE`, provider).Scan(&episode, &badSince, &alerted, &now); err != nil {
		return err
	}
	switch {
	case bad && !badSince.Valid:
		episode++
		if _, err = tx.ExecContext(ctx, `UPDATE provider_episodes SET episode=$2,bad_since=$3,alerted=false WHERE provider=$1`, provider, episode, now); err != nil {
			return err
		}
		// Old delivery receipts do not need to outlive their completed episode. Storage is bounded by
		// the number of configured providers and administrators, not the deployment's lifetime.
		if _, err = tx.ExecContext(ctx, `DELETE FROM provider_episode_deliveries WHERE provider=$1 AND episode<$2`, provider, episode); err != nil {
			return err
		}
	case bad && now.Sub(badSince.Time) >= providerOutageDelay:
		if !alerted {
			if _, err = tx.ExecContext(ctx, `UPDATE provider_episodes SET alerted=true WHERE provider=$1`, provider); err != nil {
				return err
			}
		}
		if err = deliverProviderEpisode(ctx, tx, provider, episode, "outage", name+" has been unavailable for about 10 minutes. Existing escrow deposits and settlements may be delayed. Admin → Payments (/admin/payments#xmr-escrow-health) shows the failing check and the last good pass."); err != nil {
			return err
		}
	case !bad && badSince.Valid:
		if alerted {
			if err = deliverProviderEpisode(ctx, tx, provider, episode, "recovery", name+" is available again. Check escrow health and any delayed deposits or settlements."); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE provider_episodes SET bad_since=NULL,alerted=false WHERE provider=$1`, provider); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func deliverProviderEpisode(ctx context.Context, tx *sql.Tx, provider string, episode int64, event, body string) error {
	// FOR SHARE keeps a suspension or role change from racing the recipient snapshot. The episode row
	// serializes competing watchers; the unique receipt is a second line of defence for each account.
	admins, err := collectStrings(ctx, tx, `SELECT id FROM users WHERE role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL ORDER BY id FOR SHARE`)
	if err != nil {
		return err
	}
	for _, uid := range admins {
		res, err := tx.ExecContext(ctx, `INSERT INTO provider_episode_deliveries(provider,episode,user_id,event)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, provider, episode, uid, event)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err = insertScopedNotification(ctx, tx, uid, body, "", "admin"); err != nil {
			return err
		}
	}
	return nil
}

// Reporting failures must never turn a healthy watcher pass into a wallet failure or leak driver text to logs.
// The next pass retries the transaction, including every undelivered recipient.
func (a *App) noteProviderEpisode(ctx context.Context, provider string, bad bool) {
	if err := a.observeProviderEpisode(ctx, provider, bad); err != nil && ctx.Err() == nil {
		log.Printf("provider episode %s: state not saved (%s)", provider, errorCause(err))
	}
}
