package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"opsecmkt/internal/moderation"
)

const moderationReviewPath = "/admin/moderation/review"

var moderationDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var moderationRevisionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

// These views deliberately contain neither endpoint identities nor raw model
// JSON. Every summary/evidence string remains untrusted text for html/template.
type ModerationAdminView struct {
	ModelState, PromptVersion, WeightsHash, LastPass, Failure string
	Pending, Escalations, InjectionCount, Jobs                int
	Items                                                     []ModerationSuggestionView
	Page                                                      int
	PreviousURL, NextURL                                      string
	Reviewed                                                  bool
}

type ModerationSuggestionView struct {
	ID, Kind, TargetID, TargetURL, Created, Expires string
	EscalationURL                                   string
	Disposition, DefaultAction, Summary, Reasons    string
	InputHash, OutputHash, Review                   string
	InjectionEvidence                               []string
	Risk                                            int
	Confidence                                      string
	Expired, CanAccept                              bool
}

func init() {
	registerLoader("admin", func(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
		d.PrivateModerationAccess = !a.preview && a.adminSocket != "" && privateAdminIngress(ctx) && d.User != nil && d.User.Role == "admin"
		return nil
	})
	registerPage("admin/moderation", pageSpec{Protected: true, Roles: []string{"admin"}})
	registerLoader("admin/moderation", loadModerationAdmin)
	registerRaw("/admin/moderation/escalation", moderationEscalationDownload)
	registerAction(moderationReviewPath, actionSpec{Roles: []string{"admin"}, OwnTx: true,
		ConfirmedFormNonce: true, OneAtATime: true, ReceiptAuthorize: moderationReviewAuthorize,
		Run: moderationReviewAction})
}

func moderationPrivateRoute(path string) bool {
	return path == "/admin/moderation" || strings.HasPrefix(path, "/admin/moderation/")
}

