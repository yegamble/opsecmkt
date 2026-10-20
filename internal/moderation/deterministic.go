package moderation

import (
	"encoding/hex"
	"math/bits"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
	if slices.Contains(in.NormalizationWarnings, "INVISIBLE_CHARACTERS_REMOVED") {
		// Format characters can be ordinary orthography. Record their removal
		// without claiming that it proves injection or authorizes punishment.
		add("ZERO_WIDTH_OBFUSCATION")
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
	for _, f := range in.Fields {
		if matchesReviewPhrase(f.Value, scamReviewPhrases) {
			add("SCAM_TEMPLATE")
		}
		if contextualOffPlatformSolicitation(f.Value) {
			add("OFF_PLATFORM_CONTACT")
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

var scamReviewPhrases = compileReviewPhrases(
	"guaranteed profit", "send your seed", "send your recovery phrase",
	"pay outside", "pay me directly", "send your private key",
)

var outsidePaymentPhrases = compileReviewPhrases(
	"pay outside escrow", "pay off platform", "pay offsite", "bypass escrow",
	"avoid escrow", "instead of escrow", "off escrow", "direct payment",
)

var contactSolicitationPhrases = compileReviewPhrases(
	"contact me", "message me", "reach me", "contact us", "message us",
	"buy directly", "pay me", "send payment",
)

var transactionSolicitationPhrases = compileReviewPhrases(
	"before payment", "before paying", "to pay", "to arrange payment", "to buy",
	"to purchase", "place an order", "avoid fees", "skip escrow", "discount",
	"direct payment",
)

var supportContextPhrases = compileReviewPhrases(
	"payment questions", "checkout questions", "technical support",
	"customer support", "support documentation", "through the market checkout",
	"through escrow", "using escrow",
)

// Destinations are inert review signals. They are never resolved, fetched or
// interpreted as endpoint configuration. A bare address/contact is insufficient.
var externalContactDestination = regexp.MustCompile(`(?i)(?:[a-z2-7]{56}\.onion\b|[a-z2-7]{52}\.b32\.i2p\b|(?:https?://)?(?:t\.me|telegram\.me)/[a-z0-9_]{5,32}\b|\btelegram[ \t:]+@[a-z0-9_]{5,32}\b|\b05[0-9a-f]{64}\b|simplex:/[^\s]+|https://simplex\.chat/contact[^\s]*|\b(?:xmpp|jabber)[ \t:]+[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b)`)
var paymentAddressDestination = regexp.MustCompile(`(?:^|[^A-Za-z0-9])[48][1-9A-HJ-NP-Za-km-z]{94}(?:$|[^A-Za-z0-9])`)

var reviewClauseBoundary = regexp.MustCompile(`[\n;]+|[.!?]+(?:[ \t]+|$)`)
var immediateReviewNegation = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:never|do[ \t-]+not|don['’]?t|must[ \t]+not|should[ \t]+not|avoid|refuse[ \t]+to|no[ \t]+one[ \t]+should)(?:[ \t]+ever)?[ \t:]*$`)

// compileReviewPhrases tolerates punctuation and a small fixed ASCII leetspeak
// alphabet while retaining Unicode word boundaries. It does not transliterate,
// discard non-Latin text, or rewrite the evidence. RE2 and the input cap bound
// matching work even when punctuation is repeated.
func compileReviewPhrases(phrases ...string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(phrases))
	for _, phrase := range phrases {
		var pattern strings.Builder
		pattern.WriteString(`(?i)`)
		first := true
		for _, r := range phrase {
			if r == ' ' {
				continue
			}
			if !first {
				pattern.WriteString(`[\p{P}\p{S}\p{Z}\s]*`)
			}
			first = false
			switch r {
			case 'a':
				pattern.WriteString(`[a4@]`)
			case 'e':
				pattern.WriteString(`[e3]`)
			case 'i':
				pattern.WriteString(`[i1!]`)
			case 'l':
				pattern.WriteString(`[l1|]`)
			case 'o':
				pattern.WriteString(`[o0]`)
			case 's':
				pattern.WriteString(`[s5$]`)
			case 't':
				pattern.WriteString(`[t7+]`)
			default:
				pattern.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		patterns = append(patterns, regexp.MustCompile(pattern.String()))
	}
	return patterns
}

func matchesReviewPhrase(value string, patterns []*regexp.Regexp) bool {
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringIndex(value, -1) {
			// Check boundaries without consuming them: adjacent matches must both
			// remain visible when the first one belongs to a negative instruction.
			if match[0] > 0 {
				previous, _ := utf8.DecodeLastRuneInString(value[:match[0]])
				if unicode.IsLetter(previous) || unicode.IsNumber(previous) {
					continue
				}
			}
			if match[1] < len(value) {
				next, _ := utf8.DecodeRuneInString(value[match[1]:])
				if unicode.IsLetter(next) || unicode.IsNumber(next) {
					continue
				}
			}
			// An immediate negative instruction is common in safety guidance.
			// Broader quotation/intent remains a human judgment, not a regex claim.
			prefix := value[max(0, match[0]-160):match[0]]
			if !immediateReviewNegation.MatchString(prefix) {
				return true
			}
		}
	}
	return false
}

func contextualOffPlatformSolicitation(value string) bool {
	for _, clause := range reviewClauseBoundary.Split(value, -1) {
		if !externalContactDestination.MatchString(clause) && !paymentAddressDestination.MatchString(clause) {
			continue
		}
		if matchesReviewPhrase(clause, outsidePaymentPhrases) {
			return true
		}
		if !matchesReviewPhrase(clause, supportContextPhrases) && matchesReviewPhrase(clause, contactSolicitationPhrases) && matchesReviewPhrase(clause, transactionSolicitationPhrases) {
			return true
		}
	}
	return false
}

// reviewOnlySignal reports a recognised external destination (onion, I2P,
// Telegram, Session, SimpleX, XMPP, Monero or checksum-valid Bitcoin address) or weak
// instruction-like wording in any field. Solicitation may sit in another
// sentence or field, so neither accuses nor skips the model, but either keeps
// the item out of automatic history.
func reviewOnlySignal(in NormalizedInput) bool {
	for _, f := range in.Fields {
		if externalContactDestination.MatchString(f.Value) || paymentAddressDestination.MatchString(f.Value) || bitcoinAddressDestination(f.Value) || weakInstructionMarker(f.Value) {
			return true
		}
	}
	return false
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
	if !InformativePerceptualHash(a) || !InformativePerceptualHash(b) {
		return false
	}
	// Each byte is one row of dHash bits, g(x) > g(x+1), most significant bit
	// first. A horizontal mirror reverses the row and complements each bit, so
	// the comparison also tries that form; stored hashes are never rewritten.
	distance, mirrored := 0, 0
	for i := range x {
		distance += bits.OnesCount8(x[i] ^ y[i])
		mirrored += bits.OnesCount8(x[i] ^ ^bits.Reverse8(y[i]))
	}
	return min(distance, mirrored) <= maxDistance
}

// InformativePerceptualHash excludes nearly uniform gradient fingerprints from
// similarity comparison. This inexpensive signal also handles older weak hashes
// without decoding stored images. An informative hash still does not prove a
// duplicate, and exact SHA256 comparison remains available independently.
func InformativePerceptualHash(value string) bool {
	if len(value) != 16 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	population := 0
	for _, b := range decoded {
		population += bits.OnesCount8(b)
	}
	return population >= 8 && population <= 56
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
