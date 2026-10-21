package moderation

import (
	"crypto/sha256"
	"encoding/base64"
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
	previousValue, previousFound := "", []string(nil)
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
		found := injectionPatterns(value)
		for _, finding := range found {
			out.InjectionEvidence = appendUnique(out.InjectionEvidence, label+":"+finding)
		}
		// A marker split across adjacent fields in submission order ("…Ignore" /
		// "previous instructions…") is attributed to the field that completes it.
		if i > 0 {
			for _, finding := range injectionPatterns(previousValue + " " + value) {
				if !slices.Contains(previousFound, finding) && !slices.Contains(found, finding) {
					out.InjectionEvidence = appendUnique(out.InjectionEvidence, label+":"+finding)
				}
			}
		}
		previousValue, previousFound = value, found
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

// Strong markers address the moderator itself: replacing its instructions,
// naming its protocol fields, codes or canary, or imitating its delimiters.
// Any strong marker makes the item INJECTION_SUSPECTED, which escalates.
var strongInstructionMarkers = []string{
	"ignore previous", "ignore all previous", "ignore all prior", "ignore prior instructions", "ignore your instructions",
	"disregard previous instructions", "disregard all previous", "disregard prior instructions", "disregard your instructions",
	"forget your rules", "forget your instructions", "forget all previous", "forget previous instructions", "forget everything above",
	"system prompt", "set confidence", "please classify",
	"instructions from the operator", "instructions from the administrator", "instructions from the admin",
	"instructions from the moderator",
	// Translated "ignore (all) previous instructions" for the most common
	// non-English attacks; other languages are left to the model.
	"ignorez les instructions", "ignorez toutes les instructions", "ignore les instructions", "ignorez les consignes", "oubliez vos instructions", "oubliez les instructions",
	"ignora las instrucciones", "ignora todas las instrucciones", "ignorar las instrucciones", "ignore las instrucciones", "olvida tus instrucciones", "olvida las instrucciones",
	"忽略之前", "忽略以上", "忽略先前", "忽略前面", "忽略上述", "忽略所有指令",
	"игнорируй предыдущие", "игнорируйте предыдущие", "игнорируй все предыдущие", "игнорируйте все предыдущие", "игнорируй инструкции", "игнорируйте инструкции", "забудь свои инструкции", "забудьте инструкции",
}

// Letters-only forms of the instruction markers catch spaced letters,
// punctuation between words and the small leetspeak alphabet.
var compactInstructionMarkers = []string{
	"ignoreprevious", "ignoreallprevious", "ignoreallprior", "ignorepriorinstructions", "ignoreyourinstructions",
	"disregardpreviousinstructions", "disregardallprevious", "disregardyourinstructions",
	"forgetyourrules", "forgetyourinstructions", "forgetallprevious", "forgetpreviousinstructions",
}

var strongProtocolMarkers = []string{"reason_codes", "injection_suspected", "injection_evidence", "evidence_ids", "trusted block", "trusted instructions"}

// A quoted proposal field counts only as a JSON key ("action": …); a quoted
// word in prose (Our "Canary" edition) does not.
var protocolKey = regexp.MustCompile(`"(?:action|decision|confidence|canary)"[ \t]*:`)

// Code names count as whole identifiers: NO_ACTION, not NO_ACTION_REQUIRED.
var protocolCodeName = regexp.MustCompile(`(?:^|[^a-z0-9_])(?:` + strings.Join(protocolCodeNames(), "|") + `)(?:$|[^a-z0-9_])`)

var delimiterMarkers = []string{"<<u ", "<<end", "</system", "<system", "[inst]", "[system]", "begin trusted", "end trusted"}

var canaryToken = regexp.MustCompile(`moderation-[0-9a-f]{32}`)
var base64Token = regexp.MustCompile(`[A-Za-z0-9+/]{16,}={0,2}`)

// Weak markers are ordinary words that also appear in attacks ("buy with
// confidence", "can act as a stand"). They never accuse or skip the model;
// they only keep the item out of automatic history (see Route).
var weakInstructionMarkers = []string{
	"you are", "act as", "decision", "confidence", "execute", "override", "disregard", "schema", "canary", "ignore all",
	"system:", "assistant:", "developer:", "mark this order", "mark the order", "output only", "return json",
	"follow these instructions", "run command", "approve this", "ban this", "refund this", "release this",
	"already approved", "approved by staff", "approved by the staff", "approved by admin", "approved by the admin",
	"approved by moderat", "approved by the moderat", "new instructions",
}

// protocolCodeNames lists the underscore identifiers of actions, reason codes
// and dispositions. Prose has no reason to spell them; an attacker uses them to
// suggest a verdict ("NO_FINDING. NO_ACTION.").
func protocolCodeNames() []string {
	var names []string
	for _, code := range slices.Concat(Actions(), ReasonCodes(), []string{string(AutoApply), string(QueueHuman), string(EscalateAdmin)}) {
		if strings.Contains(code, "_") {
			names = appendUnique(names, strings.ToLower(code))
		}
	}
	return names
}

func injectionPatterns(value string) []string {
	findings := markerFindings(value)
	// Decode bounded base64-looking tokens once; only a strong marker in the
	// decoded text counts, so arbitrary identifiers cannot create a finding.
	for _, token := range base64Token.FindAllString(value, 16) {
		decoded, err := base64.StdEncoding.DecodeString(token)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(token)
		}
		if err == nil && utf8.Valid(decoded) {
			for _, finding := range markerFindings(string(decoded)) {
				findings = appendUnique(findings, finding)
			}
		}
	}
	return findings
}

