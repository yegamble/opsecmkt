package market

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/moderation"
)

func TestModerationRetentionNoticePreservesLegacyCopiesAfterUpgradeExpiryAndDisable(t *testing.T) {
	// Exercise the actual pre-134 trigger and tables. The old artifact is inserted
	// before the upgrade; no immutability guard is disabled and no historical
	// snapshot or proposal is edited to manufacture legacy contents afterward.
	legacy := newPooledHistoricalFixture(t, 127)
	vendor, session := legacy.keylessUser("moderation_retention_notice_vendor", "vendor") // schema 127 predates sessions.pgp_access_key (129)
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
		records, partial := abuseReviewNotices(t, page, e.body("GET", "/account", session, nil, 200))
		assertNewAbuseReviewRecordNotice(t, stage, records, partial)
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

// abuseReviewNotices returns the /canary#records abuse-review item and the retention partial's
// abuse-review paragraph (rendered on /account), each as one HTML fragment.
func abuseReviewNotices(t *testing.T, canary, account string) (records, partial string) {
	t.Helper()
	cut := func(where, page, from, to string) string {
		t.Helper()
		i := strings.Index(page, from)
		if i < 0 {
			t.Fatalf("%s has no %q", where, from)
		}
		fragment, _, _ := strings.Cut(page[i:], to)
		return fragment
	}
	return cut("/canary", canary, "<li><strong>Abuse-review records.</strong>", "</li>"),
		cut("/account", account, `<p class="help">New abuse-review records`, "</p>")
}

// MOD72-2: new (v2) abuse-review records, not only legacy copies, are disclosed as having no automatic
// expiry, outliving the source, and as fingerprints that can confirm a guess.
func assertNewAbuseReviewRecordNotice(t *testing.T, stage, records, partial string) {
	t.Helper()
	shared := []string{
		"New abuse-review records also have no automatic expiry: their references and fingerprints remain after the original text or image is removed",
		"a digest or fingerprint can confirm a guessed text or a matching image",
		"Routine expiry and account deletion do not erase copies retained for abuse review",
	}
	for _, want := range append([]string{
		"record and order references, the capture time, digests of the checked text, an image&rsquo;s exact and perceptual fingerprints",
		"findings, decisions and staff reviews",
		"A list of records waiting to be checked holds only each record&rsquo;s type and id, when it was first and last left waiting, and whether it waits for an unflagged payment to be flagged again; withdrawn listings and hidden reviews are checked like any others, and deleting a record removes its entry at once",
		"as with any deleted record, its data can remain in earlier backups and, until PostgreSQL reuses the space, in database files and write-ahead logs",
		// A-647: the deferred reference row (migration 148).
		"When a record is created or changed while the review queue is busy, a second list holds only its type and id and when that was noted, never the account, until the next worker pass while collection is on checks its current version or adds it to the waiting list; deleting the record removes its entry at once, and while collection is off the entry can remain indefinitely",
		"For each account it holds a count of its checks in the past day, cleared after that day at the next check or worker pass or when collection stops; how many of its records are waiting and how many of those wait for a payment; for each waiting record the account deleted, a note that one such record was deleted and whether it waited for a payment, never which record, kept until the next worker pass subtracts it from those counts (while collection is off this can last indefinitely, until collection is enabled again); and whether its records are being re-read, with the type, id and creation time of the last record re-read, kept while that re-read runs and collection is on and cleared at the next worker pass if that record is deleted",
		"The account&rsquo;s entry is removed once its day has passed with no record waiting and no re-read in progress, or when the account is deleted",
		"Until then it can remain indefinitely: while an unflagged payment of the account is waiting, or while collection is off",
		"after account deletion and after collection stops",
	}, shared...) {
		if !strings.Contains(html.UnescapeString(records), html.UnescapeString(want)) {
			t.Errorf("%s /canary#records abuse-review item lacks %q", stage, html.UnescapeString(want))
		}
	}
	for _, want := range append([]string{"including exact and perceptual image fingerprints"}, shared...) {
		if !strings.Contains(partial, want) {
			t.Errorf("%s retention partial lacks %q", stage, want)
		}
	}
	for _, text := range []string{records, partial} {
		if regexp.MustCompile(`(?i)model|inference|threshold|endpoint`).MatchString(text) {
			t.Errorf("%s abuse-review notice names review internals: %s", stage, text)
		}
	}
}

// MOD72-2: on the current schema, the records item and the retention partial carry the new-record
// disclosure, word for word the same whether collection is on or off.
func TestNewAbuseReviewRecordNoticeIdenticalWithCaptureOnAndOff(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("mod72_notice_buyer", "buyer")
	render := func(enabled bool) (string, string) {
		t.Helper()
		if err := SetModerationEnabled(t.Context(), e.DB, enabled); err != nil {
			t.Fatal(err)
		}
		stage := map[bool]string{true: "capture on", false: "capture off"}[enabled]
		records, partial := abuseReviewNotices(t, e.body("GET", "/canary", "", nil, 200), e.body("GET", "/account", session, nil, 200))
		assertNewAbuseReviewRecordNotice(t, stage, records, partial)
		return records, partial
	}
	onRecords, onPartial := render(true)
	offRecords, offPartial := render(false)
	if onRecords != offRecords || onPartial != offPartial {
		t.Errorf("abuse-review notice differs with capture on and off:\non  %s\n    %s\noff %s\n    %s", onRecords, onPartial, offRecords, offPartial)
	}
}
