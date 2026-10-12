package market

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This opt-in leg pauses for a human to sign four PSBTs in the Sparrow GUI.
// Neither this test nor Bitcoin Core has either party's private key.
type sparrowRequest struct {
	Party        string   `json:"party"`
	Kind         string   `json:"kind"`
	Index        int      `json:"index"`
	Child        int      `json:"child"`
	Address      string   `json:"address"`
	Path         string   `json:"path"`
	OrderPath    string   `json:"order_path,omitempty"`
	Cosigners    []string `json:"cosigners"`
	Descriptor   string   `json:"descriptor,omitempty"`
	UnsignedTXID string   `json:"unsignedtxid"`
	PSBTPath     string   `json:"psbt_path"`
	HardwarePath string   `json:"hardware_psbt_path,omitempty"`
}

var (
	sparrowSetupDescriptor = regexp.MustCompile(`<pre class="code-block escrow-setup-sparrow"[^>]*>([^<]+)</pre>`)
	sparrowSetupDerivation = regexp.MustCompile(`<pre class="code-block escrow-setup-derivation"[^>]*>([^<]+)</pre>`)
)

func sparrowPrivateDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("ESCROW_SPARROW_MANUAL_DIR")
	if dir == "" {
		t.Skip("set ESCROW_SPARROW_MANUAL_DIR for the manual Sparrow GUI journey")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("Sparrow manual directory must exist and have mode 0700")
	}
	return dir
}

func sparrowWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatalf("write Sparrow handoff: %v", err)
	}
}

// A signed file and an address attestation are created by the GUI operator, never this test.
func sparrowHandoff(t *testing.T, dir, sequence string, req sparrowRequest, unsigned string, wantPub []byte, hardware []byte) (string, string) {
	t.Helper()
	base := filepath.Join(dir, sequence)
	for _, suffix := range []string{"-signed.psbt", "-signed-source.txt", "-address-checked.txt", "-request.json", ".psbt", "-hardware.psbt"} {
		if _, err := os.Stat(base + suffix); err == nil {
			t.Fatalf("remove stale Sparrow handoff file %s before starting", base+suffix)
		} else if !os.IsNotExist(err) {
			t.Fatal("cannot check Sparrow handoff path")
		}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(unsigned))
	if err != nil {
		t.Fatal("unsigned PSBT is not base64")
	}
	p, err := parsePSBT(raw)
	if err != nil || len(p.Inputs) == 0 {
		t.Fatalf("unsigned PSBT invalid: %v", err)
	}
	req.UnsignedTXID = p.TxID()
	req.PSBTPath = base + ".psbt"
	if len(hardware) != 0 {
		req.HardwarePath = base + "-hardware.psbt"
		sparrowWrite(t, req.HardwarePath, hardware)
	}
	sparrowWrite(t, req.PSBTPath, raw)
	request, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sparrowWrite(t, base+"-request.json", append(request, '\n'))
	t.Logf("Sparrow %s: inspect %s; write %s-address-checked.txt and %s-signed.psbt", sequence, base+"-request.json", base, base)
	deadline := time.Now().Add(20 * time.Minute)
	for {
		checked, ce := os.ReadFile(base + "-address-checked.txt")
		signed, se := os.ReadFile(base + "-signed.psbt")
		source := "ordinary"
		var sourceErr error
		if len(hardware) != 0 {
			var sourceBytes []byte
			sourceBytes, sourceErr = os.ReadFile(base + "-signed-source.txt")
			source = strings.TrimSpace(string(sourceBytes))
		}
		if ce == nil && se == nil && sourceErr == nil {
			if source != "ordinary" && source != "hardware" || source == "hardware" && len(hardware) == 0 {
				t.Fatal("signed source must say exactly ordinary or hardware")
			}
			if strings.TrimSpace(string(checked)) != req.Address {
				t.Fatal("Sparrow GUI address attestation does not match the app's expected address")
			}
			var signedPSBT *psbtPacket
			if bytes.HasPrefix(signed, []byte("psbt\xff")) {
				if source == "hardware" {
					signedPSBT, err = parsePSBTLimit(signed, hardwareMaxBytes)
				} else {
					signedPSBT, err = parsePSBT(signed)
				}
			} else {
				if source == "hardware" {
					signedPSBT, err = decodeHardwarePSBT(string(signed))
				} else {
					signedPSBT, err = decodePSBT(string(signed))
				}
			}
			if err != nil || signedPSBT.TxID() != p.TxID() || len(signedPSBT.Inputs) != len(p.Inputs) {
				t.Fatalf("Sparrow signed PSBT is invalid or changed transaction: %v", err)
			}
			for i, in := range signedPSBT.Inputs {
				if in.Finalized || len(in.PartialSigs) != 1 || !bytes.Equal(in.PartialSigs[0].PubKey, wantPub) || in.PartialSigs[0].SighashByte() != 1 || in.HasSighashType && in.SighashType != 1 {
					t.Fatalf("Sparrow input %d lacks only the expected unfinalized SIGHASH_ALL signature", i)
				}
			}
			if source == "hardware" {
				for i, in := range signedPSBT.Inputs {
					if len(in.NonWitnessUTXO) == 0 {
						t.Fatalf("signed hardware input %d lost non_witness_utxo", i)
					}
				}
			}
			t.Logf("Sparrow %s: GUI address attested and %d own SIGHASH_ALL input signature(s) checked", sequence, len(p.Inputs))
			return encodePSBT(signedPSBT.Raw), source
		}
		if ce != nil && !os.IsNotExist(ce) || se != nil && !os.IsNotExist(se) || sourceErr != nil && !os.IsNotExist(sourceErr) {
			t.Fatal("could not read Sparrow handoff response")
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sparrow %s timed out waiting for GUI files", sequence)
		}
		time.Sleep(time.Second)
	}
}

