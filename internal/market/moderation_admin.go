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
	Capacity                                                  *ModerationCapacityView
	// A-685: the queue view ("" unreviewed with a deadline ahead, "expired", "reviewed"), its kind and disposition
	// filters (allowlisted, "" for all), and the unreviewed counts the summary links to.
	State, Kind, Disposition                  string
	QueueOpen, QueueEscalations, QueueExpired int
}

// moderationQueueFilter is a validated queue view: each value is "" or one of moderationQueueStates, the moderation
// kinds or the dispositions, so the query only ever binds an allowlisted value as a parameter.
type moderationQueueFilter struct{ State, Kind, Disposition string }

var (
	moderationQueueStates       = []string{"expired", "reviewed"}
	moderationQueueKinds        = []string{"listing", "upload", "dispute", "review", "payment_flag"}
	moderationQueueDispositions = []string{"ESCALATE_ADMIN", "QUEUE_HUMAN", "HOLD", "AUTO_APPLY"}
)

// KindOptions and DispositionOptions are the queue filter's choices.
func (*ModerationAdminView) KindOptions() []string        { return moderationQueueKinds }
func (*ModerationAdminView) DispositionOptions() []string { return moderationQueueDispositions }

func moderationQueueFilterFrom(q url.Values) (moderationQueueFilter, error) {
	var f moderationQueueFilter
	for _, p := range []struct {
		key     string
		allowed []string
		dst     *string
	}{{"state", moderationQueueStates, &f.State}, {"kind", moderationQueueKinds, &f.Kind}, {"disposition", moderationQueueDispositions, &f.Disposition}} {
		values := q[p.key]
		if len(values) > 1 || len(values) == 1 && values[0] != "" && !slices.Contains(p.allowed, values[0]) {
			return f, fail(400, "Choose a moderation queue view, kind and disposition from the page.")
		}
		if len(values) == 1 {
			*p.dst = values[0]
		}
	}
	return f, nil
}

