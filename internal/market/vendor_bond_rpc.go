package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bond wallets are separate from buyer escrows. This adapter can allocate receiving
// addresses and observe held funds; it never signs, builds or sends a withdrawal.
var vendorBondBTCRPCMethods = []string{"getblockchaininfo", "getnetworkinfo", "getwalletinfo", "getaddressesbylabel", "getnewaddress", "getaddressinfo", "listunspent"}
var vendorBondXMRWalletRPCMethods = []string{"refresh", "get_height", "get_address", "get_accounts", "create_account", "store", "get_balance", "get_transfers", "incoming_transfers"}
var vendorBondXMRDaemonRPCMethods = []string{"get_info", "hard_fork_info", "get_last_block_header", "/get_transactions", "/is_key_image_spent"}

const (
	vendorBondBTCIdentitySetting = "vendor_bond_BTC_wallet_identity"
	vendorBondXMRIdentitySetting = "vendor_bond_XMR_wallet_identity"
	vendorBondBTCIdentityLabel   = "vendor-bond:identity"
	vendorBondBTCNetworkSetting  = "vendor_bond_btc_network"
	vendorBondXMRNetworkSetting  = "vendor_bond_xmr_network"
)

type vendorBondRPCError struct{ Currency, Method, Class string }

// A valid configured wallet stays attached when its node is temporarily offline.
// Initialization is serialized and retried before every operation until the wallet
// and network are checked and their permanent pins verified. The map never changes
// after startup, and no unverified destination or observation escapes this wrapper.
type lazyBondProvider struct {
	coin       string
	provider   bondProvider
	initialize func(context.Context) error
	ready      bool
	mu         bondRPCGate
}

func (p *lazyBondProvider) Currency() string { return p.coin }

func (p *lazyBondProvider) ensureReady(ctx context.Context) error {
	if p.ready {
		return nil
	}
	if err := p.initialize(ctx); err != nil {
		return err
	}
	p.ready = true
	return nil
}

func (p *lazyBondProvider) Allocate(ctx context.Context, quoteID string) (string, string, error) {
	if err := p.mu.lock(ctx); err != nil {
		return "", "", err
	}
	defer p.mu.unlock()
	if err := p.ensureReady(ctx); err != nil {
		return "", "", err
	}
	return p.provider.Allocate(ctx, quoteID)
}

func (p *lazyBondProvider) Observe(ctx context.Context, address, reference string) (int64, int64, error) {
	if err := p.mu.lock(ctx); err != nil {
		return 0, 0, err
	}
	defer p.mu.unlock()
	if err := p.ensureReady(ctx); err != nil {
		return 0, 0, err
	}
	return p.provider.Observe(ctx, address, reference)
}

// Serializing wallet calls must still respect the caller's deadline while another
// allocation/observation owns the wallet; a mutex would leave canceled requests queued.
type bondRPCGate struct {
	once sync.Once
	gate chan struct{}
}

func (g *bondRPCGate) lock(ctx context.Context) error {
	g.once.Do(func() { g.gate = make(chan struct{}, 1) })
	select {
	case g.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *bondRPCGate) unlock() { <-g.gate }

func (e *vendorBondRPCError) Error() string {
	return e.Currency + " vendor bond " + e.Method + ": " + e.Class
}

func bondRPCFailure(currency, method string, err error) error {
	if err == nil {
		return nil
	}
	var be *escrowRPCError
	if errors.As(err, &be) {
		return &vendorBondRPCError{currency, method, be.Class}
	}
	var xe *xmrRPCError
	if errors.As(err, &xe) {
		return &vendorBondRPCError{currency, method, xe.Class}
	}
	be = classifyEscrowRPC(method, err).(*escrowRPCError)
	return &vendorBondRPCError{currency, method, be.Class}
}

func bondBadReply(currency, method string) error {
	return &vendorBondRPCError{currency, method, "bad-reply"}
}

func bondQuoteLabel(id string) (string, error) {
	if len(id) < 8 || len(id) > 80 || strings.ContainsFunc(id, func(r rune) bool { return !isASCIIAlnum(r) && r != '-' && r != '_' }) {
		return "", errors.New("vendor bond quote identifier is invalid")
	}
	return "bond:" + id, nil
}

// Pin identities permanently, including when no quote has yet been paid. A
// replacement wallet must not turn old account indices into another vendor's funds.
func (a *App) pinVendorBondWallet(ctx context.Context, key, identity string) error {
	if _, err := a.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, key, identity); err != nil {
		return err
	}
	return a.checkVendorBondWalletPin(ctx, key, identity)
}

