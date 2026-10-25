package moderation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatCharactersAlreadyRemovedAndRecorded(t *testing.T) {
	for _, invisible := range []rune{'\u200b', '\u200c', '\u200d', '\u00ad', '\u202e', '\u2060', '\ufeff'} {
		t.Run(fmt.Sprintf("U+%04X", invisible), func(t *testing.T) {
			input := ordinaryInput()
			input.Fields[1].Value = "s" + string(invisible) + "end your seed"
			n, err := Normalize(input)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range n.Fields {
				if strings.ContainsRune(f.Value, invisible) {
					t.Fatal("Unicode format character survived normalization")
				}
			}
			p, decided := EvaluateDeterministic(n, DefaultConfig())
			if !decided || !slices.Contains(p.ReasonCodes, "SCAM_TEMPLATE") || !slices.Contains(p.ReasonCodes, "ZERO_WIDTH_OBFUSCATION") {
				t.Fatalf("obfuscated phrase did not retain its observations: %+v", p)
			}
		})
	}
	// A joiner in ordinary Persian is an observation requiring review, not an
	// accusation of injection, scam, or permission to punish its author.
	input := ordinaryInput()
	input.Fields[1].Value = "دفتر دست‌ساز برای یادداشت روزانه"
	n, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := EvaluateDeterministic(n, DefaultConfig())
	if p.InjectionSuspected || slices.Contains(p.ReasonCodes, "SCAM_TEMPLATE") || slices.Contains(p.ReasonCodes, "OFF_PLATFORM_CONTACT") {
		t.Fatalf("ordinary multilingual text was accused: %+v", p)
	}
	disposition, _, _ := Route(n, p, DefaultConfig())
	if disposition != QueueHuman || p.Action != ActionFlagReview {
		t.Fatalf("format-character observation exceeded review authority: %s %+v", disposition, p)
	}
}

func TestCanonicalScamSignalsSkipInference(t *testing.T) {
	for _, text := range []string{
		"s.e.n.d y0ur s3ed",
		"S3ND your recovery phr4se",
		"p4y 0uts1de escrow",
		"pay me d1rectly",
		"send your pr1vate k3y",
		"send.your.seed",
		"g.u.a.r.a.n.t.e.e.d pr0fit",
		"s\u200bend your seed",
		"never send your seed send your seed",
	} {
		t.Run(text, func(t *testing.T) {
			input := ordinaryInput()
			input.Fields[1].Value = text
			cfg := DefaultConfig()
			cfg.Enabled = true
			d, err := (Engine{Config: cfg, Model: modelFunc(func(context.Context, NormalizedInput) (Proposal, error) {
				t.Fatal("a deterministic scam observation reached inference")
				return Proposal{}, nil
			})}).Evaluate(context.Background(), input)
			if err != nil || !slices.Contains(d.ReasonCodes, "SCAM_TEMPLATE") || d.Proposal.Action != ActionFlagReview {
				t.Fatalf("missing review observation: %+v %v", d, err)
			}
		})
	}
}

func TestOffPlatformSignalsRequireSolicitationContext(t *testing.T) {
	onion := strings.Repeat("a", 56) + ".onion"
	i2p := strings.Repeat("b", 52) + ".b32.i2p"
	address := "4" + strings.Repeat("A", 94)
	session := "05" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"onion off-escrow pitch", "Contact me at " + onion + " to pay outside escrow.", true},
		{"i2p fee bypass", "Buy directly at " + i2p + " to avoid escrow.", true},
		{"wallet solicitation", "Send payment directly to " + address + " instead of escrow.", true},
		{"telegram prepayment", "Message me on Telegram @notebook_vendor before paying.", true},
		{"session purchase lure", "Contact me on Session " + session + " to purchase with a discount.", true},
		{"onion documentation", "Tor Browser documentation and an example onion address: " + onion, false},
		{"wallet product", "Monero hardware-wallet reference; example standard address " + address, false},
		{"support contacts", "Our Session support ID is " + session + ". The listing describes a notebook.", false},
		{"official checkout", "Pay using Monero through the market checkout. Support documentation: " + onion, false},
		{"ordinary payment support", "Contact us on Telegram @notebook_vendor for payment questions before paying.", false},
		{"official checkout contact", "Contact us at " + onion + " to pay through the market checkout.", false},
		{"negative instruction", "Do not pay outside escrow. Report unsolicited messages linking " + onion, false},
		{"quoted safety phrase", "Never send your seed or private key to anyone. Tor is a browser network.", false},
		{"obfuscated safety phrase", "Do not s.e.n.d your s3ed. Keep it offline.", false},
		{"unrelated word boundaries", "The notebook has a private keyboard illustration and a seeded-paper cover.", false},
		{"multilingual preserved", "دفتر دست ساز。日本語のノート。Monero and Tor documentation are included.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := ordinaryInput()
			input.Fields[1].Value = tc.text
			n, err := Normalize(input)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := EvaluateDeterministic(n, DefaultConfig())
			if got := slices.Contains(p.ReasonCodes, "OFF_PLATFORM_CONTACT"); got != tc.want {
				t.Fatalf("contextual contact signal=%v, want=%v: %+v", got, tc.want, p)
			}
			if !tc.want && slices.Contains(p.ReasonCodes, "SCAM_TEMPLATE") {
				t.Fatalf("benign support or safety text became a scam accusation: %+v", p)
			}
			if p.Action != ActionNoAction && p.Action != ActionFlagReview {
				t.Fatal("a content signal acquired punitive authority")
			}
			if !tc.want && !strings.Contains(tc.text, "\u200c") {
				// The matcher does not rewrite multilingual text into ASCII tokens.
				for _, f := range n.Fields {
					if f.Name == "description" && f.Value != tc.text {
						t.Fatalf("matching destroyed normalized language content: %q", f.Value)
					}
				}
			}
		})
	}
}

