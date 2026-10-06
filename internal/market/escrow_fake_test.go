package market

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeEscrowCore is a scripted Bitcoin Core for escrow DB tests (simulated RPC, not chain evidence). It keeps
// Core's semantics where the escrow code depends on them: getdescriptorinfo returns a public form, deriveaddresses
// needs a checksum, createpsbt and utxoupdatepsbt build real PSBT bytes, descriptorprocesspsbt signs with the
// descriptor's private key only, and finalizepsbt completes only with two valid signatures, or when the input is
// already finalized (Core trusts final fields). Its "signatures" are keyed hashes shaped as DER: fakeSig.
type fakeEscrowCore struct {
	t  *testing.T
	mu sync.Mutex

	chain     string
	arbiterFP string
	// A-439: what the sync guard reads. getblockchaininfo reports ibd, headers (0 = blocks), tipTime (0 = omitted) and
	// challenge (signet_challenge, omitted when ""); getnetworkinfo reports peers as connections.
	ibd           bool
	headers       int64
	tipTime       int64
	challenge     string
	peers         int64
	blocks        int64 // getblockchaininfo blocks (0 = 200)
	arbiterDesc   string
	arbiterCode   int    // JSON-RPC error for getrawchangeaddress (0 = none)
	prevoutExists bool   // gettxout finds the practice outpoint
	allowed       bool   // testmempoolaccept
	throwawayDone bool   // descriptorprocesspsbt answers complete=true (a node that finalized on the server key alone)
	beforeMempool func() // runs when testmempoolaccept is called: the last RPC of a practice check, before its update
	reject        string
	calls         []fakeCall

	// QA4W1-1: getnetworkinfo fails with netInfoErr (a JSON-RPC error, 0 = none) or, with netInfoForbidden, HTTP 403 (a
	// node whose rpcwhitelist lacks it).
	netInfoErr       int
	netInfoForbidden bool

	// A-441: getnetworkinfo's version (0 = 310100) and, per network (ipv4, ipv6, onion, i2p, cjdns), its proxy ("" = none)
	// and whether onion is reachable, as Core 31.1 reports them: -proxy sets the proxy of every network and makes onion
	// reachable; -onion alone sets only onion's. With private set, sendrawtransaction answers the txid and keeps the
	// transaction queued, as -privatebroadcast does with a dead proxy (probed on Core 31.1 regtest): it is not in the
	// mempool, so gettransaction does not know it and its inputs stay unspent; a resend answers the txid again.
	netVersion     int64
	netProxy       map[string]string
	onionReachable bool
	private        bool
	queued         map[string]string // txid -> raw transaction queued by a private broadcast

	// Escrow P1-b: the watch-only wallet, its imports and the deposits it reports.
	watchInfo    map[string]any      // getwalletinfo of the watch wallet (nil = not loaded, -18)
	importFail   bool                // importdescriptors answers success=false
	beforeImport func()              // runs when importdescriptors is called, before it answers
	beforeCreate func()              // runs when createpsbt is called (an escrow build), before it answers
	beforeDerive func()              // runs when deriveaddresses is called, before it answers
	bareAddrSalt string              // a non-empty salt makes deriveaddresses of a bare-key escrow disagree (a lying node)
	infoSkew     bool                // getdescriptorinfo answers another checksum than BIP 380's
	imports      []string            // descriptors imported into the watch wallet, in order
	spk          map[string][]byte   // address -> scriptPubKey, for createpsbt outputs to escrow addresses
	deposits     []fakeEscrowDeposit // what listreceivedbyaddress/gettransaction report
	txErr        int                 // JSON-RPC error for listreceivedbyaddress (0 = none)

	// Escrow P1-c1: settlement builds. gettxout reports each deposit as unspent (with its confirmations, value and
	// address) until it is in spent; utxoupdatepsbt fills a witness UTXO for an input spending a deposit.
	spent      map[string]bool      // "txid:vout" spent outpoints
	beforeUTXO func(string, int64)  // runs when gettxout is called (txid, vout), before it reads spent (A-356 races)
	utxoSkew   func(map[string]any) // edits a gettxout answer (a lying or reorged node)
	createSkew func(string) string  // edits createpsbt's answer
	updateSkew func(string) string  // edits utxoupdatepsbt's answer

	// Escrow P1-c2: the arbiter wallet's signature, the final transaction and its broadcast. walletprocesspsbt on the
	// arbiter wallet signs each input carrying a derivation from the arbiter fingerprint (unless arbiterShort: its key
	// range does not reach the child, until keypoolrefill); combinepsbt merges partial signatures; finalizepsbt builds
	// the real segwit transaction from two valid signatures per input; sendrawtransaction records it and spends its
	// inputs; gettransaction reports it with txConfs and conflicts.
	arbiterShort     bool                // the arbiter's key range is short of the market child (getaddressinfo ismine false)
	keypoolStuck     bool                // keypoolrefill does not extend the arbiter's key range
	arbiterTxCount   int64               // getwalletinfo txcount of the arbiter wallet
	arbiterEncrypted bool                // getwalletinfo of the arbiter wallet has unlocked_until
	marketSkew       func(string) string // edits walletprocesspsbt's answer
	finalSkew        func(string) string // edits finalizepsbt's raw transaction
	mempoolReject    string              // testmempoolaccept refuses a settlement transaction with this reason
	sendErr          int                 // JSON-RPC error for sendrawtransaction (0 = none)
	beforeSend       func()              // runs when sendrawtransaction is called, before it answers
	broadcast        map[string]string   // txid -> raw transaction sent
	txConfs          map[string]int64    // txid -> confirmations of a sent transaction
	conflicts        map[string][]string // txid -> walletconflicts of a sent transaction
	walletForgets    bool                // gettransaction does not know sent transactions (-5), as a wallet that missed one

	// Escrow P2-d: replacement (BIP125, as Core's mempool applies it). A sent transaction whose inputs a sent, still
	// unconfirmed transaction spends replaces it when it pays a higher fee by at least 1 sat/vB of its size; the two
	// then list each other in walletconflicts and the replaced one is refused if sent again (-26).
	spentBy  map[string]string // "txid:vout" -> the sent transaction spending it
	replaced map[string]bool   // txid -> replaced in the mempool by a sent transaction

	// Escrow P1-d1: reconciliation. The watch-only wallet's address book (label per imported address, as
	// importdescriptors with a label writes it), listed by listreceivedbyaddress with include_empty; listtransactions
	// lists deposits as "receive" and every transaction in broadcast (the market's or anyone's) as "send".
	labels    map[string]string // address -> label
	listTxErr int               // JSON-RPC error for listtransactions (0 = none)

	// Escrow P1-d3 (A-359): getaddressinfo on the watch-only wallet reports an address it holds (labelled by an import)
	// as ismine with its label, as Core 31.1 does for an imported watch-only descriptor; watchAll makes it report every
	// address so, and watchInfoErr fails the call.
	watchAll     bool
	watchInfoErr int

	// Escrow P2-e (A-366): for an address an import put in the watch-only wallet, getaddressinfo also reports its
	// scriptPubKey, its three public keys in script order and its descriptor written as multi(…) with each key's own
	// fingerprint, as Core 31.1 does (probed on regtest); any other address it holds reports only its scriptPubKey.
	watchPubkeys map[string][]string // address -> its keys in script order
}