func sparrowHardwareUpload(t *testing.T, c *shapeBChain, sess, order string, id int64, signed string) {
	t.Helper()
	page := c.e.do("GET", "/order?id="+order, sess, nil)
	if page.Code != 200 {
		t.Fatalf("hardware upload order page: HTTP %d", page.Code)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"csrf": c.e.A.csrf(sess), formNonceField: renderedFormNonce(t, page.Body.String(), "/escrow/hardware-sign"),
		"order_id": order, "settlement_id": strconv.FormatInt(id, 10),
	} {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := mw.CreateFormFile("psbt_file", "sparrow-signed.psbt")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(signed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/escrow/hardware-sign", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "session", Value: sess})
	w := httptest.NewRecorder()
	c.e.A.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatalf("Sparrow hardware settlement refused: HTTP %d", w.Code)
	}
}

func sparrowSettlement(t *testing.T, c *shapeBChain, dir, sequence, order string, id int64, party string, child int, expected string) (string, string) {
	t.Helper()
	sess := c.buyerSess
	if party == roleVendor {
		sess = c.vendorSess
	}
	index, cosigners, first := electrumSetup(t, c, order, sess)
	if child == 0 && first != expected {
		t.Fatal("setup first address differs from escrow address")
	}
	page := c.e.do("GET", "/escrow-setup?id="+order, sess, nil)
	match := sparrowSetupDescriptor.FindStringSubmatch(page.Body.String())
	pathMatch := sparrowSetupDerivation.FindStringSubmatch(page.Body.String())
	if page.Code != 200 || match == nil || pathMatch == nil {
		t.Fatal("the party's Sparrow setup descriptor is unavailable")
	}
	copy := c.e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), sess, nil)
	if copy.Code != 200 {
		t.Fatalf("settlement PSBT download: %d", copy.Code)
	}
	keys, err := c.e.A.settlementKeys(c.ctx, c.e.DB, order, id)
	if err != nil {
		t.Fatal(err)
	}
	var hw []byte
	{
		response := c.e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10)+"&hardware=1", sess, nil)
		if response.Code != 200 || response.Header().Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("hardware PSBT download: %d", response.Code)
		}
		hw = response.Body.Bytes()
		hp, err := decodeHardwarePSBT(encodePSBT(hw))
		if err != nil || len(hp.Inputs) == 0 {
			t.Fatalf("hardware PSBT: %v", err)
		}
		ordinary, err := decodePSBT(strings.TrimSpace(copy.Body.String()))
		if err != nil || hp.TxID() != ordinary.TxID() {
			t.Fatal("hardware copy changes the unsigned transaction")
		}
		for i, input := range hp.Inputs {
			if len(input.NonWitnessUTXO) == 0 || input.WitnessUTXO == nil || len(input.Derivations) != 1 {
				t.Fatalf("hardware input %d lacks funding transaction or owner origin", i)
			}
			id := hp.Tx.In[i].TxID
			slices.Reverse(id[:])
			out, err := fundingTxOutput(hex.EncodeToString(input.NonWitnessUTXO), hex.EncodeToString(id[:]), hp.Tx.In[i].Vout)
			if err != nil || out.Value != input.WitnessUTXO.Value || !bytes.Equal(out.Script, input.WitnessUTXO.Script) {
				t.Fatalf("hardware input %d does not match Core funding transaction: %v", i, err)
			}
		}
	}
	orderPath := html.UnescapeString(pathMatch[1])
	req := sparrowRequest{Party: party, Kind: "settlement", Index: index, Child: child, Address: expected, Path: orderPath + fmt.Sprintf("/0/%d", child), OrderPath: orderPath, Cosigners: cosigners, Descriptor: html.UnescapeString(match[1])}
	signed, source := sparrowHandoff(t, dir, sequence, req, copy.Body.String(), keys[party].PubKey, hw)
	if source == "hardware" {
		sparrowHardwareUpload(t, c, sess, order, id, signed)
	} else if w := c.e.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {signed}}); w.Code != 303 {
		t.Fatalf("Sparrow signed settlement refused: HTTP %d", w.Code)
	}
	return c.settled(id, sequence), source
}

