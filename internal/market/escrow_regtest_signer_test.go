package market

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"opsecmkt/internal/btc"
)

// legLog writes the leg's transcript lines (t.Log) and keeps a copy, so the leg can check at its end that nothing it
// printed holds the signer's seed or a tprv.
type legLog struct {
	t   *testing.T
	mu  sync.Mutex
	buf strings.Builder
}

func (l *legLog) Logf(format string, args ...any) {
	l.t.Helper()
	s := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.buf.WriteString(s + "\n")
	l.mu.Unlock()
	l.t.Log(s)
}

func (l *legLog) Fatalf(format string, args ...any) {
	l.t.Helper()
	l.Logf(format, args...)
	l.t.FailNow()
}

// signerProc is one escrow-signer serve process of the leg, its log in a file.
type signerProc struct {
	cmd    *exec.Cmd
	socket string
	log    string
}

// tprvPattern is a serialized test-network extended private key (111 base58 characters).
var tprvPattern = regexp.MustCompile(`tprv[1-9A-HJ-NP-Za-km-z]{100,}`)

// assertSecretNowhereInDB is assertNowhereInDB for a secret: on a hit it names the column and the label, never the
// needle. Text columns are searched only for a needle that is valid UTF-8 (PostgreSQL text cannot hold anything else);
// bytea columns always, with the needle's bytes.
func assertSecretNowhereInDB(t *testing.T, e *testEnv, needle []byte, label string) {
	t.Helper()
	rows, err := e.DB.Query(`SELECT table_name,column_name,data_type FROM information_schema.columns WHERE table_schema=current_schema() AND data_type IN ('text','character varying','bytea','jsonb','json')`)
	if err != nil {
		t.Fatal(err)
	}
	var cols [][3]string
	for rows.Next() {
		var c [3]string
		rows.Scan(&c[0], &c[1], &c[2])
		cols = append(cols, c)
	}
	rows.Close()
	if len(cols) < 20 {
		t.Fatalf("only %d columns found", len(cols))
	}
	for _, c := range cols {
		var arg any = string(needle)
		cond := `strpos("` + c[1] + `"::text, $1)>0`
		if c[2] == "bytea" {
			arg, cond = needle, `position($1::bytea in "`+c[1]+`")>0`
		} else if !utf8.Valid(needle) {
			continue
		}
		var n int
		if err := e.DB.QueryRow(`SELECT count(*) FROM "`+c[0]+`" WHERE `+cond, arg).Scan(&n); err != nil {
			t.Fatalf("%s.%s: query failed", c[0], c[1])
		}
		if n != 0 {
			t.Fatalf("%s.%s holds the %s", c[0], c[1], label)
		}
	}
}