// depositAt is the deposit the wallet reports at txid:vout.
func (c *fakeEscrowCore) depositAt(txid string, vout int64) (fakeEscrowDeposit, bool) {
	for _, d := range c.deposits {
		if d.TxID == txid && d.Vout == vout {
			return d, true
		}
	}
	return fakeEscrowDeposit{}, false
}

// spend marks txid:vout spent, as a transaction spending it would.
func (c *fakeEscrowCore) spend(txid string, vout int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spent == nil {
		c.spent = map[string]bool{}
	}
	c.spent[txid+":"+strconv.FormatInt(vout, 10)] = true
	delete(c.spentBy, txid+":"+strconv.FormatInt(vout, 10)) // spent by a transaction the fake never sent
}

// count is how many times method was called.
func (c *fakeEscrowCore) count(method string) int {
	return len(slices.DeleteFunc(c.methods(), func(m string) bool { return m != method }))
}

// countWith is how many times method was called with params containing part.
func (c *fakeEscrowCore) countWith(method, part string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		if call.Method == method && strings.Contains(string(call.Params), part) {
			n++
		}
	}
	return n
}

// fakeEscrowDeposit is one output the watch wallet reports as received.
type fakeEscrowDeposit struct {
	Address, TxID string
	Vout          int64
	Sats          int64
	Confs         int64
}

const fakeWatchWallet = "/wallet/opsecmkt-escrow-watch"

// deposit makes the watch wallet report an output to addr.
func (c *fakeEscrowCore) deposit(addr, txid string, vout, sats, confs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deposits = append(c.deposits, fakeEscrowDeposit{addr, txid, vout, sats, confs})
}

// setConfs sets the confirmations of every output of txid.
func (c *fakeEscrowCore) setConfs(txid string, confs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.deposits {
		if c.deposits[i].TxID == txid {
			c.deposits[i].Confs = confs
		}
	}
}

func (c *fakeEscrowCore) importedDescriptors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.imports)
}

type fakeCall struct {
	Path, Method string
	Params       json.RawMessage
}

const fakeArbiterFP = "a1b2c3d4"

func newFakeEscrowCore(t *testing.T) (*fakeEscrowCore, *payRPCServer) {
	c := &fakeEscrowCore{t: t, chain: "regtest", arbiterFP: fakeArbiterFP, reject: "missing-inputs", spk: map[string][]byte{},
		watchInfo: map[string]any{"walletname": "opsecmkt-escrow-watch", "descriptors": true, "private_keys_enabled": false}}
	c.arbiterDesc = "wpkh([" + fakeArbiterFP + "/84h/1h/0h]" + testBIP84Key[20:] + "/1/*)#fakechk0"
	srv := newPayRPCServer(t, "escrowapp", "escrow-rpc-pass", false, c.handle)
	srv.mu.Lock()
	srv.forbid = func(method string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return method == "getnetworkinfo" && c.netInfoForbidden
	}
	srv.mu.Unlock()
	return c, srv
}

func (c *fakeEscrowCore) methods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var m []string
	for _, call := range c.calls {
		m = append(m, call.Method)
	}
	return m
}

var fakeTprv = regexp.MustCompile(`tprv[1-9A-HJ-NP-Za-km-z]+`)

func fakePubFromTprv(tprv string) []byte {
	h := sha256.Sum256([]byte("pub:" + tprv))
	return append([]byte{3}, h[:]...)
}

