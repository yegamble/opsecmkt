package moderation

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed prompts/stage-a.txt
var stageAPrompt string

//go:embed prompts/stage-b.txt
var stageBPrompt string

//go:embed prompts/stage-a.gbnf
var stageAGrammar string

//go:embed prompts/proposal.gbnf
var proposalGrammar string

var ErrModelUnavailable = errors.New("moderation: local model unavailable")

// ErrPromptTooLarge is returned before generation; the item goes to human review.
var ErrPromptTooLarge = errors.New("moderation: prompt exceeds the model context budget")

var errReplyTooLarge = errors.New("moderation: runtime reply exceeds its bound")

// The shipped launcher (deploy/moderation/entrypoint.sh) serves one slot with
// --ctx-size 4096. A prompt plus the maximum output must fit in that slot:
// otherwise llama.cpp refuses the request, stops at the context limit, or with
// context shift discards earlier tokens, including the fixed instructions.
// The slack covers special tokens counted differently by older /tokenize builds.
// TestLauncherContextSizeMatchesTokenBudget keeps --ctx-size and this budget equal.
const (
	modelContextTokens     = 4096
	modelPredictTokens     = 1536
	modelSpecialTokenSlack = 8
	maxTokenizeReply       = 64 << 10
)

// modelItemAlias replaces the item reference on the wire. Evidence references
// travel as E1..En in DeterministicContext.EvidenceIDs order. Go maps both back,
// so trusted references and their hashes never reach a prompt.
const modelItemAlias = "ITEM"

func evidenceAliases(n int) []string {
	aliases := make([]string, n)
	for i := range aliases {
		aliases[i] = "E" + strconv.Itoa(i+1)
	}
	return aliases
}

// resolveEvidenceAliases maps a bounded, unique list of this request's aliases
// to trusted references. An unknown alias or a raw reference is refused.
func resolveEvidenceAliases(aliases, ids []string) ([]string, bool) {
	known := evidenceAliases(len(ids))
	if aliases == nil || len(aliases) > MaxEvidenceIDs || !uniqueSubset(aliases, known) {
		return nil, false
	}
	resolved := make([]string, len(aliases))
	for i, alias := range aliases {
		resolved[i] = ids[slices.Index(known, alias)]
	}
	return resolved, true
}

// LocalModelConfig is trusted operator configuration. Neither paths nor pins
// may be taken from marketplace content. This package never launches a process.
type LocalModelConfig struct {
	SocketPath    string
	WeightsPath   string
	WeightsHash   string
	ModelID       string
	PromptVersion string
	Timeout       time.Duration
}

// LocalModel talks only HTTP over a Unix socket. There is no default network
// transport, DNS resolver, proxy, redirect, remote API, or downloaded asset.
// The operator must separately sandbox the serving process and mount the same
// read-only pinned weights. A client-side hash cannot attest a running server.
type LocalModel struct {
	client  *http.Client
	timeout time.Duration
	gate    chan struct{}
	modelID string
}

func NewLocalModel(cfg LocalModelConfig) (*LocalModel, error) {
	if !filepath.IsAbs(cfg.SocketPath) || filepath.Clean(cfg.SocketPath) != cfg.SocketPath || len(cfg.SocketPath) > 100 || strings.ContainsAny(cfg.SocketPath, "\x00\r\n") || !strings.HasSuffix(cfg.SocketPath, ".sock") {
		return nil, errors.New("moderation: absolute Unix socket path ending .sock required")
	}
	if !filepath.IsAbs(cfg.WeightsPath) || cfg.PromptVersion != PromptVersion || len(cfg.ModelID) == 0 || len(cfg.ModelID) > 80 {
		return nil, errors.New("moderation: local weights, model identity and matching prompt pin required")
	}
	for _, r := range cfg.ModelID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return nil, errors.New("moderation: invalid model identity")
		}
	}
	expected, err := hex.DecodeString(cfg.WeightsHash)
	if err != nil || len(expected) != sha256.Size || cfg.WeightsHash != strings.ToLower(cfg.WeightsHash) {
		return nil, errors.New("moderation: SHA256 weights pin required")
	}
	file, err := os.Open(cfg.WeightsPath)
	if err != nil {
		return nil, ErrModelUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<30 {
		return nil, errors.New("moderation: weights must be a bounded regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(file, (64<<30)+1)); err != nil || !bytes.Equal(h.Sum(nil), expected) {
		return nil, errors.New("moderation: weights pin verification failed")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 8 * time.Second
	}
	if cfg.Timeout < 100*time.Millisecond || cfg.Timeout > 30*time.Second {
		return nil, errors.New("moderation: model timeout must be between 100ms and 30s")
	}
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", cfg.SocketPath)
		},
		DisableKeepAlives:      true,
		MaxConnsPerHost:        1,
		MaxResponseHeaderBytes: 8 << 10,
		ResponseHeaderTimeout:  cfg.Timeout,
	}
	return &LocalModel{
		client:  &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		timeout: cfg.Timeout,
		gate:    make(chan struct{}, 1),
		modelID: cfg.ModelID,
	}, nil
}

