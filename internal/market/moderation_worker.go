package market

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"opsecmkt/internal/moderation"
)

// ModerationWorker owns database work, never the model process. Inference only
// receives the explicitly assembled moderation.Input, not this object's DB/key.
type ModerationWorker struct {
	DB                 *sql.DB
	Engine             moderation.Engine
	Recipient          *AuditExportRecipient
	SigningKey         ed25519.PrivateKey
	Lease              time.Duration
	WorkerID           string
	ModelFailure       string
	lastFailure        string
	observedModelState string
}

type moderationJob struct {
	ID                                   int64
	Kind, TargetID, ActorID, ContentHash string
	Snapshot                             []byte
	QueuedAt                             time.Time
	CaptureVersion                       int
	Metadata                             []byte
	ActorRef                             string
}

var errModerationStaleSource = errors.New("moderation source changed")

func moderationActorReference(j moderationJob) string {
	if j.ActorRef != "" {
		return j.ActorRef
	}
	h := sha256.Sum256([]byte("moderation-actor-reference:" + strconv.FormatInt(j.ID, 10) + ":" + j.ContentHash))
	return hex.EncodeToString(h[:])
}

func moderationMinimalInput(j moderationJob) moderation.Input {
	var meta struct {
		ActorCreated time.Time `json:"actor_created"`
		OrderState   string    `json:"order_state"`
	}
	_ = json.Unmarshal(j.Metadata, &meta)
	return moderation.Input{ItemID: j.Kind + ":" + j.TargetID, Kind: j.Kind, Actor: moderation.Actor{ID: moderationActorReference(j), AgeDays: max(0, int(j.QueuedAt.Sub(meta.ActorCreated)/(24*time.Hour)))}, Fields: []moderation.Field{}, Context: moderation.DeterministicContext{Complete: false, Reversible: true, MoneyInvolved: j.Kind == "dispute" || j.Kind == "payment_flag", OrderState: meta.OrderState, EvidenceIDs: []string{j.Kind + ":" + j.TargetID, "content:" + j.ContentHash}, BlastRadius: 1}}
}

// No per-field digests: unkeyed hashes of small value sets (a rating, a
// category) are trivially reversible and nothing reads them. The whole-revision
// source digest binds the record to its captured revision.
func moderationStoredMetadata(j moderationJob, in moderation.Input) any {
	return struct {
		Version string `json:"metadata_version"`
		ItemID  string `json:"item_id"`
		Kind    string `json:"kind"`
		Digest  string `json:"source_digest"`
		Context any    `json:"context"`
	}{"2", in.ItemID, in.Kind, j.ContentHash, struct {
		OrderState  string   `json:"order_state"`
		EvidenceIDs []string `json:"evidence_ids"`
		Complete    bool     `json:"complete"`
		Money       bool     `json:"money_involved"`
	}{in.Context.OrderState, in.Context.EvidenceIDs, in.Context.Complete, in.Context.MoneyInvolved}}
}