// fakeChild is the fake child key of "tpub…/suffix": a point on the curve, as Core's would be.
func fakeChild(expr string) []byte {
	for i := 0; ; i++ {
		h := sha256.Sum256([]byte("child:" + expr + strings.Repeat("'", i)))
		if pub := append([]byte{2}, h[:]...); onCurve(pub) {
			return pub
		}
	}
}

// fakeSig is the fake Core's signature of txid by pub with sighash byte sh: valid DER shape, sighash byte last.
func fakeSig(pub []byte, txid string, sh byte) []byte {
	r := sha256.Sum256(append([]byte("r"+txid), pub...))
	s := sha256.Sum256(append([]byte("s"+txid+string(rune(sh))), pub...))
	r[0], s[0] = r[0]&0x7f|1, s[0]&0x7f|1
	sig := append([]byte{0x30, 0x44, 0x02, 0x20}, r[:]...)
	sig = append(append(sig, 0x02, 0x20), s[:]...)
	return append(sig, sh)
}

func (c *fakeEscrowCore) handle(path, method string, params json.RawMessage) (any, int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fakeCall{path, method, params})
	var p []json.RawMessage
	json.Unmarshal(params, &p)
	str := func(i int) string {
		var s string
		if i < len(p) {
			json.Unmarshal(p[i], &s)
		}
		return s
	}
	wallet := path == "/wallet/opsecmkt-arbiter"
	switch method {
	case "getblockchaininfo":
		blocks := c.blocks
		if blocks == 0 {
			blocks = 200
		}
		info := map[string]any{"chain": c.chain, "blocks": blocks, "headers": max(c.headers, blocks), "initialblockdownload": c.ibd}
		if c.tipTime != 0 {
			info["time"] = c.tipTime
		}
		if c.challenge != "" {
			info["signet_challenge"] = c.challenge
		}
		return info, 0, ""
	case "getnetworkinfo":
		if c.netInfoErr != 0 {
			return nil, c.netInfoErr, "Method not found"
		}
		version := c.netVersion
		if version == 0 {
			version = 310100
		}
		var networks []map[string]any
		for _, n := range []string{"ipv4", "ipv6", "onion", "i2p", "cjdns"} {
			reachable := n == "ipv4" || n == "ipv6" || n == "onion" && c.onionReachable
			networks = append(networks, map[string]any{"name": n, "limited": !reachable, "reachable": reachable, "proxy": c.netProxy[n], "proxy_randomize_credentials": c.netProxy[n] != ""})
		}
		return map[string]any{"version": version, "connections": c.peers, "networks": networks}, 0, ""
	case "getrawchangeaddress":
		if !wallet {
			return nil, -18, "Requested wallet does not exist or is not loaded"
		}
		if c.arbiterCode != 0 {
			return nil, c.arbiterCode, "Error: This wallet has no available keys"
		}
		return "bcrt1qarbiterchange000000000000000000000", 0, ""
	case "getaddressinfo":
		if path == fakeWatchWallet {
			if c.watchInfoErr != 0 {
				return nil, c.watchInfoErr, "fake getaddressinfo failure"
			}
			labels := []string{}
			if l := c.labels[str(0)]; l != "" {
				labels = append(labels, l)
			}
			info := map[string]any{"address": str(0), "ismine": c.watchAll || len(labels) > 0, "iswatchonly": false, "labels": labels}
			if len(labels) > 0 {
				spk := c.spk[str(0)]
				if spk == nil {
					spk, _ = escrowAddressScript(c.chain, str(0))
				}
				info["scriptPubKey"] = hex.EncodeToString(spk)
				if keys := c.watchPubkeys[str(0)]; keys != nil {
					var parts []string
					for _, k := range keys {
						raw, _ := hex.DecodeString(k)
						h := sha256.Sum256(raw)
						parts = append(parts, "["+hex.EncodeToString(h[:4])+"]"+k)
					}
					desc := "wsh(multi(2," + strings.Join(parts, ",") + "))"
					sum, _ := descChecksum(desc)
					info["pubkeys"], info["desc"], info["solvable"], info["script"] = keys, desc+"#"+sum, true, "multisig"
				}
			}
			return info, 0, ""
		}
		if str(0) != "bcrt1qarbiterchange000000000000000000000" {
			// P1-c2: a market child key's address is the arbiter's while its key range reaches the child.
			return map[string]any{"ismine": wallet && !c.arbiterShort}, 0, ""
		}
		return map[string]any{"ismine": true, "hdmasterfingerprint": c.arbiterFP, "parent_desc": c.arbiterDesc}, 0, ""
	case "getdescriptorinfo":
		desc := str(0)
		if a, ok := strings.CutPrefix(desc, "addr("); ok && !strings.HasPrefix(a, escrowChains[c.chain]+"1") {
			return nil, -5, "Address is not valid"
		}
		public := fakeTprv.ReplaceAllStringFunc(desc, func(k string) string { return "fakepub" + hex.EncodeToString(fakePubFromTprv(k)) })
		sum, _ := descChecksum(public)
		if c.infoSkew {
			sum = "qqqqqqqq"
		}
		return map[string]any{"descriptor": public + "#" + sum, "checksum": sum, "hasprivatekeys": public != desc}, 0, ""
	case "deriveaddresses":
		if c.beforeDerive != nil {
			c.mu.Unlock()
			c.beforeDerive()
			c.mu.Lock()
		}
		desc := str(0)
		body, sum, ok := strings.Cut(desc, "#")
		if !ok {
			return nil, -5, "Missing checksum"
		}
		if want, _ := descChecksum(body); sum != want && sum != "y" && !(c.infoSkew && sum == "qqqqqqqq") {
			return nil, -5, "Provided checksum does not match computed checksum"
		}
		return []string{c.address(body)}, 0, ""
	case "gettxout":
		if c.prevoutExists {
			return map[string]any{"confirmations": 1, "value": json.Number("0.00100000")}, 0, ""
		}
		// P1-c1: an escrow deposit the wallet reports is unspent until marked spent; utxoSkew lies about it.
		var vout int64
		json.Unmarshal(p[1], &vout)
		if c.beforeUTXO != nil {
			c.mu.Unlock()
			c.beforeUTXO(str(0), vout)
			c.mu.Lock()
		}
		if d, ok := c.depositAt(str(0), vout); ok && !c.spent[str(0)+":"+strconv.FormatInt(vout, 10)] {
			out := map[string]any{"confirmations": d.Confs, "value": json.Number(amount(d.Sats, 8)), "scriptPubKey": map[string]any{"address": d.Address}}
			if c.utxoSkew != nil {
				c.utxoSkew(out)
			}
			return out, 0, ""
		}
		return nil, 0, ""
	case "createpsbt":
		if c.beforeCreate != nil {
			c.mu.Unlock()
			c.beforeCreate()
			c.mu.Lock()
		}
		if c.createSkew != nil {
			return c.createSkew(c.createPSBT(p)), 0, ""
		}
		return c.createPSBT(p), 0, ""
	case "utxoupdatepsbt":
		var descs []string
		json.Unmarshal(p[1], &descs)
		if c.updateSkew != nil {
			return c.updateSkew(c.utxoUpdate(str(0), descs[0])), 0, ""
		}
		return c.utxoUpdate(str(0), descs[0]), 0, ""
	case "descriptorprocesspsbt":
		var descs []string
		json.Unmarshal(p[1], &descs)
		pk := c.mustParse(str(0))
		tprv := fakeTprv.FindString(descs[0])
		if tprv == "" || str(2) != "ALL" {
			return nil, -8, "unexpected fake call"
		}
		pub := fakePubFromTprv(tprv)
		out, err := pk.withInputField(0, append([]byte{0x02}, pub...), fakeSig(pub, pk.TxID(), 1))
		if err != nil {
			c.t.Errorf("fake descriptorprocesspsbt: %v", err)
		}
		return map[string]any{"psbt": encodePSBT(out), "complete": c.throwawayDone}, 0, ""
	case "finalizepsbt":
		pk := c.mustParse(str(0))
		if raw, ok := fakeFinalTx(pk); ok {
			if c.finalSkew != nil {
				raw = c.finalSkew(raw)
			}
			return map[string]any{"hex": raw, "complete": true}, 0, ""
		}
		return map[string]any{"hex": "02000000" + pk.TxID(), "complete": fakeComplete(pk)}, 0, ""
	case "testmempoolaccept":
		if c.beforeMempool != nil {
			c.beforeMempool()
		}
		var raws []string
		json.Unmarshal(p[0], &raws)
		if t := c.settlementTx(raws[0]); t != nil {
			if c.mempoolReject != "" {
				return []map[string]any{{"txid": t.TxID(), "allowed": false, "reject-reason": c.mempoolReject}}, 0, ""
			}
			if _, ok := c.replaces(t); !c.unspentInputs(t) && !ok {
				return []map[string]any{{"txid": t.TxID(), "allowed": false, "reject-reason": "missing-inputs"}}, 0, ""
			}
			return []map[string]any{{"txid": t.TxID(), "allowed": true}}, 0, ""
		}
		return []map[string]any{{"txid": "t", "allowed": c.allowed, "reject-reason": c.reject}}, 0, ""
	case "walletprocesspsbt":
		if !wallet {
			return nil, -8, "unexpected fake wallet"
		}
		pk, err := decodePSBT(str(0))
		if err != nil {
			return map[string]any{"psbt": str(0), "complete": false}, 0, "" // the adapter's params test sends a placeholder
		}
		raw := pk.Raw
		for i, in := range pk.Inputs {
			for _, d := range in.Derivations {
				if c.arbiterShort || hex.EncodeToString(d.Fingerprint[:]) != c.arbiterFP || str(2) != "ALL" {
					continue
				}
				next, _ := parsePSBT(raw)
				out, err := next.withInputField(i, append([]byte{0x02}, d.PubKey...), fakeSig(d.PubKey, pk.TxID(), 1))
				if err != nil {
					c.t.Errorf("fake walletprocesspsbt: %v", err)
				}
				raw = out
			}
		}
		signed := encodePSBT(raw)
		if c.marketSkew != nil {
			signed = c.marketSkew(signed)
		}
		return map[string]any{"psbt": signed, "complete": false}, 0, ""
	case "combinepsbt":
		var all []string
		json.Unmarshal(p[0], &all)
		first, err := decodePSBT(all[0])
		if err != nil {
			return "cHNidP8combined", 0, "" // the adapter's params test sends placeholders
		}
		raw := first.Raw
		for _, other := range all[1:] {
			op := c.mustParse(other)
			if op.TxID() != first.TxID() {
				return nil, -22, "PSBTs not compatible (different transactions)"
			}
			for i, in := range op.Inputs {
				for _, s := range in.PartialSigs {
					cur, _ := parsePSBT(raw)
					if slices.ContainsFunc(cur.Inputs[i].PartialSigs, func(have psbtSig) bool { return bytes.Equal(have.PubKey, s.PubKey) }) {
						continue
					}
					raw, _ = cur.withInputField(i, append([]byte{0x02}, s.PubKey...), s.Sig)
				}
			}
		}
		return encodePSBT(raw), 0, ""
	case "keypoolrefill":
		c.arbiterShort = c.arbiterShort && c.keypoolStuck
		return nil, 0, ""
	case "importdescriptors":
		var reqs []map[string]any
		json.Unmarshal(p[0], &reqs)
		if len(reqs) != 1 {
			return nil, -8, "unexpected fake import"
		}
		if c.beforeImport != nil {
			c.mu.Unlock()
			c.beforeImport()
			c.mu.Lock()
		}
		if c.importFail {
			return []map[string]any{{"success": false, "error": map[string]any{"code": -4, "message": "fake import failure"}}}, 0, ""
		}
		if path == fakeWatchWallet {
			c.imports = append(c.imports, reqs[0]["desc"].(string))
			if label, _ := reqs[0]["label"].(string); label != "" {
				if c.labels == nil {
					c.labels = map[string]string{}
				}
				body, _, _ := strings.Cut(reqs[0]["desc"].(string), "#")
				c.labels[c.address(body)] = label
				if m := fakeMulti.FindStringSubmatch(body + "#"); m != nil {
					keys := slices.Clone(m[1:])
					slices.Sort(keys) // a bare-key sortedmulti: the script's order
					if c.watchPubkeys == nil {
						c.watchPubkeys = map[string][]string{}
					}
					c.watchPubkeys[c.address(body)] = keys
				}
			}
		}
		return []map[string]any{{"success": true}}, 0, ""
	case "getwalletinfo":
		if wallet {
			info := map[string]any{"walletname": "opsecmkt-arbiter", "descriptors": true, "private_keys_enabled": true, "txcount": c.arbiterTxCount}
			if c.arbiterEncrypted {
				info["unlocked_until"] = 0
			}
			return info, 0, ""
		}
		if path != fakeWatchWallet || c.watchInfo == nil {
			return nil, -18, "Requested wallet does not exist or is not loaded"
		}
		return c.watchInfo, 0, ""
	case "listreceivedbyaddress":
		if path != fakeWatchWallet {
			return nil, -8, "unexpected fake wallet"
		}
		if c.txErr != 0 {
			return nil, c.txErr, "fake wallet failure"
		}
		byAddr := map[string][]string{}
		var order []string
		for _, d := range c.deposits {
			if _, ok := byAddr[d.Address]; !ok {
				order = append(order, d.Address)
			}
			if !slices.Contains(byAddr[d.Address], d.TxID) {
				byAddr[d.Address] = append(byAddr[d.Address], d.TxID)
			}
		}
		out := []map[string]any{}
		for _, a := range order {
			out = append(out, map[string]any{"address": a, "txids": byAddr[a], "involvesWatchonly": true, "label": c.labels[a], "amount": json.Number("0.001")})
		}
		var includeEmpty bool
		if len(p) > 1 {
			json.Unmarshal(p[1], &includeEmpty)
		}
		if includeEmpty {
			var empty []string
			for a := range c.labels {
				if _, ok := byAddr[a]; !ok {
					empty = append(empty, a)
				}
			}
			slices.Sort(empty)
			for _, a := range empty {
				out = append(out, map[string]any{"address": a, "txids": []string{}, "label": c.labels[a], "amount": json.Number("0.00000000")})
			}
		}
		return out, 0, ""
	case "listtransactions":
		if path != fakeWatchWallet {
			return nil, -8, "unexpected fake wallet"
		}
		if c.listTxErr != 0 {
			return nil, c.listTxErr, "fake listtransactions failure"
		}
		out := []map[string]any{}
		for _, d := range c.deposits {
			out = append(out, map[string]any{"txid": d.TxID, "category": "receive", "address": d.Address, "vout": d.Vout})
		}
		var sent []string
		for txid := range c.broadcast {
			sent = append(sent, txid)
		}
		slices.Sort(sent)
		for _, txid := range sent {
			out = append(out, map[string]any{"txid": txid, "category": "send", "confirmations": c.txConfs[txid]})
		}
		return out, 0, ""
	case "gettransaction":
		txid := str(0)
		if _, ok := c.broadcast[txid]; ok && !c.walletForgets {
			conflicts := c.conflicts[txid]
			if conflicts == nil {
				conflicts = []string{}
			}
			return map[string]any{"confirmations": c.txConfs[txid], "walletconflicts": conflicts, "details": []any{}, "hex": c.broadcast[txid]}, 0, ""
		}
		var details []map[string]any
		confs := int64(0)
		for _, d := range c.deposits {
			if d.TxID == txid {
				confs = d.Confs
				details = append(details, map[string]any{"address": d.Address, "category": "receive", "amount": json.Number(amount(d.Sats, 8)), "vout": d.Vout, "involvesWatchonly": true})
			}
		}
		if details == nil {
			return nil, -5, "Invalid or non-wallet transaction id"
		}
		return map[string]any{"confirmations": confs, "details": details}, 0, ""
	case "sendrawtransaction":
		if c.beforeSend != nil {
			c.mu.Unlock()
			c.beforeSend()
			c.mu.Lock()
		}
		if c.sendErr != 0 {
			return nil, c.sendErr, "fake send failure"
		}
		t := c.settlementTx(str(0))
		if t == nil {
			return "txid-broadcast", 0, "" // the adapter's params test sends a placeholder
		}
		txid := t.TxID()
		if _, ok := c.broadcast[txid]; !ok && c.private {
			if c.queued == nil {
				c.queued = map[string]string{}
			}
			c.queued[txid] = str(0)
			return txid, 0, ""
		}
		if _, ok := c.broadcast[txid]; ok {
			if c.txConfs[txid] > 0 {
				return nil, -27, "Transaction outputs already in utxo set"
			}
			if c.replaced[txid] {
				return nil, -26, "insufficient fee"
			}
			return txid, 0, ""
		}
		olds, replaces := c.replaces(t)
		if !c.unspentInputs(t) && !replaces {
			if len(olds) > 0 {
				return nil, -26, "insufficient fee"
			}
			return nil, -25, "bad-txns-inputs-missingorspent"
		}
		if c.conflicts == nil {
			c.conflicts = map[string][]string{}
		}
		if c.replaced == nil {
			c.replaced = map[string]bool{}
		}
		for _, old := range olds {
			c.replaced[old] = true
			c.conflicts[old] = append(c.conflicts[old], txid)
			c.conflicts[txid] = append(c.conflicts[txid], old)
		}
		for _, in := range t.In {
			id := in.TxID
			slices.Reverse(id[:])
			if c.spent == nil {
				c.spent, c.spentBy = map[string]bool{}, map[string]string{}
			}
			if c.spentBy == nil {
				c.spentBy = map[string]string{}
			}
			k := hex.EncodeToString(id[:]) + ":" + strconv.FormatInt(int64(in.Vout), 10)
			c.spent[k], c.spentBy[k] = true, txid
		}
		if c.broadcast == nil {
			c.broadcast, c.txConfs = map[string]string{}, map[string]int64{}
		}
		c.broadcast[txid], c.txConfs[txid] = str(0), 0
		return txid, 0, ""
	}
	return nil, -32601, "Method not found"
}

