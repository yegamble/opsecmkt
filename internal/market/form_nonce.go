package market

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log"
	"regexp"
	"sort"
	"time"
)

// A-130 covers normal-transaction forms whose successful second submission must replay its original response
// rather than create another record or refuse an already-completed action.
var formNoncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

const formNonceField = "form_nonce"

func init() { registerBackground(formSubmissionSweeper) }

// formNonceFor is called once per opted-in form while rendering HTML. Issuing a nonce writes nothing on GET.
func formNonceFor(path string) string {
	if spec, ok := actions[path]; ok && (spec.FormNonce || spec.ConfirmedFormNonce || spec.OwnFormNonce) {
		return randomToken()[:32]
	}
	return ""
}

// formRequestHMAC binds a nonce to the exact posted fields, action and query without retaining their text.
// In particular, a plain digest of a message or a future password field could aid offline guessing.
func (a *App) formRequestHMAC(c *actionCtx) string {
	h := hmac.New(sha256.New, a.key)
	if psbtUploadRoutes[c.R.URL.Path] {
		// Hardware PSBTs can be 8 MiB. Hash parsed multipart fields directly with length framing so
		// canonicalizing them through url.Values.Encode would allocate another expanded copy of the PSBT.
		for _, part := range []string{c.R.Method, c.R.URL.Path, c.R.URL.RawQuery} {
			writeFormHMACPart(h, []byte(part))
		}
		keys := make([]string, 0, len(c.Form))
		for key := range c.Form {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		writeFormHMACCount(h, uint64(len(keys)))
		for _, key := range keys {
			writeFormHMACPart(h, []byte(key))
			values := c.Form[key]
			writeFormHMACCount(h, uint64(len(values)))
			for _, value := range values {
				writeFormHMACPart(h, []byte(value))
			}
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	for _, part := range []string{c.R.Method, c.R.URL.Path, c.R.URL.RawQuery, c.Form.Encode()} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// completedFormSubmissionForRequest is a read-only pre-confirmation exception
// for a payment whose flag has since cleared. It does not return a result or
// authorize a change: confirmedTx still checks the current actor, session,
// party eligibility, password and exact receipt in its own transaction.
func completedFormSubmissionForRequest(c *actionCtx) (bool, error) {
	nonce := c.Form.Get(formNonceField)
	if !formNoncePattern.MatchString(nonce) || len(c.Form[formNonceField]) != 1 {
		return false, nil
	}
	var completed bool
	err := c.A.db.QueryRowContext(c.Ctx(), `SELECT EXISTS(SELECT 1 FROM form_submissions
		WHERE nonce=$1 AND user_id=$2 AND session_hash=$3 AND action_path=$4
		AND request_hmac=$5 AND redirect<>'')`, nonce, c.User.ID, digest(c.Token), c.R.URL.Path,
		c.A.formRequestHMAC(c)).Scan(&completed)
	return completed, err
}

func writeFormHMACPart(h interface{ Write([]byte) (int, error) }, part []byte) {
	writeFormHMACCount(h, uint64(len(part)))
	_, _ = h.Write(part)
}

func writeFormHMACCount(h interface{ Write([]byte) (int, error) }, count uint64) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], count)
	_, _ = h.Write(size[:])
}

var errFormNonceMismatch = fail(409, "This form was already used for a different request. Reload the page and try again. Nothing was changed.")

// completedOwnFormMode is a read-only probe used only by the four dynamic
// OwnTx actions before their old-state/RPC prechecks. It never returns a saved
// result or authorizes a mutation: confirmedTx must still lock the actor,
// verify the live session and current password when the ORIGINAL request
// required one, and match the exact receipt in its transaction. A nonce used
// for another body or session is refused without revealing its mode.
func completedOwnFormMode(c *actionCtx) (required, completed bool, err error) {
	nonce := c.Form.Get(formNonceField)
	if !formNoncePattern.MatchString(nonce) || len(c.Form[formNonceField]) != 1 {
		return false, false, fail(400, "Reload this form and try again. Nothing was changed.")
	}
	var userID, sessionHash, path, requestHMAC, redirect string
	err = c.A.db.QueryRowContext(c.Ctx(), `SELECT user_id,session_hash,action_path,request_hmac,redirect,confirmation_required
		FROM form_submissions WHERE nonce=$1`, nonce).Scan(&userID, &sessionHash, &path, &requestHMAC, &redirect, &required)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if userID != c.User.ID || sessionHash != digest(c.Token) || path != c.R.URL.Path ||
		subtle.ConstantTimeCompare([]byte(requestHMAC), []byte(c.A.formRequestHMAC(c))) != 1 {
		return false, false, errFormNonceMismatch
	}
	return required, redirect != "", nil
}

// replayOwnForm deliberately cannot mutate anything. The completed probe only
// selects the original confirmation mode; confirmedTx still checks the current
// actor and exact durable receipt under its transaction locks.
func replayOwnForm(c *actionCtx, required bool) (actionResult, error) {
	return ownConfirmedTx(c, required, func(*actionCtx) (actionResult, error) {
		return actionResult{}, errFormNonceMismatch
	})
}

// A competing exact submission can read the changed live state while the
// original receipt is still uncommitted. The database conflict then reveals
// its original mode after verifying the full actor/session/path/request
// identity. Retry once outside the transaction so bcrypt never holds its locks.
type ownFormModeChanged struct{ required bool }

func (e ownFormModeChanged) Error() string { return "original form confirmation mode changed" }

func ownConfirmedTx(c *actionCtx, required bool, run func(*actionCtx) (actionResult, error)) (actionResult, error) {
	res, err := confirmedTx(c, required, run)
	var saved ownFormModeChanged
	if errors.As(err, &saved) {
		return confirmedTx(c, saved.required, run)
	}
	// A concurrent no-password request may commit after this request's first
	// probe but before its live-state read, making the latter demand a password
	// that the original form did not need. An exact, now-completed receipt is
	// the only exception; its replay still takes the current actor/session locks.
	var unknown commitOutcomeUnknown
	if required && err != nil && !errors.As(err, &unknown) {
		savedRequired, completed, probeErr := completedOwnFormMode(c)
		if probeErr == nil && completed && !savedRequired {
			return confirmedTx(c, false, func(*actionCtx) (actionResult, error) {
				return actionResult{}, errFormNonceMismatch
			})
		}
	}
	return res, err
}

// claimFormSubmission inserts the claim before the action does any work. PostgreSQL's unique key makes a
// concurrent same-nonce request wait for the first transaction: after a rollback it can run, and after a
// commit it reads the original success. OneAtATime actions still refuse in-flight repeats before opening a
// database transaction; retrying the unchanged form after the first finishes reads its receipt. No failed
// action leaves a claim behind. A receipt proves one committed market action, not that an external wallet
// skipped address generation before a rolled-back transaction.
func claimFormSubmission(c *actionCtx) (actionResult, bool, error) {
	nonce := c.Form.Get(formNonceField)
	if !formNoncePattern.MatchString(nonce) || len(c.Form[formNonceField]) != 1 {
		return actionResult{}, false, fail(400, "Reload this form and try again. Nothing was changed.")
	}
	requestHMAC := c.A.formRequestHMAC(c)
	sessionHash := digest(c.Token)
	r, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO form_submissions(nonce,user_id,session_hash,action_path,request_hmac,redirect,confirmation_required)
		VALUES($1,$2,$3,$4,$5,'',$6) ON CONFLICT (nonce) DO NOTHING`, nonce, c.User.ID, sessionHash, c.R.URL.Path, requestHMAC, c.OwnFormNonce && c.ReceiptConfirmRequired)
	if err != nil {
		return actionResult{}, false, err
	}
	if inserted, err := r.RowsAffected(); err != nil {
		return actionResult{}, false, err
	} else if inserted == 1 {
		return actionResult{}, false, nil
	}
	var userID, savedSession, path, savedHMAC, redirect, notice string
	var savedRequired bool
	err = c.Tx.QueryRowContext(c.Ctx(), `SELECT user_id,session_hash,action_path,request_hmac,redirect,notice,confirmation_required
		FROM form_submissions WHERE nonce=$1`, nonce).Scan(&userID, &savedSession, &path, &savedHMAC, &redirect, &notice, &savedRequired)
	if err != nil {
		return actionResult{}, false, err
	}
	if userID != c.User.ID || savedSession != sessionHash || path != c.R.URL.Path || redirect == "" ||
		subtle.ConstantTimeCompare([]byte(savedHMAC), []byte(requestHMAC)) != 1 {
		return actionResult{}, false, errFormNonceMismatch
	}
	if c.OwnFormNonce && savedRequired != c.ReceiptConfirmRequired {
		return actionResult{}, false, ownFormModeChanged{required: savedRequired}
	}
	return actionResult{Redirect: redirect, Notice: notice}, true, nil
}

// finishFormSubmission writes the success outcome in the action's transaction, after its audit row and before
// Commit. The stored path is the base redirect; replay applies the same one-shot notice and fragment as first use.
func finishFormSubmission(c *actionCtx, res actionResult) error {
	_, err := c.Tx.ExecContext(c.Ctx(), `UPDATE form_submissions SET redirect=$2,notice=$3 WHERE nonce=$1`,
		c.Form.Get(formNonceField), res.Redirect, res.Notice)
	return err
}

const formSubmissionSweepInterval = time.Minute

// Old receipts are retained for 24 hours, longer than a session's 12-hour life. Sweep only on running
// processes; a bounded batch at startup and each minute catches up without a long delete or any GET write.
func formSubmissionSweeper(ctx context.Context, a *App) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sweepFormSubmissions(ctx); err != nil && ctx.Err() == nil {
			log.Printf("form submission sweep: %s", errorCause(err))
		}
		t.Reset(formSubmissionSweepInterval)
	}
}

func (a *App) sweepFormSubmissions(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for range 10 {
		r, err := a.db.ExecContext(ctx, `WITH old AS (
			SELECT nonce FROM form_submissions WHERE expires<now() ORDER BY expires LIMIT 1000 FOR UPDATE SKIP LOCKED
		) DELETE FROM form_submissions f USING old WHERE f.nonce=old.nonce`)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil || n < 1000 {
			return err
		}
	}
	return nil
}
