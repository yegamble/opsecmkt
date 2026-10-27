package moderation

import (
	"math"
	"slices"
)

// Route is an authority boundary: score influences review urgency, while hard
// rules always outrank confidence, reputation, or a model's proposed action.
// It returns no AUTO_APPLY: every proposal that the deterministic checks could
// not decide, model-derived ones included, waits for a human (A-684).
func Route(in NormalizedInput, p Proposal, cfg Config) (Disposition, int, []string) {
	reasons := append([]string{}, p.ReasonCodes...)
	add := func(code string) { reasons = appendUnique(reasons, code) }
	destination, instruction := reviewOnlySignal(in)
	if destination || instruction {
		add("HUMAN_REVIEW")
	}
	// Go-only and fixed: a listing already carries HUMAN_REVIEW with
	// MODEL_NOT_USED, so without this code a destination left no trace.
	if destination {
		add("EXTERNAL_DESTINATION")
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
	// FLAG_REVIEW asks for human attention. A NO_ACTION/NO_FINDING proposal can
	// only come from a model (deterministic checks never decide a clean item),
	// and no evaluation shows that model judgment or its stated confidence is
	// fit to clear an item, so it is queued at any confidence. An opt-in gated on
	// a measured evaluation report is separate work (A-692).
	return QueueHuman, score, reasons
}