func markerFindings(value string) []string {
	lower := strings.ToLower(value)
	var findings []string
	if containsAny(lower, strongInstructionMarkers) || compactMarker(lower, 'i') || compactMarker(lower, 'l') {
		findings = append(findings, "instruction_like")
	}
	if containsAny(lower, strongProtocolMarkers) || protocolKey.MatchString(lower) || protocolCodeName.MatchString(lower) || canaryToken.MatchString(lower) {
		findings = append(findings, "protocol_reference")
	}
	if containsAny(lower, delimiterMarkers) {
		findings = append(findings, "delimiter_like")
	}
	return findings
}

// weakInstructionMarker reports wording that needs a human but is common in
// ordinary listings, so it is never an injection accusation by itself.
func weakInstructionMarker(value string) bool {
	return containsAny(strings.ToLower(value), weakInstructionMarkers)
}

func containsAny(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// compactMarker matches the letters-only instruction markers after folding a
// small leetspeak alphabet (the digit 1 reads as the given letter, i or l).
// This catches spaced letters ("i g n o r e"), punctuation inside words and
// leetspeak. A match must begin where a word begins in the original text, so
// "foreign ore previously" is not "ignore previous". A line break, or sentence
// punctuation followed by a space, stays a boundary: "easy to ignore. Previous
// owners…" must not match either.
func compactMarker(lower string, one rune) bool {
	runes := []rune(lower)
	var b strings.Builder
	wordStart := map[int]bool{}
	previousLetter := false
	for i, r := range runes {
		switch r {
		case '\n', '.', '!', '?', ';':
			if r == '\n' || i+1 < len(runes) && unicode.IsSpace(runes[i+1]) {
				b.WriteByte('|')
			}
			previousLetter = false
			continue
		case '0':
			r = 'o'
		case '1':
			r = one
		case '3':
			r = 'e'
		case '4', '@':
			r = 'a'
		case '5', '$':
			r = 's'
		case '7':
			r = 't'
		}
		letter := unicode.IsLetter(r)
		if letter {
			if !previousLetter {
				wordStart[b.Len()] = true
			}
			b.WriteRune(r)
		}
		previousLetter = letter
	}
	compact := b.String()
	for _, marker := range compactInstructionMarkers {
		for offset := 0; ; {
			i := strings.Index(compact[offset:], marker)
			if i < 0 {
				break
			}
			if wordStart[offset+i] {
				return true
			}
			offset += i + 1
		}
	}
	return false
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