// TestEscrowRegtestSigner is A-454's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI unless the job's
// node has the signer's user): the escrow signer (cmd/escrow-signer), built from this tree and started with a fresh
// seed in a temporary directory against the regtest node as its own whitelisted RPC user (decodepsbt,
// descriptorprocesspsbt, getdescriptorinfo), co-signs a hand-built shape-(b) escrow through its socket, using the App's
// client. Its account key, which Core computed from the signer's account tprv (getdescriptorinfo, QA4W2-1), agrees with
// Core's derivation from the seed's master key along m/48h/1h/0h/2h (Go's hardened CKDpriv is Core's). Two Core
// wallets hold the parties' BIP48 accounts; the escrow at per-order index 5 is wsh(sortedmulti(2,…)) of the buyer's and
// the vendor's per-order keys (A-453's Go derivation, served rooted) and the signer's account/5/0/0 (its rooted account
// key from info). It is funded on regtest; the signer's signature plus the buyer's (Core descriptorprocesspsbt with the
// buyer's private root) are combined, finalized, accepted by testmempoolaccept, broadcast and mined. Refused, with
// nothing signed: an input marked SIGHASH_NONE, another index, a script holding two of the signer's keys, and two
// escrows in one request. The App pins the signer's fingerprint and key HMAC at startup; restarted against a second
// signer with another seed it reports the pin mismatch (shape (b) unavailable, shape (a) escrow still on, the pins
// unchanged); with the signer stopped, waiting. The seed is backed up to a throwaway operator PGP key, decrypted and
// restored into a new seed file (break-glass: a signer started from the restored seed against the same node signs the
// same PSBT with the identical signature). Afterwards no database column holds the seed or a tprv, and neither the
// signer's logs nor this leg's transcript lines do.
func TestEscrowRegtestSigner(t *testing.T) {
	appURL, opURL, signerURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL"), os.Getenv("ESCROW_REGTEST_SIGNER_RPC_URL")
	if appURL == "" || opURL == "" || signerURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL, ESCROW_REGTEST_OPERATOR_RPC_URL and ESCROW_REGTEST_SIGNER_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	lg := &legLog{t: t}
	ctx := context.Background()
	op, err := newRPCClient("regtest operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newRPCClient("regtest app", appURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := newRPCClient("regtest signer", signerURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*rpcClient{"operator": op, "app": app} {
		checkRealChain(t, ctx, name, c)
	}
	// The signer's user is whitelisted to decodepsbt, descriptorprocesspsbt and getdescriptorinfo only: getblockchaininfo
	// is refused.
	if err := classifyEscrowRPC("getblockchaininfo", sig.call(ctx, "", "getblockchaininfo", []any{}, nil)); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("the signer's RPC user may call getblockchaininfo (%v): whitelist it to decodepsbt,descriptorprocesspsbt,getdescriptorinfo", err)
	}
	lg.Logf("whitelist: the signer's RPC user is refused getblockchaininfo (forbidden)")
	call := func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := op.call(ctx, path, method, params, out); err != nil {
			lg.Fatalf("operator %s: %v", method, err)
		}
	}
	s := randomToken()[:8]
	dir := t.TempDir()

	// Build the signer from this tree and make a fresh seed.
	bin := filepath.Join(dir, "escrow-signer")
	if out, err := exec.Command("go", "build", "-o", bin, "opsecmkt/cmd/escrow-signer").CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/escrow-signer: %v\n%s", err, out)
	}
	seedPath := filepath.Join(dir, "seed1", "seed")
	// init, restore and serve ask Core for the account key (getdescriptorinfo) as the signer's RPC user.
	signerCmd := func(args ...string) *exec.Cmd {
		c := exec.Command(bin, args...)
		c.Env = []string{"ESCROW_SIGNER_RPC_URL=" + signerURL}
		return c
	}
	initOut, err := signerCmd("init", "-seed", seedPath).CombinedOutput()
	if err != nil {
		t.Fatalf("escrow-signer init: %v\n%s", err, initOut)
	}
	seed, err := os.ReadFile(seedPath) // in memory only, for the checks below; never logged
	if err != nil || len(seed) != 32 {
		t.Fatalf("seed file: %d bytes, %v", len(seed), err)
	}
	if st, _ := os.Stat(seedPath); st.Mode().Perm() != 0o400 {
		t.Fatalf("seed file mode %v", st.Mode())
	}
	lg.Logf("escrow-signer init: a new 32-byte seed file, mode 0400:\n%s", strings.TrimSpace(string(initOut)))
	masterTprv := func(seed []byte) string { // BIP32 master key, as Core reads it; only ever sent to the operator's RPC
		m := hmac.New(sha512.New, []byte("Bitcoin seed"))
		m.Write(seed)
		sum := m.Sum(nil)
		raw := binary.BigEndian.AppendUint32(nil, 0x04358394)
		raw = append(raw, make([]byte, 9)...)
		raw = append(raw, sum[32:]...)
		raw = append(append(raw, 0), sum[:32]...)
		return btc.Base58CheckEncode(raw)
	}(seed)

	var procs []*signerProc
	start := func(name, seedFile string) *signerProc {
		t.Helper()
		p := &signerProc{socket: filepath.Join(dir, name+".sock"), log: filepath.Join(dir, name+".log")}
		logf, err := os.Create(p.log)
		if err != nil {
			t.Fatal(err)
		}
		p.cmd = signerCmd("serve", "-seed", seedFile, "-socket", p.socket)
		p.cmd.Stdout, p.cmd.Stderr = logf, logf
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, p)
		for range 100 {
			if c, err := net.Dial("unix", p.socket); err == nil {
				c.Close()
				st, _ := os.Lstat(p.socket)
				lg.Logf("%s: escrow-signer serve listening on its socket (mode %04o)", name, st.Mode().Perm())
				return p
			}
			time.Sleep(50 * time.Millisecond)
		}
		b, _ := os.ReadFile(p.log)
		lg.Fatalf("%s did not start: %s", name, b)
		return nil
	}
	stop := func(p *signerProc) {
		if p.cmd.ProcessState == nil {
			p.cmd.Process.Signal(syscall.SIGTERM)
			p.cmd.Wait()
		}
	}
	t.Cleanup(func() {
		for _, p := range procs {
			stop(p)
		}
	})
	signer1 := start("signer1", seedPath)

	// The App: escrow on (the Core arbiter wallet for shape (a)) and ESCROW_SIGNER_SOCKET at signer1.
	arbiter, watch, miner, buyer, vendor := "a454-arbiter-"+s, "a454-watch-"+s, "a454-miner-"+s, "a454-buyer-"+s, "a454-vendor-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, miner)
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	t.Setenv("ESCROW_SIGNER_SOCKET", signer1.socket)
	e := newTestApp(t)
	if e.A.escrow.Load() == nil || e.A.escrowSignerState() != signerReady {
		lg.Fatalf("startup: escrow enabled %v, signer state %q", e.A.escrow.Load() != nil, e.A.escrowSignerState())
	}
	pins := agStrings(t, e, "SELECT key||'='||value FROM settings WHERE key IN ($1,$2) ORDER BY key", escrowSignerFingerprintPin, escrowSignerXPubPin)
	info, err := e.A.escrowSigner.info(ctx)
	if err != nil || len(pins) != 2 || pins[0] != escrowSignerFingerprintPin+"="+info.Fingerprint {
		lg.Fatalf("pins %v, info %v", pins, err)
	}
	signerTpub := info.Key.Rooted(btc.VersionTpub)
	lg.Logf("App startup: signer ready; pinned %s and %s (64 hex); signer account %s… fingerprint %s", pins[0], escrowSignerXPubPin, signerTpub[:24], info.Fingerprint)

	// The signer's account key (from info: Core's getdescriptorinfo of the account tprv Go derived with hardened
	// CKDpriv) agrees with Core's own derivation from the seed's master tprv along m/48h/1h/0h/2h.
	const index = 5
	esc := e.A.escrow.Load()
	derive := func(desc string, operator bool) string {
		t.Helper()
		var addrs []string
		if operator {
			call("", "deriveaddresses", &addrs, withChecksum(desc))
		} else if addrs, err = esc.deriveAddresses(ctx, withChecksum(desc), -1); err != nil {
			lg.Fatalf("deriveaddresses: %v", err)
		}
		if len(addrs) != 1 {
			lg.Fatalf("deriveaddresses: %d addresses", len(addrs))
		}
		return addrs[0]
	}
	for _, path := range []string{"/5/0/0", "/5/1/19", "/1000/0/0"} {
		fromAccount := derive("wpkh("+signerTpub+path+")", false)
		fromSeed := derive("wpkh("+masterTprv+"/48h/1h/0h/2h"+path+")", true)
		if fromAccount != fromSeed {
			lg.Fatalf("account%s: the signer's account key gives %s, Core from the seed %s", path, fromAccount, fromSeed)
		}
		lg.Logf("account%s: %s from the signer's rooted account key and from the seed's master key along m/48h/1h/0h/2h", path, fromAccount)
	}

	// The parties' BIP48 accounts (A-453's leg does the same) and their per-order keys at index 5, served rooted.
	type party struct {
		root string
		key  escrowKey
	}
	account := func(name string) party {
		t.Helper()
		call("", "createwallet", nil, name)
		var priv struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &priv, true)
		var root string
		for _, d := range priv.Descriptors {
			if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
				root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
			}
		}
		d := withChecksum("wpkh(" + root + "/48h/1h/0h/2h/0/*)")
		var imported []struct {
			Success bool `json:"success"`
		}
		call(name, "importdescriptors", &imported, []map[string]any{{"desc": d, "active": false, "range": []int{0, 2}, "timestamp": "now"}})
		var pub struct {
			Descriptors []struct {
				Desc string `json:"desc"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &pub)
		for _, d := range pub.Descriptors {
			if strings.Contains(d.Desc, "/48h/1h/0h/2h]") {
				k, err := parseEscrowKey(strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0])
				if err != nil {
					t.Fatal(err)
				}
				return party{root, k}
			}
		}
		t.Fatalf("%s: no BIP48 account", name)
		return party{}
	}
	pb, pv := account(buyer), account(vendor)
	rootedOrderKey := func(p party) string {
		t.Helper()
		child, used, err := deriveOrderKey(p.key, index)
		if err != nil || used != index {
			t.Fatal(err)
		}
		return child.Rooted(btc.VersionTpub)
	}
	buyerKey, vendorKey := rootedOrderKey(pb), rootedOrderKey(pv)
	signerKeyAt := func(chain, i uint32) string {
		return signerTpub + "/" + strconv.Itoa(index) + "/" + strconv.Itoa(int(chain)) + "/" + strconv.Itoa(int(i))
	}
	escrowDesc := func(keys ...string) string { return "wsh(sortedmulti(2," + strings.Join(keys, ",") + "))" }
	descA := escrowDesc(buyerKey+"/0/0", vendorKey+"/0/0", signerKeyAt(0, 0))
	descB := escrowDesc(buyerKey+"/0/0", signerKeyAt(0, 0), signerKeyAt(0, 1)) // two of the signer's keys
	descC := escrowDesc(buyerKey+"/0/0", vendorKey+"/0/0", signerKeyAt(1, 3))  // the same order's sibling escrow
	addrA, addrB, addrC := derive(descA, false), derive(descB, false), derive(descC, false)
	lg.Logf("shape-(b) escrow at index %d: %s = wsh(sortedmulti(2, buyer/0/0, vendor/0/0, signer account/%d/0/0)), rooted per-order keys", index, addrA, index)

	// Fund A twice, B and C in one transaction.
	var mine string
	call(miner, "getnewaddress", &mine)
	matureFunds(t, ctx, op, mine)
	// The regtest subsidy halves every 150 blocks and the other legs mine many: mine until the wallet can pay.
	for range 40 {
		var balance float64
		if call(miner, "getbalance", &balance); balance >= 0.02 {
			break
		}
		mustMine(t, ctx, op, 25, mine)
	}
	var fundTxid string
	call(miner, "sendmany", &fundTxid, "", map[string]any{addrA: 0.005, addrB: 0.003, addrC: 0.002})
	call(miner, "sendtoaddress", nil, addrA, 0.004)
	mustMine(t, ctx, op, 1, mine)
	var unspent []struct {
		Txid    string  `json:"txid"`
		Vout    int     `json:"vout"`
		Address string  `json:"address"`
		Amount  float64 `json:"amount"`
	}
	scan := func() {
		var res struct {
			Unspents []struct {
				Txid   string  `json:"txid"`
				Vout   int     `json:"vout"`
				Desc   string  `json:"desc"`
				Amount float64 `json:"amount"`
			} `json:"unspents"`
		}
		call("", "scantxoutset", &res, "start", []string{"addr(" + addrA + ")", "addr(" + addrB + ")", "addr(" + addrC + ")"})
		unspent = unspent[:0]
		for _, u := range res.Unspents {
			a := ""
			for _, x := range []string{addrA, addrB, addrC} {
				if strings.Contains(u.Desc, x) {
					a = x
				}
			}
			unspent = append(unspent, struct {
				Txid    string  `json:"txid"`
				Vout    int     `json:"vout"`
				Address string  `json:"address"`
				Amount  float64 `json:"amount"`
			}{u.Txid, u.Vout, a, u.Amount})
		}
	}
	scan()
	utxo := func(addr string, amount float64) (string, int) {
		for _, u := range unspent {
			if u.Address == addr && u.Amount == amount {
				return u.Txid, u.Vout
			}
		}
		lg.Fatalf("no %v BTC output at %s", amount, addr)
		return "", 0
	}
	lg.Logf("funded on regtest: A 0.005 and 0.004, B 0.003, C 0.002 BTC (txid %s and one more), 1 confirmation", fundTxid)

	// psbt spends the given outputs to the miner, with each escrow's descriptor for utxoupdatepsbt (public keys only).
	psbt := func(descs []string, ins ...[2]any) string {
		t.Helper()
		var inputs []map[string]any
		var total float64
		for _, in := range ins {
			txid, vout := utxo(in[0].(string), in[1].(float64))
			inputs = append(inputs, map[string]any{"txid": txid, "vout": vout})
			total += in[1].(float64)
		}
		var p string
		call("", "createpsbt", &p, inputs, []map[string]any{{mine: fmt.Sprintf("%.8f", total-0.0002)}})
		var withChecksums []string
		for _, d := range descs {
			withChecksums = append(withChecksums, withChecksum(d))
		}
		var up string
		call("", "utxoupdatepsbt", &up, p, withChecksums)
		return up
	}
	type decodedSigs struct {
		Inputs []struct {
			PartialSignatures map[string]string `json:"partial_signatures"`
			Sighash           string            `json:"sighash"`
		} `json:"inputs"`
	}
	decode := func(p string) decodedSigs {
		t.Helper()
		var d decodedSigs
		call("", "decodepsbt", &d, p)
		return d
	}
	signerPub := func(chain, i uint32) string {
		k := info.Key
		for _, step := range []uint32{index, chain, i} {
			k, _, _ = btc.CKDPub(k, step)
		}
		return hex.EncodeToString(k.PubKey[:])
	}
	refused := func(name string, p string, i uint32, want string) {
		t.Helper()
		out, err := e.A.escrowSigner.sign(ctx, p, i)
		var r *signerRefusal
		if out != "" || !errors.As(err, &r) || r.Class != want {
			lg.Fatalf("%s: %q, %v (want refused %s)", name, out, err, want)
		}
		lg.Logf("refused through the socket, nothing signed: %s -> %s", name, want)
	}

	// The happy path: the signer co-signs A through the socket, the buyer signs with Core, combine, finalize, accept,
	// broadcast, mine.
	pA := psbt([]string{descA}, [2]any{addrA, 0.005})
	signed, err := e.A.escrowSigner.sign(ctx, pA, index)
	if err != nil {
		lg.Fatalf("sign A: %v", err)
	}
	if d := decode(signed); len(d.Inputs) != 1 || len(d.Inputs[0].PartialSignatures) != 1 || d.Inputs[0].PartialSignatures[signerPub(0, 0)] == "" {
		lg.Fatalf("signer's result: %+v", d)
	}
	lg.Logf("signer co-signed A at index %d: one partial signature, under account/%d/0/0 (%s…)", index, index, signerPub(0, 0)[:16])
	hexKey := func(rooted string) string {
		k, err := btc.ParseExtPub(rooted, btc.VersionTpub)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range []uint32{0, 0} {
			k, _, _ = btc.CKDPub(k, step)
		}
		return hex.EncodeToString(k.PubKey[:])
	}
	var partySigned struct {
		PSBT string `json:"psbt"`
	}
	call("", "descriptorprocesspsbt", &partySigned, pA, []string{withChecksum(escrowDesc(pb.root+"/48h/1h/0h/2h/"+strconv.Itoa(index)+"/0/0", hexKey(vendorKey), signerPub(0, 0)))}, "ALL", false, false)
	if d := decode(partySigned.PSBT); len(d.Inputs[0].PartialSignatures) != 1 || d.Inputs[0].PartialSignatures[hexKey(buyerKey)] == "" {
		lg.Fatalf("buyer's signature: %+v", d)
	}
	var combined string
	call("", "combinepsbt", &combined, []string{signed, partySigned.PSBT})
	var fin struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	call("", "finalizepsbt", &fin, combined)
	if !fin.Complete {
		lg.Fatalf("finalizepsbt: not complete")
	}
	var accept []struct {
		Allowed bool   `json:"allowed"`
		Txid    string `json:"txid"`
	}
	call("", "testmempoolaccept", &accept, []string{fin.Hex})
	if len(accept) != 1 || !accept[0].Allowed {
		lg.Fatalf("testmempoolaccept: %+v", accept)
	}
	var txid string
	call("", "sendrawtransaction", &txid, fin.Hex)
	mined := mustMine(t, ctx, op, 1, mine)
	var block struct {
		Tx []string `json:"tx"`
	}
	call("", "getblock", &block, mined[0])
	found := false
	for _, x := range block.Tx {
		found = found || x == txid
	}
	if !found || txid != accept[0].Txid {
		lg.Fatalf("settlement %s not mined in %s", txid, mined[0])
	}
	lg.Logf("buyer signed with Core; combined, finalized, testmempoolaccept allowed, broadcast and mined: txid %s in block %s", txid, mined[0])

	// Refusals on real Core, nothing signed.
	scan()
	pA2 := psbt([]string{descA}, [2]any{addrA, 0.004})
	var noneSigned struct {
		PSBT string `json:"psbt"`
	}
	call("", "descriptorprocesspsbt", &noneSigned, pA2, []string{withChecksum(escrowDesc(pb.root+"/48h/1h/0h/2h/"+strconv.Itoa(index)+"/0/0", hexKey(vendorKey), signerPub(0, 0)))}, "NONE", false, false)
	if d := decode(noneSigned.PSBT); d.Inputs[0].Sighash != "NONE" {
		lg.Fatalf("the buyer's NONE signature did not mark the input: %+v", d)
	}
	refused("an input marked SIGHASH_NONE (the buyer signed NONE with Core)", noneSigned.PSBT, index, "sighash")
	refused("another index (6 for an escrow of index 5)", pA2, index+1, "key-not-found")
	refused("a script holding the signer's account/5/0/0 and account/5/0/1", psbt([]string{descB}, [2]any{addrB, 0.003}), index, "two-signer-keys")
	refused("two escrows (A and its sibling C) in one request", psbt([]string{descA, descC}, [2]any{addrA, 0.004}, [2]any{addrC, 0.002}), index, "mixed-scripts")

	// Backup to a throwaway operator key, decrypt, restore into a new seed file; a signer from the restored seed signs A2
	// with the same signature as signer1 (RFC 6979: the same key and message give the same signature).
	operator, err := openpgp.NewEntity("Throwaway operator", "", "operator@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var pub bytes.Buffer
	w, _ := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	operator.Serialize(w)
	w.Close()
	keyFile := filepath.Join(dir, "operator.asc")
	os.WriteFile(keyFile, pub.Bytes(), 0o644)
	backupCmd := exec.Command(bin, "backup", "-seed", seedPath, "-key", keyFile)
	var backup, backupErr bytes.Buffer
	backupCmd.Stdout, backupCmd.Stderr = &backup, &backupErr
	if err := backupCmd.Run(); err != nil {
		lg.Fatalf("backup: %v %s", err, backupErr.String())
	}
	blk, err := armor.Decode(bytes.NewReader(backup.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	md, err := openpgp.ReadMessage(blk.Body, openpgp.EntityList{operator}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(md.UnverifiedBody)
	restoredPath := filepath.Join(dir, "seed3", "seed")
	restore := signerCmd("restore", "-seed", restoredPath, "-fingerprint", info.Fingerprint)
	restore.Stdin = bytes.NewReader(plain)
	restoreOut, err := restore.CombinedOutput()
	if err != nil {
		lg.Fatalf("restore: %v %s", err, restoreOut)
	}
	lg.Logf("backup: %d bytes of armored PGP MESSAGE (%s); restored with -fingerprint %s:\n%s", backup.Len(), strings.TrimSpace(backupErr.String()), info.Fingerprint, strings.TrimSpace(string(restoreOut)))
	sig1, err := e.A.escrowSigner.sign(ctx, pA2, index)
	if err != nil {
		lg.Fatalf("signer1 on A2: %v", err)
	}
	signer3 := start("signer3", restoredPath)
	sig3, err := (&escrowSignerClient{socket: signer3.socket}).sign(ctx, pA2, index)
	if err != nil || decode(sig3).Inputs[0].PartialSignatures[signerPub(0, 0)] != decode(sig1).Inputs[0].PartialSignatures[signerPub(0, 0)] {
		lg.Fatalf("the restored signer's signature differs: %v", err)
	}
	lg.Logf("break-glass rehearsal: a signer started from the restored seed signed A2 with the identical signature")
	stop(signer3)

	// Pin mismatch: the App restarted against a second signer with another seed.
	seed2 := filepath.Join(dir, "seed2", "seed")
	if out, err := signerCmd("init", "-seed", seed2).CombinedOutput(); err != nil {
		lg.Fatalf("init seed2: %v %s", err, out)
	}
	signer2 := start("signer2", seed2)
	t.Setenv("ESCROW_SIGNER_SOCKET", signer2.socket)
	logs := captureLog(t)
	e.restart()
	after := agStrings(t, e, "SELECT key||'='||value FROM settings WHERE key IN ($1,$2) ORDER BY key", escrowSignerFingerprintPin, escrowSignerXPubPin)
	if e.A.escrowSignerState() != signerMismatch || e.A.escrow.Load() == nil || strings.Join(after, ",") != strings.Join(pins, ",") || !strings.Contains(logs.String(), escrowLogSignerMismatch) {
		lg.Fatalf("against another signer: state %q, escrow on %v, pins %v", e.A.escrowSignerState(), e.A.escrow.Load() != nil, after)
	}
	lg.Logf("App restarted against a signer with another seed: %q (shape (b) unavailable), shape (a) escrow still on, the pins unchanged; log: %s", signerMismatch, escrowLogSignerMismatch)

	// Back to signer1, then signer1 stopped: waiting.
	t.Setenv("ESCROW_SIGNER_SOCKET", signer1.socket)
	e.restart()
	if e.A.escrowSignerState() != signerReady {
		lg.Fatalf("back on signer1: %q", e.A.escrowSignerState())
	}
	stop(signer1)
	e.A.checkEscrowSigner(ctx)
	if _, err := e.A.escrowSigner.sign(ctx, pA2, index); e.A.escrowSignerState() != signerWaiting || !errors.Is(err, errSignerUnreachable) {
		lg.Fatalf("signer down: state %q, sign %v", e.A.escrowSignerState(), err)
	}
	lg.Logf("signer1 stopped: the App's client reports %q and a sign request errSignerUnreachable", signerWaiting)
	stop(signer2)

	// No seed or tprv anywhere: the database, the signers' logs, this leg's transcript lines.
	for label, needle := range map[string][]byte{"seed (hex)": []byte(hex.EncodeToString(seed)), "seed (raw)": seed, "master tprv": []byte(masterTprv), "tprv prefix": []byte("tprv")} {
		assertSecretNowhereInDB(t, e, needle, label)
	}
	for _, p := range procs {
		b, _ := os.ReadFile(p.log)
		if tprvPattern.Match(b) || bytes.Contains(b, []byte(hex.EncodeToString(seed))) || bytes.Contains(b, seed) || bytes.Contains(b, []byte(masterTprv)) {
			lg.Fatalf("%s's log holds key material", filepath.Base(p.log))
		}
		lg.Logf("%s (no key material):\n%s", filepath.Base(p.log), strings.TrimSpace(string(b)))
	}
	if tr := lg.buf.String(); tprvPattern.MatchString(tr) || strings.Contains(tr, hex.EncodeToString(seed)) {
		t.Fatal("the leg's transcript lines hold key material")
	}
	lg.Logf("no column of the database holds the seed (hex or raw), the master tprv or any \"tprv\"; neither do the signers' logs or this leg's transcript lines")
}
