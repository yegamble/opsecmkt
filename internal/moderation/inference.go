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

func stageAReasons() []string {
	return []string{"SPAM_TEMPLATE", "SCAM_TEMPLATE", "DUPLICATE_TEXT", "DUPLICATE_IMAGE", "PRICE_OUTLIER", "INCOMPLETE_LISTING", "INVALID_FIELDS", "INJECTION_SUSPECTED", "UNICODE_MIXED_SCRIPT", "ZERO_WIDTH_OBFUSCATION", "OFF_PLATFORM_CONTACT", "DETERMINISTIC_CONFLICT", "NOVEL_CASE", "NO_FINDING", "HUMAN_REVIEW"}
}

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
		if !slices.Contains(stageAReasons(), f.ReasonCode) || seen[f.ReasonCode] || f.EvidenceIDs == nil || len(f.EvidenceIDs) > 32 || !uniqueSubset(f.EvidenceIDs, in.Context.EvidenceIDs) {
			return findings, ErrInvalidOutput
		}
		seen[f.ReasonCode] = true
		if f.ReasonCode == "INJECTION_SUSPECTED" {
			findings.InjectionSuspected = true
		}
	}
	if findings.InjectionSuspected {
		return findings, ErrSuspiciousOutput
	}
	return findings, nil
}

// Propose runs two independent completion requests with no shared chat history.
// Stage B receives codes/references only: Stage A never forwards free-form text.
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
	protocol, _ := json.Marshal(struct {
		Canary      string   `json:"canary"`
		EvidenceIDs []string `json:"evidence_ids"`
		ReasonCodes []string `json:"reason_codes"`
	}{canary, in.Context.EvidenceIDs, stageAReasons()})
	data, err := json.Marshal(in.Fields)
	if err != nil || len(data) > 96<<10 || len(in.Fields) > 32 {
		return Proposal{}, ErrInvalidOutput
	}
	// JSON escapes '<' and '>' in the data. The fence is chosen after encoding,
	// so it cannot collide. Neither escaping nor a fence establishes trust.
	fence := "UNTRUSTED_" + in.InputHash
	for strings.Contains(string(data), fence) {
		fence += "_"
	}
	promptA := stageAPrompt + "\nTRUSTED_PROTOCOL\n" + string(protocol) + "\n" + fence + "\n" + string(data) + "\nEND_" + fence
	grammarA := strings.NewReplacer("{{CANARY}}", grammarLiteral(canary), "{{REASONS}}", grammarEnums(stageAReasons()), "{{EVIDENCE_ARRAY}}", grammarEvidenceArray(in.Context.EvidenceIDs)).Replace(stageAGrammar)
	raw, err := m.complete(ctx, promptA, grammarA)
	if err != nil {
		return Proposal{}, err
	}
	findings, err := decodeFindings(raw, in, canary)
	if err != nil {
		return Proposal{}, err
	}
	trusted, err := json.Marshal(struct {
		ItemID      string               `json:"item_id"`
		Kind        string               `json:"kind"`
		Actor       Actor                `json:"actor"`
		Context     DeterministicContext `json:"context"`
		Canary      string               `json:"canary"`
		Actions     []string             `json:"actions"`
		ReasonCodes []string             `json:"reason_codes"`
		Findings    []codedFinding       `json:"findings"`
	}{in.ItemID, in.Kind, in.Actor, in.Context, canary, ModelActions(), ReasonCodes(), findings.Findings})
	if err != nil || len(trusted) > 96<<10 {
		return Proposal{}, ErrInvalidOutput
	}
	grammarB := strings.NewReplacer("{{CANARY}}", grammarLiteral(canary), "{{ITEM_ID}}", grammarLiteral(in.ItemID), "{{ACTIONS}}", grammarEnums(ModelActions()), "{{REASONS}}", grammarEnums(ReasonCodes()), "{{EVIDENCE_ARRAY}}", grammarEvidenceArray(in.Context.EvidenceIDs)).Replace(proposalGrammar)
	raw, err = m.complete(ctx, stageBPrompt+"\nTRUSTED_PROTOCOL\n"+string(trusted), grammarB)
	if err != nil {
		return Proposal{}, err
	}
	return DecodeProposal(raw, in, canary)
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

func (m *LocalModel) complete(ctx context.Context, prompt, grammar string) ([]byte, error) {
	body, err := json.Marshal(struct {
		Prompt      string  `json:"prompt"`
		Grammar     string  `json:"grammar"`
		Temperature float64 `json:"temperature"`
		Seed        int     `json:"seed"`
		NPredict    int     `json:"n_predict"`
		Stream      bool    `json:"stream"`
		CachePrompt bool    `json:"cache_prompt"`
	}{Prompt: prompt, Grammar: grammar, NPredict: 1536})
	if err != nil || len(body) > 192<<10 {
		return nil, ErrInvalidOutput
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://moderation/completion", bytes.NewReader(body))
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
	raw, err := io.ReadAll(io.LimitReader(res.Body, (128<<10)+1))
	if err != nil || len(raw) > 128<<10 || rejectDuplicateKeys(raw) != nil {
		return nil, ErrInvalidOutput
	}
	var envelope struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Content) == 0 || len(envelope.Content) > maxModelOutput {
		return nil, ErrInvalidOutput
	}
	return []byte(envelope.Content), nil
}
