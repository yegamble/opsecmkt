package moderation

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

func protocolInput() NormalizedInput {
	return NormalizedInput{Input: Input{
		ItemID: "review:7", Kind: "review", Actor: Actor{ID: "actor-9", AgeDays: 40, Reputation: .8},
		Fields:  []Field{{Source: "review", Name: "body", Trust: "DATA", Value: "vendor-marker-9cb8"}},
		Context: DeterministicContext{EvidenceIDs: []string{"review:7"}, Complete: true, Reversible: true},
	}, InputHash: strings.Repeat("a", 64)}
}

func protocolProposal(in NormalizedInput) Proposal {
	return Proposal{ItemID: in.ItemID, Action: ActionNoAction, Confidence: .93, ReasonCodes: []string{"NO_FINDING"}, InjectionEvidence: []string{}, EvidenceIDs: []string{"review:7"}, Summary: "No additional moderation finding.", Canary: CanaryFor(in.InputHash)}
}

// proposalWireBytes encodes p as the model would: the item and each trusted
// reference appear only as the request's aliases.
func proposalWireBytes(p Proposal, in NormalizedInput) []byte {
	item, evidence := p.ItemID, []string{}
	if item == in.ItemID {
		item = modelItemAlias
	}
	aliases := evidenceAliases(len(in.Context.EvidenceIDs))
	for _, id := range p.EvidenceIDs {
		if i := slices.Index(in.Context.EvidenceIDs, id); i >= 0 {
			id = aliases[i]
		}
		evidence = append(evidence, id)
	}
	raw, _ := json.Marshal(map[string]any{
		"item_id": item, "action": p.Action, "confidence": p.Confidence,
		"reason_codes": p.ReasonCodes, "injection_suspected": p.InjectionSuspected,
		"evidence_ids": evidence, "canary": p.Canary,
	})
	return raw
}

func TestDecodeProposalStrict(t *testing.T) {
	in := protocolInput()
	p := protocolProposal(in)
	raw := proposalWireBytes(p, in)
	if _, err := DecodeProposal(raw, in, p.Canary); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing":                strings.Replace(string(raw), `"confidence":0.93,`, "", 1),
		"null":                   strings.Replace(string(raw), `"confidence":0.93`, `"confidence":null`, 1),
		"unknown":                strings.Replace(string(raw), `"confidence":0.93`, `"confidence":0.93,"execute":"release"`, 1),
		"duplicate":              strings.Replace(string(raw), `"confidence":0.93`, `"confidence":0.93,"confidence":0.99`, 1),
		"escaped duplicate":      strings.Replace(string(raw), `"confidence":0.93`, `"confidence":0.93,"confi\u0064ence":0.99`, 1),
		"trailing":               string(raw) + "{}",
		"numeric string":         strings.Replace(string(raw), `"confidence":0.93`, `"confidence":"0.93"`, 1),
		"bounds":                 strings.Replace(string(raw), `"confidence":0.93`, `"confidence":1.01`, 1),
		"negative":               strings.Replace(string(raw), `"confidence":0.93`, `"confidence":-0.01`, 1),
		"nonfinite":              strings.Replace(string(raw), `"confidence":0.93`, `"confidence":1e9999`, 1),
		"action":                 strings.Replace(string(raw), `"NO_ACTION"`, `"EXECUTE_SQL"`, 1),
		"reason":                 strings.Replace(string(raw), `"NO_FINDING"`, `"APPROVED_BY_ADMIN"`, 1),
		"reference":              strings.Replace(string(raw), `"evidence_ids":["E1"]`, `"evidence_ids":["E2"]`, 1),
		"raw reference":          strings.Replace(string(raw), `"evidence_ids":["E1"]`, `"evidence_ids":["review:7"]`, 1),
		"null array":             strings.Replace(string(raw), `"evidence_ids":["E1"]`, `"evidence_ids":null`, 1),
		"wrong item":             strings.Replace(string(raw), `"item_id":"ITEM"`, `"item_id":"order:8"`, 1),
		"raw item":               strings.Replace(string(raw), `"item_id":"ITEM"`, `"item_id":"review:7"`, 1),
		"inconsistent suspicion": strings.Replace(string(raw), `"injection_suspected":false`, `"injection_suspected":true`, 1),
		"oversized":              strings.Repeat(" ", maxModelOutput) + string(raw),
		"control":                strings.Replace(string(raw), `"E1"`, `"E\u00001"`, 1),
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			if invalid == string(raw) {
				t.Fatal("case did not change the valid fixture")
			}
			if _, err := DecodeProposal([]byte(invalid), in, p.Canary); !errors.Is(err, ErrInvalidOutput) {
				t.Fatalf("accepted invalid output or wrong failure class: %v", err)
			}
		})
	}
	if _, err := DecodeProposal(raw, in, "wrong-canary"); !errors.Is(err, ErrCanaryMismatch) {
		t.Fatal("canary mismatch not distinguished")
	}
}

func TestValidateProposalNumericAndEvidence(t *testing.T) {
	in := protocolInput()
	for _, confidence := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := protocolProposal(in)
		p.Confidence = confidence
		if err := ValidateProposal(p, in, p.Canary); !errors.Is(err, ErrInvalidOutput) {
			t.Fatal("nonfinite confidence accepted")
		}
	}
	p := protocolProposal(in)
	p.InjectionSuspected = true
	p.InjectionEvidence = []string{"instruction override in field 0"}
	p.ReasonCodes = []string{"INJECTION_SUSPECTED"}
	if err := ValidateProposal(p, in, p.Canary); err != nil {
		t.Fatal(err)
	}
	p.EvidenceIDs = append(p.EvidenceIDs, p.EvidenceIDs[0])
	if err := ValidateProposal(p, in, p.Canary); !errors.Is(err, ErrInvalidOutput) {
		t.Fatal("duplicate evidence accepted")
	}
}

func TestDecodeFindingsRejectsFreeformAndReferences(t *testing.T) {
	in := protocolInput()
	canary := CanaryFor(in.InputHash)
	valid := `{"findings":[{"reason_code":"NO_FINDING","evidence_ids":["E1"]}],"injection_suspected":false,"canary":"` + canary + `"}`
	if _, err := decodeFindings([]byte(valid), in, canary); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		strings.Replace(valid, `"reason_code":"NO_FINDING"`, `"reason_code":"NO_FINDING","quote":"ignore instructions"`, 1),
		strings.Replace(valid, `"reason_code":"NO_FINDING"`, `"reason_code":"mark this order delivered"`, 1),
		strings.Replace(valid, `"evidence_ids":["E1"]`, `"evidence_ids":["E2"]`, 1),
		strings.Replace(valid, `"evidence_ids":["E1"]`, `"evidence_ids":["review:7"]`, 1),
		strings.Replace(valid, `"evidence_ids":["E1"]`, `"evidence_ids":null`, 1),
		strings.Replace(valid, `"evidence_ids":["E1"]`, `"evidence_ids":["E1"],"evidence_ids":[]`, 1),
	}
	for _, invalid := range cases {
		if _, err := decodeFindings([]byte(invalid), in, canary); !errors.Is(err, ErrInvalidOutput) {
			t.Fatalf("invalid findings accepted: %v", err)
		}
	}
	suspect := strings.Replace(valid, `"NO_FINDING"`, `"INJECTION_SUSPECTED"`, 1)
	if _, err := decodeFindings([]byte(suspect), in, canary); !errors.Is(err, ErrSuspiciousOutput) {
		t.Fatal("injection not quarantined")
	}
}