// SetModerationEnabled controls future transactional enqueueing. Stopping the
// worker does not turn enqueueing off: interrupted work must remain durable.
func SetModerationEnabled(ctx context.Context, db *sql.DB, enabled bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE settings SET value=$1 WHERE key='moderation_enabled'`, strconv.FormatBool(enabled))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errors.New("moderation migration required")
	}
	if !enabled {
		if _, err = tx.ExecContext(ctx, `UPDATE moderation_worker_status SET present=false,model_state='disabled',failure_class='',last_pass=now() WHERE singleton`); err != nil {
			return err
		}
		// No worker pass will expire per-actor share windows while capture is
		// off; they are meaningless then, so no daily count outlives the switch.
		// Rows that still track waiting markers or a pending rescan keep only
		// that state, so re-enabling capture loses no refused target.
		var counters bool
		if err = tx.QueryRowContext(ctx, `SELECT to_regclass('moderation_actor_allocations') IS NOT NULL`).Scan(&counters); err != nil {
			return err
		}
		if counters {
			// Lock order matches admission and the sweep: the capacity row
			// first, then allocation rows, so the switch cannot deadlock them.
			if _, err = tx.ExecContext(ctx, `SELECT 1 FROM moderation_capacity WHERE singleton FOR UPDATE`); err != nil {
				return err
			}
			// Rows an account erasure is deleting are skipped, not waited for.
			if _, err = tx.ExecContext(ctx, `DELETE FROM moderation_actor_allocations WHERE actor_id IN (SELECT actor_id FROM moderation_actor_allocations WHERE markers=0 AND NOT rescan FOR UPDATE SKIP LOCKED)`); err != nil {
				return err
			}
			// A rescan restarts from the beginning when capture is enabled again;
			// its position (a record reference) is not kept while capture is off.
			if _, err = tx.ExecContext(ctx, `UPDATE moderation_actor_allocations SET rescan_ts=NULL,rescan_kind=NULL,rescan_id=NULL WHERE actor_id IN (SELECT actor_id FROM moderation_actor_allocations WHERE rescan_ts IS NOT NULL FOR UPDATE SKIP LOCKED)`); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE moderation_actor_allocations SET allocations=0 WHERE actor_id IN (SELECT actor_id FROM moderation_actor_allocations WHERE allocations>0 FOR UPDATE SKIP LOCKED)`); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func moderationPolicyHash(cfg moderation.Config) string {
	b, _ := json.Marshal(cfg)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Pass processes at most limit jobs. A lease outlives each bounded inference
// call; a killed worker's work is available again after the lease. Nothing is
// removed from a marketplace request or target by this worker.
func (w *ModerationWorker) Pass(ctx context.Context, limit int) (int, error) {
	if w.DB == nil || limit < 1 || limit > 100 {
		return 0, errors.New("moderation worker configuration invalid")
	}
	if w.Lease < time.Minute || w.Lease > 10*time.Minute {
		w.Lease = 2 * time.Minute
	}
	if w.WorkerID == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, errors.New("moderation worker identity unavailable")
		}
		w.WorkerID = hex.EncodeToString(b[:])
	}
	// Capture the current revision of targets whose capture was refused, before
	// claiming work, in its own short transaction.
	sweepCtx, sweepCancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := moderationBackfillSweep(sweepCtx, w.DB, limit)
	sweepCancel()
	if err != nil {
		return 0, errors.New("moderation backfill unavailable")
	}
	count := 0
	for count < limit {
		claimCtx, claimCancel := context.WithTimeout(ctx, 5*time.Second)
		job, err := w.claim(claimCtx)
		claimCancel()
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return count, errors.New("moderation queue unavailable")
		}
		processCtx, processCancel := context.WithTimeout(ctx, w.Lease*3/4)
		err = w.process(processCtx, job)
		processCancel()
		if err != nil {
			return count, err
		}
		count++
	}
	state := "absent"
	if w.Engine.Model != nil && w.Engine.Config.Enabled {
		state = "not_observed"
		if w.observedModelState != "" {
			state = w.observedModelState
		}
	}
	failure := w.lastFailure
	if state == "absent" && failure == "" {
		failure = "model_absent"
	}
	if w.ModelFailure != "" {
		failure = w.ModelFailure
		state = "failed"
	}
	statusCtx, statusCancel := context.WithTimeout(ctx, 5*time.Second)
	defer statusCancel()
	_, err = w.DB.ExecContext(statusCtx, `UPDATE moderation_worker_status SET present=true,model_state=$1,prompt_version=$2,model_id=$3,weights_hash=$4,last_pass=now(),failure_class=$5 WHERE singleton`, state, w.Engine.Config.PromptVersion, w.Engine.Config.ModelID, w.Engine.Config.WeightsHash, failure)
	if err != nil {
		return count, errors.New("moderation status unavailable")
	}
	return count, nil
}