func (a *App) checkVendorBondWalletPin(ctx context.Context, key, identity string) error {
	var pinned string
	if err := a.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", key).Scan(&pinned); err != nil {
		return err
	}
	if pinned != identity {
		return errors.New("vendor bond wallet identity or network differs from this database's permanent pin; restore the original dedicated bond wallet")
	}
	return nil
}

func bondConfirmations(getenv func(string) string, key string, def, floor int64) (int64, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < floor || n > 1000 {
		return 0, fmt.Errorf("%s must be a whole number from %d to 1000", key, floor)
	}
	return n, nil
}

// configureVendorBondProviders reads only the dedicated bond settings. It never
// falls back to an order escrow or the removed pooled-wallet configuration.
func (a *App) configureVendorBondProviders(ctx context.Context) error {
	providers, err := a.vendorBondProvidersFromEnv(ctx, os.Getenv)
	if err == nil {
		a.bondProviders = providers
	}
	return err
}

func (a *App) vendorBondProvidersFromEnv(ctx context.Context, getenv func(string) string) (map[string]bondProvider, error) {
	out := make(map[string]bondProvider)
	btcURL, btcWallet := strings.TrimSpace(getenv("VENDOR_BOND_BTC_RPC_URL")), strings.TrimSpace(getenv("VENDOR_BOND_BTC_WALLET"))
	if btcURL != "" || btcWallet != "" {
		if btcURL == "" || btcWallet == "" {
			return nil, errors.New("Bitcoin vendor bonds need VENDOR_BOND_BTC_RPC_URL and VENDOR_BOND_BTC_WALLET")
		}
		for _, key := range []string{"ESCROW_BTC_WATCH_WALLET", "ESCROW_BTC_ARBITER_WALLET"} {
			name := strings.TrimSpace(getenv(key))
			if name == "" {
				name = map[string]string{"ESCROW_BTC_WATCH_WALLET": "opsecmkt-escrow-watch", "ESCROW_BTC_ARBITER_WALLET": "opsecmkt-arbiter"}[key]
			}
			if btcWallet == name {
				return nil, errors.New("VENDOR_BOND_BTC_WALLET must be a dedicated wallet, separate from the escrow watch and arbiter wallets")
			}
		}
		confs, err := bondConfirmations(getenv, "VENDOR_BOND_BTC_CONFIRMATIONS", 3, 1)
		if err != nil {
			return nil, err
		}
		chain := strings.TrimSpace(getenv("BITCOIN_CHAIN"))
		if chain != "" && escrowChains[chain] == "" || a.live && chain != "main" {
			return nil, errors.New("Bitcoin vendor bonds need a supported BITCOIN_CHAIN; mainnet requires BITCOIN_CHAIN=main")
		}
		rpc, err := newRPCClient("bitcoin vendor bond", btcURL, "1.0", false)
		if err != nil {
			return nil, errors.New("VENDOR_BOND_BTC_RPC_URL must be http(s)://[user:password@]host:port")
		}
		if u, _ := url.Parse(rpc.endpoint); strings.Contains(u.Path, "/wallet/") {
			return nil, errors.New("VENDOR_BOND_BTC_RPC_URL must be the node URL without a wallet path; choose the wallet with VENDOR_BOND_BTC_WALLET")
		}
		p := &btcBondProvider{rpc: rpc, wallet: btcWallet, path: "/wallet/" + url.PathEscape(btcWallet), chain: chain, live: a.live, confirmations: confs}
		out["BTC"] = &lazyBondProvider{coin: "BTC", provider: p, initialize: func(ctx context.Context) error {
			// Failed initialization can have read a replacement wallet. Retry from
			// the configured network, rather than treating that failed read as a pin.
			p.chain, p.chainID, p.identityAddress, p.identity, p.pin = chain, "", "", "", nil
			if err := p.check(ctx); err != nil {
				return err
			}
			address, err := p.labelAddress(ctx, vendorBondBTCIdentityLabel, true)
			if err != nil {
				return err
			}
			p.identityAddress = address
			p.identity = p.chainID + "|" + btcWallet + "|" + address
			if err = a.pinVendorBondWallet(ctx, vendorBondBTCIdentitySetting, p.identity); err != nil {
				return err
			}
			if err = a.pinVendorBondWallet(ctx, vendorBondBTCNetworkSetting, p.chain); err != nil {
				return err
			}
			p.pin = func(ctx context.Context) error {
				return a.checkVendorBondWalletPin(ctx, vendorBondBTCIdentitySetting, p.identity)
			}
			return nil
		}}
	}
	wmr, dmr := strings.TrimSpace(getenv("VENDOR_BOND_XMR_WALLET_RPC_URL")), strings.TrimSpace(getenv("VENDOR_BOND_XMR_DAEMON_URL"))
	if wmr != "" || dmr != "" {
		if wmr == "" || dmr == "" {
			return nil, errors.New("Monero vendor bonds need VENDOR_BOND_XMR_WALLET_RPC_URL and VENDOR_BOND_XMR_DAEMON_URL")
		}
		w, err := newRPCClient("monero vendor bond wallet", wmr, "2.0", true)
		if err != nil {
			return nil, errors.New("VENDOR_BOND_XMR_WALLET_RPC_URL must be http(s)://[user:password@]host:port")
		}
		if escrowURL := strings.TrimSpace(getenv("ESCROW_XMR_WALLET_RPC_URL")); escrowURL != "" {
			escrowRPC, e := newRPCClient("monero escrow", escrowURL, "2.0", true)
			if e == nil && sameBondRPCEndpoint(w.endpoint, escrowRPC.endpoint) {
				return nil, errors.New("VENDOR_BOND_XMR_WALLET_RPC_URL must name a separate wallet-rpc service from ESCROW_XMR_WALLET_RPC_URL")
			}
		}
		d, err := newRPCClient("monero vendor bond daemon", dmr, "2.0", true)
		if err != nil {
			return nil, errors.New("VENDOR_BOND_XMR_DAEMON_URL must be http(s)://[user:password@]host:port")
		}
		confs, err := bondConfirmations(getenv, "VENDOR_BOND_XMR_CONFIRMATIONS", 10, 10)
		if err != nil {
			return nil, err
		}
		network := strings.TrimSpace(getenv("MONERO_NETWORK"))
		if network != "" && network != "fakechain" && network != "stagenet" && network != "mainnet" || a.live && network != "mainnet" {
			return nil, errors.New("Monero vendor bonds need MONERO_NETWORK=fakechain, stagenet or mainnet; mainnet requires MONERO_NETWORK=mainnet")
		}
		p := &xmrBondProvider{rpc: &xmrEscrowRPC{wallet: w, daemon: d, mainnet: a.live}, network: network, confirmations: confs}
		out["XMR"] = &lazyBondProvider{coin: "XMR", provider: p, initialize: func(ctx context.Context) error {
			p.network, p.primary, p.identity, p.pin = network, "", "", nil
			if err := p.check(ctx); err != nil {
				return err
			}
			var escrowWallet bool
			if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM xmr_escrow_wallets WHERE primary_address=$1)", p.primary).Scan(&escrowWallet); err != nil {
				return err
			}
			if escrowWallet {
				return errors.New("the Monero vendor bond wallet must not be an order escrow wallet")
			}
			p.identity = p.network + "|" + p.primary
			if err := a.pinVendorBondWallet(ctx, vendorBondXMRIdentitySetting, p.identity); err != nil {
				return err
			}
			if err := a.pinVendorBondWallet(ctx, vendorBondXMRNetworkSetting, p.network); err != nil {
				return err
			}
			p.pin = func(ctx context.Context) error {
				return a.checkVendorBondWalletPin(ctx, vendorBondXMRIdentitySetting, p.identity)
			}
			return nil
		}}
	}
	// Invalid settings above are startup errors. An RPC outage, an unsynchronized
	// node or an unavailable/mismatched wallet pauses this bond adapter only; old
	// order fulfillment and the rest of the market remain available.
	for _, coin := range []string{"BTC", "XMR"} {
		if p, ok := out[coin].(*lazyBondProvider); ok {
			if err := p.mu.lock(ctx); err == nil {
				_ = p.ensureReady(ctx)
				p.mu.unlock()
			}
		}
	}
	return out, nil
}

