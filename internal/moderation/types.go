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

// PromptVersion names the prompts, grammars, allowlists and request limits.
// v1 through v5 pins are refused; their stored decisions remain readable.
const PromptVersion = "moderation-v6"

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

// ReasonCodes is the complete vocabulary of stored decisions, including codes
// that only Go may add. Use ModelReasonCodes for what a model may assert.
func ReasonCodes() []string {
	return []string{"SPAM_TEMPLATE", "SCAM_TEMPLATE", "DUPLICATE_TEXT", "DUPLICATE_IMAGE", "PRICE_OUTLIER", "INCOMPLETE_LISTING", "INVALID_FIELDS", "ORDER_STATE_VIOLATION", "TIMER_DUE", "ABUSE_RATE", "INJECTION_SUSPECTED", "UNICODE_MIXED_SCRIPT", "ZERO_WIDTH_OBFUSCATION", "OFF_PLATFORM_CONTACT", "INPUT_TRUNCATED", "DETERMINISTIC_CONFLICT", "LOW_CONFIDENCE", "NEW_ACTOR", "MONEY_INVOLVED", "NOVEL_CASE", "MODEL_ABSENT", "MODEL_NOT_USED", "MODEL_FAILURE", "INVALID_OUTPUT", "CANARY_MISMATCH", "NO_FINDING", "HUMAN_REVIEW", "POLICY_BOUNDARY", "SLA_EXPIRED", "EXTERNAL_DESTINATION"}
}

// ModelReasonCodes is the final-proposal allowlist: content observations a
// model can ground in what it is shown, plus NO_FINDING (only alone) and
// HUMAN_REVIEW. Deterministic, routing, failure, protocol and deadline codes are
// added by Go alone, so a model cannot forge an audit record's provenance.
func ModelReasonCodes() []string {
	return []string{"SPAM_TEMPLATE", "SCAM_TEMPLATE", "OFF_PLATFORM_CONTACT", "INJECTION_SUSPECTED", "DETERMINISTIC_CONFLICT", "NO_FINDING", "HUMAN_REVIEW"}
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

// Reputation is reserved and no model sees it. The market never sets it
// (TestNoMarketCodeSetsActorReputation), so every market job takes Route's
// unrated-actor weight, and the field stays in the canonical input encoding so
// input hashes keep their form. A Go reputation rule is a separate change.
type Actor struct {
	ID         string  `json:"id"`
	AgeDays    int     `json:"age_days"`
	Reputation float64 `json:"reputation"`
}

// modelReadsKind reports whether an item kind may reach a model. By owner
// directive the model never judges listings by text: listing and upload items
// are evaluated by deterministic checks only and record MODEL_NOT_USED. Kinds
// the market does not queue (message, order) never reach it either.
func modelReadsKind(kind string) bool {
	return kind == "review" || kind == "dispute" || kind == "payment_flag"
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
// token, or proof that a prompt injection was contained. The grammar forces it
// into every completion, so it is ten ASCII characters: at most ten tokens.
func CanaryFor(inputHash string) string {
	h := sha256.Sum256([]byte("moderation-canary-v3:" + inputHash))
	return "m-" + hex.EncodeToString(h[:4])
}
