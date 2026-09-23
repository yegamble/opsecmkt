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
	"time"
)

// Bitcoin Core adapter (testnet3 "test", testnet4, signet, regtest). Mainnet is refused at startup.

func init() { registerProvider(bitcoinFromEnv) }

var bitcoinTestChains = map[string]string{"test": "tb1", "testnet4": "tb1", "signet": "tb1", "regtest": "bcrt1"}

type bitcoinProvider struct {
	rpc          *rpcClient
	walletName   string
	wallet       string // "/wallet/<name>"
	wantChain    string // BITCOIN_CHAIN ("" = any test chain the node reports)
	chain        string // as reported by getblockchaininfo; set by the first successful Check, never changed
	prefix       string // bech32 HRP prefix for this chain
	confirmation int
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
		if _, ok := bitcoinTestChains[wantChain]; !ok {
			return nil, fmt.Errorf("refusing to start: BITCOIN_CHAIN %q is not a test network (use test for testnet3, testnet4, signet or regtest)", wantChain)
		}
	}
	c, err := newRPCClient("bitcoin", rawURL, "1.0", false)
	if err != nil {
		return nil, err
	}
	return &bitcoinProvider{rpc: c, walletName: wallet, wallet: "/wallet/" + url.PathEscape(wallet), wantChain: wantChain, confirmation: confs}, nil
}

// newBitcoinProvider builds the provider and checks it once (node reachable, test chain, wallet loaded).
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

// Check refuses non-test chains (and a chain that differs from BITCOIN_CHAIN or from the chain seen before),
// loads the wallet when the node reports it is not loaded (after a node restart, for example) and reports
// initialblockdownload as syncing.
func (p *bitcoinProvider) Check(ctx context.Context) (bool, error) {
	var info struct {
		Chain string `json:"chain"`
		IBD   bool   `json:"initialblockdownload"`
	}
	if err := p.rpc.call(ctx, "", "getblockchaininfo", nil, &info); err != nil {
		return false, fmt.Errorf("bitcoin node check failed: %w", err)
	}
	prefix, ok := bitcoinTestChains[info.Chain]
	if !ok {
		return false, refusef("bitcoin chain %q is not a test network (allowed: testnet3 \"test\", testnet4, signet, regtest)", info.Chain)
	}
	if p.wantChain != "" && p.wantChain != info.Chain {
		return false, refusef("bitcoin node reports chain %q but BITCOIN_CHAIN is %q", info.Chain, p.wantChain)
	}
	if p.chain != "" && p.chain != info.Chain {
		return false, refusef("bitcoin node now reports chain %q; it was %q", info.Chain, p.chain)
	}
	var winfo struct {
		WalletName string `json:"walletname"`
	}
	err := p.rpc.call(ctx, p.wallet, "getwalletinfo", nil, &winfo)
	var re *rpcError
	if errors.As(err, &re) && re.Code == -18 {
		if lerr := p.rpc.call(ctx, "", "loadwallet", []any{p.walletName}, nil); lerr != nil {
			return false, fmt.Errorf("bitcoin wallet %q is not loaded and could not be loaded (create it once with the createwallet RPC; see docs/testnet-runbook.md): %w", p.walletName, lerr)
		}
		err = p.rpc.call(ctx, p.wallet, "getwalletinfo", nil, &winfo)
	}
	if err != nil {
		return false, fmt.Errorf("bitcoin wallet %q unavailable: %w", p.walletName, err)
	}
	if p.chain == "" { // first success, before the provider is published to request handlers
		p.chain, p.prefix = info.Chain, prefix
	}
	return info.IBD, nil
}

func (p *bitcoinProvider) Currency() string { return "BTC" }
func (p *bitcoinProvider) Network() string {
	chain := p.chain
	if chain == "" {
		chain = p.wantChain // before the first successful check
	}
	if chain == "test" {
		return "testnet3"
	}
	return chain
}
func (p *bitcoinProvider) Confirmations() int { return p.confirmation }

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