// settlementTx reads rawHex as a segwit transaction spending recorded escrow deposits, or nil (a practice
// transaction, or a placeholder).
func (c *fakeEscrowCore) settlementTx(rawHex string) *signedTx {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil
	}
	t, err := parseSignedTx(raw)
	if err != nil {
		return nil
	}
	for _, in := range t.In {
		id := in.TxID
		slices.Reverse(id[:])
		if _, ok := c.depositAt(hex.EncodeToString(id[:]), int64(in.Vout)); !ok {
			return nil
		}
	}
	return t
}

// unspentInputs reports whether every input of t spends a deposit not yet spent.
func (c *fakeEscrowCore) unspentInputs(t *signedTx) bool {
	for _, in := range t.In {
		id := in.TxID
		slices.Reverse(id[:])
		if c.spent[hex.EncodeToString(id[:])+":"+strconv.FormatInt(int64(in.Vout), 10)] {
			return false
		}
	}
	return true
}

// replaces reports the sent, unconfirmed transactions that spend t's spent inputs (olds) and whether t replaces them
// (BIP125 as Core applies it: every spent input is spent by such a transaction, and t pays a higher fee than each by at
// least 1 sat/vB of t's size). An input spent by anything else (spend, or a confirmed transaction) makes it no
// replacement.
func (c *fakeEscrowCore) replaces(t *signedTx) ([]string, bool) {
	var olds []string
	for _, in := range t.In {
		id := in.TxID
		slices.Reverse(id[:])
		k := hex.EncodeToString(id[:]) + ":" + strconv.FormatInt(int64(in.Vout), 10)
		if !c.spent[k] {
			continue
		}
		old := c.spentBy[k]
		if old == "" || c.txConfs[old] != 0 {
			return nil, false
		}
		if !slices.Contains(olds, old) {
			olds = append(olds, old)
		}
	}
	if len(olds) == 0 {
		return nil, false
	}
	fee := c.txFee(t)
	for _, old := range olds {
		raw, _ := hex.DecodeString(c.broadcast[old])
		ot, err := parseSignedTx(raw)
		if err != nil || fee < c.txFee(ot)+t.VSize {
			return olds, false
		}
	}
	return olds, true
}

