package market

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Caller verifies ownership/step-up before invoking this helper. The returned
// operation ID must be finalized after the containing transaction commits, or
// aborted after it rolls back. An unresolved commit stops startup on ambiguity.
func (a *App) requestAccountDeletionTx(ctx context.Context, tx *sql.Tx, userID string) (string, error) {
	s := a.retention
	if s == nil {
		return "", fail(409, "Account deletion requires configured retention maintenance.")
	}
	unlock, err := s.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err = s.read(); err != nil {
		return "", err
	}
	if err = a.checkRetentionSequence(ctx, tx); err != nil {
		return "", err
	}
	if len(s.state.Pending) > 0 {
		return "", fail(409, "A retention operation awaits reconciliation. Contact the operator.")
	}
	var requested, closed sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT deletion_requested_at,closed_at FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&requested, &closed); err != nil {
		return "", err
	}
	if closed.Valid {
		return "", fail(409, "This account is already closed.")
	}
	if requested.Valid {
		return "", nil
	}
	now := time.Now().UTC()
	op := retentionOperation{ID: randomToken(), At: now, Request: &retentionRequest{ID: userID, At: now}}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET deletion_requested_at=$2 WHERE id=$1`, userID, now); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)`, randomToken(), userID, "Your verified account-deletion request was accepted. Profile and sign-in material will be erased within 30 days; scoped preservation and unresolved financial records may remain."); err != nil {
		return "", err
	}
	err = a.prepareRetentionOperation(ctx, tx, op)
	return op.ID, err
}

func (a *App) requestAccountDeletion(ctx context.Context, userID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	id, err := a.requestAccountDeletionTx(ctx, tx, userID)
	commitAttempted := false
	if err == nil {
		commitAttempted = true
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		tx.Rollback()
		if commitAttempted {
			return err
		}
		if abortErr := a.abortRetentionOperation(ctx, id); abortErr != nil {
			return errors.Join(err, abortErr)
		}
		return err
	}
	return a.finalizeRetentionOperation(ctx, id)
}

func (a *App) touchRetentionActivity(ctx context.Context, userID string) error {
	if a.retention == nil {
		return nil
	}
	// Rate-limit successful-session activity writes; admission is already tied
	// to a live authenticated session and closure is checked in the same UPDATE.
	result, err := a.db.ExecContext(ctx, `UPDATE users SET last_authenticated_at=now(),inactivity_notified_at=NULL,retention_restricted_at=CASE WHEN deletion_requested_at IS NULL THEN NULL ELSE retention_restricted_at END
	 WHERE id=$1 AND closed_at IS NULL AND (last_authenticated_at IS NULL OR last_authenticated_at<now()-interval '1 hour' OR inactivity_notified_at IS NOT NULL OR retention_restricted_at IS NOT NULL AND deletion_requested_at IS NULL)`, userID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed > 0 {
		if _, err = a.db.ExecContext(ctx, `UPDATE retention_account_exceptions e SET resolved_at=COALESCE(e.resolved_at,now()),last_checked=now() FROM users u WHERE u.id=$1 AND e.user_id=u.id AND u.closed_at IS NULL AND u.deletion_requested_at IS NULL AND u.retention_restricted_at IS NULL`, userID); err != nil {
			return err
		}
	}
	var active bool
	if err = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND closed_at IS NULL)`, userID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return fail(401, "This account is closed.")
	}
	return nil
}

func (a *App) setRetentionHoldTx(ctx context.Context, tx *sql.Tx, h retentionHold) (string, error) {
	s := a.retention
	if s == nil {
		return "", fail(409, "Configure retention before registering preservation.")
	}
	now := time.Now().UTC()
	if h.ID == "" {
		h.ID = randomToken()
	}
	if h.BeganAt.IsZero() {
		h.BeganAt = now
	}
	h.ReleasedAt = nil
	if err := validateRetentionHold(h); err != nil {
		return "", err
	}
	unlock, err := s.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err = s.read(); err != nil {
		return "", err
	}
	if err = a.checkRetentionSequence(ctx, tx); err != nil {
		return "", err
	}
	if len(s.state.Pending) > 0 {
		return "", fail(409, "A retention operation awaits reconciliation. Contact the operator.")
	}
	for _, old := range s.state.Holds {
		if old.ID == h.ID {
			if old.Scope != h.Scope || old.RecordID != h.RecordID || old.ReleasedAt != nil {
				return "", fail(409, "A hold scope cannot change or be reused after release.")
			}
			h.BeganAt = old.BeganAt
		}
	}
	newHold := true
	for _, old := range s.state.Holds {
		if old.ID == h.ID {
			newHold = false
			break
		}
	}
	if newHold {
		var exists bool
		table := ""
		switch h.Scope {
		case "account":
			table = "users"
		case "message":
			table = "messages"
		case "order":
			table = "orders"
		case "audit":
			table = "audit_events"
		}
		var target any = h.RecordID
		if h.Scope == "audit" {
			id, parseErr := strconv.ParseInt(h.RecordID, 10, 64)
			if parseErr != nil || id < 1 {
				return "", fail(400, "Use the numeric audit event ID for this preservation scope.")
			}
			target = id
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, target).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return "", fail(404, "Preservation target not found. Use the record's opaque ID rather than its account handle.")
		}
	}
	if err = upsertRetentionHold(ctx, tx, h); err != nil {
		return "", err
	}
	op := retentionOperation{ID: randomToken(), At: now, Hold: &h}
	err = a.prepareRetentionOperation(ctx, tx, op)
	return op.ID, err
}

func upsertRetentionHold(ctx context.Context, tx *sql.Tx, h retentionHold) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO retention_holds(id,scope,record_id,authority,reason,owner,readers,release_condition,began_at,review_at,released_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(id) DO UPDATE SET authority=EXCLUDED.authority,reason=EXCLUDED.reason,owner=EXCLUDED.owner,readers=EXCLUDED.readers,release_condition=EXCLUDED.release_condition,review_at=EXCLUDED.review_at,released_at=EXCLUDED.released_at`, h.ID, h.Scope, h.RecordID, h.Authority, h.Reason, h.Owner, h.Readers, h.ReleaseCondition, h.BeganAt, h.ReviewAt, h.ReleasedAt)
	return retentionHoldMutationError(err)
}

func retentionHoldMutationError(err error) error {
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "55P03" {
		return fail(409, "Wallet destruction is in progress; retry preservation registration or release.")
	}
	return err
}

func (a *App) releaseRetentionHoldTx(ctx context.Context, tx *sql.Tx, id, owner string) (string, error) {
	s := a.retention
	if s == nil {
		return "", fail(409, "Retention is not configured.")
	}
	unlock, err := s.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err = s.read(); err != nil {
		return "", err
	}
	if err = a.checkRetentionSequence(ctx, tx); err != nil {
		return "", err
	}
	if len(s.state.Pending) > 0 {
		return "", fail(409, "A retention operation awaits reconciliation. Contact the operator.")
	}
	var found *retentionHold
	for _, h := range s.state.Holds {
		if h.ID == id {
			copy := h
			found = &copy
			break
		}
	}
	if found == nil {
		return "", fail(404, "Preservation hold not found.")
	}
	if found.ReleasedAt != nil {
		return "", nil
	}
	if owner == "" {
		return "", fail(400, "Record the responsible preservation owner.")
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE retention_holds SET released_at=$2 WHERE id=$1`, id, now); err != nil {
		return "", retentionHoldMutationError(err)
	}
	op := retentionOperation{ID: randomToken(), At: now, ReleaseHold: id}
	err = a.prepareRetentionOperation(ctx, tx, op)
	return op.ID, err
}
