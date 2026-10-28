package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"opsecmkt/internal/moderation"
)

// A-651 (owner directive: "The model must not judge listings by text").

// moderationCaptureRuntime serves a llama.cpp-shaped runtime on a Unix socket.
// It records every prompt and answers a clean closed result whose canary it
// reads from the prompt's trusted protocol block. Fake-model tier: it proves
// what is sent and that nothing is sent, never model quality.
func moderationCaptureRuntime(t *testing.T) (*moderation.LocalModel, func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "modscope-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	weights := []byte("test-only weights fixture for prompt capture")
	path := filepath.Join(dir, "weights.bin")
	if err = os.WriteFile(path, weights, 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "inference.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var prompts []string
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokenize" {
			tokens := make([]int, 64)
			reply(w, map[string]any{"tokens": tokens})
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
		}
		if r.URL.Path != "/completion" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		prompts = append(prompts, req.Prompt)
		mu.Unlock()
		_, block, _ := strings.Cut(req.Prompt, "TRUSTED_PROTOCOL\n")
		block, _, _ = strings.Cut(block, "\n")
		var protocol struct {
			Canary string  `json:"canary"`
			ItemID *string `json:"item_id"`
		}
		if json.Unmarshal([]byte(block), &protocol) != nil {
			http.Error(w, "no protocol block", http.StatusBadRequest)
			return
		}
		var content any = map[string]any{"findings": []any{map[string]any{"reason_code": "NO_FINDING", "evidence_ids": []string{}}}, "injection_suspected": false, "canary": protocol.Canary}
		if protocol.ItemID != nil {
			content = map[string]any{"item_id": *protocol.ItemID, "action": moderation.ActionNoAction, "confidence": .95, "reason_codes": []string{"NO_FINDING"}, "injection_suspected": false, "evidence_ids": []string{}, "canary": protocol.Canary}
		}
		raw, _ := json.Marshal(content)
		reply(w, map[string]any{"content": string(raw), "stop": true, "stop_type": "eos", "truncated": false, "tokens_evaluated": 64})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	hash := sha256.Sum256(weights)
	model, err := moderation.NewLocalModel(moderation.LocalModelConfig{SocketPath: socket, WeightsPath: path, WeightsHash: hex.EncodeToString(hash[:]), ModelID: "scope-fixture", PromptVersion: moderation.PromptVersion, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return model, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(prompts)
	}
}

// kindRecordingModel records the kind of every item that reaches the model.
type kindRecordingModel struct {
	next  moderation.Model
	mu    sync.Mutex
	kinds []string
}

func (m *kindRecordingModel) Propose(ctx context.Context, in moderation.NormalizedInput) (moderation.Proposal, error) {
	m.mu.Lock()
	m.kinds = append(m.kinds, in.Kind)
	m.mu.Unlock()
	return m.next.Propose(ctx, in)
}

// With the model enabled, listing and upload jobs never reach it and record
// MODEL_NOT_USED with the model-absent routing; reviews, disputes and payment
// flags still do. No prompt carries listing text, a handle, PGP fingerprint,
// image hash, payment address, staff free text or a reputation figure.
func TestModerationListingSkipsModelWhenEnabled(t *testing.T) {
	e := newTestApp(t)
	vendor, _ := e.user("moderation_scope_vendor", "vendor")
	buyer, _ := e.user("moderation_scope_buyer", "buyer")
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	p := randomToken()
	agExec(e, `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,'listing-sentinel-title','listing-sentinel-description','Hardware','Worldwide','digital',100000,500000000000,5)`, p, vendor)
	im := e.image(vendor, p, "photo")
	order := e.order(buyer, p, "XMR", stateCompleted)
	agExec(e, "INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES($1,$2,$3,$4,4,'Arrived well packed')", randomToken(), order, p, buyer)
	agExec(e, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The parcel never arrived')", randomToken(), order)
	agExec(e, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,flagged) VALUES($1,'XMR',$2,0,'payment-sentinel-address',1,true)", order, randomToken())
	// Staff free text the worker must never read into a prompt.
	agExec(e, "UPDATE users SET suspended_at=now(),banned_at=now(),ban_reason='staff-sentinel-ban-reason' WHERE id=$1", vendor)
	agExec(e, "UPDATE products SET archived=true,admin_archive_reason='staff-sentinel-archive-reason',review_state='rejected',review_rejection_reason='staff-sentinel-rejection-reason' WHERE id=$1", p)

	local, prompts := moderationCaptureRuntime(t)
	recorder := &kindRecordingModel{next: local}
	w := moderationTestWorker(e)
	w.Engine.Config.Enabled = true
	w.Engine.Model = recorder
	if n, err := w.Pass(t.Context(), 20); err != nil || n != 5 {
		t.Fatalf("pass=%d err=%v", n, err)
	}
	slices.Sort(recorder.kinds)
	if !slices.Equal(recorder.kinds, []string{"dispute", "payment_flag", "review"}) {
		t.Fatalf("kinds that reached the model: %v", recorder.kinds)
	}
	if n := agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE kind IN ('listing','upload') AND reason_codes ? 'MODEL_NOT_USED' AND NOT reason_codes ? 'MODEL_ABSENT' AND disposition='QUEUE_HUMAN' AND failure_class='' AND confidence=0`); n != 2 {
		t.Fatalf("listing/upload proposals recorded as not model-judged: %d of 2", n)
	}
	if n := agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE kind NOT IN ('listing','upload') AND (reason_codes ? 'MODEL_NOT_USED' OR failure_class<>'')`); n != 0 {
		t.Fatalf("%d modelled proposals lack a successful model result", n)
	}
	// A clean model result still waits for a human (A-684).
	if got := agStr(e, `SELECT disposition FROM moderation_suggestions WHERE kind='review'`); got != string(moderation.QueueHuman) {
		t.Fatalf("review routed %s with a clean model result", got)
	}
	if n := agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE kind IN ('dispute','payment_flag') AND disposition='ESCALATE_ADMIN'`); n != 2 {
		t.Fatalf("money-adjacent proposals escalated: %d of 2", n)
	}
	var vendorHandle, buyerHandle, vendorKey, buyerKey string
	if err := e.DB.QueryRow(`SELECT v.handle,b.handle,v.pgp_fingerprint,b.pgp_fingerprint FROM users v,users b WHERE v.id=$1 AND b.id=$2`, vendor, buyer).Scan(&vendorHandle, &buyerHandle, &vendorKey, &buyerKey); err != nil {
		t.Fatal(err)
	}
	if vendorKey == "" || buyerKey == "" {
		t.Fatal("fixture accounts lack PGP fingerprints")
	}
	imageHash := sha256.Sum256(im.full)
	perceptual := agStr(e, `SELECT coalesce(metadata->>'perceptual_hash',snapshot->>'perceptual_hash','') FROM moderation_jobs WHERE kind='upload'`)
	forbidden := []string{"listing-sentinel", vendorHandle, buyerHandle, vendorKey, buyerKey, hex.EncodeToString(imageHash[:]), "payment-sentinel-address", "staff-sentinel", "reput", vendor, buyer, p, order, im.id}
	if perceptual != "" {
		forbidden = append(forbidden, perceptual)
	}
	seen := prompts()
	if len(seen) != 6 {
		t.Fatalf("want two prompts for each of three modelled jobs, got %d", len(seen))
	}
	if !slices.ContainsFunc(seen, func(prompt string) bool { return strings.Contains(prompt, "Arrived well packed") }) {
		t.Fatal("capture did not record the review's Stage A prompt")
	}
	for i, prompt := range seen {
		lower := strings.ToLower(prompt)
		for _, value := range forbidden {
			if strings.Contains(lower, strings.ToLower(value)) {
				t.Errorf("prompt %d carries %q", i+1, value)
			}
		}
	}
}

// A decision stored under moderation-v4 keeps its bytes and still renders after
// the current worker runs, beside a new listing decision (A-651).
func TestModerationStoredV4DecisionStaysReadable(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("moderation_v4_admin", "admin")
	vendor, _ := e.user("moderation_v4_vendor", "vendor")
	if err := SetModerationEnabled(t.Context(), e.DB, true); err != nil {
		t.Fatal(err)
	}
	p := e.product(vendor, "digital")
	var j moderationJob
	if err := e.DB.QueryRow(`SELECT id,kind,target_id,actor_id,content_hash,snapshot,queued_at,capture_version,metadata,actor_ref FROM moderation_jobs WHERE target_id=$1`, p).Scan(&j.ID, &j.Kind, &j.TargetID, &j.ActorID, &j.ContentHash, &j.Snapshot, &j.QueuedAt, &j.CaptureVersion, &j.Metadata, &j.ActorRef); err != nil {
		t.Fatal(err)
	}
	hash := func(data string) string {
		sum := sha256.Sum256([]byte(data))
		return hex.EncodeToString(sum[:])
	}
	// The row a v4 worker wrote for a listing with no model configured.
	in := moderationMinimalInput(j)
	reasons := []string{"HUMAN_REVIEW", "LOW_CONFIDENCE", "MODEL_ABSENT", "NEW_ACTOR"}
	inputHash := hash("moderation-v4-input:" + j.ContentHash)
	proposal := moderation.Proposal{ItemID: in.ItemID, Action: moderation.ActionFlagReview, Confidence: 0, ReasonCodes: reasons, InjectionEvidence: []string{}, EvidenceIDs: in.Context.EvidenceIDs, Summary: moderation.SummaryForReasons(reasons), Canary: moderation.CanaryFor(inputHash)}
	proposalBytes, _ := json.Marshal(proposal)
	decision := moderation.Decision{ID: hash("moderation-v4-decision:" + j.ContentHash), InputHash: inputHash, OutputHash: hash(string(proposalBytes)), Proposal: proposal, Disposition: moderation.QueueHuman, RiskScore: 41, ReasonCodes: reasons, FailureClass: "model_absent", Actor: "moderation-worker", PromptVersion: "moderation-v4", SLA: 24 * time.Hour, DefaultAction: moderation.Hold}
	decisionBytes, _ := json.Marshal(decision)
	inputBytes, _ := json.Marshal(moderationStoredMetadata(j, in))
	reasonBytes, _ := json.Marshal(reasons)
	agExec(e, `INSERT INTO moderation_suggestions(id,job_id,kind,target_id,actor_id,content_hash,policy_hash,input,decision,disposition,risk_score,confidence,reason_codes,input_hash,output_hash,prompt_version,model_id,weights_hash,expires_at,failure_class)
	 VALUES($1,$2,'listing',$3,'',$4,$5,$6,$7,'QUEUE_HUMAN',41,0,$8,$9,$10,'moderation-v4','','',now()+interval '24 hours','model_absent')`, decision.ID, j.ID, p, j.ContentHash, hash("moderation-v4-policy"), string(inputBytes), string(decisionBytes), string(reasonBytes), inputHash, decision.OutputHash)
	agExec(e, `UPDATE moderation_jobs SET completed_at=now() WHERE id=$1`, j.ID)
	stored := agStr(e, `SELECT input::text||decision::text||reason_codes::text||output_hash||prompt_version FROM moderation_suggestions WHERE id=$1`, decision.ID)

	agExec(e, `UPDATE products SET description='A revised v5 description' WHERE id=$1`, p)
	w := moderationTestWorker(e)
	w.Engine.Config.Enabled = true
	recorder := &kindRecordingModel{next: unavailableModerationModel{}}
	w.Engine.Model = recorder
	if _, _, err := ReplayModerationJobs(t.Context(), e.DB); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Pass(t.Context(), 20); err != nil || n != 1 || len(recorder.kinds) != 0 {
		t.Fatalf("v5 pass=%d model calls=%v err=%v", n, recorder.kinds, err)
	}
	if got := agStr(e, `SELECT input::text||decision::text||reason_codes::text||output_hash||prompt_version FROM moderation_suggestions WHERE id=$1`, decision.ID); got != stored {
		t.Fatal("the stored v4 decision changed")
	}
	if n := agInt(e, `SELECT count(*) FROM moderation_suggestions WHERE target_id=$1 AND prompt_version=$2 AND reason_codes ? 'MODEL_NOT_USED'`, p, moderation.PromptVersion); n != 1 {
		t.Fatalf("current-protocol listing decisions: %d", n)
	}
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	page := privateHostRequest(e, "GET", "/admin/moderation", admin, nil, true)
	e.check(page, 200)
	mustContain(t, page.Body.String(), "contextual classification unavailable", "fixed-rule checks only (no model reads this kind)")
}

// Actor.Reputation is reserved: no market or command code assigns it, so every
// market job keeps Route's fixed unrated-actor weight and the canonical input
// hash keeps a zero reputation (A-651; a Go reputation rule is A-653).
func TestNoMarketCodeSetsActorReputation(t *testing.T) {
	detect := func(file *ast.File) []token.Pos {
		var found []token.Pos
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "Reputation" {
					found = append(found, n.Pos())
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Reputation" {
					found = append(found, n.Pos())
				}
			}
			return true
		})
		return found
	}
	fset := token.NewFileSet()
	probe, err := parser.ParseFile(fset, "probe.go", "package p\nfunc f(a *moderation.Actor) { a.Reputation = 1; _ = moderation.Actor{Reputation: 1} }\n", 0)
	if err != nil || len(detect(probe)) != 2 {
		t.Fatal("source guard does not detect a Reputation assignment")
	}
	parsed := 0
	// TestMain runs the package's tests from the repository root.
	for _, root := range []string{"internal/market", "cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && path != root && root == "internal/market" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			parsed++
			for _, pos := range detect(file) {
				t.Errorf("%s uses Reputation; market code must not set Actor.Reputation", fset.Position(pos))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if parsed < 50 {
		t.Fatalf("source guard parsed only %d files", parsed)
	}
}