func sameBondRPCEndpoint(first, second string) bool {
	canonical := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port + strings.TrimRight(u.EscapedPath(), "/")
	}
	return canonical(first) == canonical(second)
}

type btcBondProvider struct {
	rpc             *rpcClient
	wallet, path    string
	chain, chainID  string
	identityAddress string
	identity        string
	live            bool
	confirmations   int64
	pin             func(context.Context) error
	mu              bondRPCGate
}

func (*btcBondProvider) Currency() string { return "BTC" }

func (p *btcBondProvider) call(ctx context.Context, wallet bool, method string, params, out any) error {
	path := ""
	if wallet {
		path = p.path
	}
	return bondRPCFailure("BTC", method, p.rpc.call(ctx, path, method, params, out))
}

func (p *btcBondProvider) check(ctx context.Context) error {
	var node struct {
		bitcoinNode
		CheckedBlocks  *int64 `json:"blocks"`
		CheckedHeaders *int64 `json:"headers"`
		CheckedIBD     *bool  `json:"initialblockdownload"`
	}
	if err := p.call(ctx, false, "getblockchaininfo", []any{}, &node); err != nil {
		return err
	}
	if node.CheckedBlocks == nil || node.CheckedHeaders == nil || node.CheckedIBD == nil {
		return bondBadReply("BTC", "getblockchaininfo")
	}
	node.Blocks, node.Headers, node.IBD = *node.CheckedBlocks, *node.CheckedHeaders, *node.CheckedIBD
	if escrowChains[node.Chain] == "" || isLiveNetwork(node.Chain) != p.live || p.chain != "" && p.chain != node.Chain {
		return &vendorBondRPCError{"BTC", "network", "network-mismatch"}
	}
	id := escrowChainID(node.Chain, node.SignetChallenge)
	if p.chainID != "" && p.chainID != id {
		return &vendorBondRPCError{"BTC", "network", "network-mismatch"}
	}
	if node.Blocks < 0 || node.Headers < 0 {
		return bondBadReply("BTC", "getblockchaininfo")
	}
	if node.Chain != "regtest" {
		var n struct {
			Connections int64 `json:"connections"`
		}
		if err := p.call(ctx, false, "getnetworkinfo", []any{}, &n); err != nil {
			return err
		}
		node.Connections = n.Connections
	}
	if node.notSynced(time.Now(), maxTipAge()) != "" {
		return &vendorBondRPCError{"BTC", "network", "node-not-synchronized"}
	}
	var wallet struct {
		Name        string          `json:"walletname"`
		PrivateKeys *bool           `json:"private_keys_enabled"`
		Descriptors *bool           `json:"descriptors"`
		Scanning    json.RawMessage `json:"scanning"`
	}
	if err := p.call(ctx, true, "getwalletinfo", []any{}, &wallet); err != nil {
		return err
	}
	if wallet.Name != p.wallet || wallet.PrivateKeys == nil || *wallet.PrivateKeys || wallet.Descriptors == nil || !*wallet.Descriptors {
		return &vendorBondRPCError{"BTC", "wallet", "dedicated-watch-only-descriptor-wallet-required"}
	}
	if len(wallet.Scanning) == 0 || string(wallet.Scanning) != "false" {
		return &vendorBondRPCError{"BTC", "wallet", "wallet-scanning"}
	}
	if p.identityAddress != "" {
		address, err := p.labelAddress(ctx, vendorBondBTCIdentityLabel, false)
		if err != nil {
			return err
		}
		if address != p.identityAddress {
			return &vendorBondRPCError{"BTC", "wallet", "wallet-identity-mismatch"}
		}
	}
	if p.pin != nil {
		if err := p.pin(ctx); err != nil {
			return err
		}
	}
	p.chain, p.chainID = node.Chain, id
	return nil
}