// txFee is t's fee: the deposits it spends less its outputs.
func (c *fakeEscrowCore) txFee(t *signedTx) int64 {
	var fee int64
	for _, in := range t.In {
		id := in.TxID
		slices.Reverse(id[:])
		d, _ := c.depositAt(hex.EncodeToString(id[:]), int64(in.Vout))
		fee += d.Sats
	}
	for _, o := range t.Out {
		fee -= o.Value
	}
	return fee
}

// setTxConfs sets the confirmations of a sent transaction (negative: conflicted, with the conflicting txid).
func (c *fakeEscrowCore) setTxConfs(txid string, confs int64, conflict string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.txConfs[txid] = confs
	if conflict != "" {
		if c.conflicts == nil {
			c.conflicts = map[string][]string{}
		}
		c.conflicts[txid] = []string{conflict}
	}
}

// fakeFinalTx is finalizepsbt's raw transaction when every input of a multisig spend carries two valid signatures from
// its witness script's keys and no invalid one: the unsigned transaction with each input's witness [empty, the
// signatures in key order, witness script]. A PSBT with a finalized input, or any other shape, is not one (ok false).
func fakeFinalTx(pk *psbtPacket) (string, bool) {
	var witnesses [][]byte
	for _, in := range pk.Inputs {
		if in.Finalized || len(in.WitnessScript) != 105 {
			return "", false
		}
		keys := [][]byte{in.WitnessScript[2:35], in.WitnessScript[36:69], in.WitnessScript[70:103]}
		var sigs [][]byte
		for _, k := range keys {
			for _, s := range in.PartialSigs {
				if !bytes.Equal(s.PubKey, k) {
					continue
				}
				if !bytes.Equal(s.Sig, fakeSig(s.PubKey, pk.TxID(), s.SighashByte())) {
					return "", false
				}
				sigs = append(sigs, s.Sig)
			}
		}
		if len(sigs) < 2 {
			return "", false
		}
		w := []byte{4, 0}
		for _, item := range [][]byte{sigs[0], sigs[1], in.WitnessScript} {
			w = append(append(w, compactSize(len(item))...), item...)
		}
		witnesses = append(witnesses, w)
	}
	u := pk.UnsignedTx
	raw := slices.Concat(u[:4], []byte{0, 1}, u[4:len(u)-4], slices.Concat(witnesses...), u[len(u)-4:])
	return hex.EncodeToString(raw), true
}

