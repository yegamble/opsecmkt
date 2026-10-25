package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

type Engine struct {
	Config Config
	Model  Model
}

func configDefaults(cfg Config) (Config, error) {
	defaults := DefaultConfig()
	if cfg.PromptVersion == "" {
		cfg.PromptVersion = defaults.PromptVersion
	}
	if cfg.ConfidenceFloor == 0 {
		cfg.ConfidenceFloor = defaults.ConfidenceFloor
	}
	if cfg.NewActorDays == 0 {
		cfg.NewActorDays = defaults.NewActorDays
	}
	if cfg.AbuseThreshold == 0 {
		cfg.AbuseThreshold = defaults.AbuseThreshold
	}
	if cfg.HumanSLA == 0 {
		cfg.HumanSLA = defaults.HumanSLA
	}
	if cfg.PromptVersion != PromptVersion || cfg.ConfidenceFloor <= 0 || cfg.ConfidenceFloor > 1 || math.IsNaN(cfg.ConfidenceFloor) || cfg.NewActorDays < 0 || cfg.AbuseThreshold < 1 || cfg.HumanSLA < 0 || cfg.HumanSLA > 365*24*time.Hour {
		return cfg, errors.New("invalid moderation policy")
	}
	if len(cfg.ModelID) > 128 || strings.ContainsAny(cfg.ModelID, "\r\n\x00") {
		return cfg, errors.New("invalid moderation model pin")
	}
	if cfg.WeightsHash != "" {
		decoded, err := hex.DecodeString(cfg.WeightsHash)
		if err != nil || len(decoded) != 32 || strings.ToLower(cfg.WeightsHash) != cfg.WeightsHash {
			return cfg, errors.New("invalid moderation model pin")
		}
	}
	return cfg, nil
}

func decisionFrom(in NormalizedInput, p Proposal, cfg Config, failure string) Decision {
	disposition, score, reasons := Route(in, p, cfg)
	p.ReasonCodes = reasons
	p = renderTrustedProposal(p, in)
	encoded, _ := json.Marshal(p)
	// Identity excludes inference output: repeated evaluation cannot create a
	// second audit identity for the same snapshot and policy/model pins. The
	// durable store keeps the first validated result, never silently replaces it.
	identity, _ := json.Marshal(struct {
		Hash   string
		Config Config
	}{in.InputHash, cfg})
	return Decision{ID: digest(identity), InputHash: in.InputHash, OutputHash: digest(encoded), Proposal: p, Disposition: disposition, RiskScore: score, ReasonCodes: reasons, FailureClass: failure, Actor: "moderation-worker", PromptVersion: cfg.PromptVersion, ModelID: cfg.ModelID, WeightsHash: cfg.WeightsHash, SLA: cfg.HumanSLA, DefaultAction: Hold}
}