func (p *btcBondProvider) validateAddress(ctx context.Context, address string) error {
	canonical, ok := escrowAddressForm(p.chain, address)
	if !ok || canonical != address {
		return bondBadReply("BTC", "getaddressinfo")
	}
	var info struct {
		Address     string `json:"address"`
		IsMine      *bool  `json:"ismine"`
		IsWatchOnly *bool  `json:"iswatchonly"`
		Solvable    *bool  `json:"solvable"`
	}
	if err := p.call(ctx, true, "getaddressinfo", []any{address}, &info); err != nil {
		return err
	}
	// Descriptor watch wallets can report ismine=true while private keys are
	// disabled. solvable is required; the wallet-wide private-key check is decisive.
	if info.Address != address || info.Solvable == nil || !*info.Solvable || (info.IsMine == nil || !*info.IsMine) && (info.IsWatchOnly == nil || !*info.IsWatchOnly) {
		return bondBadReply("BTC", "getaddressinfo")
	}
	return nil
}

func (p *btcBondProvider) labelAddress(ctx context.Context, label string, create bool) (string, error) {
	var found map[string]struct {
		Purpose string `json:"purpose"`
	}
	err := p.rpc.call(ctx, p.path, "getaddressesbylabel", []any{label}, &found)
	var re *rpcError
	if errors.As(err, &re) && re.Code == -11 {
		err = nil
	}
	if err != nil {
		return "", bondRPCFailure("BTC", "getaddressesbylabel", err)
	}
	var address string
	if len(found) > 1 {
		return "", bondBadReply("BTC", "getaddressesbylabel")
	}
	for candidate, info := range found {
		if info.Purpose != "receive" {
			return "", bondBadReply("BTC", "getaddressesbylabel")
		}
		address = candidate
	}
	if address == "" && create {
		if err := p.call(ctx, true, "getnewaddress", []any{label, "bech32"}, &address); err != nil {
			return "", err
		}
	}
	if address == "" {
		return "", &vendorBondRPCError{"BTC", "wallet", "wallet-identity-missing"}
	}
	return address, p.validateAddress(ctx, address)
}

