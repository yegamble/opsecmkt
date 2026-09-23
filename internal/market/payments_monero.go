package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// monero-wallet-rpc adapter (stagenet, testnet). Mainnet wallets and daemons are refused at startup.

func init() { registerProvider(moneroFromEnv) }

// Address prefixes: primary / subaddress (integrated addresses share the primary network byte family).
var moneroNetworks = map[string]struct{ primary, sub string }{
	"stagenet": {"5", "7"},
	"testnet":  {"9A", "B"},
}

type moneroProvider struct {
	wallet       *rpcClient
	daemon       *rpcClient // nil unless MONERO_RPC_URL is set
	wantNetwork  string     // MONERO_NETWORK ("" = any test network the wallet reports)
	network      string     // from the wallet's primary address; set by the first successful Check, never changed
	confirmation int

	mu    sync.Mutex
	index map[string]int64 // subaddress -> minor index (account 0)
}

func moneroFromEnv(ctx context.Context) (PaymentProvider, error) {
	raw := strings.TrimSpace(os.Getenv("MONERO_WALLET_RPC_URL"))
	if raw == "" {
		return nil, nil
	}
	if _, err := pollInterval(); err != nil {
		return nil, err
	}
	if _, err := paymentExpiry(); err != nil {
		return nil, err
	}
	confs, err := envConfirmations("PAYMENT_CONFIRMATIONS_XMR", 10)
	if err != nil {
		return nil, err
	}
	return buildMoneroProvider(raw, strings.TrimSpace(os.Getenv("MONERO_RPC_URL")), strings.TrimSpace(os.Getenv("MONERO_NETWORK")), confs)
}