func (c *fakeEscrowCore) mustParse(b64 string) *psbtPacket {
	pk, err := decodePSBT(b64)
	if err != nil {
		c.t.Fatalf("fake core got an unreadable PSBT: %v", err)
	}
	return pk
}

// fakeComplete is finalizepsbt's verdict: a finalized input, or two valid signatures from the witness script's
// keys and no invalid one.
func fakeComplete(pk *psbtPacket) bool {
	in := pk.Inputs[0]
	if in.Finalized {
		return true
	}
	if len(in.WitnessScript) != 105 {
		return false
	}
	keys := [][]byte{in.WitnessScript[2:35], in.WitnessScript[36:69], in.WitnessScript[70:103]}
	valid := 0
	for _, s := range in.PartialSigs {
		if !slices.ContainsFunc(keys, func(k []byte) bool { return bytes.Equal(k, s.PubKey) }) {
			continue
		}
		if !bytes.Equal(s.Sig, fakeSig(s.PubKey, pk.TxID(), s.SighashByte())) {
			return false
		}
		valid++
	}
	return valid >= 2
}

func (c *fakeEscrowCore) createPSBT(p []json.RawMessage) string {
	var ins []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	var outs []map[string]json.Number
	json.Unmarshal(p[0], &ins)
	json.Unmarshal(p[1], &outs)
	tx := []byte{2, 0, 0, 0}
	tx = append(tx, compactSize(len(ins))...)
	for _, in := range ins {
		id, _ := hex.DecodeString(in.TxID)
		slices.Reverse(id)
		tx = append(tx, id...)
		tx = binary.LittleEndian.AppendUint32(tx, in.Vout)
		tx = append(tx, 0, 0xfd, 0xff, 0xff, 0xff)
	}
	tx = append(tx, compactSize(len(outs))...)
	for _, o := range outs {
		for addr, amt := range o {
			sats, _ := parseAmount(string(amt), 8)
			tx = binary.LittleEndian.AppendUint64(tx, uint64(sats))
			if spk := c.spk[addr]; spk != nil {
				tx = append(append(tx, byte(len(spk))), spk...)
				continue
			}
			if spk, err := escrowAddressScript(c.chain, addr); err == nil { // P1-c2: a real address pays its own script
				tx = append(append(tx, byte(len(spk))), spk...)
				continue
			}
			h := sha256.Sum256([]byte(addr))
			tx = append(append(tx, 34, 0, 32), h[:]...)
		}
	}
	tx = append(tx, 0, 0, 0, 0)
	maps := [][]byte{psbtMap(kv{[]byte{0}, tx})}
	for range ins {
		maps = append(maps, psbtMap())
	}
	for range outs {
		maps = append(maps, psbtMap())
	}
	return base64.StdEncoding.EncodeToString(buildPSBT(maps...))
}