func (p *btcBondProvider) Allocate(ctx context.Context, quoteID string) (string, string, error) {
	label, err := bondQuoteLabel(quoteID)
	if err != nil {
		return "", "", err
	}
	if err = p.mu.lock(ctx); err != nil {
		return "", "", err
	}
	defer p.mu.unlock()
	if p.pin == nil {
		return "", "", &vendorBondRPCError{"BTC", "wallet", "identity-not-verified"}
	}
	if err = p.check(ctx); err != nil {
		return "", "", err
	}
	address, err := p.labelAddress(ctx, label, true)
	if err != nil {
		return "", "", err
	}
	return address, quoteID, nil
}

func (p *btcBondProvider) Observe(ctx context.Context, address, reference string) (int64, int64, error) {
	label, err := bondQuoteLabel(reference)
	if err != nil {
		return 0, 0, err
	}
	if err = p.mu.lock(ctx); err != nil {
		return 0, 0, err
	}
	defer p.mu.unlock()
	if p.pin == nil {
		return 0, 0, &vendorBondRPCError{"BTC", "wallet", "identity-not-verified"}
	}
	if err = p.check(ctx); err != nil {
		return 0, 0, err
	}
	owned, err := p.labelAddress(ctx, label, false)
	if err != nil {
		return 0, 0, err
	}
	if owned != address {
		return 0, 0, &vendorBondRPCError{"BTC", "wallet", "quote-address-mismatch"}
	}
	var outputs []struct {
		Address       string      `json:"address"`
		TxID          string      `json:"txid"`
		Vout          json.Number `json:"vout"`
		Amount        json.Number `json:"amount"`
		Confirmations json.Number `json:"confirmations"`
		Safe          *bool       `json:"safe"`
	}
	if err = p.call(ctx, true, "listunspent", []any{0, 9999999, []string{address}, true}, &outputs); err != nil {
		return 0, 0, err
	}
	var confirmed, pending int64
	seen := make(map[string]bool)
	for _, o := range outputs {
		value, e := parseAmount(string(o.Amount), 8)
		confs, ok := xmrCount(o.Confirmations)
		vout, indexOK := xmrCount(o.Vout)
		key := o.TxID + ":" + strconv.FormatInt(vout, 10)
		if o.Address != address || !xmrHex64(o.TxID) || !indexOK || vout > math.MaxUint32 || e != nil || !ok || o.Safe == nil || seen[key] || confirmed > math.MaxInt64-value-pending {
			return 0, 0, bondBadReply("BTC", "listunspent")
		}
		seen[key] = true
		if *o.Safe && confs >= p.confirmations {
			confirmed += value
		} else {
			pending += value
		}
	}
	return confirmed, pending, nil
}

