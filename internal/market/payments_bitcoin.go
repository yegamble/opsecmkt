package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bitcoin Core adapter (testnet3 "test", testnet4, signet, regtest, and mainnet "main"). Mainnet is used only
// when BITCOIN_CHAIN=main names it; a node that reports main otherwise is refused.

func init() { registerProvider(bitcoinFromEnv) }

// bitcoinChains maps each chain getblockchaininfo reports to its bech32 address prefix.
var bitcoinChains = map[string]string{"test": "tb1", "testnet4": "tb1", "signet": "tb1", "regtest": "bcrt1", "main": "bc1"}

type bitcoinProvider struct {
	rpc          *rpcClient
	walletName   string
	wallet       string // "/wallet/<name>"
	wantChain    string // BITCOIN_CHAIN ("" = any test chain the node reports; "main" = mainnet, never implied)
	chain        string // as reported by getblockchaininfo; set by the first successful Check, never changed
	prefix       string // bech32 HRP prefix for this chain
	confirmation int

	mu          sync.Mutex
	nodeVersion string // getnetworkinfo, read by each successful Check and shown on /admin (A-174)
}

func bitcoinFromEnv(ctx context.Context) (PaymentProvider, error) {
	raw := strings.TrimSpace(os.Getenv("BITCOIN_RPC_URL"))
	if raw == "" {
		return nil, nil
	}
	if _, err := pollInterval(); err != nil {
		return nil, err
	}
	if _, err := paymentExpiry(); err != nil {
		return nil, err
	}
	if _, err := parseMaxTipAge(os.Getenv("BITCOIN_MAX_TIP_AGE")); err != nil {
		return nil, err
	}
	confs, err := envConfirmations("PAYMENT_CONFIRMATIONS_BTC", 3)
	if err != nil {
		return nil, err
	}
	wallet := strings.TrimSpace(os.Getenv("BITCOIN_WALLET"))
	if wallet == "" {
		wallet = "opsecmkt"
	}
	return buildBitcoinProvider(raw, wallet, strings.TrimSpace(os.Getenv("BITCOIN_CHAIN")), confs)
}

// buildBitcoinProvider validates configuration without contacting the node.
func buildBitcoinProvider(rawURL, wallet, wantChain string, confs int) (*bitcoinProvider, error) {
	if wantChain != "" {
		if _, ok := bitcoinChains[wantChain]; !ok {
			return nil, fmt.Errorf("refusing to start: BITCOIN_CHAIN %q is not a known chain (use main for mainnet, or test for testnet3, testnet4, signet or regtest)", wantChain)
		}
	}
	c, err := newRPCClient("bitcoin", rawURL, "1.0", false)
	if err != nil {
		return nil, err
	}
	return &bitcoinProvider{rpc: c, walletName: wallet, wallet: "/wallet/" + url.PathEscape(wallet), wantChain: wantChain, confirmation: confs}, nil
}

