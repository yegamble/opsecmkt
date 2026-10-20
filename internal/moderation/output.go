package moderation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidOutput    = errors.New("moderation: invalid model output")
	ErrCanaryMismatch   = errors.New("moderation: model canary mismatch")
	ErrSuspiciousOutput = errors.New("moderation: suspicious model output")
)

const maxModelOutput = 32 << 10

var proposalKeys = []string{"item_id", "action", "confidence", "reason_codes", "injection_suspected", "evidence_ids", "canary"}

// modelProposal is deliberately distinct from the persisted Proposal. No free
// prose or free-form evidence field exists on the inference wire protocol.
type modelProposal struct {
	ItemID             string   `json:"item_id"`
	Action             string   `json:"action"`
	Confidence         float64  `json:"confidence"`
	ReasonCodes        []string `json:"reason_codes"`
	InjectionSuspected bool     `json:"injection_suspected"`
	EvidenceIDs        []string `json:"evidence_ids"`
	Canary             string   `json:"canary"`
}

// DecodeProposal rejects duplicate keys, missing/unknown fields, nulls and
// trailing data before applying semantic checks. A grammar is a sampling aid;
// this validator remains mandatory even with a trusted inference runtime.
func DecodeProposal(raw []byte, in NormalizedInput, canary string) (Proposal, error) {
	var wire modelProposal
	if err := strictObject(raw, proposalKeys, &wire); err != nil {
		return Proposal{}, err
	}
	p := Proposal{ItemID: wire.ItemID, Action: wire.Action, Confidence: wire.Confidence, ReasonCodes: wire.ReasonCodes, InjectionSuspected: wire.InjectionSuspected, EvidenceIDs: wire.EvidenceIDs, Canary: wire.Canary}
	if err := ValidateProposal(p, in, canary); err != nil {
		return Proposal{}, err
	}
	return renderTrustedProposal(p, in), nil
}

// ValidateProposal checks only the closed model contract. Compatibility display
// fields are deliberately ignored and must be replaced before hashing or use.
func ValidateProposal(p Proposal, in NormalizedInput, canary string) error {
	if p.Canary != canary {
		return ErrCanaryMismatch
	}
	if p.ItemID != in.ItemID || !boundedText(p.ItemID, 128) || !slices.Contains(ModelActions(), p.Action) || math.IsNaN(p.Confidence) || math.IsInf(p.Confidence, 0) || p.Confidence < 0 || p.Confidence > 1 {
		return ErrInvalidOutput
	}
	if p.ReasonCodes == nil || len(p.ReasonCodes) == 0 || len(p.ReasonCodes) > 32 || !uniqueSubset(p.ReasonCodes, ReasonCodes()) {
		return ErrInvalidOutput
	}
	if p.EvidenceIDs == nil || len(p.EvidenceIDs) > 32 || !uniqueSubset(p.EvidenceIDs, in.Context.EvidenceIDs) {
		return ErrInvalidOutput
	}
	if p.InjectionSuspected != slices.Contains(p.ReasonCodes, "INJECTION_SUSPECTED") {
		return ErrInvalidOutput
	}
	return nil
}

var reasonDescriptions = map[string]string{
	"SPAM_TEMPLATE":          "a spam-pattern signal",
	"SCAM_TEMPLATE":          "a scam-pattern signal",
	"DUPLICATE_TEXT":         "similar listing text",
	"DUPLICATE_IMAGE":        "similar image hashes",
	"PRICE_OUTLIER":          "a price outside the comparison range",
	"INCOMPLETE_LISTING":     "incomplete listing information",
	"INVALID_FIELDS":         "invalid or unsupported input fields",
	"ORDER_STATE_VIOLATION":  "an invalid proposed order transition",
	"TIMER_DUE":              "a due timer observation",
	"ABUSE_RATE":             "an elevated activity counter",
	"INJECTION_SUSPECTED":    "a possible instruction-injection attempt",
	"UNICODE_MIXED_SCRIPT":   "mixed-script text",
	"ZERO_WIDTH_OBFUSCATION": "removed invisible characters",
	"OFF_PLATFORM_CONTACT":   "a possible off-platform solicitation",
	"INPUT_TRUNCATED":        "truncated input",
	"DETERMINISTIC_CONFLICT": "a conflict with deterministic context",
	"LOW_CONFIDENCE":         "insufficient confidence",
	"NEW_ACTOR":              "a recently created account",
	"MONEY_INVOLVED":         "financial involvement",
	"NOVEL_CASE":             "an unfamiliar case",
	"MODEL_ABSENT":           "contextual classification unavailable",
	"MODEL_FAILURE":          "contextual classification failed",
	"INVALID_OUTPUT":         "an invalid classification response",
	"CANARY_MISMATCH":        "a response protocol mismatch",
	"NO_FINDING":             "no additional review signal",
	"HUMAN_REVIEW":           "human review required",
	"POLICY_BOUNDARY":        "an action outside automatic authority",
	"SLA_EXPIRED":            "an expired review deadline",
}

// SummaryForReasons renders only fixed application-owned phrases. Unknown
// identifiers are never interpolated. This is also safe for historical rows.
func SummaryForReasons(reasons []string) string {
	codes := append([]string{}, reasons...)
	slices.Sort(codes)
	codes = slices.Compact(codes)
	var labels []string
	for _, code := range codes {
		if description, known := reasonDescriptions[code]; known {
			labels = append(labels, description)
		}
	}
	if len(labels) == 0 {
		labels = []string{"human review required"}
	}
	return "Review signals: " + strings.Join(labels, "; ") + ". No marketplace state or funds changed."
}