func TestReducedInputBudgetPreservesDigestAndCaller(t *testing.T) {
	if MaxTotalRunes != 8192 || MaxRawInputBytes != 64*1024 {
		t.Fatal("unexpected moderation resource budget")
	}
	input := ordinaryInput()
	input.Fields = []Field{}
	for i := range 4 {
		input.Fields = append(input.Fields, Field{Source: "listing", Name: fmt.Sprintf("field%d", i), Trust: "DATA", Value: strings.Repeat("文", 3000)})
	}
	before, _ := json.Marshal(input)
	first, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range first.Fields {
		total += utf8.RuneCountInString(f.Value)
	}
	if total != MaxTotalRunes || !slices.Contains(first.NormalizationWarnings, "INPUT_TRUNCATED") {
		t.Fatalf("normalized size=%d warnings=%v", total, first.NormalizationWarnings)
	}
	after, _ := json.Marshal(input)
	if !bytes.Equal(before, after) {
		t.Fatal("budgeting modified the caller's evidence")
	}
	input.Fields[3].Value += "different original tail"
	second, err := Normalize(input)
	if err != nil || first.InputHash == second.InputHash || first.TextHash != second.TextHash {
		t.Fatalf("truncated tail lost its independent evidence digest: %v", err)
	}
	input.Fields[3].Value = strings.Repeat("x", MaxRawInputBytes)
	if _, err = Normalize(input); err == nil {
		t.Fatal("oversized raw input reached normalization")
	}
	// Normalizing the same complete original remains deterministic.
	var original Input
	_ = json.Unmarshal(before, &original)
	repeated, err := Normalize(original)
	if err != nil || !reflect.DeepEqual(first, repeated) {
		t.Fatalf("budgeting changed audit identity on replay: %v", err)
	}
}

func TestWeakPerceptualHashesDoNotCreateDuplicateAccusations(t *testing.T) {
	for _, tc := range []struct {
		hash string
		want bool
	}{
		{"0000000000000000", false},
		{"000000000000007f", false}, // seven bits
		{"00000000000000ff", true},  // eight bits
		{"ffffffffffffff00", true},  // 56 bits
		{"ffffffffffffff80", false}, // 57 bits
		{"ffffffffffffffff", false},
		{"0123456789ABCDEF", false},
		{"not-an-imagehash", false},
	} {
		if got := InformativePerceptualHash(tc.hash); got != tc.want {
			t.Errorf("informative(%s)=%v want %v", tc.hash, got, tc.want)
		}
	}
	for _, hash := range []string{"0000000000000000", "ffffffffffffffff", "000000000000007f"} {
		if ImageHashesMatch(hash, hash, 5) || ImageHashesMatch(hash, "0123456789abcdef", 64) || ImageHashesMatch("0123456789abcdef", hash, 64) {
			t.Fatal("a legacy weak fingerprint produced a perceptual duplicate signal")
		}
	}
	if !ImageHashesMatch(strings.Repeat("0", 64), strings.Repeat("0", 64), 5) {
		t.Fatal("perceptual gating changed exact SHA256 byte matching")
	}
	input := ordinaryInput()
	input.Context.ImageHashes = []string{"0000000000000000"}
	input.Context.KnownImageHashes = []string{"0000000000000000"}
	n, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := EvaluateDeterministic(n, DefaultConfig())
	if slices.Contains(p.ReasonCodes, "DUPLICATE_IMAGE") {
		t.Fatal("weak legacy image metadata became a duplicate accusation")
	}
}