type sparrowOutput struct {
	N       int    `json:"n"`
	Role    string `json:"role"`
	Address string `json:"address"`
	Sats    int64  `json:"sats"`
}

func sparrowPlannedOutputs(t *testing.T, c *shapeBChain, id int64) []sparrowOutput {
	t.Helper()
	rows, err := c.e.DB.Query("SELECT n,role,address,sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var outputs []sparrowOutput
	for rows.Next() {
		var out sparrowOutput
		if err := rows.Scan(&out.N, &out.Role, &out.Address, &out.Sats); err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, out)
	}
	if err := rows.Err(); err != nil || len(outputs) == 0 {
		t.Fatalf("planned outputs unavailable: %v", err)
	}
	return outputs
}

// Read the mined transaction from Core with its block hash, then compare every output to the frozen app plan.
func sparrowConfirmOutputs(t *testing.T, c *shapeBChain, txid string, planned []sparrowOutput) int64 {
	t.Helper()
	var walletTx struct {
		BlockHash     string `json:"blockhash"`
		Confirmations int64  `json:"confirmations"`
	}
	c.call(os.Getenv("ESCROW_BTC_WATCH_WALLET"), "gettransaction", &walletTx, txid, true)
	if walletTx.BlockHash == "" || walletTx.Confirmations < c.e.A.escrow.Load().confirmations {
		t.Fatal("Core has not confirmed the settlement transaction")
	}
	var raw struct {
		TxID string `json:"txid"`
		Vout []struct {
			N            int         `json:"n"`
			Value        json.Number `json:"value"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	c.call("", "getrawtransaction", &raw, txid, true, walletTx.BlockHash)
	if raw.TxID != txid || len(raw.Vout) != len(planned) {
		t.Fatal("Core transaction ID or output count differs from frozen plan")
	}
	for i, out := range raw.Vout {
		value, ok := new(big.Rat).SetString(out.Value.String())
		if !ok {
			t.Fatal("Core output value is not decimal")
		}
		value.Mul(value, big.NewRat(100_000_000, 1))
		if !value.IsInt() || !value.Num().IsInt64() || out.N != planned[i].N || out.ScriptPubKey.Address != planned[i].Address || value.Num().Int64() != planned[i].Sats {
			t.Fatalf("Core output %d differs from frozen plan", i)
		}
	}
	return walletTx.Confirmations
}

func TestEscrowRegtestSparrowManualJourney(t *testing.T) {
	dir := sparrowPrivateDir(t)
	if os.Getenv("ESCROW_SPARROW_VERSION") == "" || !regexp.MustCompile(`^[0-9a-fA-F]{64}$`).MatchString(os.Getenv("ESCROW_SPARROW_DMG_SHA256")) || !regexp.MustCompile(`^[0-9a-fA-F]{40}$`).MatchString(os.Getenv("ESCROW_SPARROW_APP_REVISION")) {
		t.Fatal("set verified Sparrow version, DMG SHA-256, and frozen app revision before the manual journey")
	}
	if realChainNet() != "regtest" {
		t.Skip("Sparrow manual journey requires regtest")
	}
	accountBytes, err := os.ReadFile(filepath.Join(dir, "public-accounts.json"))
	if err != nil {
		t.Fatal("read public Sparrow accounts")
	}
	var accounts map[string]string
	if err := json.Unmarshal(accountBytes, &accounts); err != nil || len(accounts) != 2 {
		t.Fatal("public-accounts.json must map buyer and vendor to public rooted account keys")
	}
	for _, party := range []string{roleBuyer, roleVendor} {
		key, err := parseEscrowKey(accounts[party], keyNetTest)
		if err != nil || !key.BIP48 {
			t.Fatalf("%s has no public BIP48 account key: %v", party, err)
		}
	}
	c := newShapeBChainWithPartyKeys(t, "a458-sparrow", accounts, func(c *shapeBChain) {
		for i, party := range []struct{ name, uid, sess string }{{roleBuyer, c.buyer, c.buyerSess}, {roleVendor, c.vendor, c.vendorSess}} {
			if code, _ := registerKey(c.e, party.sess, accounts[party.name], "layout", "per_order"); code != 303 {
				t.Fatalf("%s registration: HTTP %d", party.name, code)
			}
			psbt, cosigners, address, path := perOrderPractice(t, c.e, party.sess)
			key := mustParseKey(t, accounts[party.name])
			if path != fmt.Sprintf("[%s/48h/1h/0h/2h/2147483647]", key.Fingerprint) {
				t.Fatal("practice uses unexpected reserved path")
			}
			p, err := decodePSBT(psbt)
			if err != nil || len(p.Inputs) != 1 || len(p.Inputs[0].Derivations) == 0 {
				t.Fatalf("practice PSBT missing owner derivation: %v", err)
			}
			var pub []byte
			fp, _ := hex.DecodeString(key.Fingerprint)
			for _, d := range p.Inputs[0].Derivations {
				if bytes.Equal(d.Fingerprint[:], fp) {
					pub = d.PubKey
				}
			}
			if len(pub) == 0 {
				t.Fatal("practice PSBT has no registered owner's derivation")
			}
			req := sparrowRequest{Party: party.name, Kind: "practice", Index: escrowPracticeIndex, Child: 0, Address: address, Path: "m/48h/1h/0h/2h/2147483647/0/0", Cosigners: cosigners}
			signed, _ := sparrowHandoff(t, dir, fmt.Sprintf("seq-%02d%spractice", i+1, party.name), req, psbt, pub, nil)
			if code, _ := submitPractice(c.e, party.sess, signed); code != 303 {
				t.Fatalf("%s Sparrow practice refused: HTTP %d", party.name, code)
			}
			if agInt(c.e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL AND layout='per_order'", party.uid) != 1 {
				t.Fatalf("%s practice did not activate", party.name)
			}
		}
	})
	order, addr := c.paidEscrow()
	var claimTo string
	c.call(c.funder, "getnewaddress", &claimTo)
	if w := c.e.do("POST", "/escrow/claim", c.vendorSess, url.Values{"order_id": {order}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("vendor claim: HTTP %d", w.Code)
	}
	c.pass()
	var release int64
	var state string
	if err := c.e.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='release'", order).Scan(&release, &state); err != nil || state != "awaiting_signature" {
		t.Fatalf("release state %s: %v", state, err)
	}
	releaseFunding := agStrings(t, c.e, "SELECT DISTINCT txid FROM payments WHERE order_id=$1 AND address=$2 ORDER BY txid", order, addr)
	if len(releaseFunding) == 0 {
		t.Fatal("release funding transaction not recorded")
	}
	releaseOutputs := sparrowPlannedOutputs(t, c, release)
	releaseTXID, releaseSource := sparrowSettlement(t, c, dir, "seq-03vendorrelease", order, release, roleVendor, 0, addr)
	releaseConfirmations := sparrowConfirmOutputs(t, c, releaseTXID, releaseOutputs)
	siblingOrder, _ := c.paidEscrow()
	var sibling string
	if err := c.e.DB.QueryRow("SELECT s.address FROM escrow_siblings s JOIN escrows es ON es.id=s.escrow_id WHERE es.order_id=$1 AND s.branch=0 AND s.idx=3", siblingOrder).Scan(&sibling); err != nil {
		t.Fatal(err)
	}
	var deposit string
	c.call(c.funder, "sendtoaddress", &deposit, sibling, json.Number("0.0007"))
	mustMine(t, c.ctx, c.op, int(c.e.A.escrow.Load().confirmations), c.mine)
	c.pass()
	c.pass()
	var refund int64
	if err := c.e.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='stray_refund'", siblingOrder).Scan(&refund, &state); err != nil || state != "awaiting_signature" {
		t.Fatalf("sibling refund state %s: %v", state, err)
	}
	keys, err := c.e.A.settlementKeys(c.ctx, c.e.DB, siblingOrder, refund)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := c.e.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", refund).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	p, err := decodePSBT(stored)
	if err != nil || len(p.Inputs) != 1 || !bytes.Equal(p.Inputs[0].WitnessScript, sortedMultisigScript(keys[roleBuyer].PubKey, keys[roleVendor].PubKey, keys["market"].PubKey)) || !strings.HasSuffix(keys["market"].Suffix, "/0/3") {
		t.Fatal("refund PSBT does not spend the funded sibling script")
	}
	refundOutputs := sparrowPlannedOutputs(t, c, refund)
	refundTXID, refundSource := sparrowSettlement(t, c, dir, "seq-04buyerrefund", siblingOrder, refund, roleBuyer, 3, sibling)
	refundConfirmations := sparrowConfirmOutputs(t, c, refundTXID, refundOutputs)
	c.noSecrets()
	var network struct {
		Version int `json:"version"`
	}
	c.call("", "getnetworkinfo", &network)
	manifest := map[string]any{
		"status": "confirmed", "chain": "regtest", "core_version": network.Version,
		"operator_attested": map[string]any{
			"sparrow_version": os.Getenv("ESCROW_SPARROW_VERSION"), "sparrow_dmg_sha256": os.Getenv("ESCROW_SPARROW_DMG_SHA256"),
			"frozen_app_git_revision": os.Getenv("ESCROW_SPARROW_APP_REVISION"),
			"address_checks":          []string{"buyer practice", "vendor practice", "vendor release /0/0", "buyer refund /0/3"},
			"release_signed_source":   releaseSource, "refund_signed_source": refundSource,
		},
		"machine_checked":                map[string]any{"practice": "buyer and vendor accepted", "signature_shape": "own partial SIGHASH_ALL on every input", "core_transaction_outputs": "all matched frozen plans"},
		"release":                        map[string]any{"status": "confirmed", "txid": releaseTXID, "confirmations": releaseConfirmations, "funding_txids": releaseFunding, "planned_outputs": releaseOutputs},
		"sibling_refund":                 map[string]any{"status": "confirmed", "txid": refundTXID, "confirmations": refundConfirmations, "funding_txids": []string{deposit}, "planned_outputs": refundOutputs},
		"descriptor_gui_import_attested": false, "hardware_device_attested": false,
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sparrowWrite(t, filepath.Join(dir, "manifest.json"), append(b, '\n'))
}
