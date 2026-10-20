package moderation

import (
	"encoding/hex"
	"math/bits"
	"slices"
	"strings"
)

func baseProposal(in NormalizedInput) Proposal {
	return Proposal{ItemID: in.ItemID, Action: ActionNoAction, Confidence: 1, ReasonCodes: []string{}, InjectionEvidence: []string{}, EvidenceIDs: append([]string{}, in.Context.EvidenceIDs...), Summary: "No deterministic finding. Human review is required for contextual judgment.", Canary: CanaryFor(in.InputHash)}
}

// EvaluateDeterministic returns observations and whether they are sufficient to
// route without inference. A true result authorizes only recording metadata.
func EvaluateDeterministic(in NormalizedInput, cfg Config) (Proposal, bool) {
	p := baseProposal(in)
	add := func(code string) { p.ReasonCodes = appendUnique(p.ReasonCodes, code) }
	if in.InjectionSuspected {
		p.InjectionSuspected = true
		p.InjectionEvidence = append([]string{}, in.InjectionEvidence...)
		add("INJECTION_SUSPECTED")
		p.Confidence = .5
	}
	if slices.Contains(in.NormalizationWarnings, "INPUT_TRUNCATED") {
		add("INPUT_TRUNCATED")
		p.Confidence = min(p.Confidence, .5)
	}
	if slices.Contains(in.NormalizationWarnings, "UNICODE_MIXED_SCRIPT") {
		add("UNICODE_MIXED_SCRIPT")
		p.Confidence = min(p.Confidence, .5)
	}
	if slices.Contains(in.NormalizationWarnings, "INVISIBLE_CHARACTERS_REMOVED") || slices.Contains(in.NormalizationWarnings, "CONTROL_CHARACTERS_REMOVED") {
		add("INVALID_FIELDS")
		p.Confidence = min(p.Confidence, .5)
	}
	var all strings.Builder
	for _, f := range in.Fields {
		all.WriteString(strings.ToLower(f.Value))
		all.WriteByte('\n')
	}
	text := all.String()
	if in.Context.OrderState != "" {
		// Claims in evidence are observations, never state changes. A contradiction
		// lowers confidence while retaining the database's authoritative state.
		for _, state := range []string{"draft", "awaiting_payment", "paid", "shipped", "delivered", "completed", "disputed", "resolved", "cancelled"} {
			if state != in.Context.OrderState && (strings.Contains(text, "order is "+state) || strings.Contains(text, "order state: "+state) || strings.Contains(text, "\"order_state\":\""+state+"\"")) {
				add("DETERMINISTIC_CONFLICT")
				p.Confidence = min(p.Confidence, .5)
			}
		}
	}
	for _, template := range []string{"guaranteed profit", "send your seed", "send your recovery phrase", "pay outside", "pay me directly", "send your private key"} {
		if strings.Contains(text, template) {
			add("SCAM_TEMPLATE")
		}
	}
	if strings.Count(text, "http://")+strings.Count(text, "https://") >= 4 || strings.Contains(text, "buy now buy now buy now") || repeatedWord(text) {
		add("SPAM_TEMPLATE")
	}
	if slices.Contains(in.Context.KnownTextHashes, in.TextHash) {
		add("DUPLICATE_TEXT")
	}
	for _, h := range in.Context.ImageHashes {
		for _, known := range in.Context.KnownImageHashes {
			if ImageHashesMatch(h, known, 5) {
				add("DUPLICATE_IMAGE")
			}
		}
	}
	if in.Kind == "listing" {
		if !in.Context.Complete || len(in.Fields) == 0 || in.Context.PriceAtomic <= 0 {
			add("INCOMPLETE_LISTING")
		}
		// Division avoids multiplication overflow at the atomic-amount limit.
		price, median := in.Context.PriceAtomic, in.Context.CategoryMedianAtomic
		if price > 0 && median > 0 && (price/median >= 10 || median/price >= 10) {
			add("PRICE_OUTLIER")
		}
	}
	if in.Context.AbuseCount >= cfg.AbuseThreshold {
		add("ABUSE_RATE")
	}
	if in.Context.ProposedOrderState != "" {
		if !orderEdge(in.Context.OrderState, in.Context.ProposedOrderState) {
			add("ORDER_STATE_VIOLATION")
		} else if !in.Context.DeterministicInvariant {
			add("DETERMINISTIC_CONFLICT")
			p.Confidence = min(p.Confidence, .5)
		}
	}
	if in.Context.TimerDue {
		if slices.Contains([]string{"auto_release", "auto_refund", "auto_expiry"}, in.Context.TimerKind) {
			add("TIMER_DUE")
		} else {
			add("INVALID_FIELDS")
			p.Confidence = min(p.Confidence, .5)
		}
	}
	if len(p.ReasonCodes) == 0 {
		p.ReasonCodes = []string{"NO_FINDING"}
		return p, false
	}
	p.Action = ActionFlagReview
	p.Summary = "Deterministic checks recorded review findings. Marketplace state and funds are unchanged."
	slices.Sort(p.ReasonCodes)
	return p, true
}

func repeatedWord(value string) bool {
	last, run := "", 0
	for _, word := range strings.Fields(value) {
		if word == last {
			run++
		} else {
			last, run = word, 1
		}
		if run >= 8 && len(word) >= 3 {
			return true
		}
	}
	return false
}

// ImageHashesMatch accepts only exact SHA256 equality or a supplied 64-bit
// perceptual hash. It never decodes uploads or interprets SHA256 as perceptual.
func ImageHashesMatch(a, b string, maxDistance int) bool {
	if len(a) != len(b) || (len(a) != 16 && len(a) != 64) || maxDistance < 0 || maxDistance > 64 {
		return false
	}
	x, err := hex.DecodeString(a)
	if err != nil {
		return false
	}
	y, err := hex.DecodeString(b)
	if err != nil {
		return false
	}
	if len(a) == 64 {
		return slices.Equal(x, y)
	}
	distance := 0
	for i := range x {
		distance += bits.OnesCount8(x[i] ^ y[i])
	}
	return distance <= maxDistance
}

// This advisory edge check catches impossible observations. It cannot replace
// market.transition's transactional actor, kind, escrow and financial checks.
func orderEdge(from, to string) bool {
	edges := map[string][]string{
		"draft":            {"awaiting_payment", "cancelled"},
		"awaiting_payment": {"paid", "cancelled"},
		"paid":             {"shipped", "delivered", "cancelled", "disputed"},
		"shipped":          {"completed", "disputed"},
		"delivered":        {"completed", "disputed"},
		"disputed":         {"resolved", "completed", "cancelled"},
	}
	return slices.Contains(edges[from], to)
}
