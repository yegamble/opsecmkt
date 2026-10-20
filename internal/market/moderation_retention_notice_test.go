package market

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/moderation"
)

func TestModerationRetentionNoticePreservesLegacyCopiesAfterUpgradeExpiryAndDisable(t *testing.T) {
	// Exercise the actual pre-129 trigger and tables. The old artifact is inserted
	// before the upgrade; no immutability guard is disabled and no historical
	// snapshot or proposal is edited to manufacture legacy contents afterward.
	legacy := newPooledHistoricalFixture(t, 127)
	vendor, session := legacy.user("moderation_retention_notice_vendor", "vendor")
	product := legacy.product(vendor, "digital")
	const marker = "MOD69-retained-plaintext-marker-6a7d"
	agExec(legacy, `UPDATE settings SET value='true' WHERE key='moderation_enabled'`)
	agExec(legacy, `UPDATE products SET description=$2 WHERE id=$1`, product, marker)
	var job int64
	var contentHash, originalSnapshot string
	if err := legacy.DB.QueryRow(`SELECT id,content_hash,snapshot::text FROM moderation_jobs WHERE target_id=$1`, product).Scan(&job, &contentHash, &originalSnapshot); err != nil {
		t.Fatal(err)
	}
	hash := func(data []byte) string {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	input := moderation.Input{ItemID: "listing:" + product, Kind: "listing", Actor: moderation.Actor{ID: vendor}, Fields: []moderation.Field{{Source: "listing", Name: "description", Trust: "DATA", Value: marker}}, Context: moderation.DeterministicContext{EvidenceIDs: []string{"listing:" + product}, Reversible: true}}
	inputBytes, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	inputHash := hash(inputBytes)
	id := hash([]byte("historical-moderation-v1:" + contentHash))
	proposal := moderation.Proposal{ItemID: input.ItemID, Action: moderation.ActionFlagReview, Confidence: 0, ReasonCodes: []string{"MODEL_ABSENT", "HUMAN_REVIEW"}, InjectionEvidence: []string{}, EvidenceIDs: input.Context.EvidenceIDs, Summary: "Legacy review copy: " + marker, Canary: moderation.CanaryFor(inputHash)}
	proposalBytes, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	decision := moderation.Decision{ID: id, InputHash: inputHash, OutputHash: hash(proposalBytes), Proposal: proposal, Disposition: moderation.QueueHuman, RiskScore: 25, ReasonCodes: proposal.ReasonCodes, FailureClass: "model_absent", Actor: "moderation-worker", PromptVersion: "moderation-v1", SLA: 24 * time.Hour, DefaultAction: moderation.Hold}
	decisionBytes, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	reasons, _ := json.Marshal(proposal.ReasonCodes)
	agExec(legacy, `INSERT INTO moderation_suggestions(id,job_id,kind,target_id,actor_id,content_hash,policy_hash,input,decision,disposition,risk_score,confidence,reason_codes,input_hash,output_hash,prompt_version,model_id,weights_hash,expires_at,failure_class)
	 VALUES($1,$2,'listing',$3,$4,$5,$6,$7,$8,'QUEUE_HUMAN',25,0,$9,$10,$11,'moderation-v1','','',now()+interval '24 hours','model_absent')`, id, job, product, vendor, contentHash, hash([]byte("historical-moderation-v1-policy")), string(inputBytes), string(decisionBytes), string(reasons), inputHash, decision.OutputHash)
	agExec(legacy, `UPDATE moderation_jobs SET completed_at=now() WHERE id=$1`, job)
	originalInput := agStr(legacy, `SELECT input::text FROM moderation_suggestions WHERE id=$1`, id)
	originalDecision := agStr(legacy, `SELECT decision::text FROM moderation_suggestions WHERE id=$1`, id)

	// Start the current application against that historical schema, letting its
	// normal migration path establish legacy versions rather than spoofing them.
	dsn, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("search_path", agStr(legacy, `SELECT current_schema()`))
	dsn.RawQuery = query.Encode()
	e := startTestApp(t, dsn.String())
	cfg := retentionConfig{Enabled: true, StateDir: filepath.Join(t.TempDir(), "legacy-retention-state")}
	if err := e.A.initRetention(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if agInt(e, `SELECT capture_version FROM moderation_jobs WHERE id=$1`, job) != 0 || agInt(e, `SELECT storage_version FROM moderation_suggestions WHERE id=$1`, id) != 0 {
		t.Fatal("upgrade did not preserve explicit historical capture/storage versions")
	}
	assertCopies := func() {
		t.Helper()
		if agStr(e, `SELECT snapshot::text FROM moderation_jobs WHERE id=$1`, job) != originalSnapshot || agStr(e, `SELECT input::text FROM moderation_suggestions WHERE id=$1`, id) != originalInput || agStr(e, `SELECT decision::text FROM moderation_suggestions WHERE id=$1`, id) != originalDecision {
			t.Fatal("historical plaintext snapshot or immutable proposal changed")
		}
		if !strings.Contains(originalSnapshot, marker) || !strings.Contains(originalInput, marker) || !strings.Contains(originalDecision, marker) {
			t.Fatal("legacy fixture does not contain the historical plaintext copies")
		}
	}
	assertNotice := func(stage string) {
		t.Helper()
		for _, path := range []string{"/canary", "/account"} {
			page := e.body("GET", path, session, nil, 200)
			for _, text := range []string{"Routine expiry and account deletion do not erase copies retained for abuse review", "These copies currently have no automatic expiry"} {
				if !strings.Contains(page, text) {
					t.Errorf("%s %s omitted retained-copy notice %q", stage, path, text)
				}
			}
			if strings.Contains(page, marker) {
				t.Errorf("%s %s disclosed private review contents", stage, path)
			}
		}
		page := e.body("GET", "/canary", "", nil, 200)
		if !strings.Contains(page, "<strong>Abuse-review records.</strong>") || !strings.Contains(page, "after collection stops") {
			t.Errorf("%s public records inventory omitted the review-copy lifecycle", stage)
		}
		account := e.body("GET", "/account", session, nil, 200)
		if !strings.Contains(account, "Account deletion does not erase retained abuse-review copies") {
			t.Errorf("%s deletion confirmation omitted retained abuse-review copies", stage)
		}
	}
	assertCopies()
	assertNotice("historical capture upgraded")
	// Advance only the existing listing/enrollment clocks. The real retention
	// sweep removes the original; this change does not introduce evidence expiry.
	agExec(e, `UPDATE products SET archived=true,archived_at=now()-interval '31 days',updated=now()-interval '31 days' WHERE id=$1`, product)
	ageRetentionEnrollment(t, e.A, time.Now().UTC().Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, time.Now().UTC())
	if agInt(e, `SELECT count(*) FROM products WHERE id=$1`, product) != 0 {
		t.Fatal("original listing did not expire through ordinary retention")
	}
	assertCopies()
	assertNotice("original expired")
	if err := SetModerationEnabled(t.Context(), e.DB, false); err != nil {
		t.Fatal(err)
	}
	assertCopies()
	assertNotice("capture disabled with historical copies")
}
