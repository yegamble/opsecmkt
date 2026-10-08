package market

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"regexp"
	"time"
)

// A-130 covers normal-transaction forms whose successful second submission must replay its original response
// rather than create another record or refuse an already-completed action.
var formNoncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

const formNonceField = "form_nonce"

func init() { registerBackground(formSubmissionSweeper) }

// formNonceFor is called once per opted-in form while rendering HTML. Issuing a nonce writes nothing on GET.
func formNonceFor(path string) string {
	if spec, ok := actions[path]; ok && (spec.FormNonce || spec.ConfirmedFormNonce) {
		return randomToken()[:32]
	}
	return ""
}

// formRequestHMAC binds a nonce to the exact posted fields, action and query without retaining their text.
// In particular, a plain digest of a message or a future password field could aid offline guessing.
func (a *App) formRequestHMAC(c *actionCtx) string {
	h := hmac.New(sha256.New, a.key)
	for _, part := range []string{c.R.Method, c.R.URL.Path, c.R.URL.RawQuery, c.Form.Encode()} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var errFormNonceMismatch = fail(409, "This form was already used for a different request. Reload the page and try again. Nothing was changed.")

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
	r, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO form_submissions(nonce,user_id,session_hash,action_path,request_hmac,redirect)
		VALUES($1,$2,$3,$4,$5,'') ON CONFLICT (nonce) DO NOTHING`, nonce, c.User.ID, sessionHash, c.R.URL.Path, requestHMAC)
	if err != nil {
		return actionResult{}, false, err
	}
	if inserted, err := r.RowsAffected(); err != nil {
		return actionResult{}, false, err
	} else if inserted == 1 {
		return actionResult{}, false, nil
	}
	var userID, savedSession, path, savedHMAC, redirect, notice string
	err = c.Tx.QueryRowContext(c.Ctx(), `SELECT user_id,session_hash,action_path,request_hmac,redirect,notice
		FROM form_submissions WHERE nonce=$1`, nonce).Scan(&userID, &savedSession, &path, &savedHMAC, &redirect, &notice)
	if err != nil {
		return actionResult{}, false, err
	}
	if userID != c.User.ID || savedSession != sessionHash || path != c.R.URL.Path || redirect == "" ||
		subtle.ConstantTimeCompare([]byte(savedHMAC), []byte(requestHMAC)) != 1 {
		return actionResult{}, false, errFormNonceMismatch
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
