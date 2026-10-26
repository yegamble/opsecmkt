package moderation

import (
	"math"
	"slices"
)

// Route is an authority boundary: score influences review urgency, while hard
// rules always outrank confidence, reputation, or a model's proposed action.
func Route(in NormalizedInput, p Proposal, cfg Config) (Disposition, int, []string) {
	reasons := append([]string{}, p.ReasonCodes...)
	add := func(code string) { reasons = appendUnique(reasons, code) }
	if reviewOnlySignal(in) {
		add("HUMAN_REVIEW")
	}
	score := min(30, in.Context.BlastRadius)
	if !in.Context.Reversible {
		score += 15
	}
	if in.Actor.AgeDays < cfg.NewActorDays {
		score += 10
		add("NEW_ACTOR")
	}
	// The market never sets Reputation, so its jobs always take this weight.
	if in.Actor.Reputation < .5 {
		score += 5
	}
	if in.Context.Novel {
		score += 20
		add("NOVEL_CASE")
	}
	if p.Confidence < cfg.ConfidenceFloor || math.IsNaN(p.Confidence) || math.IsInf(p.Confidence, 0) {
		score += 25
		add("LOW_CONFIDENCE")
	}
	money := in.Context.MoneyInvolved || p.Action == ActionSuggestRelease || p.Action == ActionSuggestRefund || (in.Context.TimerDue && (in.Context.TimerKind == "auto_release" || in.Context.TimerKind == "auto_refund"))
	if money {
		score += 40
		add("MONEY_INVOLVED")
	}
	injection := in.InjectionSuspected || p.InjectionSuspected || slices.Contains(reasons, "INJECTION_SUSPECTED") || slices.Contains(reasons, "CANARY_MISMATCH")
	if injection {
		score = 100
		add("INJECTION_SUSPECTED")
	}
	score = min(100, score)
	slices.Sort(reasons)
	if injection || money || p.Action == ActionSuggestBan || in.Context.BlastRadius >= 50 || score >= 70 {
		return EscalateAdmin, score, reasons
	}
	// The allowlist cannot be enlarged by model wording. Even a passing financial
	// invariant does not give this component any authority to move money.
	if p.Action != ActionNoAction && p.Action != ActionFlagReview {
		add("POLICY_BOUNDARY")
		slices.Sort(reasons)
		return QueueHuman, score, reasons
	}
	// FLAG_REVIEW is a request for human attention, including a high-confidence
	// deterministic signal. Automatic history is reserved for a benign NO_ACTION.
	if p.Action == ActionFlagReview {
		return QueueHuman, score, reasons
	}
	if p.Confidence < cfg.ConfidenceFloor || math.IsNaN(p.Confidence) || math.IsInf(p.Confidence, 0) || in.Context.Novel || score >= 30 || slices.Contains(reasons, "INPUT_TRUNCATED") || slices.Contains(reasons, "DETERMINISTIC_CONFLICT") || slices.Contains(reasons, "ORDER_STATE_VIOLATION") || slices.Contains(reasons, "INVALID_FIELDS") || slices.Contains(reasons, "MODEL_ABSENT") || slices.Contains(reasons, "MODEL_NOT_USED") || slices.Contains(reasons, "MODEL_FAILURE") || slices.Contains(reasons, "INVALID_OUTPUT") {
		return QueueHuman, score, reasons
	}
	// A model cannot conceal a finding by pairing its reason with NO_ACTION.
	// Only an explicit clean result and age metadata belong in automatic history.
	if !slices.Contains(reasons, "NO_FINDING") {
		return QueueHuman, score, reasons
	}
	for _, code := range reasons {
		if code != "NO_FINDING" && code != "NEW_ACTOR" {
			return QueueHuman, score, reasons
		}
	}
	return AutoApply, score, reasons
}