var fakeMulti = regexp.MustCompile(`^wsh\(sortedmulti\(2,([^,]+),([^,]+),([^,]+)\)\)#`)

// utxoUpdate adds, as Core does when the input has a witness UTXO, the witness script and the derivations of keys
// with an origin: "[fp/path]tpub…/suffix" (the fake child fakeChild) and "fakepub<hex>" (its own fingerprint).
func (c *fakeEscrowCore) utxoUpdate(b64, desc string) string {
	pk := c.mustParse(b64)
	raw := pk.Raw
	add := func(i int, key, value []byte) {
		next, err := parsePSBT(raw)
		if err == nil {
			raw, err = next.withInputField(i, key, value)
		}
		if err != nil {
			c.t.Fatalf("fake utxoupdatepsbt: %v", err)
		}
	}
	// P1-c1: as Core does from its UTXO set, the witness UTXO of an input spending an unspent output it knows (a
	// deposit the watch wallet reports).
	for i, in := range pk.Tx.In {
		id := in.TxID
		slices.Reverse(id[:])
		d, ok := c.depositAt(hex.EncodeToString(id[:]), int64(in.Vout))
		if pk.Inputs[i].WitnessUTXO != nil || !ok || c.spent[hex.EncodeToString(id[:])+":"+strconv.FormatInt(int64(in.Vout), 10)] || c.spk[d.Address] == nil {
			continue
		}
		spk := c.spk[d.Address]
		add(i, []byte{0x01}, append(append(binary.LittleEndian.AppendUint64(nil, uint64(d.Sats)), compactSize(len(spk))...), spk...))
	}
	m := fakeMulti.FindStringSubmatch(desc)
	if m == nil {
		c.t.Fatalf("fake utxoupdatepsbt: descriptor %q", desc)
	}
	type entry struct{ pub, deriv []byte }
	var entries []entry
	for _, el := range m[1:] {
		pub, deriv := c.fakeKeyOf(el)
		entries = append(entries, entry{pub, deriv})
	}
	ws := fakeMultiScript(entries[0].pub, entries[1].pub, entries[2].pub)
	sum := sha256.Sum256(ws)
	for i := range pk.Tx.In {
		cur, _ := parsePSBT(raw)
		if u := cur.Inputs[i].WitnessUTXO; u == nil || !bytes.Equal(u.Script, append([]byte{0, 32}, sum[:]...)) {
			continue // Core adds the script and derivations only to an input paying the descriptor's script
		}
		add(i, []byte{0x05}, ws)
		added := map[string]bool{}
		for _, e := range entries {
			if added[string(e.pub)] {
				continue // one derivation per key, as Core writes a repeated key once
			}
			added[string(e.pub)] = true
			deriv := e.deriv
			if deriv == nil {
				deriv = slices.Clone(e.pub[1:5]) // a bare key: Core writes its own fingerprint (hash160) and an empty path
			}
			add(i, append([]byte{0x06}, e.pub...), deriv)
		}
	}
	return encodePSBT(raw)
}