// newBitcoinProvider builds the provider and checks it once (node reachable, configured chain, wallet loaded).
func newBitcoinProvider(ctx context.Context, rawURL, wallet, wantChain string, confs int) (*bitcoinProvider, error) {
	p, err := buildBitcoinProvider(rawURL, wallet, wantChain, confs)
	if err != nil {
		return nil, err
	}
	if _, err = p.Check(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Check refuses mainnet unless BITCOIN_CHAIN=main (and a chain that differs from BITCOIN_CHAIN or from the
// chain seen before), loads the wallet when the node reports it is not loaded (after a node restart, for example) and reports
// "blocks" as the tip (the height gettransaction counts confirmations from) and the node as syncing while it is not
// synced (bitcoinNode.notSynced, A-439: initial block download, headers ahead of blocks, and on a public chain no
// peers or a tip older than BITCOIN_MAX_TIP_AGE). A failed getnetworkinfo fails nothing, but on a public chain the
// node then counts as not synced (its peer count is unknown).
func (p *bitcoinProvider) Check(ctx context.Context) (nodeStatus, error) {
	var info struct {
		Chain   string      `json:"chain"`
		IBD     bool        `json:"initialblockdownload"`
		Blocks  json.Number `json:"blocks"`
		Headers int64       `json:"headers"`
		Time    int64       `json:"time"`
	}
	if err := p.rpc.call(ctx, "", "getblockchaininfo", nil, &info); err != nil {
		return nodeStatus{}, fmt.Errorf("bitcoin node check failed: %w", err)
	}
	prefix, ok := bitcoinChains[info.Chain]
	if !ok {
		return nodeStatus{}, refusef("bitcoin chain %q is not a known chain", info.Chain)
	}
	if info.Chain == "main" && p.wantChain != "main" {
		return nodeStatus{}, refusef("bitcoin chain %q is not a test network (set BITCOIN_CHAIN=main to accept mainnet; test networks: testnet3 \"test\", testnet4, signet, regtest)", info.Chain)
	}
	if p.wantChain != "" && p.wantChain != info.Chain {
		return nodeStatus{}, refusef("bitcoin node reports chain %q but BITCOIN_CHAIN is %q", info.Chain, p.wantChain)
	}
	if p.chain != "" && p.chain != info.Chain {
		return nodeStatus{}, refusef("bitcoin node now reports chain %q; it was %q", info.Chain, p.chain)
	}
	var tip int64
	if info.Blocks != "" {
		var err error
		if tip, err = info.Blocks.Int64(); err != nil || tip < 0 {
			return nodeStatus{}, fmt.Errorf("bitcoin node check failed: bad block height %q", truncate(string(info.Blocks), 20))
		}
	}
	var winfo struct {
		WalletName string `json:"walletname"`
	}
	err := p.rpc.call(ctx, p.wallet, "getwalletinfo", nil, &winfo)
	var re *rpcError
	if errors.As(err, &re) && re.Code == -18 {
		if lerr := p.rpc.call(ctx, "", "loadwallet", []any{p.walletName}, nil); lerr != nil {
			return nodeStatus{}, fmt.Errorf("bitcoin wallet %q is not loaded and could not be loaded (create it once with the createwallet RPC; see docs/testnet-runbook.md): %w", p.walletName, lerr)
		}
		err = p.rpc.call(ctx, p.wallet, "getwalletinfo", nil, &winfo)
	}
	if err != nil {
		return nodeStatus{}, fmt.Errorf("bitcoin wallet %q unavailable: %w", p.walletName, err)
	}
	peers, err := p.readNodeVersion(ctx)
	if err != nil {
		peers = -1 // unknown: on a public chain the node then counts as not synced
	}
	if p.chain == "" { // first success, before the provider is published to request handlers
		p.chain, p.prefix = info.Chain, prefix
	}
	node := bitcoinNode{Chain: info.Chain, IBD: info.IBD, Headers: info.Headers, Blocks: tip, Time: info.Time, Connections: peers}
	why := node.notSynced(time.Now(), maxTipAge())
	return nodeStatus{Syncing: why != "", Why: why, Tip: tip}, nil
}

// readNodeVersion records getnetworkinfo's version (10000*major + 100*minor + build) for the admin page and returns
// its connections (the node's peer count, for the sync guard) with the call's error. The Bitcoin send classification
// does not depend on the version (walletRejected), so a failed call fails nothing.
func (p *bitcoinProvider) readNodeVersion(ctx context.Context) (int64, error) {
	var ni struct {
		Version     json.Number `json:"version"`
		Connections int64       `json:"connections"`
	}
	callErr := p.rpc.call(ctx, "", "getnetworkinfo", nil, &ni)
	err := callErr
	var v int64
	if err == nil {
		if v, err = ni.Version.Int64(); err == nil && v <= 0 {
			err = fmt.Errorf("bad version %d", v)
		}
	}
	text := fmt.Sprintf("Bitcoin Core %d.%d.%d", v/10000, v/100%100, v%100)
	if err != nil {
		text = "Bitcoin Core version unknown: getnetworkinfo failed: " + truncate(err.Error(), 200)
	}
	p.mu.Lock()
	p.nodeVersion = text
	p.mu.Unlock()
	return ni.Connections, callErr
}

// WalletVersion is the node version the last successful check read (versionReporter). It is never unverified:
// a Bitcoin send error is classified the same whatever the version.
func (p *bitcoinProvider) WalletVersion() (text string, unverified bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nodeVersion == "" {
		return "Bitcoin Core version not read yet", false
	}
	return p.nodeVersion, false
}

func (p *bitcoinProvider) Currency() string { return "BTC" }
func (p *bitcoinProvider) Network() string {
	chain := p.chain
	if chain == "" {
		chain = p.wantChain // before the first successful check
	}
	switch chain {
	case "test":
		return "testnet3"
	case "main":
		return "mainnet"
	}
	return chain
}
func (p *bitcoinProvider) Confirmations() int      { return p.confirmation }
func (p *bitcoinProvider) MainnetConfigured() bool { return p.wantChain == "main" }

func (p *bitcoinProvider) NewAddress(ctx context.Context, orderID string) (string, error) {
	var addr string
	if err := p.rpc.call(ctx, p.wallet, "getnewaddress", []any{"order:" + orderID, "bech32"}, &addr); err != nil {
		return "", err
	}
	if !p.formatOK(addr) {
		return "", fmt.Errorf("refusing address: bitcoin wallet returned %q, not a %s address for chain %s", truncate(addr, 16), p.prefix, p.chain)
	}
	return addr, nil
}

// formatOK is the offline half of ValidAddress: lowercase bech32 with this chain's prefix.
func (p *bitcoinProvider) formatOK(addr string) bool {
	if len(addr) < 14 || len(addr) > 90 || !strings.HasPrefix(addr, p.prefix) {
		return false
	}
	if p.prefix == "tb1" && strings.HasPrefix(addr, "bcrt1") {
		return false
	}
	for _, r := range addr {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Incoming lists wallet receipts for addresses: listreceivedbyaddress (minconf 0) gives txids, gettransaction
// gives per-output details and confirmations (negative when conflicted).
func (p *bitcoinProvider) Incoming(ctx context.Context, addresses []string) ([]Incoming, error) {
	want := map[string]bool{}
	for _, a := range addresses {
		want[a] = true
	}
	var received []struct {
		Address string   `json:"address"`
		TxIDs   []string `json:"txids"`
	}
	if err := p.rpc.call(ctx, p.wallet, "listreceivedbyaddress", []any{0, false, true}, &received); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Incoming
	for _, r := range received {
		if !want[r.Address] {
			continue
		}
		for _, txid := range r.TxIDs {
			if seen[txid] {
				continue
			}
			seen[txid] = true
			var tx struct {
				Confirmations json.Number `json:"confirmations"`
				Details       []struct {
					Address  string      `json:"address"`
					Category string      `json:"category"`
					Amount   json.Number `json:"amount"`
					Vout     json.Number `json:"vout"`
				} `json:"details"`
			}
			if err := p.rpc.call(ctx, p.wallet, "gettransaction", []any{txid, true}, &tx); err != nil {
				return nil, err
			}
			confs, err := tx.Confirmations.Int64()
			if err != nil {
				return nil, fmt.Errorf("bitcoin gettransaction %s: bad confirmations", truncate(txid, 16))
			}
			for _, d := range tx.Details {
				if d.Category != "receive" || !want[d.Address] {
					continue
				}
				amt, err := parseAmount(string(d.Amount), 8)
				if err != nil {
					continue // zero or malformed outputs are not payments
				}
				vout, err := d.Vout.Int64()
				if err != nil {
					return nil, fmt.Errorf("bitcoin gettransaction %s: bad vout", truncate(txid, 16))
				}
				out = append(out, Incoming{Address: d.Address, TxID: txid, Index: vout, Amount: amt, Confirmations: confs})
			}
		}
	}
	return out, nil
}

// Send pays from the pooled wallet; the network fee is subtracted from the amount sent.
func (p *bitcoinProvider) Send(ctx context.Context, to string, amt int64) (string, error) {
	if !p.formatOK(to) {
		return "", errors.New("bitcoin payout address is not valid for " + p.chain)
	}
	var txid string
	err := p.rpc.callWithin(ctx, payoutSendTimeout, p.wallet, "sendtoaddress", []any{to, json.Number(amount(amt, 8)), "", "", true}, &txid)
	return txid, err
}

// SendFeeFromAmount is Send: Bitcoin payouts always take the network fee from the amount sent.
func (p *bitcoinProvider) SendFeeFromAmount(ctx context.Context, to string, amt int64) (string, error) {
	return p.Send(ctx, to, amt)
}

func (p *bitcoinProvider) ValidAddress(addr string) bool {
	if !p.formatOK(addr) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	var v struct {
		IsValid bool `json:"isvalid"`
	}
	return p.rpc.call(ctx, "", "validateaddress", []any{addr}, &v) == nil && v.IsValid
}

func envConfirmations(key string, def int) (int, error) {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 1000 {
		return 0, fmt.Errorf("%s must be a whole number from 1 to 1000", key)
	}
	return n, nil
}

// paymentExpiry reads PAYMENT_EXPIRY (Go duration, 10m to 720h; default 24h): how long an order may await
// payment with no deposit seen before the watcher cancels it and its reserved stock is returned.
func paymentExpiry() (time.Duration, error) {
	s := strings.TrimSpace(os.Getenv("PAYMENT_EXPIRY"))
	if s == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 10*time.Minute || d > 720*time.Hour {
		return 0, errors.New("PAYMENT_EXPIRY must be a duration from 10m to 720h, e.g. 24h")
	}
	return d, nil
}

// expiryWindow is the payment window the watcher's expiry (settleOrder) and the order page's cancellation time
// both use (A-123). PAYMENT_EXPIRY is validated at startup by the provider factories; 24h if it is not.
func expiryWindow() time.Duration {
	d, err := paymentExpiry()
	if err != nil {
		return 24 * time.Hour
	}
	return d
}

// pollInterval reads PAYMENT_POLL_INTERVAL (Go duration, 1s to 1h; default 30s).
func pollInterval() (time.Duration, error) {
	s := strings.TrimSpace(os.Getenv("PAYMENT_POLL_INTERVAL"))
	if s == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second || d > time.Hour {
		return 0, errors.New("PAYMENT_POLL_INTERVAL must be a duration from 1s to 1h, e.g. 30s")
	}
	return d, nil
}
