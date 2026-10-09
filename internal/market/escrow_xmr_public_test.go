package market

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This deliberately separate public-stagenet leg never mines, moves the clock, or uses the fakechain helpers'
// wallet creation. scripts/test-escrow-xmr-public-stagenet.py is the only intended entry point. It is not CI.
const xmrPublicPrice int64 = 10_000_000_000 // 0.01 XMR, paid by a dedicated, already funded stagenet wallet.

func xmrPublicLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.Port() == "" || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func xmrPublicAuthenticatedLoopback(raw string) bool {
	if !xmrPublicLoopback(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	if u.User == nil {
		return false
	}
	password, hasPassword := u.User.Password()
	return u.User.Username() != "" && hasPassword && password != ""
}

func xmrPublicDatabaseLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" || u.Port() == "" || strings.Trim(u.Path, "/") == "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func xmrPublicRefusal(n moneroNode, hf int64, now time.Time) string {
	if n.NetType != "stagenet" {
		return "daemon is not stagenet"
	}
	if hf < xmrMinHardFork {
		return "hard fork below 16"
	}
	if n.Restricted {
		return "daemon RPC is restricted; peer count cannot be proved"
	}
	if n.Height <= 0 || n.TargetHeight < 0 || n.TipTime <= 0 || n.TipTime > now.Add(5*time.Minute).Unix() {
		return "daemon height or tip is invalid"
	}
	if why := n.notSynced(now, time.Hour); why != "" {
		return "daemon is offline, unpeered, stale or unsynchronized"
	}
	return ""
}

func xmrPublicAnchorRefusal(current, height int64, hash, source, localHash string, localHeight int64, recent bool) string {
	u, err := url.Parse(source)
	if height <= 0 || !xmrHex64(hash) || err != nil || u.Scheme != "https" ||
		u.Hostname() != "stagenet.xmrchain.net" || u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "/block/"+hash && u.Path != "/block/"+strconv.FormatInt(height, 10)) {
		return "public stagenet anchor is invalid"
	}
	if current < height || recent && current-height > 20 {
		return "public stagenet anchor is not near the daemon tip"
	}
	if localHeight != height || localHash != hash {
		return "daemon block does not match the independent public stagenet anchor"
	}
	return ""
}

