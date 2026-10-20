package moderation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	MaxFields        = 16
	MaxFieldRunes    = 4096
	MaxTotalRunes    = 8192
	MaxRawInputBytes = 64 * 1024
	MaxEvidenceIDs   = 32
)

var safeLabel = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,64}$`)
var safeReference = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func appendUnique(dst []string, value string) []string {
	if !slices.Contains(dst, value) {
		return append(dst, value)
	}
	return dst
}

// Normalize caps allocation before Unicode normalization and retains a digest of
// the complete original input so different truncated tails cannot share a job.
// Its metadata is fixed vocabulary rather than quoting an attacker's commands.
func Normalize(input Input) (NormalizedInput, error) {
	out := NormalizedInput{Input: input, InjectionEvidence: []string{}, NormalizationWarnings: []string{}}
	if !safeReference.MatchString(input.ItemID) || !safeReference.MatchString(input.Actor.ID) {
		return out, errors.New("invalid moderation reference")
	}
	if !slices.Contains([]string{"listing", "message", "dispute", "upload", "order", "review", "payment_flag"}, input.Kind) {
		return out, errors.New("invalid moderation kind")
	}
	if len(input.Fields) > MaxFields || len(input.Context.EvidenceIDs) > MaxEvidenceIDs || input.Actor.AgeDays < 0 || math.IsNaN(input.Actor.Reputation) || math.IsInf(input.Actor.Reputation, 0) || input.Actor.Reputation < 0 || input.Actor.Reputation > 1 {
		return out, errors.New("invalid moderation bounds")
	}
	if input.Context.PriceAtomic < 0 || input.Context.CategoryMedianAtomic < 0 || input.Context.AbuseCount < 0 || input.Context.BlastRadius < 0 || input.Context.BlastRadius > 100 {
		return out, errors.New("invalid deterministic context")
	}
	states := []string{"", "draft", "awaiting_payment", "paid", "shipped", "delivered", "completed", "disputed", "resolved", "cancelled"}
	if !slices.Contains(states, input.Context.OrderState) || !slices.Contains(states, input.Context.ProposedOrderState) || !slices.Contains([]string{"", "auto_release", "auto_refund", "auto_expiry"}, input.Context.TimerKind) {
		return out, errors.New("invalid deterministic state")
	}
	for _, hashes := range [][]string{input.Context.KnownTextHashes, input.Context.ImageHashes, input.Context.KnownImageHashes} {
		for _, h := range hashes {
			decoded, err := hex.DecodeString(h)
			if err != nil || (len(decoded) != 8 && len(decoded) != 32) {
				return out, errors.New("invalid moderation hash")
			}
		}
	}
	for _, ids := range [][]string{input.Context.EvidenceIDs, input.Context.KnownTextHashes, input.Context.ImageHashes, input.Context.KnownImageHashes} {
		if len(ids) > MaxEvidenceIDs {
			return out, errors.New("too many moderation references")
		}
		for _, id := range ids {
			if !safeReference.MatchString(id) {
				return out, errors.New("invalid moderation evidence")
			}
		}
	}
	// Check raw sizes before marshaling or NFKC expansion. Oversized user text is
	// handled by the worker as a failed snapshot for human review, never a penalty.
	rawBytes := 0
	for _, f := range input.Fields {
		if !safeLabel.MatchString(f.Source) || !safeLabel.MatchString(f.Name) || f.Trust != "DATA" || !utf8.ValidString(f.Value) {
			return out, errors.New("invalid moderation field")
		}
		rawBytes += len(f.Value)
		if rawBytes > MaxRawInputBytes {
			return out, errors.New("moderation input exceeds hard limit")
		}
	}
	original, err := json.Marshal(input)
	if err != nil {
		return out, errors.New("invalid moderation input")
	}
	// Deep-copy each mutable slice. Normalization never changes the caller's DB
	// snapshot, which is also the basis of the post-model freshness check.
	out.Fields = append([]Field{}, input.Fields...)
	out.Context.EvidenceIDs = append([]string{}, input.Context.EvidenceIDs...)
	out.Context.KnownTextHashes = append([]string{}, input.Context.KnownTextHashes...)
	out.Context.ImageHashes = append([]string{}, input.Context.ImageHashes...)
	out.Context.KnownImageHashes = append([]string{}, input.Context.KnownImageHashes...)
	seen := map[string]bool{}
	total := 0
	for i := range out.Fields {
		f := &out.Fields[i]
		label := f.Source + "." + f.Name
		if seen[label] {
			return out, errors.New("duplicate moderation field")
		}
		seen[label] = true
		for _, finding := range injectionPatterns(f.Value) {
			out.InjectionEvidence = appendUnique(out.InjectionEvidence, label+":"+finding)
		}
		var b strings.Builder
		for _, r := range norm.NFKC.String(f.Value) {
			if unicode.Is(unicode.Cf, r) || r == '\u034f' || r == '\u180e' || r == '\ufe0f' || r == '\ufe0e' {
				out.NormalizationWarnings = appendUnique(out.NormalizationWarnings, "INVISIBLE_CHARACTERS_REMOVED")
				continue
			}
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				out.NormalizationWarnings = appendUnique(out.NormalizationWarnings, "CONTROL_CHARACTERS_REMOVED")
				continue
			}
			b.WriteRune(r)
		}
		value := strings.TrimSpace(b.String())
		for _, finding := range injectionPatterns(value) {
			out.InjectionEvidence = appendUnique(out.InjectionEvidence, label+":"+finding)
		}
		if mixedScripts(value) {
			out.NormalizationWarnings = appendUnique(out.NormalizationWarnings, "UNICODE_MIXED_SCRIPT")
			out.InjectionEvidence = appendUnique(out.InjectionEvidence, label+":mixed_script")
		}
		runes := []rune(value)
		limit := min(MaxFieldRunes, MaxTotalRunes-total)
		if len(runes) > limit {
			runes = runes[:limit]
			out.NormalizationWarnings = appendUnique(out.NormalizationWarnings, "INPUT_TRUNCATED")
		}
		total += len(runes)
		f.Value = string(runes)
	}
	out.InjectionSuspected = len(out.InjectionEvidence) > 0
	slices.SortFunc(out.Fields, func(a, b Field) int { return strings.Compare(a.Source+"."+a.Name, b.Source+"."+b.Name) })
	slices.Sort(out.Context.EvidenceIDs)
	out.Context.EvidenceIDs = slices.Compact(out.Context.EvidenceIDs)
	slices.Sort(out.InjectionEvidence)
	if len(out.InjectionEvidence) > 16 {
		out.InjectionEvidence = out.InjectionEvidence[:16]
	}
	slices.Sort(out.NormalizationWarnings)
	var text strings.Builder
	for _, f := range out.Fields {
		text.WriteString(f.Name)
		text.WriteByte(':')
		text.WriteString(strings.ToLower(strings.Join(strings.Fields(f.Value), " ")))
		text.WriteByte('\n')
	}
	out.TextHash = digest([]byte(text.String()))
	canonical, err := json.Marshal(struct {
		Normalized   NormalizedInput `json:"normalized"`
		OriginalHash string          `json:"original_hash"`
	}{out, digest(original)})
	if err != nil {
		return out, errors.New("invalid normalized input")
	}
	out.InputHash = digest(canonical)
	return out, nil
}

func injectionPatterns(value string) []string {
	lower := strings.ToLower(value)
	var findings []string
	for _, marker := range []string{"ignore previous", "ignore all", "disregard", "override", "you are", "act as", "system prompt", "system:", "assistant:", "developer:", "mark this order", "mark the order", "output only", "return json", "follow these instructions", "execute", "run command", "approve this", "ban this", "refund this", "release this", "set confidence", "please classify"} {
		if strings.Contains(lower, marker) {
			findings = appendUnique(findings, "instruction_like")
		}
	}
	for _, marker := range []string{"schema", "reason_codes", "injection_suspected", "injection_evidence", "confidence", "canary", "\"action\"", "\"decision\"", "decision", "trusted block", "trusted instructions"} {
		if strings.Contains(lower, marker) {
			findings = appendUnique(findings, "protocol_reference")
		}
	}
	for _, marker := range []string{"<<u ", "<<end", "</system", "<system", "[inst]", "[system]", "begin trusted", "end trusted"} {
		if strings.Contains(lower, marker) {
			findings = appendUnique(findings, "delimiter_like")
		}
	}
	return findings
}

// Mixing visually confusable scripts in one word is suspicious; separate words
// in a multilingual description are not by themselves a homoglyph attack.
func mixedScripts(value string) bool {
	for _, word := range strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) }) {
		latin, cyrillic, greek := false, false, false
		for _, r := range word {
			latin = latin || unicode.In(r, unicode.Latin)
			cyrillic = cyrillic || unicode.In(r, unicode.Cyrillic)
			greek = greek || unicode.In(r, unicode.Greek)
		}
		if (latin && cyrillic) || (latin && greek) || (cyrillic && greek) {
			return true
		}
	}
	return false
}