// fakeOwnerSign is the owner's wallet in the fake: it adds a signature (sighash sh) from the practice PSBT's
// derivation at fingerprint fp, the owner's practice child key.
func fakeOwnerSign(t *testing.T, practice, fp string, sh byte) string {
	t.Helper()
	pk, err := decodePSBT(practice)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range pk.Inputs[0].Derivations {
		if hex.EncodeToString(d.Fingerprint[:]) == fp {
			out, err := pk.withInputField(0, append([]byte{0x02}, d.PubKey...), fakeSig(d.PubKey, pk.TxID(), sh))
			if err != nil {
				t.Fatal(err)
			}
			return encodePSBT(out)
		}
	}
	t.Fatalf("no derivation at fingerprint %s", fp)
	return ""
}

// fakeKeyOf is the fake's reading of one key expression in a descriptor: "[fp/path]tpub…/suffix" (the fake child
// fakeChild, with its derivation), "fakepub<hex>" (its own fingerprint) or a bare hex key (no derivation).
func (c *fakeEscrowCore) fakeKeyOf(el string) (pub, deriv []byte) {
	switch {
	case strings.HasPrefix(el, "["):
		end := strings.IndexByte(el, ']')
		fp, _ := hex.DecodeString(el[1:9])
		rest := el[end+1:]
		path, err := parseOriginPath(el[9:end] + rest[strings.IndexByte(rest, '/'):])
		if err != nil {
			c.t.Fatalf("fake core: path in %q: %v", el, err)
		}
		d := slices.Clone(fp)
		for _, step := range path {
			d = binary.LittleEndian.AppendUint32(d, step)
		}
		return fakeChild(el[end+1:]), d
	case strings.HasPrefix(el, "fakepub"):
		pub, _ := hex.DecodeString(el[len("fakepub"):])
		return pub, pub[1:5]
	}
	pub, _ = hex.DecodeString(el)
	return pub, nil
}

// fakeMultiScript is 2 <keys sorted> 3 CHECKMULTISIG.
func fakeMultiScript(keys ...[]byte) []byte {
	keys = slices.Clone(keys)
	slices.SortFunc(keys, bytes.Compare)
	ws := []byte{0x52}
	for _, k := range keys {
		ws = append(append(ws, 33), k...)
	}
	return append(ws, 0x53, 0xae)
}

var fakeWpkh = regexp.MustCompile(`^wpkh\(([^()]+)\)$`)

// address is deriveaddresses in the fake: a wsh(sortedmulti(2,…)) pays its witness script (the same address whether
// its keys are written as extended keys with origins or bare, as in Core; bareAddrSalt makes the bare form differ),
// a wpkh(…) its key; anything else a hash of the descriptor. Escrow addresses remember their scriptPubKey for
// createpsbt.
func (c *fakeEscrowCore) address(desc string) string {
	if m := fakeMulti.FindStringSubmatch(desc + "#"); m != nil {
		var keys [][]byte
		bare := true
		for _, el := range m[1:] {
			pub, deriv := c.fakeKeyOf(el)
			bare = bare && deriv == nil
			keys = append(keys, pub)
		}
		h := sha256.Sum256(fakeMultiScript(keys...))
		addr := segwitAddress("bcrt", 0, h[:]) // a real P2WSH address, so its script decodes as Core's would (QA-P2e-1)
		if bare && c.bareAddrSalt != "" {
			addr = "bcrt1q" + c.bareAddrSalt + hex.EncodeToString(h[:])[len(c.bareAddrSalt):]
		}
		c.spk[addr] = append([]byte{0, 32}, h[:]...)
		return addr
	}
	if m := fakeWpkh.FindStringSubmatch(desc); m != nil {
		pub, _ := c.fakeKeyOf(m[1])
		h := sha256.Sum256(pub)
		return "bcrt1q" + hex.EncodeToString(h[:])[:40]
	}
	h := sha256.Sum256([]byte(desc))
	return "bcrt1q" + hex.EncodeToString(h[:])[:40]
}