type ModerationCapacityView struct {
	Pending, MaxPending, AllocatedRecords, MaxRecords, AllocatedBytes, MaxBytes, LegacyJobs, LegacyBytes, CompletedMinimal int64
	Backfill, MaxBackfill, ActorShare, Parked, Rescans, ActorMarkers                                                       int64
	// Deferred counts records whose capture found the capacity row busy (moderation_deferred) and that have no marker;
	// Awaiting is Backfill plus Deferred, so a record both deferred and marked counts once (A-662).
	Deferred, Awaiting int64
	Overflow           []ModerationOverflowView
}
type ModerationOverflowView struct {
	Kind, Reason, Last string
	Count              int64
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
	// Proposed is the stored disposition of an unreviewed proposal shown as HOLD because its deadline passed.
	Proposed string
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
	filter, err := moderationQueueFilterFrom(r.URL.Query())
	if err != nil {
		return err
	}
	v := &ModerationAdminView{Page: page, Reviewed: filter.State == "reviewed", State: filter.State, Kind: filter.Kind, Disposition: filter.Disposition}
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
	if err = a.db.QueryRowContext(ctx, `SELECT unreviewed,admin_pending,injections,pending FROM moderation_capacity WHERE singleton`).Scan(&v.Pending, &v.Escalations, &v.InjectionCount, &v.Jobs); err != nil {
		return err
	}
	v.Capacity = &ModerationCapacityView{}
	cap := v.Capacity
	if err = a.db.QueryRowContext(ctx, `SELECT pending,max_pending,allocated_records,max_records,allocated_bytes,max_bytes,legacy_jobs,legacy_bytes,completed_v2,backfill,max_backfill,GREATEST(8,max_records*2/100),(SELECT count(*) FROM moderation_backfill WHERE parked),(SELECT count(*) FROM moderation_actor_allocations WHERE rescan),GREATEST(0,LEAST(max_actor_markers,max_backfill-1)),(SELECT count(*) FROM moderation_deferred d WHERE NOT EXISTS(SELECT 1 FROM moderation_backfill b WHERE b.kind=d.kind AND b.target_id=d.target_id)) FROM moderation_capacity WHERE singleton`).Scan(&cap.Pending, &cap.MaxPending, &cap.AllocatedRecords, &cap.MaxRecords, &cap.AllocatedBytes, &cap.MaxBytes, &cap.LegacyJobs, &cap.LegacyBytes, &cap.CompletedMinimal, &cap.Backfill, &cap.MaxBackfill, &cap.ActorShare, &cap.Parked, &cap.Rescans, &cap.ActorMarkers, &cap.Deferred); err != nil {
		return err
	}
	cap.Awaiting = cap.Backfill + cap.Deferred
	overflows, err := a.db.QueryContext(ctx, `SELECT kind,reason,occurrences,last_seen FROM moderation_overflow WHERE occurrences>0 ORDER BY kind,reason`)
	if err != nil {
		return err
	}
	for overflows.Next() {
		var x ModerationOverflowView
		var at time.Time
		if err = overflows.Scan(&x.Kind, &x.Reason, &x.Count, &at); err != nil {
			overflows.Close()
			return err
		}
		x.Last = at.UTC().Format(time.RFC3339)
		cap.Overflow = append(cap.Overflow, x)
	}
	err = overflows.Err()
	overflows.Close()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	// The summary's unreviewed counts, bounded by the lifetime allocation budget like the queue itself.
	if err = a.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE s.expires_at>$1),count(*) FILTER (WHERE s.expires_at>$1 AND s.disposition='ESCALATE_ADMIN'),
	 count(*) FILTER (WHERE s.expires_at<=$1) FROM moderation_suggestions s
	 WHERE s.disposition<>'AUTO_APPLY' AND NOT EXISTS(SELECT 1 FROM moderation_reviews r WHERE r.suggestion_id=s.id)`, now).Scan(&v.QueueOpen, &v.QueueEscalations, &v.QueueExpired); err != nil {
		return err
	}
	// A-685: the default view holds unreviewed proposals whose deadline is ahead, escalations first and then the
	// soonest deadline; "expired" holds the unreviewed ones past it (shown as HOLD), escalations first and then the
	// most recently expired; "reviewed" is the history, newest first. Every filter value is allowlisted and bound.
	rows, err := a.db.QueryContext(ctx, `SELECT s.id,s.kind,s.target_id,s.disposition,s.risk_score,s.confidence,
	 s.input_hash,s.output_hash,s.created_at,s.expires_at,s.decision,coalesce(r.action,''),coalesce(j.metadata->>'order_id',j.snapshot->>'order_id',''),
	 coalesce(e.id,0),coalesce(octet_length(e.encrypted_payload),0),s.storage_version,
	 coalesce(i.kind,''),coalesce(i.product_id,''),coalesce(i.order_id,''),coalesce(iu.handle,'')
	 FROM moderation_suggestions s JOIN moderation_jobs j ON j.id=s.job_id
	 LEFT JOIN moderation_reviews r ON r.suggestion_id=s.id
	 LEFT JOIN moderation_escalations e ON e.latest_suggestion_id=s.id
	 LEFT JOIN images i ON s.kind='upload' AND i.id=s.target_id LEFT JOIN users iu ON iu.id=i.owner_user_id
	 WHERE CASE $1 WHEN 'reviewed' THEN r.suggestion_id IS NOT NULL OR s.disposition='AUTO_APPLY'
	  ELSE r.suggestion_id IS NULL AND s.disposition<>'AUTO_APPLY' AND (s.expires_at<=$2)=($1='expired') END
	  AND ($3='' OR s.kind=$3) AND ($4='' OR s.disposition=$4)
	 ORDER BY CASE WHEN $1<>'reviewed' AND s.disposition='ESCALATE_ADMIN' THEN 0 ELSE 1 END,
	  CASE WHEN $1='' THEN s.expires_at END,CASE WHEN $1='expired' THEN s.expires_at END DESC,
	  CASE WHEN $1='reviewed' THEN s.created_at END DESC,s.id DESC LIMIT 51 OFFSET $5`,
		filter.State, now, filter.Kind, filter.Disposition, (page-1)*50)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item ModerationSuggestionView
		var created, expires time.Time
		var confidence float64
		var decision []byte
		var review, orderID string
		var escalationID, encryptedBytes int64
		var storageVersion int
		var image moderationImageRef
		if err = rows.Scan(&item.ID, &item.Kind, &item.TargetID, &item.Disposition, &item.Risk, &confidence,
			&item.InputHash, &item.OutputHash, &created, &expires, &decision, &review, &orderID, &escalationID, &encryptedBytes, &storageVersion,
			&image.Kind, &image.Product, &image.Order, &image.Handle); err != nil {
			return err
		}
		if len(v.Items) == 50 {
			v.NextURL = moderationQueueURL(page+1, filter)
			continue
		}
		item.Created, item.Expires = created.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339)
		automatic := item.Disposition == string(moderation.AutoApply)
		item.DefaultAction, item.Expired = "HOLD", !expires.After(now)
		item.CanAccept = review == "" && !item.Expired && item.Disposition != string(moderation.Hold)
		if item.Expired && review == "" && !automatic {
			if item.Disposition != string(moderation.Hold) && slices.Contains(moderationQueueDispositions, item.Disposition) {
				item.Proposed = item.Disposition
			}
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
		item.TargetURL = moderationTargetURL(item.Kind, item.TargetID, orderID, image)
		if escalationID > 0 && encryptedBytes > 0 && encryptedBytes <= 256<<10 {
			item.EscalationURL = "/admin/moderation/escalation?id=" + strconv.FormatInt(escalationID, 10)
		}
		var decoded moderation.Decision
		if len(decision) > 131072 || json.Unmarshal(decision, &decoded) != nil {
			item.Summary, item.CanAccept = "Stored proposal is unavailable. Review the source evidence.", false
		} else {
			if storageVersion == 2 {
				decoded.Proposal = moderation.RenderStoredProposalDisplay(decoded.Proposal)
			} else {
				decoded.Proposal = moderation.RenderProposalDisplay(decoded.Proposal)
			}
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
		v.PreviousURL = moderationQueueURL(page-1, filter)
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
		// The capacity row first, before the proposal and source rows
		// (SEC72AA-1): recording the review updates its counters.
		//
		// SEC87-V1: confirmedTx has already locked this administrator's own
		// account row (FOR NO KEY UPDATE), the session row (FOR SHARE) and the
		// form receipt; a confirmed receipt form has no hook before those locks
		// (confirmedTxWith refuses Before for ConfirmedFormNonce), so the capacity
		// row cannot come first without changing that shared contract. It cannot
		// deadlock: no transaction holding the capacity row waits on those rows.
		// Admission takes the capacity row with SKIP LOCKED, and an admitting
		// marketplace transaction then holds it until commit: no application path
		// known (QA91-2) then locks an administrator's account, session or receipt
		// row other than its own actor row, which confirmedTx takes first; if one
		// ever did, PostgreSQL's deadlock detection aborts one transaction rather
		// than hanging. The worker's sweep,
		// results and cached completion, the offline review and the capture
		// switch reach an account row only through a foreign-key check or FOR KEY
		// SHARE (SKIP LOCKED in the sweep), which does not conflict with FOR NO
		// KEY UPDATE, and never sessions or receipts; the budget raise takes the
		// same order as this review (TestModerationReviewAccountRowBeforeCapacityNoWait).
		if _, err := c.Tx.ExecContext(c.Ctx(), `SELECT 1 FROM moderation_capacity WHERE singleton FOR UPDATE`); err != nil {
			return actionResult{}, err
		}
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

// moderationQueueURL is a queue page that keeps the view and its filters.
func moderationQueueURL(page int, f moderationQueueFilter) string {
	q := url.Values{"page": {strconv.Itoa(page)}}
	for key, value := range map[string]string{"state": f.State, "kind": f.Kind, "disposition": f.Disposition} {
		if value != "" {
			q.Set(key, value)
		}
	}
	return "/admin/moderation?" + q.Encode()
}

// moderationImageRef is the current image row behind an upload proposal (all "" once the image is gone).
type moderationImageRef struct{ Kind, Product, Order, Handle string }

// moderationTargetURL is the page showing a proposal's current source (A-685: an upload's photo on its listing,
// dispute evidence on its order, an avatar on its account page), or "" when there is none or an id fails the
// allowlist.
func moderationTargetURL(kind, id, order string, img moderationImageRef) string {
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
	case "upload":
		switch {
		case img.Kind == "photo" && signInNextID.MatchString(img.Product):
			return "/product?id=" + url.QueryEscape(img.Product)
		case img.Kind == "evidence" && signInNextID.MatchString(img.Order):
			return "/order?id=" + url.QueryEscape(img.Order)
		case img.Kind == "avatar" && handlePattern.MatchString(img.Handle):
			return adminUserURL(img.Handle)
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
		return "Held; proposal closed"
	default:
		return "Pending"
	}
}