// buildMoneroProvider validates configuration without contacting the wallet or daemon.
func buildMoneroProvider(walletURL, daemonURL, wantNetwork string, confs int) (*moneroProvider, error) {
	if wantNetwork != "" {
		if _, ok := moneroNetworks[wantNetwork]; !ok {
			return nil, fmt.Errorf("refusing to start: MONERO_NETWORK %q is not a test network (use stagenet or testnet)", wantNetwork)
		}
	}
	w, err := newRPCClient("monero wallet", walletURL, "2.0", true)
	if err != nil {
		return nil, err
	}
	p := &moneroProvider{wallet: w, wantNetwork: wantNetwork, confirmation: confs, index: map[string]int64{}}
	if daemonURL != "" {
		if p.daemon, err = newRPCClient("monero daemon", daemonURL, "2.0", true); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// newMoneroProvider builds the provider and checks it once (wallet open, test network, daemon agrees).
func newMoneroProvider(ctx context.Context, walletURL, daemonURL, wantNetwork string, confs int) (*moneroProvider, error) {
	p, err := buildMoneroProvider(walletURL, daemonURL, wantNetwork, confs)
	if err != nil {
		return nil, err
	}
	if _, err = p.Check(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Check opens the conventional wallet when monero-wallet-rpc has none open (for example after the wallet
// service restarted), refuses wallets and daemons that are not on a test network or disagree with
// MONERO_NETWORK or with each other, and reports the daemon as syncing while its target_height is above its
// height. Without MONERO_RPC_URL the sync state is not checked.
func (p *moneroProvider) Check(ctx context.Context) (bool, error) {
	var addr struct {
		Address string `json:"address"`
	}
	getAddress := func() error {
		return p.wallet.call(ctx, "/json_rpc", "get_address", map[string]any{"account_index": 0}, &addr)
	}
	err := getAddress()
	var re *rpcError
	if errors.As(err, &re) && re.Code == -13 { // no wallet file open: try the conventional wallet once
		if oerr := p.wallet.call(ctx, "/json_rpc", "open_wallet", map[string]any{"filename": "opsecmkt", "password": ""}, nil); oerr != nil {
			return false, fmt.Errorf("monero-wallet-rpc has no open wallet and wallet \"opsecmkt\" could not be opened (create it with the create_wallet RPC; see docs/testnet-runbook.md): %w", oerr)
		}
		err = getAddress()
	}
	if err != nil {
		return false, fmt.Errorf("monero wallet check failed: %w", err)
	}
	network := ""
	for name, n := range moneroNetworks {
		if addr.Address != "" && strings.ContainsRune(n.primary, rune(addr.Address[0])) {
			network = name
		}
	}
	if network == "" {
		kind := "unknown"
		if addr.Address != "" && strings.ContainsRune("48", rune(addr.Address[0])) {
			kind = "mainnet"
		}
		return false, refusef("monero wallet primary address is a %s address (prefix %q); only stagenet or testnet wallets are allowed", kind, truncate(addr.Address, 1))
	}
	if p.wantNetwork != "" && p.wantNetwork != network {
		return false, refusef("monero wallet is on %s but MONERO_NETWORK is %q", network, p.wantNetwork)
	}
	if p.network != "" && p.network != network {
		return false, refusef("monero wallet is now on %s; it was on %s", network, p.network)
	}
	syncing := false
	if p.daemon != nil {
		var info struct {
			NetType      string      `json:"nettype"`
			Mainnet      bool        `json:"mainnet"`
			Stagenet     bool        `json:"stagenet"`
			Testnet      bool        `json:"testnet"`
			Height       json.Number `json:"height"`
			TargetHeight json.Number `json:"target_height"`
		}
		if err = p.daemon.call(ctx, "/json_rpc", "get_info", nil, &info); err != nil {
			return false, fmt.Errorf("monero daemon check failed: %w", err)
		}
		nt := info.NetType
		if nt == "" {
			switch {
			case info.Stagenet:
				nt = "stagenet"
			case info.Testnet:
				nt = "testnet"
			default:
				nt = "mainnet"
			}
		}
		if _, ok := moneroNetworks[nt]; !ok || info.Mainnet {
			return false, refusef("monero daemon network %q is not a test network", nt)
		}
		if nt != network {
			return false, refusef("monero daemon is on %s but the wallet is on %s", nt, network)
		}
		height, herr := info.Height.Int64()
		target, terr := info.TargetHeight.Int64()
		syncing = herr == nil && terr == nil && target > height
	}
	if p.network == "" { // first success, before the provider is published to request handlers
		p.network = network
	}
	return syncing, nil
}

func (p *moneroProvider) Currency() string { return "XMR" }
func (p *moneroProvider) Network() string {
	if p.network == "" {
		return p.wantNetwork // before the first successful check
	}
	return p.network
}
func (p *moneroProvider) Confirmations() int { return p.confirmation }

func (p *moneroProvider) NewAddress(ctx context.Context, orderID string) (string, error) {
	var r struct {
		Address      string      `json:"address"`
		AddressIndex json.Number `json:"address_index"`
	}
	if err := p.wallet.call(ctx, "/json_rpc", "create_address", map[string]any{"account_index": 0, "label": "order:" + orderID}, &r); err != nil {
		return "", err
	}
	if r.Address == "" || !strings.HasPrefix(r.Address, moneroNetworks[p.network].sub) {
		return "", fmt.Errorf("refusing address: monero wallet returned a subaddress with prefix %q, not a %s subaddress", truncate(r.Address, 1), p.network)
	}
	if idx, err := r.AddressIndex.Int64(); err == nil {
		p.mu.Lock()
		p.index[r.Address] = idx
		p.mu.Unlock()
	}
	return r.Address, nil
}

// minorIndex resolves (and caches) a subaddress's minor index in account 0.
func (p *moneroProvider) minorIndex(ctx context.Context, addr string) (int64, bool, error) {
	p.mu.Lock()
	idx, ok := p.index[addr]
	p.mu.Unlock()
	if ok {
		return idx, true, nil
	}
	var r struct {
		Index struct {
			Major json.Number `json:"major"`
			Minor json.Number `json:"minor"`
		} `json:"index"`
	}
	if err := p.wallet.call(ctx, "/json_rpc", "get_address_index", map[string]any{"address": addr}, &r); err != nil {
		var re *rpcError
		if errors.As(err, &re) && re.Code == -2 { // WALLET_RPC_ERROR_CODE_WRONG_ADDRESS
			return 0, false, nil // not an address of this wallet
		}
		// Any other error (no wallet open, busy) is a failed read: the caller must not treat the address as empty.
		return 0, false, err
	}
	major, _ := r.Index.Major.Int64()
	minor, err := r.Index.Minor.Int64()
	if err != nil || major != 0 {
		return 0, false, nil
	}
	p.mu.Lock()
	p.index[addr] = minor
	p.mu.Unlock()
	return minor, true, nil
}

// Incoming uses get_transfers (in + pool) for the subaddresses; double_spend_seen reports -1 confirmations and
// a non-zero unlock_time marks the transfer Locked.
func (p *moneroProvider) Incoming(ctx context.Context, addresses []string) ([]Incoming, error) {
	byIndex := map[int64]string{}
	var indices []int64
	for _, a := range addresses {
		idx, ok, err := p.minorIndex(ctx, a)
		if err != nil {
			return nil, err
		}
		if ok {
			byIndex[idx] = a
			indices = append(indices, idx)
		}
	}
	if len(indices) == 0 {
		return nil, nil
	}
	type transfer struct {
		TxID            string      `json:"txid"`
		Amount          json.Number `json:"amount"`
		Confirmations   json.Number `json:"confirmations"`
		DoubleSpendSeen bool        `json:"double_spend_seen"`
		UnlockTime      json.Number `json:"unlock_time"`
		SubaddrIndex    struct {
			Major json.Number `json:"major"`
			Minor json.Number `json:"minor"`
		} `json:"subaddr_index"`
	}
	var r struct {
		In   []transfer `json:"in"`
		Pool []transfer `json:"pool"`
	}
	if err := p.wallet.call(ctx, "/json_rpc", "get_transfers", map[string]any{"in": true, "pool": true, "account_index": 0, "subaddr_indices": indices}, &r); err != nil {
		return nil, err
	}
	var out []Incoming
	for i, list := range [][]transfer{r.In, r.Pool} {
		for _, t := range list {
			minor, err := t.SubaddrIndex.Minor.Int64()
			addr, ok := byIndex[minor]
			if err != nil || !ok || t.TxID == "" {
				continue
			}
			amt, err := t.Amount.Int64()
			if err != nil || amt <= 0 {
				continue
			}
			var confs int64
			if i == 0 && t.Confirmations != "" {
				if confs, err = t.Confirmations.Int64(); err != nil {
					return nil, errors.New("monero get_transfers: bad confirmations")
				}
			}
			if t.DoubleSpendSeen {
				confs = -1
			}
			locked := t.UnlockTime != "" && t.UnlockTime != "0" // spendable only later: reported, never credited
			out = append(out, Incoming{Address: addr, TxID: t.TxID, Index: minor, Amount: amt, Confirmations: confs, Locked: locked})
		}
	}
	return out, nil
}

// Send transfers from account 0 of the pooled wallet; the network fee is paid by the wallet on top of amount.
func (p *moneroProvider) Send(ctx context.Context, to string, amt int64) (string, error) {
	var r struct {
		TxHash string `json:"tx_hash"`
	}
	err := p.wallet.call(ctx, "/json_rpc", "transfer", map[string]any{"destinations": []map[string]any{{"amount": amt, "address": to}}, "account_index": 0, "priority": 0}, &r)
	if err == nil && r.TxHash == "" {
		err = errors.New("monero transfer returned no transaction hash")
	}
	return r.TxHash, err
}

func (p *moneroProvider) ValidAddress(addr string) bool {
	if len(addr) < 90 || len(addr) > 110 || strings.ContainsRune("48", rune(addr[0])) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	var v struct {
		Valid   bool   `json:"valid"`
		NetType string `json:"nettype"`
	}
	err := p.wallet.call(ctx, "/json_rpc", "validate_address", map[string]any{"address": addr, "any_net_type": true}, &v)
	return err == nil && v.Valid && v.NetType == p.network
}
