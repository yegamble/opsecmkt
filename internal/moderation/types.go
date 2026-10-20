// Package moderation evaluates abuse reports without authority to change accounts,
// listings, orders or funds. Its inputs must be assembled from a trusted snapshot
// by the application, never deserialized directly from a marketplace request.
package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const PromptVersion = "moderation-v2"

const (
	ActionNoAction       = "NO_ACTION"
	ActionFlagReview     = "FLAG_REVIEW"
	ActionSuggestHide    = "SUGGEST_HIDE"
	ActionSuggestBan     = "SUGGEST_BAN"
	ActionSuggestRelease = "SUGGEST_RELEASE"
	ActionSuggestRefund  = "SUGGEST_REFUND"
	ActionDraftReply     = "DRAFT_REPLY"
)

// Actions and ReasonCodes return fresh allowlists, so a caller cannot alter the
// validator's policy by mutating shared memory.
func Actions() []string {
	return []string{ActionNoAction, ActionFlagReview, ActionSuggestHide, ActionSuggestBan, ActionSuggestRelease, ActionSuggestRefund, ActionDraftReply}
}

// ModelActions excludes historical actions that required generated prose.
// Actions retains those identifiers for reading old persisted proposals.
func ModelActions() []string {
	return []string{ActionNoAction, ActionFlagReview, ActionSuggestHide, ActionSuggestBan, ActionSuggestRelease, ActionSuggestRefund}
}

func ReasonCodes() []string {
	return []string{"SPAM_TEMPLATE", "SCAM_TEMPLATE", "DUPLICATE_TEXT", "DUPLICATE_IMAGE", "PRICE_OUTLIER", "INCOMPLETE_LISTING", "INVALID_FIELDS", "ORDER_STATE_VIOLATION", "TIMER_DUE", "ABUSE_RATE", "INJECTION_SUSPECTED", "UNICODE_MIXED_SCRIPT", "ZERO_WIDTH_OBFUSCATION", "OFF_PLATFORM_CONTACT", "INPUT_TRUNCATED", "DETERMINISTIC_CONFLICT", "LOW_CONFIDENCE", "NEW_ACTOR", "MONEY_INVOLVED", "NOVEL_CASE", "MODEL_ABSENT", "MODEL_FAILURE", "INVALID_OUTPUT", "CANARY_MISMATCH", "NO_FINDING", "HUMAN_REVIEW", "POLICY_BOUNDARY", "SLA_EXPIRED"}
}

type Disposition string

const (
	AutoApply     Disposition = "AUTO_APPLY"
	QueueHuman    Disposition = "QUEUE_HUMAN"
	EscalateAdmin Disposition = "ESCALATE_ADMIN"
	Hold          Disposition = "HOLD"
)

type Config struct {
	Enabled         bool
	PromptVersion   string
	ModelID         string
	WeightsHash     string
	ConfidenceFloor float64
	NewActorDays    int
	AbuseThreshold  int
	HumanSLA        time.Duration
}

// Defaults are conservative development values. Enabling a worker or selecting
// production review durations remains an explicit operator action.
func DefaultConfig() Config {
	return Config{PromptVersion: PromptVersion, ConfidenceFloor: .9, NewActorDays: 14, AbuseThreshold: 5, HumanSLA: 24 * time.Hour}
}

type Actor struct {
	ID         string  `json:"id"`
	AgeDays    int     `json:"age_days"`
	Reputation float64 `json:"reputation"`
}

// Trust is always DATA. It describes provenance; it is not a prompt capability.
type Field struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	Trust  string `json:"trust"`
	Value  string `json:"value"`
}

// DeterministicContext contains application-derived observations. For example,
// OrderState is read from the database; a listing saying "delivered" cannot set it.
// Hashes are gateway-computed image hashes, never image data to decode here.
type DeterministicContext struct {
	EvidenceIDs            []string `json:"evidence_ids"`
	Complete               bool     `json:"complete"`
	PriceAtomic            int64    `json:"price_atomic"`
	CategoryMedianAtomic   int64    `json:"category_median_atomic"`
	KnownTextHashes        []string `json:"known_text_hashes"`
	ImageHashes            []string `json:"image_hashes"`
	KnownImageHashes       []string `json:"known_image_hashes"`
	OrderState             string   `json:"order_state"`
	ProposedOrderState     string   `json:"proposed_order_state"`
	TimerKind              string   `json:"timer_kind"`
	TimerDue               bool     `json:"timer_due"`
	DeterministicInvariant bool     `json:"deterministic_invariant"`
	MoneyInvolved          bool     `json:"money_involved"`
	Novel                  bool     `json:"novel"`
	BlastRadius            int      `json:"blast_radius"`
	Reversible             bool     `json:"reversible"`
	AbuseCount             int      `json:"abuse_count"`
}

type Input struct {
	ItemID  string               `json:"item_id"`
	Kind    string               `json:"kind"`
	Actor   Actor                `json:"actor"`
	Fields  []Field              `json:"fields"`
	Context DeterministicContext `json:"context"`
}

type NormalizedInput struct {
	Input
	InputHash             string   `json:"input_hash"`
	TextHash              string   `json:"text_hash"`
	InjectionSuspected    bool     `json:"injection_suspected"`
	InjectionEvidence     []string `json:"injection_evidence"`
	NormalizationWarnings []string `json:"normalization_warnings"`
}

// Proposal is the persisted DTO, including historical display fields. Current
// inference responses contain only closed identifiers/codes, numeric confidence,
// a boolean and the canary; Summary and InjectionEvidence are rendered by Go.
// Neither this object nor the model interface grants execution authority.
type Proposal struct {
	ItemID             string   `json:"item_id"`
	Action             string   `json:"action"`
	Confidence         float64  `json:"confidence"`
	ReasonCodes        []string `json:"reason_codes"`
	InjectionSuspected bool     `json:"injection_suspected"`
	InjectionEvidence  []string `json:"injection_evidence"`
	EvidenceIDs        []string `json:"evidence_ids"`
	Summary            string   `json:"summary"`
	Canary             string   `json:"canary"`
}

type Model interface {
	Propose(context.Context, NormalizedInput) (Proposal, error)
}

type Decision struct {
	ID            string        `json:"id"`
	InputHash     string        `json:"input_hash"`
	OutputHash    string        `json:"output_hash"`
	Proposal      Proposal      `json:"proposal"`
	Disposition   Disposition   `json:"disposition"`
	RiskScore     int           `json:"risk_score"`
	ReasonCodes   []string      `json:"reason_codes"`
	FailureClass  string        `json:"failure_class"`
	Actor         string        `json:"actor"`
	PromptVersion string        `json:"prompt_version"`
	ModelID       string        `json:"model_id"`
	WeightsHash   string        `json:"weights_hash"`
	SLA           time.Duration `json:"sla"`
	DefaultAction Disposition   `json:"default_action"`
}

// CanaryFor detects protocol mismatches; it is not a secret, authentication
// token, or proof that a prompt injection was contained.
func CanaryFor(inputHash string) string {
	h := sha256.Sum256([]byte("moderation-canary-v2:" + inputHash))
	return "moderation-" + hex.EncodeToString(h[:16])
}
