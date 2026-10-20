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

var proposalKeys = []string{"item_id", "action", "confidence", "reason_codes", "injection_suspected", "injection_evidence", "evidence_ids", "summary", "canary"}

// DecodeProposal rejects duplicate keys, missing/unknown fields, nulls and
// trailing data before applying semantic checks. A grammar is a sampling aid;
// this validator remains mandatory even with a trusted inference runtime.
func DecodeProposal(raw []byte, in NormalizedInput, canary string) (Proposal, error) {
	var p Proposal
	if err := strictObject(raw, proposalKeys, &p); err != nil {
		return p, err
	}
	if err := ValidateProposal(p, in, canary); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

func ValidateProposal(p Proposal, in NormalizedInput, canary string) error {
	if p.Canary != canary {
		return ErrCanaryMismatch
	}
	if p.ItemID != in.ItemID || !boundedText(p.ItemID, 128) || !slices.Contains(Actions(), p.Action) || math.IsNaN(p.Confidence) || math.IsInf(p.Confidence, 0) || p.Confidence < 0 || p.Confidence > 1 {
		return ErrInvalidOutput
	}
	if p.ReasonCodes == nil || len(p.ReasonCodes) == 0 || len(p.ReasonCodes) > 32 || !uniqueSubset(p.ReasonCodes, ReasonCodes()) {
		return ErrInvalidOutput
	}
	if p.InjectionEvidence == nil || len(p.InjectionEvidence) > 16 || p.EvidenceIDs == nil || len(p.EvidenceIDs) > 32 || !uniqueSubset(p.EvidenceIDs, in.Context.EvidenceIDs) {
		return ErrInvalidOutput
	}
	if p.InjectionSuspected != (len(p.InjectionEvidence) > 0) || p.InjectionSuspected != slices.Contains(p.ReasonCodes, "INJECTION_SUSPECTED") {
		return ErrInvalidOutput
	}
	for _, value := range p.InjectionEvidence {
		if !boundedText(value, 256) || strings.TrimSpace(value) == "" {
			return ErrInvalidOutput
		}
	}
	if !boundedText(p.Summary, 2048) || strings.TrimSpace(p.Summary) == "" {
		return ErrInvalidOutput
	}
	return nil
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