// RenderProposalDisplay replaces historical stored prose for presentation only.
// It does not validate, authorize, rewrite or re-hash the original audit row.
// Without the original trusted source snapshot it emits fixed code evidence.
func RenderProposalDisplay(p Proposal) Proposal {
	return renderTrustedProposal(p, NormalizedInput{})
}

// RenderProposalForInput preserves fixed field-detection codes for a new result.
// It never forwards a field value or previously generated evidence prose.
func RenderProposalForInput(p Proposal, in Input) Proposal {
	normalized, err := Normalize(in)
	if err != nil {
		return RenderProposalDisplay(p)
	}
	return renderTrustedProposal(p, normalized)
}

// RenderStoredProposalDisplay accepts only the finite field/code vocabulary
// written by v2. Historical free-form evidence must use RenderProposalDisplay.
func RenderStoredProposalDisplay(p Proposal) Proposal {
	stored := p.InjectionEvidence
	p = RenderProposalDisplay(p)
	if !p.InjectionSuspected {
		return p
	}
	var evidence []string
	for _, value := range stored[:min(len(stored), 16)] {
		field, code, ok := strings.Cut(value, ":")
		if !ok || !slices.Contains([]string{"instruction_like", "protocol_reference", "delimiter_like", "mixed_script"}, code) {
			continue
		}
		switch field {
		case "listing.title", "listing.description", "listing.category", "listing.region", "listing.kind",
			"upload.description", "dispute.reason", "dispute.resolution", "review.body", "review.rating", "payment_flag.disposition":
			evidence = appendUnique(evidence, value)
		}
	}
	if len(evidence) > 0 {
		if slices.Contains(p.ReasonCodes, "CANARY_MISMATCH") {
			evidence = appendUnique(evidence, "model:CANARY_MISMATCH")
		}
		slices.Sort(evidence)
		p.InjectionEvidence = evidence
	}
	return p
}

func renderTrustedProposal(p Proposal, in NormalizedInput) Proposal {
	p.ReasonCodes = append([]string{}, p.ReasonCodes...)
	slices.Sort(p.ReasonCodes)
	p.ReasonCodes = slices.Compact(p.ReasonCodes)
	p.EvidenceIDs = append([]string{}, p.EvidenceIDs...)
	slices.Sort(p.EvidenceIDs)
	p.EvidenceIDs = slices.Compact(p.EvidenceIDs)
	p.Summary = SummaryForReasons(p.ReasonCodes)
	// Discard even structurally valid prose supplied by custom Model adapters.
	p.InjectionEvidence = []string{}
	if !p.InjectionSuspected {
		return p
	}
	for _, field := range in.Fields {
		if !safeLabel.MatchString(field.Source) || !safeLabel.MatchString(field.Name) {
			continue
		}
		for _, suffix := range []string{"instruction_like", "protocol_reference", "delimiter_like", "mixed_script"} {
			finding := field.Source + "." + field.Name + ":" + suffix
			if slices.Contains(in.InjectionEvidence, finding) {
				p.InjectionEvidence = appendUnique(p.InjectionEvidence, finding)
			}
		}
	}
	if slices.Contains(p.ReasonCodes, "CANARY_MISMATCH") {
		p.InjectionEvidence = appendUnique(p.InjectionEvidence, "model:CANARY_MISMATCH")
	}
	if len(p.InjectionEvidence) == 0 {
		p.InjectionEvidence = append(p.InjectionEvidence, "signal:INJECTION_SUSPECTED")
	}
	for _, reference := range p.EvidenceIDs {
		if safeReference.MatchString(reference) && slices.Contains(in.Context.EvidenceIDs, reference) {
			p.InjectionEvidence = appendUnique(p.InjectionEvidence, "evidence:"+reference)
		}
	}
	slices.Sort(p.InjectionEvidence)
	if len(p.InjectionEvidence) > 16 {
		p.InjectionEvidence = p.InjectionEvidence[:16]
	}
	return p
}

func uniqueSubset(values, allowed []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] || !slices.Contains(allowed, value) {
			return false
		}
		seen[value] = true
	}
	return true
}

func boundedText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

// strictObject is intentionally shared with the smaller Stage A schema.
func strictObject(raw []byte, required []string, dst any) error {
	if len(raw) == 0 || len(raw) > maxModelOutput || !utf8.Valid(raw) {
		return ErrInvalidOutput
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return ErrInvalidOutput
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil || len(fields) != len(required) {
		return ErrInvalidOutput
	}
	for _, key := range required {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalidOutput
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return ErrInvalidOutput
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrInvalidOutput
	}
	return nil
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := walkJSON(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalidOutput
	}
	return nil
}

func walkJSON(dec *json.Decoder, depth int) error {
	if depth > 8 {
		return ErrInvalidOutput
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrInvalidOutput
			}
			seen[name] = true
			if err := walkJSON(dec, depth+1); err != nil {
				return err
			}
		}
		close, err := dec.Token()
		if err != nil || close != json.Delim('}') {
			return ErrInvalidOutput
		}
	case '[':
		for dec.More() {
			if err := walkJSON(dec, depth+1); err != nil {
				return err
			}
		}
		close, err := dec.Token()
		if err != nil || close != json.Delim(']') {
			return ErrInvalidOutput
		}
	default:
		return ErrInvalidOutput
	}
	return nil
}