func loadModerationAdmin(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	d.Moderation = nil
	if !privateAdminIngress(ctx) || a.adminSocket == "" || a.preview || a.db == nil || d.User == nil || d.User.Role != "admin" {
		return sql.ErrNoRows
	}
	var totp bool
	err := a.db.QueryRowContext(ctx, `SELECT totp_enabled FROM users WHERE id=$1 AND role='admin'
	 AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL`, d.User.ID).Scan(&totp)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	d.Security = &SecurityView{TOTPEnabled: totp}
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if r.URL.Query().Get("page") == "" {
		page = 1
	} else if err != nil || page < 1 || page > 10000 {
		return fail(400, "Choose a moderation queue page between 1 and 10000.")
	}
	v := &ModerationAdminView{Page: page, Reviewed: r.URL.Query().Get("state") == "reviewed"}
	var state, failure, prompt, weights string
	var last sql.NullTime
	err = a.db.QueryRowContext(ctx, `SELECT model_state,prompt_version,weights_hash,last_pass,failure_class
	 FROM moderation_worker_status WHERE singleton`).Scan(&state, &prompt, &weights, &last, &failure)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	v.ModelState = moderationModelState(state)
	v.Failure = moderationFailureLabel(failure)
	if moderationRevisionPattern.MatchString(prompt) {
		v.PromptVersion = prompt
	} else {
		v.PromptVersion = "Not reported"
	}
	v.WeightsHash = moderationSafeHash(weights)
	if last.Valid {
		v.LastPass = last.Time.UTC().Format(time.RFC3339)
	}
	if err = a.db.QueryRowContext(ctx, `SELECT
	 (SELECT count(*) FROM moderation_suggestions s WHERE s.disposition<>'AUTO_APPLY' AND NOT EXISTS(SELECT 1 FROM moderation_reviews r WHERE r.suggestion_id=s.id)),
	 (SELECT count(*) FROM moderation_suggestions s WHERE s.disposition='ESCALATE_ADMIN' AND NOT EXISTS(SELECT 1 FROM moderation_reviews r WHERE r.suggestion_id=s.id)),
	 (SELECT count(*) FROM moderation_injection_events),
	 (SELECT count(*) FROM moderation_jobs WHERE completed_at IS NULL)`).Scan(&v.Pending, &v.Escalations, &v.InjectionCount, &v.Jobs); err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT s.id,s.kind,s.target_id,s.disposition,s.risk_score,s.confidence,
	 s.input_hash,s.output_hash,s.created_at,s.expires_at,s.decision,coalesce(r.action,''),coalesce(j.snapshot->>'order_id',''),
	 coalesce(e.id,0),coalesce(octet_length(e.encrypted_payload),0)
	 FROM moderation_suggestions s JOIN moderation_jobs j ON j.id=s.job_id
	 LEFT JOIN moderation_reviews r ON r.suggestion_id=s.id
	 LEFT JOIN moderation_escalations e ON e.latest_suggestion_id=s.id
	 WHERE (r.suggestion_id IS NOT NULL OR s.disposition='AUTO_APPLY')=$1 ORDER BY s.created_at DESC,s.id DESC LIMIT 51 OFFSET $2`, v.Reviewed, (page-1)*50)
	if err != nil {
		return err
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var item ModerationSuggestionView
		var created, expires time.Time
		var confidence float64
		var decision []byte
		var review, orderID string
		var escalationID, encryptedBytes int64
		if err = rows.Scan(&item.ID, &item.Kind, &item.TargetID, &item.Disposition, &item.Risk, &confidence,
			&item.InputHash, &item.OutputHash, &created, &expires, &decision, &review, &orderID, &escalationID, &encryptedBytes); err != nil {
			return err
		}
		if len(v.Items) == 50 {
			v.NextURL = moderationQueueURL(page+1, v.Reviewed)
			continue
		}
		item.Created, item.Expires = created.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339)
		automatic := item.Disposition == string(moderation.AutoApply)
		item.DefaultAction, item.Expired = "HOLD", !expires.After(now)
		item.CanAccept = review == "" && !item.Expired && item.Disposition != string(moderation.Hold)
		if item.Expired && review == "" && !automatic {
			item.Disposition = string(moderation.Hold)
		}
		if !slices.Contains([]string{"AUTO_APPLY", "QUEUE_HUMAN", "ESCALATE_ADMIN", "HOLD"}, item.Disposition) {
			item.Disposition, item.CanAccept = "HOLD", false
		}
		item.Review = moderationReviewLabel(review)
		if automatic && review == "" {
			item.Review, item.CanAccept = "Automatic classification recorded", false
		}
		item.Confidence = strconv.FormatFloat(confidence, 'f', 2, 64)
		item.InputHash, item.OutputHash = moderationSafeHash(item.InputHash), moderationSafeHash(item.OutputHash)
		item.TargetURL = moderationTargetURL(item.Kind, item.TargetID, orderID)
		if escalationID > 0 && encryptedBytes > 0 && encryptedBytes <= 256<<10 {
			item.EscalationURL = "/admin/moderation/escalation?id=" + strconv.FormatInt(escalationID, 10)
		}
		var decoded moderation.Decision
		if len(decision) > 131072 || json.Unmarshal(decision, &decoded) != nil {
			item.Summary, item.CanAccept = "Stored proposal is unavailable. Review the source evidence.", false
		} else {
			item.Summary = moderationDisplayText(decoded.Proposal.Summary, 2000)
			var codes []string
			for _, code := range decoded.ReasonCodes {
				if slices.Contains(moderation.ReasonCodes(), code) && !slices.Contains(codes, code) && len(codes) < 32 {
					codes = append(codes, code)
				}
			}
			item.Reasons = strings.Join(codes, ", ")
			for _, evidence := range decoded.Proposal.InjectionEvidence {
				if len(item.InjectionEvidence) == 8 {
					break
				}
				item.InjectionEvidence = append(item.InjectionEvidence, moderationDisplayText(evidence, 250))
			}
		}
		v.Items = append(v.Items, item)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if page > 1 {
		v.PreviousURL = moderationQueueURL(page-1, v.Reviewed)
	}
	d.Moderation, d.Title = v, "Moderation review"
	return nil
}

// Every replay rechecks the current actor/session through confirmedTx before
// this route-specific private-ingress check. A receipt confers no new authority.
func moderationReviewAuthorize(c *actionCtx) error {
	if !privateAdminIngress(c.Ctx()) || c.A.adminSocket == "" || c.User == nil || c.User.Role != "admin" {
		return fail(http.StatusNotFound, "Unknown action")
	}
	return nil
}

func moderationReviewAction(c *actionCtx) (actionResult, error) {
	if err := moderationReviewAuthorize(c); err != nil {
		return actionResult{}, err
	}
	allowed := []string{"csrf", formNonceField, "password", "code", "suggestion_id", "action"}
	for key, values := range c.Form {
		if !slices.Contains(allowed, key) || len(values) != 1 {
			return actionResult{}, fail(400, "Reload the moderation form and choose one review action.")
		}
	}
	id, action := c.Form.Get("suggestion_id"), c.Form.Get("action")
	if !moderationDigestPattern.MatchString(id) || !slices.Contains([]string{"accept", "dismiss", "hold"}, action) {
		return actionResult{}, fail(400, "Choose an existing proposal and a review action.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		var expires time.Time
		var disposition string
		err := c.Tx.QueryRowContext(c.Ctx(), `SELECT expires_at,disposition FROM moderation_suggestions WHERE id=$1 FOR UPDATE`, id).Scan(&expires, &disposition)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(404, "Proposal not found.")
		}
		if err != nil {
			return actionResult{}, err
		}
		// Read after acquiring the proposal lock, so a concurrent review committed
		// while this request waited is visible under READ COMMITTED.
		var reviewed bool
		if err = c.Tx.QueryRowContext(c.Ctx(), `SELECT EXISTS(SELECT 1 FROM moderation_reviews WHERE suggestion_id=$1)`, id).Scan(&reviewed); err != nil {
			return actionResult{}, err
		}
		if reviewed {
			return actionResult{}, fail(409, "This proposal already has a review. Reload the queue.")
		}
		if action == "accept" {
			if !expires.After(time.Now().UTC()) || disposition == string(moderation.Hold) {
				return actionResult{}, fail(409, "This proposal expired and is on HOLD. Review current evidence before taking any marketplace action.")
			}
			current, err := ModerationSuggestionCurrent(c.Ctx(), c.Tx, id)
			if err != nil {
				return actionResult{}, err
			}
			if !current {
				return actionResult{}, fail(409, "The source changed after this proposal. Review current evidence; marketplace state is unchanged.")
			}
		}
		if _, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO moderation_reviews(suggestion_id,reviewer_user_id,action) VALUES($1,$2,$3)`, id, c.User.ID, action); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/admin/moderation", Audit: "Moderation proposal " + id + " review recorded: " + action + "; marketplace state unchanged"}, nil
	})
}