type xmrBondProvider struct {
	rpc                        *xmrEscrowRPC
	network, primary, identity string
	confirmations              int64
	pin                        func(context.Context) error
	mu                         bondRPCGate
}

func (*xmrBondProvider) Currency() string { return "XMR" }

func (p *xmrBondProvider) walletCall(ctx context.Context, method string, params, out any) error {
	return bondRPCFailure("XMR", method, p.rpc.wallet.call(ctx, "/json_rpc", method, params, out))
}

func (p *xmrBondProvider) check(ctx context.Context) error {
	info, err := p.rpc.checkDaemon(ctx)
	if err != nil {
		return &vendorBondRPCError{"XMR", "network", "daemon-check-failed"}
	}
	if p.network != "" && p.network != info.NetType {
		return &vendorBondRPCError{"XMR", "network", "network-mismatch"}
	}
	n := moneroNode{NetType: info.NetType, Synchronized: info.Synchronized, Height: info.Height, TargetHeight: info.TargetHeight,
		Offline: info.Offline, Restricted: info.Restricted, Connections: info.Connections}
	if info.NetType != "fakechain" {
		if n.TipTime, err = p.rpc.tipTime(ctx); err != nil {
			return bondRPCFailure("XMR", "get_last_block_header", err)
		}
	}
	if n.notSynced(time.Now(), moneroMaxTipAge()) != "" {
		return &vendorBondRPCError{"XMR", "network", "node-not-synchronized"}
	}
	// Keep refresh request-bounded. A slow wallet disables quotes/credit until a
	// later successful observation, instead of trusting its old balance.
	if err = p.walletCall(ctx, "refresh", nil, nil); err != nil {
		return err
	}
	h, err := p.rpc.height(ctx)
	if err != nil {
		return bondRPCFailure("XMR", "get_height", err)
	}
	// Bonds need a current wallet; accepting a ten-block lag could count a
	// withdrawal or reorg before this wallet has learned about it.
	if h < info.Height || h > info.Height+1 {
		return &vendorBondRPCError{"XMR", "wallet", "wallet-not-synchronized"}
	}
	p.rpc.net.Store(info.NetType)
	primary, err := p.rpc.primaryAddress(ctx)
	if err != nil {
		return bondRPCFailure("XMR", "get_address", err)
	}
	if p.primary != "" && p.primary != primary {
		return &vendorBondRPCError{"XMR", "wallet", "wallet-identity-mismatch"}
	}
	if p.pin != nil {
		if err = p.pin(ctx); err != nil {
			return err
		}
	}
	p.network, p.primary = info.NetType, primary
	return nil
}

func (p *xmrBondProvider) accountForLabel(ctx context.Context, label string) (string, int64, bool, error) {
	var res struct {
		Accounts []struct {
			Index   json.Number `json:"account_index"`
			Address string      `json:"base_address"`
			Label   string      `json:"label"`
		} `json:"subaddress_accounts"`
	}
	if err := p.walletCall(ctx, "get_accounts", nil, &res); err != nil {
		return "", 0, false, err
	}
	var address string
	var account int64
	for _, a := range res.Accounts {
		if a.Label != label {
			continue
		}
		idx, ok := xmrCount(a.Index)
		parsed, e := parseXMRAddress(a.Address, p.network)
		if !ok || !xmrAccountOK(idx) || e != nil || parsed.Kind != xmrSubaddress || address != "" {
			return "", 0, false, bondBadReply("XMR", "get_accounts")
		}
		address, account = a.Address, idx
	}
	return address, account, address != "", nil
}