func xmrPublicExplorerRefusal(height int64, hash string, body []byte) string {
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Height int64  `json:"block_height"`
			Hash   string `json:"hash"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || response.Status != "success" || response.Data.Height != height || response.Data.Hash != hash {
		return "public explorer block height or hash does not match the supplied anchor"
	}
	return ""
}

func xmrPublicVerifyExplorer(ctx context.Context, height int64, hash string) string {
	const maxBody = 2 << 20
	endpoint := "https://stagenet.xmrchain.net/api/block/" + strconv.FormatInt(height, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "public explorer request is invalid"
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 16 << 10},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "public explorer API is unreadable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "public explorer API is unavailable"
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return "public explorer response is unreadable or too large"
	}
	return xmrPublicExplorerRefusal(height, hash, body)
}

func TestXMREscrowPublicRefusal(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	good := moneroNode{NetType: "stagenet", Synchronized: true, Height: 1000, TargetHeight: 1000, Connections: 2, TipTime: now.Unix()}
	if got := xmrPublicRefusal(good, 16, now); got != "" {
		t.Fatal(got)
	}
	for name, change := range map[string]func(*moneroNode){
		"mainnet":        func(n *moneroNode) { n.NetType = "mainnet" },
		"testnet":        func(n *moneroNode) { n.NetType = "testnet" },
		"fakechain":      func(n *moneroNode) { n.NetType = "fakechain" },
		"offline":        func(n *moneroNode) { n.Offline = true },
		"unpeered":       func(n *moneroNode) { n.Connections = 0 },
		"stale":          func(n *moneroNode) { n.TipTime -= 3601 },
		"behind":         func(n *moneroNode) { n.TargetHeight++ },
		"unsynchronized": func(n *moneroNode) { n.Synchronized = false },
		"restricted":     func(n *moneroNode) { n.Restricted = true },
		"future tip":     func(n *moneroNode) { n.TipTime += 301 },
	} {
		n := good
		change(&n)
		if xmrPublicRefusal(n, 16, now) == "" {
			t.Errorf("accepted %s", name)
		}
	}
	if xmrPublicRefusal(good, 15, now) == "" {
		t.Error("accepted old hard fork")
	}
	anchorHash := strings.Repeat("a", 64)
	anchorSource := "https://stagenet.xmrchain.net/block/" + anchorHash
	if got := xmrPublicAnchorRefusal(1000, 990, anchorHash, anchorSource, anchorHash, 990, true); got != "" {
		t.Error(got)
	}
	for _, c := range []struct {
		name                    string
		current, height         int64
		hash, source, localHash string
		localHeight             int64
	}{
		{"private fork", 1000, 990, anchorHash, anchorSource, strings.Repeat("b", 64), 990},
		{"stale anchor", 1000, 979, anchorHash, anchorSource, anchorHash, 979},
		{"mainnet source", 1000, 990, anchorHash, "https://xmrchain.net/block/" + anchorHash, anchorHash, 990},
		{"wrong height", 1000, 990, anchorHash, anchorSource, anchorHash, 991},
	} {
		if xmrPublicAnchorRefusal(c.current, c.height, c.hash, c.source, c.localHash, c.localHeight, true) == "" {
			t.Errorf("accepted %s", c.name)
		}
	}
	for name, body := range map[string]string{
		"wrong height": `{"status":"success","data":{"block_height":991,"hash":"` + anchorHash + `"}}`,
		"wrong hash":   `{"status":"success","data":{"block_height":990,"hash":"` + strings.Repeat("b", 64) + `"}}`,
		"bad status":   `{"status":"failed","data":{"block_height":990,"hash":"` + anchorHash + `"}}`,
		"malformed":    `{`,
	} {
		if xmrPublicExplorerRefusal(990, anchorHash, []byte(body)) == "" {
			t.Errorf("accepted explorer %s", name)
		}
	}
	if got := xmrPublicExplorerRefusal(990, anchorHash, []byte(`{"status":"success","data":{"block_height":990,"hash":"`+anchorHash+`"}}`)); got != "" {
		t.Error(got)
	}
	for _, at := range []string{"http://127.0.0.1:38081", "http://[::1]:38081"} {
		if !xmrPublicLoopback(at) {
			t.Errorf("refused loopback %s", at)
		}
	}
	for _, at := range []string{"https://127.0.0.1:38081", "http://8.8.8.8:18081", "http://node.example:18081", "http://127.0.0.1:18081/?x=1"} {
		if xmrPublicLoopback(at) {
			t.Errorf("accepted non-local or ambiguous URL %s", at)
		}
	}
	if !xmrPublicAuthenticatedLoopback("http://user:secret@127.0.0.1:38081") ||
		xmrPublicAuthenticatedLoopback("http://127.0.0.1:38081") ||
		xmrPublicAuthenticatedLoopback("http://user:@127.0.0.1:38081") {
		t.Error("public RPC authentication boundary failed")
	}
	if !xmrPublicDatabaseLoopback("postgres://smoke:private@127.0.0.1:59954/public_xmr?sslmode=disable") ||
		xmrPublicDatabaseLoopback("postgres://smoke:private@8.8.8.8:5432/public_xmr") ||
		xmrPublicDatabaseLoopback("postgres://smoke:private@127.0.0.1:59954/") {
		t.Error("database loopback boundary failed")
	}
}

type xmrPublicEvent struct {
	At     string `json:"at"`
	Height int64  `json:"height"`
	Txid   string `json:"txid,omitempty"`
}

type xmrPublicProof struct {
	Network                 string         `json:"network"`
	HardFork                int64          `json:"hard_fork"`
	AnchorHeight            int64          `json:"anchor_height"`
	AnchorHash              string         `json:"anchor_hash"`
	AnchorSource            string         `json:"anchor_source"`
	EscrowWalletRPCVersion  int64          `json:"escrow_wallet_rpc_version"`
	FunderWalletRPCVersion  int64          `json:"funder_wallet_rpc_version"`
	StartHeight             int64          `json:"start_height"`
	FundingSubmitted        xmrPublicEvent `json:"funding_submitted"`
	FundingConfirmed        xmrPublicEvent `json:"funding_confirmed"`
	SettlementRequested     xmrPublicEvent `json:"settlement_requested"`
	SettlementBuilt         xmrPublicEvent `json:"settlement_built"`
	SettlementRelayed       xmrPublicEvent `json:"settlement_relayed"`
	SettlementConfirmed     xmrPublicEvent `json:"settlement_confirmed"`
	FundingConfirmations    int64          `json:"funding_confirmations"`
	SettlementConfirmations int64          `json:"settlement_confirmations"`
	ClaimReceivedPiconero   int64          `json:"claim_received_piconero"`
}

func TestXMREscrowPublicStagenetSmoke(t *testing.T) {
	if os.Getenv("ESCROW_XMR_PUBLIC_RUN") == "" {
		t.Skip("manual public stagenet smoke only")
	}
	if os.Getenv("ESCROW_XMR_PUBLIC_RUN") != "I_ACKNOWLEDGE_STAGENET_FUNDS" {
		t.Fatal("refusing: explicit stagenet-funds opt-in is required")
	}
	daemonURL, walletURL, funderURL := os.Getenv("ESCROW_XMR_PUBLIC_DAEMON_URL"), os.Getenv("ESCROW_XMR_PUBLIC_WALLET_URL"), os.Getenv("ESCROW_XMR_PUBLIC_FUNDER_URL")
	daemonEndpoint, _ := url.Parse(daemonURL)
	walletEndpoint, _ := url.Parse(walletURL)
	funderEndpoint, _ := url.Parse(funderURL)
	if !xmrPublicAuthenticatedLoopback(daemonURL) || !xmrPublicAuthenticatedLoopback(walletURL) || !xmrPublicAuthenticatedLoopback(funderURL) ||
		daemonEndpoint.Port() == walletEndpoint.Port() || daemonEndpoint.Port() == funderEndpoint.Port() || walletEndpoint.Port() == funderEndpoint.Port() {
		t.Fatal("refusing: daemon and distinct wallet RPC URLs must be loopback HTTP endpoints")
	}
	mins, err := strconv.Atoi(os.Getenv("ESCROW_XMR_PUBLIC_WAIT_MINUTES"))
	if err != nil || mins < 30 || mins > 1440 {
		t.Fatal("refusing: wait must be 30–1440 minutes")
	}
	if !xmrPublicDatabaseLoopback(os.Getenv("TEST_DATABASE_URL")) {
		t.Fatal("refusing: a disposable loopback PostgreSQL database is required")
	}
	anchorHeight, err := strconv.ParseInt(os.Getenv("ESCROW_XMR_PUBLIC_ANCHOR_HEIGHT"), 10, 64)
	if err != nil {
		t.Fatal("refusing: public stagenet anchor height is invalid")
	}
	anchorHash, anchorSource := os.Getenv("ESCROW_XMR_PUBLIC_ANCHOR_HASH"), os.Getenv("ESCROW_XMR_PUBLIC_ANCHOR_SOURCE")
	if xmrPublicAnchorRefusal(anchorHeight, anchorHeight, anchorHash, anchorSource, anchorHash, anchorHeight, false) != "" {
		t.Fatal("refusing: public stagenet anchor is invalid")
	}
	rpc, err := newXMREscrowRPC(walletURL, daemonURL) // constructs clients; no wallet call yet.
	if err != nil {
		t.Fatal("refusing: invalid RPC endpoints")
	}
	preflight := func(recent bool) (moneroNode, int64) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		n, err := rpc.node(ctx)
		if err != nil {
			t.Fatal("refusing: daemon preflight unreadable")
		}
		flags, err := rpc.daemonInfo(ctx)
		if err != nil || !flags.Stagenet || flags.Mainnet || flags.Testnet {
			t.Fatal("refusing: daemon network flags disagree with stagenet")
		}
		hf, err := rpc.hardForkVersion(ctx)
		if err != nil {
			t.Fatal("refusing: hard fork unreadable")
		}
		if why := xmrPublicRefusal(n, hf, time.Now()); why != "" {
			t.Fatalf("refusing: %s", why)
		}
		var block struct {
			Header struct {
				Hash   string `json:"hash"`
				Height int64  `json:"height"`
			} `json:"block_header"`
		}
		if err := rpc.daemonCall(ctx, "get_block_header_by_height", map[string]any{"height": anchorHeight}, &block); err != nil {
			t.Fatal("refusing: public stagenet anchor block is unreadable")
		}
		if why := xmrPublicAnchorRefusal(n.Height, anchorHeight, anchorHash, anchorSource, block.Header.Hash, block.Header.Height, recent); why != "" {
			t.Fatalf("refusing: %s", why)
		}
		return n, hf
	}
	n, hf := preflight(true) // absolutely before any funder or escrow wallet RPC.
	explorerCtx, explorerCancel := context.WithTimeout(context.Background(), 20*time.Second)
	if why := xmrPublicVerifyExplorer(explorerCtx, anchorHeight, anchorHash); why != "" {
		explorerCancel()
		t.Fatalf("refusing: %s", why)
	}
	explorerCancel()
	funder, err := newRPCClient("public stagenet funder", funderURL, "2.0", true)
	if err != nil {
		t.Fatal("refusing: invalid funder endpoint")
	}
	r := &xmrRegtest{xmrEscrowEnv: &xmrEscrowEnv{testEnv: &testEnv{t: t}}, daemon: rpc.daemon, funder: funder}
	var escrowVersion, funderVersion struct {
		Version int64 `json:"version"`
	}
	if err := rpc.wallet.call(context.Background(), "/json_rpc", "get_version", nil, &escrowVersion); err != nil || escrowVersion.Version <= 0 {
		t.Fatal("refusing: escrow wallet RPC version is unreadable")
	}
	r.call(funder, "get_version", nil, &funderVersion)
	if funderVersion.Version <= 0 {
		t.Fatal("refusing: funder wallet RPC version is unreadable")
	}
	r.call(funder, "refresh", nil, nil)
	var balance struct {
		Unlocked int64 `json:"unlocked_balance"`
	}
	r.call(funder, "get_balance", map[string]any{"account_index": 0}, &balance)
	if balance.Unlocked < 2*xmrPublicPrice {
		t.Fatal("refusing: funder needs at least 0.02 unlocked stagenet XMR, including a fee cushion")
	}
	refund := r.funderAddress()
	if _, err = parseXMRAddress(refund, "stagenet"); err != nil {
		t.Fatal("refusing: funder address is not stagenet")
	}
	key, pub := testSeedKey(t, nil)
	r.seedKey, r.keyPath = key, writeSeedKey(t, pub)
	for k, v := range map[string]string{
		"ESCROW_XMR": "on", "ESCROW_XMR_DAEMON_URL": daemonURL, "ESCROW_XMR_WALLET_RPC_URL": walletURL,
		"ESCROW_XMR_SEED_KEY": r.keyPath, "MONERO_WALLET_SECRET": randomToken() + randomToken(),
		"ESCROW_XMR_SLOTS": "5", "ESCROW_XMR_FEE_BPS": "0", "ESCROW_XMR_FEE_ADDRESS": "",
		"MONERO_WALLET_RPC_URL": "", "MONERO_NETWORK": "", "BITCOIN_CHAIN": "", "ESCROW_BTC": "", "BITCOIN_RPC_URL": "",
	} {
		t.Setenv(k, v)
	}
	r.testEnv = newTestApp(t)
	if !r.A.xmrEscrowReady() {
		t.Fatal("refusing: stagenet escrow did not start")
	}
	if agInt(r.testEnv, "SELECT count(*) FROM xmr_escrow_wallets") != 0 {
		t.Fatal("refusing: escrow database is not fresh")
	}
	r.mustPoll() // create this month's wallet and the five free slots, at the real current wall clock.
	r.buyer, r.buyerSess = r.user("xp_b_"+randomToken()[:8], "buyer")
	r.vendor, r.vendorSess = r.user("xp_v_"+randomToken()[:8], "vendor")
	r.product = r.digital(r.vendor)
	agExec(r.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", r.product, xmrPublicPrice)
	order := r.order(r.buyer, r.product, "XMR", stateDraft)
	if w := r.checkout(r.buyerSess, order, refund); w.Code != 303 {
		t.Fatalf("public checkout refused: HTTP %d", w.Code)
	}
	addr := agStrings(t, r.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
	if _, err = parseXMRAddress(addr, "stagenet"); err != nil {
		t.Fatal("refusing: escrow address is not stagenet")
	}
	preflight(true) // refuse a node that fell out of sync or diverged before spending test coins.
	proof := xmrPublicProof{Network: "stagenet", HardFork: hf, StartHeight: n.Height,
		EscrowWalletRPCVersion: escrowVersion.Version, FunderWalletRPCVersion: funderVersion.Version,
		AnchorHeight: anchorHeight, AnchorHash: anchorHash, AnchorSource: anchorSource}
	eventAt := func(txid string, at time.Time) xmrPublicEvent {
		t.Helper()
		return xmrPublicEvent{At: at.UTC().Format(time.RFC3339), Height: r.height(), Txid: txid}
	}
	event := func(txid string) xmrPublicEvent { return eventAt(txid, time.Now()) }
	fundingTx := r.pay(map[string]int64{addr: xmrPublicPrice})
	if !xmrHex64(fundingTx) {
		t.Fatal("funder returned no valid transaction hash")
	}
	proof.FundingSubmitted = event(fundingTx)
	wait := time.Duration(mins) * time.Minute
	until := time.Now().Add(wait)
	for time.Now().Before(until) {
		preflight(false)
		if err := r.poll(); err == nil && r.state(order) == stateCompleted {
			txs, missed, err := rpc.transactions(context.Background(), []string{fundingTx})
			if err == nil && len(missed) == 0 && len(txs) == 1 && txs[0].Confirmations >= xmrMinConfirmations {
				proof.FundingConfirmations = txs[0].Confirmations
				proof.FundingConfirmed = event(fundingTx)
				break
			}
		}
		time.Sleep(20 * time.Second)
	}
	if proof.FundingConfirmed.At == "" {
		t.Fatal("public funding did not complete within the bounded wait")
	}
	var claimAddress struct {
		Address string `json:"address"`
		Index   int64  `json:"address_index"`
	}
	r.call(funder, "create_address", map[string]any{"account_index": 0}, &claimAddress)
	claim := claimAddress.Address
	if _, err = parseXMRAddress(claim, "stagenet"); err != nil {
		t.Fatal("refusing: claim address is not stagenet")
	}
	if code, _ := r.xmrClaim(order, r.vendorSess, claim, testPassword); code != 303 {
		t.Fatalf("public vendor claim refused: HTTP %d", code)
	}
	var settlementID int64
	var requestedAt time.Time
	if err := r.DB.QueryRow("SELECT id,created FROM xmr_settlements WHERE order_id=$1 AND kind='release'", order).Scan(&settlementID, &requestedAt); err != nil {
		t.Fatal("claim did not create a release")
	}
	proof.SettlementRequested = eventAt("", requestedAt)
	until = time.Now().Add(wait)
	for time.Now().Before(until) {
		preflight(false)
		_ = r.poll() // a pass can commit one stage and then report an unrelated read error; inspect durable state.
		s := r.settlement(settlementID)
		if proof.SettlementBuilt.At == "" && s.State == "built" {
			if !xmrHex64(s.TxHash) {
				t.Fatal("built settlement has no transaction hash")
			}
			var builtAt time.Time
			if err := r.DB.QueryRow("SELECT updated FROM xmr_settlements WHERE id=$1 AND state='built'", settlementID).Scan(&builtAt); err != nil {
				t.Fatal("built settlement timestamp is unreadable")
			}
			proof.SettlementBuilt = eventAt(s.TxHash, builtAt)
		}
		if proof.SettlementRelayed.At == "" && s.State == "relayed" {
			var relayedAt time.Time
			if err := r.DB.QueryRow("SELECT relayed_at FROM xmr_settlements WHERE id=$1 AND state='relayed'", settlementID).Scan(&relayedAt); err != nil {
				t.Fatal("relayed settlement timestamp is unreadable")
			}
			proof.SettlementRelayed = eventAt(s.TxHash, relayedAt)
		}
		if s.State == "confirmed" && xmrHex64(s.TxHash) {
			txs, missed, err := rpc.transactions(context.Background(), []string{s.TxHash})
			if err == nil && len(missed) == 0 && len(txs) == 1 && txs[0].Confirmations >= xmrMinConfirmations {
				proof.SettlementConfirmations = txs[0].Confirmations
				var confirmedAt time.Time
				if err := r.DB.QueryRow("SELECT confirmed_at FROM xmr_settlements WHERE id=$1 AND state='confirmed'", settlementID).Scan(&confirmedAt); err != nil {
					t.Fatal("confirmed settlement timestamp is unreadable")
				}
				proof.SettlementConfirmed = eventAt(s.TxHash, confirmedAt)
				break
			}
		}
		time.Sleep(20 * time.Second)
	}
	if proof.SettlementConfirmed.At == "" {
		t.Fatal("public settlement did not confirm within the bounded wait")
	}
	if proof.SettlementBuilt.Txid != proof.SettlementRelayed.Txid || proof.SettlementBuilt.Txid != proof.SettlementConfirmed.Txid {
		t.Fatal("settlement transaction changed during build, relay or confirmation")
	}
	if r.state(order) != stateCompleted || agInt(r.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order) != 1 ||
		agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order) != 1 {
		t.Fatal("order, settlement or credited deposit is not unique")
	}
	var plannedAddress string
	var plannedAmount int64
	if err := r.DB.QueryRow("SELECT address,amount FROM xmr_settlement_outputs WHERE settlement_id=$1 AND role='claimant'", settlementID).Scan(&plannedAddress, &plannedAmount); err != nil || plannedAddress != claim || plannedAmount <= 0 {
		t.Fatal("settlement claimant output does not match the vendor's claim address")
	}
	r.call(funder, "refresh", nil, nil)
	var incoming struct {
		In []xmrTransferJSON `json:"in"`
	}
	r.call(funder, "get_transfers", map[string]any{"in": true, "account_index": 0}, &incoming)
	for _, raw := range incoming.In {
		entry, ok := raw.parse()
		if ok && entry.TxID == proof.SettlementConfirmed.Txid && entry.Account == 0 && entry.Minor == claimAddress.Index && entry.Confirmations >= xmrMinConfirmations {
			proof.ClaimReceivedPiconero += entry.Amount
		}
	}
	if proof.ClaimReceivedPiconero != plannedAmount {
		t.Fatal("funder wallet did not confirm the planned claimant amount at the claim subaddress")
	}
	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), daemonURL) || strings.Contains(string(data), walletURL) || strings.Contains(string(data), funderURL) {
		t.Fatal("proof unexpectedly contains an RPC URL")
	}
	t.Logf("xmr-public: proof %s", data)
}