type codedFinding struct {
	ReasonCode  string   `json:"reason_code"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type extractedFindings struct {
	Findings           []codedFinding `json:"findings"`
	InjectionSuspected bool           `json:"injection_suspected"`
	Canary             string         `json:"canary"`
}

// stageAReasons are the observations Stage A can ground in the submitted text
// alone. Duplicate, price, completeness, normalization and novelty signals come
// from deterministic Go checks, never from the model.
func stageAReasons() []string {
	return []string{"SPAM_TEMPLATE", "SCAM_TEMPLATE", "OFF_PLATFORM_CONTACT", "INJECTION_SUSPECTED", "HUMAN_REVIEW", "NO_FINDING"}
}

// Stage A findings carry evidence aliases; Go resolves them when merging.
func decodeFindings(raw []byte, in NormalizedInput, canary string) (extractedFindings, error) {
	var findings extractedFindings
	if err := strictObject(raw, []string{"findings", "injection_suspected", "canary"}, &findings); err != nil {
		return findings, err
	}
	if findings.Canary != canary {
		return findings, ErrCanaryMismatch
	}
	if findings.Findings == nil || len(findings.Findings) > 16 {
		return findings, ErrInvalidOutput
	}
	seen := make(map[string]bool)
	for _, f := range findings.Findings {
		if _, known := resolveEvidenceAliases(f.EvidenceIDs, in.Context.EvidenceIDs); !known || !slices.Contains(stageAReasons(), f.ReasonCode) || seen[f.ReasonCode] {
			return findings, ErrInvalidOutput
		}
		seen[f.ReasonCode] = true
		if f.ReasonCode == "INJECTION_SUSPECTED" {
			findings.InjectionSuspected = true
		}
	}
	if seen["NO_FINDING"] && len(findings.Findings) > 1 {
		return findings, ErrInvalidOutput
	}
	if findings.InjectionSuspected {
		return findings, ErrSuspiciousOutput
	}
	return findings, nil
}

// withStageAFindings keeps every validated Stage A finding in the final
// proposal. Stage B sees findings only as data and cannot drop one; a finding
// replaces NO_FINDING, so routing never treats the item as clean.
func withStageAFindings(p Proposal, findings []codedFinding, in NormalizedInput) Proposal {
	merged := false
	for _, f := range findings {
		if f.ReasonCode == "NO_FINDING" {
			continue
		}
		merged = true
		p.ReasonCodes = appendUnique(p.ReasonCodes, f.ReasonCode)
		evidence, _ := resolveEvidenceAliases(f.EvidenceIDs, in.Context.EvidenceIDs)
		for _, id := range evidence {
			p.EvidenceIDs = appendUnique(p.EvidenceIDs, id)
		}
	}
	if merged {
		p.ReasonCodes = slices.DeleteFunc(p.ReasonCodes, func(code string) bool { return code == "NO_FINDING" })
	}
	return renderTrustedProposal(p, in)
}

// stageBContext is Stage B's trusted view. Go has already compared the text and
// image fingerprints, so Stage B receives outcomes and counts, not hashes. The
// actor reference is omitted and item/evidence references are aliased.
type stageBContext struct {
	ItemID                 string         `json:"item_id"`
	Kind                   string         `json:"kind"`
	ActorAgeDays           int            `json:"actor_age_days"`
	ActorReputation        float64        `json:"actor_reputation"`
	EvidenceIDs            []string       `json:"evidence_ids"`
	Complete               bool           `json:"complete"`
	PriceAtomic            int64          `json:"price_atomic"`
	CategoryMedianAtomic   int64          `json:"category_median_atomic"`
	KnownTextsCompared     int            `json:"known_texts_compared"`
	KnownTextMatch         bool           `json:"known_text_match"`
	Images                 int            `json:"images"`
	KnownImagesCompared    int            `json:"known_images_compared"`
	KnownImageMatch        bool           `json:"known_image_match"`
	OrderState             string         `json:"order_state"`
	ProposedOrderState     string         `json:"proposed_order_state"`
	TimerKind              string         `json:"timer_kind"`
	TimerDue               bool           `json:"timer_due"`
	DeterministicInvariant bool           `json:"deterministic_invariant"`
	MoneyInvolved          bool           `json:"money_involved"`
	Novel                  bool           `json:"novel"`
	BlastRadius            int            `json:"blast_radius"`
	Reversible             bool           `json:"reversible"`
	AbuseCount             int            `json:"abuse_count"`
	Canary                 string         `json:"canary"`
	Actions                []string       `json:"actions"`
	ReasonCodes            []string       `json:"reason_codes"`
	Findings               []codedFinding `json:"findings"`
}

func stageBView(in NormalizedInput, canary string, aliases []string, findings []codedFinding) stageBContext {
	c := in.Context
	imageMatch := false
	for _, h := range c.ImageHashes {
		for _, known := range c.KnownImageHashes {
			imageMatch = imageMatch || ImageHashesMatch(h, known, 5)
		}
	}
	return stageBContext{ItemID: modelItemAlias, Kind: in.Kind, ActorAgeDays: in.Actor.AgeDays, ActorReputation: in.Actor.Reputation, EvidenceIDs: aliases,
		Complete: c.Complete, PriceAtomic: c.PriceAtomic, CategoryMedianAtomic: c.CategoryMedianAtomic,
		KnownTextsCompared: len(c.KnownTextHashes), KnownTextMatch: slices.Contains(c.KnownTextHashes, in.TextHash),
		Images: len(c.ImageHashes), KnownImagesCompared: len(c.KnownImageHashes), KnownImageMatch: imageMatch,
		OrderState: c.OrderState, ProposedOrderState: c.ProposedOrderState, TimerKind: c.TimerKind, TimerDue: c.TimerDue,
		DeterministicInvariant: c.DeterministicInvariant, MoneyInvolved: c.MoneyInvolved, Novel: c.Novel, BlastRadius: c.BlastRadius,
		Reversible: c.Reversible, AbuseCount: c.AbuseCount, Canary: canary, Actions: ModelActions(), ReasonCodes: ModelReasonCodes(), Findings: findings}
}

// modelJSON encodes a value for a prompt. llama.cpp's /completion tokenizes the
// prompt with special-token parsing enabled, so control tokens spelled with
// brackets, such as Mistral's [INST], would become real control tokens if data
// carried them. encoding/json already escapes '<', '>' and '&'; this also writes
// '[' and ']' inside string values as \u005b and \u005d. Array syntax and the
// JSON value are unchanged.
func modelJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(raw))
	inString, escaped := false, false
	for _, b := range raw {
		switch {
		case escaped:
			escaped = false
		case inString && b == '\\':
			escaped = true
		case b == '"':
			inString = !inString
		case inString && b == '[':
			out = append(out, `\u005b`...)
			continue
		case inString && b == ']':
			out = append(out, `\u005d`...)
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// Propose runs two independent completion requests with no shared chat history.
// Stage B receives codes, aliases and derived context only: Stage A never
// forwards free-form text, and validated Stage A findings stay in the result.
func (m *LocalModel) Propose(ctx context.Context, in NormalizedInput) (Proposal, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	case <-ctx.Done():
		return Proposal{}, ErrModelUnavailable
	}
	canary := CanaryFor(in.InputHash)
	aliases := evidenceAliases(len(in.Context.EvidenceIDs))
	protocol, _ := modelJSON(struct {
		Canary      string   `json:"canary"`
		EvidenceIDs []string `json:"evidence_ids"`
		ReasonCodes []string `json:"reason_codes"`
	}{canary, aliases, stageAReasons()})
	data, err := modelJSON(in.Fields)
	if err != nil || len(data) > 96<<10 || len(in.Fields) > 32 {
		return Proposal{}, ErrInvalidOutput
	}
	// modelJSON escapes '<', '>', '[' and ']' in the data's strings. The fence is
	// chosen after encoding, so it cannot collide. Neither escaping nor a fence
	// establishes trust.
	fence := "UNTRUSTED_" + in.InputHash[:min(16, len(in.InputHash))]
	for strings.Contains(string(data), fence) {
		fence += "_"
	}
	promptA := stageAPrompt + "\nTRUSTED_PROTOCOL\n" + string(protocol) + "\n" + fence + "\n" + string(data) + "\nEND_" + fence
	raw, err := m.complete(ctx, promptA, stageAGrammarFor(canary, aliases))
	if err != nil {
		return Proposal{}, err
	}
	findings, err := decodeFindings(raw, in, canary)
	if err != nil {
		return Proposal{}, err
	}
	trusted, err := modelJSON(stageBView(in, canary, aliases, findings.Findings))
	if err != nil || len(trusted) > 96<<10 {
		return Proposal{}, ErrInvalidOutput
	}
	raw, err = m.complete(ctx, stageBPrompt+"\nTRUSTED_PROTOCOL\n"+string(trusted), proposalGrammarFor(canary, aliases))
	if err != nil {
		return Proposal{}, err
	}
	p, err := DecodeProposal(raw, in, canary)
	if err != nil {
		return Proposal{}, err
	}
	return withStageAFindings(p, findings.Findings, in), nil
}

// findingCodes lists the codes a non-empty finding list may hold. The grammars
// offer NO_FINDING only as the sole entry; Go validation enforces the same.
func findingCodes(codes []string) []string {
	return slices.DeleteFunc(codes, func(code string) bool { return code == "NO_FINDING" })
}

func stageAGrammarFor(canary string, aliases []string) string {
	codes := findingCodes(stageAReasons())
	return strings.NewReplacer("{{CANARY}}", grammarLiteral(canary), "{{NO_FINDING}}", grammarLiteral("NO_FINDING"), "{{REASONS}}", grammarEnums(codes), "{{MORE}}", strconv.Itoa(len(codes)-1), "{{EVIDENCE_ARRAY}}", grammarEvidenceArray(aliases)).Replace(stageAGrammar)
}

func proposalGrammarFor(canary string, aliases []string) string {
	codes := findingCodes(ModelReasonCodes())
	return strings.NewReplacer("{{CANARY}}", grammarLiteral(canary), "{{ITEM_ID}}", grammarLiteral(modelItemAlias), "{{ACTIONS}}", grammarEnums(ModelActions()), "{{NO_FINDING}}", grammarLiteral("NO_FINDING"), "{{REASONS}}", grammarEnums(codes), "{{MORE}}", strconv.Itoa(len(codes)-1), "{{EVIDENCE_ARRAY}}", grammarEvidenceArray(aliases)).Replace(proposalGrammar)
}

func grammarLiteral(value string) string { return strconv.Quote(strconv.Quote(value)) }

func grammarEnums(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = grammarLiteral(value)
	}
	return strings.Join(quoted, " | ")
}

// Both stages may emit only this request's finite evidence identifiers. Empty
// contexts permit only []; arrays are bounded at decoding and again in Go.
func grammarEvidenceArray(values []string) string {
	if len(values) == 0 {
		return `"[" ws "]"`
	}
	identifiers := grammarEnums(values)
	return `"[" ws ((` + identifiers + `) (ws "," ws (` + identifiers + `)){0,31})? ws "]"`
}

// complete counts the prompt with the runtime's own tokenizer, refuses one that
// cannot fit beside the maximum output, then accepts only a finished generation.
func (m *LocalModel) complete(ctx context.Context, prompt, grammar string) ([]byte, error) {
	tokens, err := m.promptTokens(ctx, prompt)
	if err != nil {
		return nil, err
	}
	if tokens+modelSpecialTokenSlack+modelPredictTokens > modelContextTokens {
		return nil, ErrPromptTooLarge
	}
	body, err := json.Marshal(struct {
		Prompt      string  `json:"prompt"`
		Grammar     string  `json:"grammar"`
		Temperature float64 `json:"temperature"`
		Seed        int     `json:"seed"`
		NPredict    int     `json:"n_predict"`
		Stream      bool    `json:"stream"`
		CachePrompt bool    `json:"cache_prompt"`
	}{Prompt: prompt, Grammar: grammar, NPredict: modelPredictTokens})
	if err != nil || len(body) > 192<<10 {
		return nil, ErrInvalidOutput
	}
	raw, err := m.post(ctx, "/completion", body, 128<<10)
	if errors.Is(err, errReplyTooLarge) {
		return nil, ErrInvalidOutput
	}
	if err != nil {
		return nil, err
	}
	var envelope completionEnvelope
	if rejectDuplicateKeys(raw) != nil || json.Unmarshal(raw, &envelope) != nil || len(envelope.Content) == 0 || len(envelope.Content) > maxModelOutput || !envelope.finished(tokens) {
		return nil, ErrInvalidOutput
	}
	return []byte(envelope.Content), nil
}

// completionEnvelope holds the llama.cpp /completion fields used here. Current
// builds report stop_type ("none", "eos", "limit", "word"); older builds
// reported stopped_eos, stopped_word and stopped_limit instead.
type completionEnvelope struct {
	Content         string  `json:"content"`
	Truncated       *bool   `json:"truncated"`
	StopType        *string `json:"stop_type"`
	StoppedEOS      *bool   `json:"stopped_eos"`
	StoppedWord     *bool   `json:"stopped_word"`
	StoppedLimit    *bool   `json:"stopped_limit"`
	TokensEvaluated *int    `json:"tokens_evaluated"`
}

// finished accepts a generation that ended at end-of-sequence or a stop word,
// was not truncated and evaluated the whole counted prompt within budget.
// Missing metadata counts as unfinished: a grammar-valid prefix is not enough.
func (e completionEnvelope) finished(promptTokens int) bool {
	if e.Truncated == nil || *e.Truncated || e.TokensEvaluated == nil {
		return false
	}
	if evaluated := *e.TokensEvaluated; evaluated < promptTokens-modelSpecialTokenSlack || evaluated > modelContextTokens-modelPredictTokens {
		return false
	}
	if e.StoppedLimit != nil && *e.StoppedLimit {
		return false
	}
	if e.StopType != nil {
		return *e.StopType == "eos" || *e.StopType == "word"
	}
	return e.StoppedEOS != nil && *e.StoppedEOS || e.StoppedWord != nil && *e.StoppedWord
}

// promptTokens asks the runtime's /tokenize endpoint, over the same socket, how
// many tokens the prompt costs including BOS. A reply too long to belong to a
// prompt within budget is refused as oversized without decoding it.
func (m *LocalModel) promptTokens(ctx context.Context, prompt string) (int, error) {
	body, err := json.Marshal(struct {
		Content    string `json:"content"`
		AddSpecial bool   `json:"add_special"`
	}{prompt, true})
	if err != nil || len(body) > 192<<10 {
		return 0, ErrPromptTooLarge
	}
	raw, err := m.post(ctx, "/tokenize", body, maxTokenizeReply)
	if errors.Is(err, errReplyTooLarge) {
		return 0, ErrPromptTooLarge
	}
	if err != nil {
		return 0, err
	}
	var reply struct {
		Tokens []int `json:"tokens"`
	}
	if rejectDuplicateKeys(raw) != nil || json.Unmarshal(raw, &reply) != nil || len(reply.Tokens) == 0 {
		return 0, ErrInvalidOutput
	}
	return len(reply.Tokens), nil
}

// post sends one bounded JSON request over the Unix socket.
func (m *LocalModel) post(ctx context.Context, path string, body []byte, limit int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://moderation"+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrModelUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := m.client.Do(req)
	if err != nil {
		return nil, ErrModelUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, ErrModelUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
	if err != nil {
		return nil, ErrInvalidOutput
	}
	if len(raw) > limit {
		return nil, errReplyTooLarge
	}
	return raw, nil
}