func (p *xmrBondProvider) Allocate(ctx context.Context, quoteID string) (string, string, error) {
	label, err := bondQuoteLabel(quoteID)
	if err != nil {
		return "", "", err
	}
	if err = p.mu.lock(ctx); err != nil {
		return "", "", err
	}
	defer p.mu.unlock()
	if p.pin == nil {
		return "", "", &vendorBondRPCError{"XMR", "wallet", "identity-not-verified"}
	}
	if err = p.check(ctx); err != nil {
		return "", "", err
	}
	address, account, found, err := p.accountForLabel(ctx, label)
	if err != nil {
		return "", "", err
	}
	if !found {
		var result struct {
			Address string      `json:"address"`
			Account json.Number `json:"account_index"`
		}
		if err = p.walletCall(ctx, "create_account", map[string]any{"label": label}, &result); err != nil {
			return "", "", err
		}
		var ok bool
		account, ok = xmrCount(result.Account)
		parsed, e := parseXMRAddress(result.Address, p.network)
		if !ok || !xmrAccountOK(account) || e != nil || parsed.Kind != xmrSubaddress {
			return "", "", bondBadReply("XMR", "create_account")
		}
		address = result.Address
	}
	// Persist the label/index before exposing an address. If store times out a
	// retry finds the same label and stores it again, never creating another one.
	if err = p.walletCall(ctx, "store", nil, nil); err != nil {
		return "", "", err
	}
	return address, strconv.FormatInt(account, 10) + ":" + quoteID, nil
}