// Evaluate is pure except for the explicitly supplied model capability. Its
// returned disposition records metadata; it never applies a target mutation.
func (e Engine) Evaluate(ctx context.Context, input Input) (Decision, error) {
	cfg, err := configDefaults(e.Config)
	if err != nil {
		return Decision{}, err
	}
	in, err := Normalize(input)
	if err != nil {
		// Invalid snapshots remain reviewable and do not cause an endless job retry.
		// Reference errors are programming/caller errors rather than user text.
		if !safeReference.MatchString(input.ItemID) || !safeReference.MatchString(input.Actor.ID) {
			return Decision{}, err
		}
		hash := shaInputWithoutAllocation(input)
		actor := input.Actor
		if math.IsNaN(actor.Reputation) || math.IsInf(actor.Reputation, 0) {
			actor.Reputation = 0
		}
		in = NormalizedInput{Input: Input{ItemID: input.ItemID, Kind: input.Kind, Actor: actor, Context: DeterministicContext{Reversible: true, MoneyInvolved: input.Context.MoneyInvolved, BlastRadius: max(0, min(100, input.Context.BlastRadius))}}, InputHash: hash, InjectionEvidence: []string{}, NormalizationWarnings: []string{}}
		p := baseProposal(in)
		p.Action, p.Confidence, p.ReasonCodes = ActionFlagReview, 0, []string{"INVALID_FIELDS", "HUMAN_REVIEW"}
		p.Summary = "The input exceeded moderation validation limits. Human review is required; no marketplace state changed."
		return decisionFrom(in, p, cfg, "input_invalid"), nil
	}
	p, decidable := EvaluateDeterministic(in, cfg)
	failure := ""
	if !decidable {
		switch {
		case !cfg.Enabled || e.Model == nil:
			p.Confidence, p.Action = 0, ActionFlagReview
			p.ReasonCodes = []string{"MODEL_ABSENT", "HUMAN_REVIEW"}
			p.Summary = "Contextual judgment is unavailable. The item is queued for human review; no marketplace state changed."
			failure = "model_absent"
		case ctx.Err() != nil:
			p.Confidence, p.Action = 0, ActionFlagReview
			p.ReasonCodes = []string{"MODEL_FAILURE", "HUMAN_REVIEW"}
			p.Summary = "The moderation pass was interrupted. Human review is required; no marketplace state changed."
			failure = "model_interrupted"
		default:
			candidate, modelErr := e.Model.Propose(ctx, in)
			if modelErr == nil {
				modelErr = ValidateProposal(candidate, in, CanaryFor(in.InputHash))
			}
			if modelErr != nil {
				p.Action, p.Confidence, p.ReasonCodes = ActionFlagReview, 0, []string{"MODEL_FAILURE", "HUMAN_REVIEW"}
				p.Summary = "The model result could not be validated. Human review is required; no marketplace state changed."
				failure = "model_failure"
				if errors.Is(modelErr, ErrInvalidOutput) {
					p.ReasonCodes = append(p.ReasonCodes, "INVALID_OUTPUT")
					failure = "invalid_output"
				}
				if errors.Is(modelErr, ErrCanaryMismatch) || errors.Is(modelErr, ErrSuspiciousOutput) {
					p.InjectionSuspected = true
					p.InjectionEvidence = []string{"model:protocol_mismatch"}
					p.ReasonCodes = append(p.ReasonCodes, "INJECTION_SUSPECTED")
					failure = "suspect_output"
					if errors.Is(modelErr, ErrCanaryMismatch) {
						p.ReasonCodes = append(p.ReasonCodes, "CANARY_MISMATCH")
					}
				}
			} else {
				// Re-run deterministic checks even though in is immutable. The worker
				// must separately re-read/re-hash the DB snapshot under its row lock.
				p = candidate
				check, _ := EvaluateDeterministic(in, cfg)
				if !slices.Equal(check.ReasonCodes, []string{"NO_FINDING"}) {
					p.Confidence = min(p.Confidence, .5)
					p.ReasonCodes = appendUnique(p.ReasonCodes, "DETERMINISTIC_CONFLICT")
				}
			}
		}
	}
	return decisionFrom(in, p, cfg, failure), nil
}

func shaInputWithoutAllocation(input Input) string {
	// Hash large raw values incrementally. Prefixing byte lengths makes the
	// framing unambiguous without allocating another copy of invalid user text.
	// Nonfinite floats are retained by bit pattern and cannot collapse to JSON's
	// empty/error encoding or collide across different item references.
	h := sha256.New()
	write := func(value string) {
		_ = binary.Write(h, binary.BigEndian, uint64(len(value)))
		for len(value) > 0 {
			n := min(len(value), 4096)
			_, _ = h.Write([]byte(value[:n]))
			value = value[n:]
		}
	}
	write("moderation-invalid-input-v1")
	write(input.ItemID)
	write(input.Kind)
	write(input.Actor.ID)
	_ = binary.Write(h, binary.BigEndian, int64(input.Actor.AgeDays))
	_ = binary.Write(h, binary.BigEndian, math.Float64bits(input.Actor.Reputation))
	_ = binary.Write(h, binary.BigEndian, uint64(len(input.Fields)))
	for _, field := range input.Fields {
		write(field.Source)
		write(field.Name)
		write(field.Trust)
		write(field.Value)
	}
	// Context is caller-controlled trusted metadata with no free text. Its
	// bounded normal path is preferred; invalid contexts still retain a digest.
	contextJSON, _ := json.Marshal(input.Context)
	_, _ = h.Write(contextJSON)
	return hex.EncodeToString(h.Sum(nil))
}