func (w *ModerationWorker) claim(ctx context.Context) (moderationJob, error) {
	var j moderationJob
	err := w.DB.QueryRowContext(ctx, `SELECT id,kind,target_id,actor_id,content_hash,snapshot,queued_at,capture_version,metadata,actor_ref FROM moderation_claim($1,$2,$3)`, w.WorkerID, w.Lease.Milliseconds(), moderationPolicyHash(w.Engine.Config)).Scan(&j.ID, &j.Kind, &j.TargetID, &j.ActorID, &j.ContentHash, &j.Snapshot, &j.QueuedAt, &j.CaptureVersion, &j.Metadata, &j.ActorRef)
	return j, err
}

func (w *ModerationWorker) process(ctx context.Context, j moderationJob) error {
	policy := moderationPolicyHash(w.Engine.Config)
	var cached bool
	if err := w.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM moderation_suggestions WHERE job_id=$1 AND policy_hash=$2)`, j.ID, policy).Scan(&cached); err != nil {
		return errors.New("moderation cache unavailable")
	}
	if cached {
		_, err := w.DB.ExecContext(ctx, `UPDATE moderation_jobs SET completed_at=now(),lease_until=NULL,claimed_by='' WHERE id=$1 AND claimed_by=$2`, j.ID, w.WorkerID)
		return err
	}
	in, err := w.input(ctx, j)
	stale := errors.Is(err, errModerationStaleSource)
	if err != nil && !stale {
		return errors.New("moderation context unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, w.Lease/2)
	engine := w.Engine
	if stale {
		engine.Model = nil
	}
	observed := &moderationObservedModel{model: engine.Model}
	if engine.Model != nil {
		engine.Model = observed
	}
	decision, err := engine.Evaluate(callCtx, in)
	cancel()
	if err != nil {
		return errors.New("moderation evaluation unavailable")
	}
	if stale {
		if decision.Disposition != moderation.EscalateAdmin {
			decision.Disposition = moderation.Hold
		}
		decision.DefaultAction = moderation.Hold
		decision.FailureClass = "stale_snapshot"
		decision.ReasonCodes = []string{"DETERMINISTIC_CONFLICT", "HUMAN_REVIEW"}
		if in.Context.MoneyInvolved {
			decision.ReasonCodes = append(decision.ReasonCodes, "MONEY_INVOLVED")
		}
		decision.Proposal.ReasonCodes = decision.ReasonCodes
		decision.Proposal.Confidence = 0
		decision.Proposal = moderation.RenderProposalDisplay(decision.Proposal)
		encoded, _ := json.Marshal(decision.Proposal)
		h := sha256.Sum256(encoded)
		decision.OutputHash = hex.EncodeToString(h[:])
	}
	if observed.called {
		if decision.FailureClass == "" {
			w.observedModelState = "ready"
			w.lastFailure = ""
		} else {
			w.observedModelState = "failed"
			w.lastFailure = decision.FailureClass
		}
	}
	return w.persist(ctx, j, policy, in, decision)
}

// Observe only an actual inference attempt; deterministic work cannot establish
// that a configured runtime is healthy or clear its last observed failure.
type moderationObservedModel struct {
	model  moderation.Model
	called bool
}

func (m *moderationObservedModel) Propose(ctx context.Context, in moderation.NormalizedInput) (moderation.Proposal, error) {
	m.called = true
	return m.model.Propose(ctx, in)
}

func (w *ModerationWorker) input(ctx context.Context, j moderationJob) (moderation.Input, error) {
	snapshot := j.Snapshot
	if j.CaptureVersion == 2 {
		var hash string
		err := w.DB.QueryRowContext(ctx, `SELECT digest,source FROM moderation_live_source($1,$2)`, j.Kind, j.TargetID).Scan(&hash, &snapshot)
		if errors.Is(err, sql.ErrNoRows) || err == nil && hash != j.ContentHash {
			return moderationMinimalInput(j), errModerationStaleSource
		}
		if err != nil {
			return moderation.Input{}, err
		}
	}
	var raw struct {
		ActorCreated   time.Time `json:"actor_created"`
		Title          string    `json:"title"`
		Description    string    `json:"description"`
		Category       string    `json:"category"`
		Region         string    `json:"region"`
		ProductKind    string    `json:"kind"`
		Price          int64     `json:"price_atomic"`
		Reason         string    `json:"reason"`
		Status         string    `json:"status"`
		Resolution     string    `json:"resolution"`
		OrderID        string    `json:"order_id"`
		OrderState     string    `json:"order_state"`
		Body           string    `json:"body"`
		Rating         int       `json:"rating"`
		ImageHash      string    `json:"image_hash"`
		PerceptualHash string    `json:"perceptual_hash"`
		Disposition    string    `json:"disposition"`
	}
	if err := json.Unmarshal(snapshot, &raw); err != nil {
		return moderation.Input{}, err
	}
	age := max(0, int(j.QueuedAt.Sub(raw.ActorCreated)/(24*time.Hour)))
	// Bind the full-content digest even when the snapshot text is an excerpt.
	// Two distinct oversized revisions must not share an audit identity merely
	// because their bounded excerpts are equal.
	in := moderation.Input{ItemID: j.Kind + ":" + j.TargetID, Kind: j.Kind, Actor: moderation.Actor{ID: moderationActorReference(j), AgeDays: age}, Fields: []moderation.Field{}, Context: moderation.DeterministicContext{Complete: true, Reversible: true, EvidenceIDs: []string{j.Kind + ":" + j.TargetID, "content:" + j.ContentHash}, BlastRadius: 1}}
	add := func(name, value string) {
		in.Fields = append(in.Fields, moderation.Field{Source: j.Kind, Name: name, Trust: "DATA", Value: value})
	}
	switch j.Kind {
	case "listing":
		add("title", raw.Title)
		add("description", raw.Description)
		add("category", raw.Category)
		add("region", raw.Region)
		add("kind", raw.ProductKind)
		in.Context.Complete = strings.TrimSpace(raw.Title) != "" && strings.TrimSpace(raw.Description) != "" && strings.TrimSpace(raw.Category) != "" && raw.Price > 0
		in.Context.PriceAtomic = raw.Price
		// A bounded sample makes the baseline a signal rather than a market-wide
		// expensive aggregate. Its exact values are retained in the input record.
		if err := w.DB.QueryRowContext(ctx, `SELECT COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY xmr),0) FROM (SELECT xmr FROM products WHERE category=$1 AND id<>$2 AND NOT archived ORDER BY id LIMIT 1000) p`, raw.Category, j.TargetID).Scan(&in.Context.CategoryMedianAtomic); err != nil {
			return in, err
		}
		rows, err := w.DB.QueryContext(ctx, `SELECT text_hash FROM moderation_suggestions WHERE kind='listing' AND target_id<>$1 AND text_hash<>'' ORDER BY created_at DESC LIMIT 32`, j.TargetID)
		if err != nil {
			return in, err
		}
		for rows.Next() {
			var h string
			if err = rows.Scan(&h); err != nil {
				rows.Close()
				return in, err
			}
			in.Context.KnownTextHashes = append(in.Context.KnownTextHashes, h)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return in, err
		}
	case "upload":
		add("description", raw.Description)
		in.Context.ImageHashes = []string{raw.ImageHash}
		if raw.PerceptualHash != "" {
			in.Context.ImageHashes = append(in.Context.ImageHashes, raw.PerceptualHash)
		}
		rows, err := w.DB.QueryContext(ctx, `SELECT coalesce(metadata->>'image_hash',snapshot->>'image_hash'),coalesce(metadata->>'perceptual_hash',snapshot->>'perceptual_hash','') FROM moderation_jobs WHERE kind='upload' AND target_id<>$1 AND coalesce(metadata->>'image_hash',snapshot->>'image_hash') IS NOT NULL ORDER BY queued_at DESC LIMIT 16`, j.TargetID)
		if err != nil {
			return in, err
		}
		for rows.Next() {
			var h, ph string
			if err = rows.Scan(&h, &ph); err != nil {
				rows.Close()
				return in, err
			}
			in.Context.KnownImageHashes = append(in.Context.KnownImageHashes, h)
			if ph != "" {
				in.Context.KnownImageHashes = append(in.Context.KnownImageHashes, ph)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return in, err
		}
	case "dispute":
		add("reason", raw.Reason)
		add("resolution", raw.Resolution)
		in.Context.MoneyInvolved = true
		in.Context.Novel = true
	case "review":
		add("body", raw.Body)
		add("rating", strconv.Itoa(raw.Rating))
	case "payment_flag":
		// Do not send txids, deposit addresses or wallet details to inference.
		add("disposition", raw.Disposition)
		in.Context.MoneyInvolved = true
	}
	in.Context.OrderState = raw.OrderState
	// Inspect at most 1000 recent captures, resolving ownership from current
	// source rows. This signal survives completion without archiving actor IDs.
	if err := w.DB.QueryRowContext(ctx, `SELECT count(*) FROM(SELECT kind,target_id FROM moderation_jobs WHERE queued_at BETWEEN $2::timestamptz-interval '1 hour' AND $2::timestamptz ORDER BY queued_at DESC,id DESC LIMIT 1000) a WHERE moderation_live_actor(kind,target_id)=$1`, j.ActorID, j.QueuedAt).Scan(&in.Context.AbuseCount); err != nil {
		return in, err
	}
	return in, nil
}

func (w *ModerationWorker) persist(ctx context.Context, j moderationJob, policy string, in moderation.Input, d moderation.Decision) error {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("moderation transaction unavailable")
	}
	defer tx.Rollback()
	// The lease owner must still hold the job. A late response from an old
	// process never overwrites a replacement worker's completed decision.
	var owner string
	var lease time.Time
	if err = tx.QueryRowContext(ctx, `SELECT claimed_by,lease_until FROM moderation_jobs WHERE id=$1 FOR UPDATE`, j.ID).Scan(&owner, &lease); err != nil {
		return err
	}
	if owner != w.WorkerID || !lease.After(time.Now()) {
		return errors.New("moderation lease expired")
	}
	current, err := moderationTargetCurrent(ctx, tx, j.Kind, j.TargetID, j.ContentHash)
	if err != nil {
		return err
	}
	if current && j.Kind == "listing" {
		var category string
		if err = tx.QueryRowContext(ctx, `SELECT category FROM products WHERE id=$1`, j.TargetID).Scan(&category); err != nil {
			return err
		}
		var median int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(percentile_disc(0.5) WITHIN GROUP(ORDER BY xmr),0) FROM (SELECT xmr FROM products WHERE category=$1 AND id<>$2 AND NOT archived ORDER BY id LIMIT 1000) p`, category, j.TargetID).Scan(&median); err != nil {
			return err
		}
		current = median == in.Context.CategoryMedianAtomic
	}
	if !current {
		// A stale proposal cannot be accepted, but an injection/high-risk signal
		// must still reach the private administrator queue.
		if d.Disposition != moderation.EscalateAdmin {
			d.Disposition = moderation.Hold
		}
		d.DefaultAction = moderation.Hold
		d.FailureClass = "stale_snapshot"
		if !slices.Contains(d.ReasonCodes, "DETERMINISTIC_CONFLICT") {
			d.ReasonCodes = append(d.ReasonCodes, "DETERMINISTIC_CONFLICT")
		}
		d.Proposal.ReasonCodes = d.ReasonCodes
		d.Proposal.Confidence = min(d.Proposal.Confidence, .5)
		d.Proposal.Summary = "The target changed or was removed while moderation was running. Preserve current state and review the current evidence."
		output, _ := json.Marshal(d.Proposal)
		h := sha256.Sum256(output)
		d.OutputHash = hex.EncodeToString(h[:])
	}
	d.Proposal = moderation.RenderProposalForInput(d.Proposal, in)
	finalProposal, _ := json.Marshal(d.Proposal)
	finalHash := sha256.Sum256(finalProposal)
	d.OutputHash = hex.EncodeToString(finalHash[:])
	inBytes, _ := json.Marshal(moderationStoredMetadata(j, in))
	outBytes, _ := json.Marshal(d)
	reasons, _ := json.Marshal(d.ReasonCodes)
	// Only listing duplicate-text detection reads this fingerprint.
	var textHash string
	if j.Kind == "listing" {
		if normalized, normErr := moderation.Normalize(in); normErr == nil {
			textHash = normalized.TextHash
		}
	}
	expires := j.QueuedAt.Add(d.SLA)
	if d.SLA <= 0 {
		expires = j.QueuedAt.Add(24 * time.Hour)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO moderation_suggestions(id,job_id,kind,target_id,actor_id,content_hash,policy_hash,input,decision,disposition,risk_score,confidence,reason_codes,input_hash,output_hash,prompt_version,model_id,weights_hash,expires_at,failure_class,text_hash)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) ON CONFLICT DO NOTHING`, d.ID, j.ID, j.Kind, j.TargetID, "", j.ContentHash, policy, string(inBytes), string(outBytes), d.Disposition, d.RiskScore, d.Proposal.Confidence, string(reasons), d.InputHash, d.OutputHash, d.PromptVersion, d.ModelID, d.WeightsHash, expires, d.FailureClass, textHash)
	if err != nil {
		return fmt.Errorf("moderation record unavailable: %w", err)
	}
	inserted, _ := res.RowsAffected()
	if inserted > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO moderation_risk_scores(suggestion_id,score,disposition,reasons) VALUES($1,$2,$3,$4)`, d.ID, d.RiskScore, d.Disposition, string(reasons)); err != nil {
			return err
		}
		if d.Proposal.InjectionSuspected {
			evidence, _ := json.Marshal(d.Proposal.InjectionEvidence)
			if _, err = tx.ExecContext(ctx, `INSERT INTO moderation_injection_events(suggestion_id,evidence,input_hash) VALUES($1,$2,$3)`, d.ID, string(evidence), d.InputHash); err != nil {
				return err
			}
		}
		if d.Disposition == moderation.EscalateAdmin {
			if err = w.escalate(ctx, tx, j, in, d, expires); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(action) VALUES($1)`, "Moderation proposal recorded: "+d.ID+" ("+string(d.Disposition)+")"); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE moderation_jobs SET completed_at=now(),lease_until=NULL,claimed_by='',failure_class=$2 WHERE id=$1`, j.ID, d.FailureClass); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *ModerationWorker) escalate(ctx context.Context, tx *sql.Tx, j moderationJob, in moderation.Input, d moderation.Decision, expires time.Time) error {
	var encrypted, signature []byte
	var payloadHash string
	// If encryption is not configured the durable private queue still exists,
	// but there is deliberately no plaintext export masquerading as encryption.
	if w.Recipient != nil && len(w.SigningKey) == ed25519.PrivateKeySize {
		payload, err := json.Marshal(struct {
			Version        string                          `json:"version"`
			Decision       moderation.Decision             `json:"decision"`
			Context        moderation.DeterministicContext `json:"context"`
			EvidenceDigest string                          `json:"evidence_digest"`
			Expires        string                          `json:"expires_utc"`
			DefaultAction  string                          `json:"default_action"`
			Alternatives   []string                        `json:"alternatives"`
		}{"1", d, in.Context, j.ContentHash, expires.UTC().Format(time.RFC3339), "HOLD", []string{"review evidence using existing staff workflow", "dismiss proposal", "preserve current state"}})
		if err != nil {
			return err
		}
		hash := sha256.Sum256(payload)
		payloadHash = hex.EncodeToString(hash[:])
		signature = ed25519.Sign(w.SigningKey, payload)
		bundle, err := json.Marshal(struct {
			Payload   json.RawMessage `json:"payload"`
			Signature string          `json:"signature"`
			PublicKey string          `json:"public_key"`
		}{payload, hex.EncodeToString(signature), hex.EncodeToString(w.SigningKey.Public().(ed25519.PublicKey))})
		if err != nil {
			return err
		}
		encrypted, err = w.Recipient.Encrypt(bundle)
		if err != nil {
			return errors.New("moderation escalation encryption unavailable")
		}
	}
	patternBytes, _ := json.Marshal(d.ReasonCodes)
	pattern := sha256.Sum256(patternBytes)
	actorKey := j.ActorID
	if j.CaptureVersion == 2 {
		// A source is already referenced by this queue. Coalescing within that
		// source adds no stable identity across an actor's unrelated targets.
		source := sha256.Sum256([]byte(j.Kind + ":" + j.TargetID))
		actorKey = "source:" + hex.EncodeToString(source[:])
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO moderation_escalations(actor_key,pattern_hash,latest_suggestion_id,encrypted_payload,signature,payload_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)
	ON CONFLICT(actor_key,pattern_hash) DO UPDATE SET latest_suggestion_id=EXCLUDED.latest_suggestion_id,occurrences=LEAST(2147483647,moderation_escalations.occurrences+1),encrypted_payload=EXCLUDED.encrypted_payload,signature=EXCLUDED.signature,payload_hash=EXCLUDED.payload_hash,last_created=now(),expires_at=EXCLUDED.expires_at`, actorKey, hex.EncodeToString(pattern[:]), d.ID, encrypted, signature, payloadHash, expires)
	return err
}

// ModerationSuggestionCurrent re-reads and locks the whitelisted target before
// review acceptance. Acceptance acknowledges a proposal; it grants no executor.
func ModerationSuggestionCurrent(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var kind, target, hash string
	if err := tx.QueryRowContext(ctx, `SELECT kind,target_id,content_hash FROM moderation_suggestions WHERE id=$1`, id).Scan(&kind, &target, &hash); err != nil {
		return false, err
	}
	return moderationTargetCurrent(ctx, tx, kind, target, hash)
}

func moderationTargetCurrent(ctx context.Context, tx *sql.Tx, kind, target, hash string) (bool, error) {
	var body string
	var err error
	switch kind {
	case "listing":
		err = tx.QueryRowContext(ctx, `SELECT jsonb_build_object('title',p.title,'description',p.description,'category',p.category,'region',p.region,'kind',p.kind,'price_atomic',p.xmr,'actor_id',p.vendor_id,'actor_created',u.created)::text FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=$1 FOR UPDATE OF p`, target).Scan(&body)
	case "upload":
		err = tx.QueryRowContext(ctx, `SELECT moderation_upload_snapshot($1)`, target).Scan(&body)
	case "dispute":
		err = tx.QueryRowContext(ctx, `SELECT jsonb_build_object('reason',p.reason,'status',p.status,'resolution',p.resolution,'order_id',p.order_id,'order_state',o.state,'actor_id',o.buyer_id,'actor_created',u.created)::text FROM disputes p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.buyer_id WHERE p.id=$1 FOR UPDATE OF p,o`, target).Scan(&body)
	case "review":
		err = tx.QueryRowContext(ctx, `SELECT jsonb_build_object('body',p.body,'rating',p.rating,'product_id',p.product_id,'order_id',p.order_id,'actor_id',p.buyer_id,'actor_created',u.created)::text FROM reviews p JOIN users u ON u.id=p.buyer_id WHERE p.id=$1 FOR UPDATE OF p`, target).Scan(&body)
	case "payment_flag":
		err = tx.QueryRowContext(ctx, `SELECT jsonb_build_object('order_id',p.order_id,'flagged',p.flagged,'disposition',p.disposition,'order_state',o.state,'actor_id',o.buyer_id,'actor_created',u.created)::text FROM payments p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.buyer_id WHERE p.id::text=$1 FOR UPDATE OF p,o`, target).Scan(&body)
	default:
		return false, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:]) == hash, nil
}