func (p *xmrBondProvider) Observe(ctx context.Context, address, reference string) (int64, int64, error) {
	rawAccount, quoteID, ok := strings.Cut(reference, ":")
	account, parseErr := strconv.ParseInt(rawAccount, 10, 64)
	label, err := bondQuoteLabel(quoteID)
	if !ok || parseErr != nil || !xmrAccountOK(account) || err != nil {
		return 0, 0, errors.New("Monero vendor bond account reference is invalid")
	}
	if err = p.mu.lock(ctx); err != nil {
		return 0, 0, err
	}
	defer p.mu.unlock()
	if p.pin == nil {
		return 0, 0, &vendorBondRPCError{"XMR", "wallet", "identity-not-verified"}
	}
	if err = p.check(ctx); err != nil {
		return 0, 0, err
	}
	owned, ownedAccount, found, err := p.accountForLabel(ctx, label)
	if err != nil {
		return 0, 0, err
	}
	if !found || owned != address || ownedAccount != account {
		return 0, 0, &vendorBondRPCError{"XMR", "wallet", "quote-address-mismatch"}
	}
	var balance struct {
		Balance              json.Number `json:"balance"`
		Unlocked             json.Number `json:"unlocked_balance"`
		MultisigImportNeeded bool        `json:"multisig_import_needed"`
	}
	if err = p.walletCall(ctx, "get_balance", map[string]any{"account_index": account, "address_indices": []int64{0}}, &balance); err != nil {
		return 0, 0, err
	}
	bal, ok1 := xmrCount(balance.Balance)
	unlocked, ok2 := xmrCount(balance.Unlocked)
	if !ok1 || !ok2 || unlocked > bal || balance.MultisigImportNeeded {
		return 0, 0, bondBadReply("XMR", "get_balance")
	}
	var transfers struct {
		In   []bondXMRTransfer `json:"in"`
		Pool []bondXMRTransfer `json:"pool"`
	}
	if err = p.walletCall(ctx, "get_transfers", map[string]any{"in": true, "pool": true, "account_index": account, "subaddr_indices": []int64{0}}, &transfers); err != nil {
		return 0, 0, err
	}
	metadata := make(map[string]bondXMRTransfer)
	var pending int64
	for _, t := range transfers.In {
		if !t.valid(account, "in") {
			return 0, 0, bondBadReply("XMR", "get_transfers")
		}
		if _, exists := metadata[t.TxID]; exists {
			return 0, 0, bondBadReply("XMR", "get_transfers")
		}
		metadata[t.TxID] = t
	}
	for _, t := range transfers.Pool {
		value, valid := xmrCount(t.Amount)
		if !t.valid(account, "pool") || !valid || pending > math.MaxInt64-value {
			return 0, 0, bondBadReply("XMR", "get_transfers")
		}
		if _, exists := metadata[t.TxID]; exists {
			return 0, 0, bondBadReply("XMR", "get_transfers")
		}
		metadata[t.TxID] = t
		if !t.DoubleSpend {
			pending += value
		}
	}
	outputs, err := p.rpc.incomingTransfers(ctx, account)
	if err != nil {
		return 0, 0, bondRPCFailure("XMR", "incoming_transfers", err)
	}
	var keys, hashes []string
	seenKeys, seenHashes := make(map[string]bool), make(map[string]bool)
	for _, o := range outputs {
		if seenKeys[o.KeyImage] || o.KeyImage == strings.Repeat("0", 64) {
			return 0, 0, bondBadReply("XMR", "incoming_transfers")
		}
		seenKeys[o.KeyImage] = true
		if o.Minor != 0 || o.Spent || o.Frozen {
			continue
		}
		keys = append(keys, o.KeyImage)
		if !seenHashes[o.TxHash] {
			hashes = append(hashes, o.TxHash)
			seenHashes[o.TxHash] = true
		}
	}
	if len(keys) == 0 {
		return 0, pending, nil
	}
	spent, err := p.rpc.keyImagesSpent(ctx, keys)
	if err != nil {
		return 0, 0, bondRPCFailure("XMR", "is_key_image_spent", err)
	}
	txs, _, err := p.rpc.transactions(ctx, hashes)
	if err != nil {
		return 0, 0, bondRPCFailure("XMR", "get_transactions", err)
	}
	chainTx := make(map[string]xmrDaemonTx)
	for _, tx := range txs {
		if !seenHashes[tx.Hash] {
			return 0, 0, bondBadReply("XMR", "get_transactions")
		}
		if _, exists := chainTx[tx.Hash]; exists {
			return 0, 0, bondBadReply("XMR", "get_transactions")
		}
		chainTx[tx.Hash] = tx
	}
	var confirmed, incomingPending int64
	i := 0
	for _, o := range outputs {
		if o.Minor != 0 || o.Spent || o.Frozen {
			continue
		}
		isSpent := spent[i] != 0
		i++
		if isSpent {
			continue
		}
		m, exists := metadata[o.TxHash]
		chain, onChain := chainTx[o.TxHash]
		if !exists || !onChain || m.Type == "pool" || m.DoubleSpend || chain.DoubleSpendSeen {
			continue
		}
		if confirmed > math.MaxInt64-o.Amount-incomingPending {
			return 0, 0, bondBadReply("XMR", "incoming_transfers")
		}
		confs, valid := xmrCount(orZero(m.Confirmations))
		if !valid {
			return 0, 0, bondBadReply("XMR", "get_transfers")
		}
		if m.Type == "in" && m.Locked != nil && !*m.Locked && string(orZero(m.UnlockTime)) == "0" && o.Unlocked && !chain.InPool && confs >= p.confirmations && chain.Confirmations >= p.confirmations && chain.BlockHeight == o.BlockHeight {
			confirmed += o.Amount
		} else {
			incomingPending += o.Amount
		}
	}
	// get_balance limits the result after withdrawals and supplies an independent
	// cross-check. It cannot by itself enforce a configured confirmation depth.
	confirmed = min(confirmed, unlocked)
	incomingPending = min(incomingPending, max(int64(0), bal-confirmed))
	if pending > math.MaxInt64-incomingPending {
		return 0, 0, bondBadReply("XMR", "get_balance")
	}
	return confirmed, pending + incomingPending, nil
}

type bondXMRTransfer struct {
	TxID          string      `json:"txid"`
	Type          string      `json:"type"`
	Amount        json.Number `json:"amount"`
	Confirmations json.Number `json:"confirmations"`
	UnlockTime    json.Number `json:"unlock_time"`
	Locked        *bool       `json:"locked"`
	DoubleSpend   bool        `json:"double_spend_seen"`
	Index         struct {
		Major json.Number `json:"major"`
		Minor json.Number `json:"minor"`
	} `json:"subaddr_index"`
}

func (t bondXMRTransfer) valid(account int64, kind string) bool {
	major, ok1 := xmrCount(t.Index.Major)
	minor, ok2 := xmrCount(t.Index.Minor)
	_, ok3 := xmrCount(t.Amount)
	_, ok4 := xmrCount(orZero(t.Confirmations))
	_, ok5 := xmrCount(orZero(t.UnlockTime))
	return xmrHex64(t.TxID) && t.Type == kind && ok1 && ok2 && ok3 && ok4 && ok5 && major == account && minor == 0
}