func moderationQueueURL(page int, reviewed bool) string {
	q := url.Values{"page": {strconv.Itoa(page)}}
	if reviewed {
		q.Set("state", "reviewed")
	}
	return "/admin/moderation?" + q.Encode()
}

func moderationTargetURL(kind, id, order string) string {
	if !signInNextID.MatchString(id) {
		return ""
	}
	switch kind {
	case "listing":
		return "/product?id=" + url.QueryEscape(id)
	case "dispute", "payment_flag", "review":
		if signInNextID.MatchString(order) {
			return "/order?id=" + url.QueryEscape(order)
		}
	}
	return ""
}

func moderationSafeHash(value string) string {
	if moderationDigestPattern.MatchString(value) {
		return value
	}
	return "Not reported"
}

func moderationDisplayText(value string, cap int) string {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > cap {
		return "Text omitted: invalid or over its display limit."
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, value)
}

func moderationModelState(value string) string {
	switch value {
	case "not_observed":
		return "Configured; not observed"
	case "ready":
		return "Validated response at last inference"
	case "disabled":
		return "Disabled"
	case "failed":
		return "Unavailable"
	default:
		return "Absent"
	}
}

// The stored export is already encrypted; this handler neither decrypts it nor
// falls back to a plaintext snapshot. inspectEncrypted verifies its packet form
// without needing a private key. The bundled signing key must be pinned offline.
func moderationEscalationDownload(a *App, w http.ResponseWriter, r *http.Request, u *User) {
	if !privateAdminIngress(r.Context()) || a.adminSocket == "" || a.db == nil || a.preview {
		http.NotFound(w, r)
		return
	}
	if u == nil || u.Role != "admin" {
		http.Error(w, "Administrator access required", http.StatusForbidden)
		return
	}
	var authorized bool
	if err := a.db.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='admin'
	 AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL)`, u.ID).Scan(&authorized); err != nil {
		writeDBFailure(w, r, err)
		return
	}
	if !authorized {
		http.Error(w, "Administrator access required", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id < 1 || len(r.URL.Query()["id"]) != 1 {
		http.Error(w, "Choose an existing escalation.", http.StatusBadRequest)
		return
	}
	if !a.allow(ingressKey(r.Context(), "moderation-export:"+u.ID), 10) {
		http.Error(w, "Too many exports. Try again later.", http.StatusTooManyRequests)
		return
	}
	var payload []byte
	err = a.db.QueryRowContext(r.Context(), `SELECT encrypted_payload FROM moderation_escalations
	 WHERE id=$1 AND octet_length(encrypted_payload) BETWEEN 1 AND 262144`, id).Scan(&payload)
	if err == sql.ErrNoRows {
		http.Error(w, "Encrypted escalation is unavailable.", http.StatusNotFound)
		return
	}
	if err != nil {
		writeDBFailure(w, r, err)
		return
	}
	if _, encrypted, parseErr := inspectEncrypted(string(payload)); parseErr != nil || !encrypted {
		http.Error(w, "Encrypted escalation is unavailable.", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/pgp-encrypted")
	w.Header().Set("Content-Disposition", `attachment; filename="moderation-escalation.asc"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(payload)
	}
}

func moderationFailureLabel(value string) string {
	switch value {
	case "db_unavailable":
		return "Database unavailable"
	case "model_unavailable":
		return "Model unavailable"
	case "model_absent":
		return "No model configured"
	case "model_interrupted":
		return "Model request interrupted"
	case "model_failure":
		return "Model request failed"
	case "invalid_output":
		return "Model output refused"
	case "suspect_output":
		return "Model output flagged for review"
	case "input_invalid":
		return "Input refused"
	case "stale_snapshot":
		return "Source changed; proposal held"
	case "", "none":
		return "None reported"
	default:
		return "Unclassified failure"
	}
}

func moderationReviewLabel(value string) string {
	switch value {
	case "accept":
		return "Reviewed; marketplace state unchanged"
	case "dismiss":
		return "Dismissed"
	case "hold":
		return "Held"
	default:
		return "Pending"
	}
}
