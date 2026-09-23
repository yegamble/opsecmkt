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
	network      string
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
	return newMoneroProvider(ctx, raw, strings.TrimSpace(os.Getenv("MONERO_RPC_URL")), strings.TrimSpace(os.Getenv("MONERO_NETWORK")), confs)
}

func newMoneroProvider(ctx context.Context, walletURL, daemonURL, wantNetwork string, confs int) (*moneroProvider, error) {
	if wantNetwork != "" {
		if _, ok := moneroNetworks[wantNetwork]; !ok {
			return nil, fmt.Errorf("refusing to start: MONERO_NETWORK %q is not a test network (use stagenet or testnet)", wantNetwork)
		}
	}
	w, err := newRPCClient("monero wallet", walletURL, "2.0", true)
	if err != nil {
		return nil, err
	}
	var addr struct {
		Address string `json:"address"`
	}
	getAddress := func() error {
		return w.call(ctx, "/json_rpc", "get_address", map[string]any{"account_index": 0}, &addr)
	}
	err = getAddress()
	var re *rpcError
	if errors.As(err, &re) && re.Code == -13 { // no wallet file open: try the conventional wallet once
		if oerr := w.call(ctx, "/json_rpc", "open_wallet", map[string]any{"filename": "opsecmkt", "password": ""}, nil); oerr != nil {
			return nil, fmt.Errorf("monero-wallet-rpc has no open wallet and wallet \"opsecmkt\" could not be opened (create it with the create_wallet RPC): %w", oerr)
		}
		err = getAddress()
	}
	if err != nil {
		return nil, fmt.Errorf("monero payments configured but the wallet check failed: %w", err)
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
		return nil, fmt.Errorf("refusing to start: monero wallet primary address is a %s address (prefix %q); only stagenet or testnet wallets are allowed", kind, truncate(addr.Address, 1))
	}
	if wantNetwork != "" && wantNetwork != network {
		return nil, fmt.Errorf("refusing to start: monero wallet is on %s but MONERO_NETWORK is %q", network, wantNetwork)
	}
	if daemonURL != "" {
		d, err := newRPCClient("monero daemon", daemonURL, "2.0", true)
		if err != nil {
			return nil, err
		}
		var info struct {
			NetType  string `json:"nettype"`
			Mainnet  bool   `json:"mainnet"`
			Stagenet bool   `json:"stagenet"`
			Testnet  bool   `json:"testnet"`
		}
		if err = d.call(ctx, "/json_rpc", "get_info", nil, &info); err != nil {
			return nil, fmt.Errorf("monero daemon check failed: %w", err)
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
			return nil, fmt.Errorf("refusing to start: monero daemon network %q", nt)
		}
		if nt != network {
			return nil, fmt.Errorf("refusing to start: monero daemon is on %s but the wallet is on %s", nt, network)
		}
	}
	return &moneroProvider{wallet: w, network: network, confirmation: confs, index: map[string]int64{}}, nil
}

func (p *moneroProvider) Currency() string   { return "XMR" }
func (p *moneroProvider) Network() string    { return p.network }
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
		if errors.As(err, &re) {
			return 0, false, nil // not an address of this wallet
		}
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
