package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// WalletEvidence is advice from a bounded, on-demand wallet read. It never changes a payout or satisfies
// a recovery confirmation: an address match may belong to another payment, and wallet history can be incomplete.
type WalletEvidence struct {
	PayoutID int64
	State    string // ready, limited, unavailable, busy, unsupported, or no-address
	Scope    string
	Entries  []WalletEvidenceEntry
}

type WalletEvidenceEntry struct {
	TxID, Amount, Confirmations, Status string
}

type walletEvidenceRead struct {
	Entries []WalletEvidenceEntry
	Scope   string
	Limited bool
}

type walletEvidenceReader interface {
	ReadWalletEvidence(context.Context, string) (walletEvidenceRead, error)
}

const (
	walletEvidenceMaxMatches  = 10
	bitcoinEvidenceEntries    = 200
	moneroEvidenceBlockWindow = 21600
)

var walletEvidenceDeadline = 6 * time.Second // shortened only by timeout tests

// An admin can inspect one payout at a time. The shared, non-waiting gate prevents concurrent page GETs from
// loading either pooled wallet; a busy read renders advice, not an HTTP error or an action refusal.
var walletEvidenceSlots = make(chan struct{}, 2)

func (a *App) loadWalletEvidence(ctx context.Context, r *http.Request, d *PageData) error {
	if r == nil || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	raw := r.URL.Query().Get("wallet_evidence")
	if raw == "" {
		return nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return sql.ErrNoRows
	}
	var payout *PayoutRow
	for i := range d.Payouts {
		p := &d.Payouts[i]
		if p.ID == id && payoutRecoveryVisible(p) {
			payout = p
			break
		}
	}
	if payout == nil {
		return sql.ErrNoRows
	}
	v := &WalletEvidence{PayoutID: id}
	d.WalletEvidence = v
	if payout.Address == "" {
		v.State = "no-address"
		return nil
	}
	provider := a.provider(payout.Currency)
	if provider == nil {
		v.State = "unavailable"
		return nil
	}
	reader, ok := provider.(walletEvidenceReader)
	if !ok {
		v.State = "unsupported"
		return nil
	}
	select {
	case walletEvidenceSlots <- struct{}{}:
		defer func() { <-walletEvidenceSlots }()
	default:
		v.State = "busy"
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, walletEvidenceDeadline)
	defer cancel()
	read, err := reader.ReadWalletEvidence(readCtx, payout.Address)
	if err != nil {
		v.State = "unavailable" // never return the wallet's error text, address, comments or RPC credentials
		return nil
	}
	v.Entries, v.Scope = read.Entries, read.Scope
	v.State = "ready"
	if read.Limited {
		v.State = "limited"
	}
	return nil
}

func payoutRecoveryVisible(p *PayoutRow) bool {
	return p.State == "pending" || p.State == "blocked" || p.State == "held" || p.State == "failed" || p.State == "sending" && p.Attention
}

func walletTxID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func evidenceConfs(n json.Number) string {
	if n == "" {
		return "unknown"
	}
	v, err := n.Int64()
	if err != nil {
		return "unknown"
	}
	return strconv.FormatInt(v, 10)
}

// Core 31.0 listtransactions returns one send entry per destination, newest count/skip bounded; it may also
// return a receive entry for a wallet-owned destination. Match only category=send and the exact payout address.
func (p *bitcoinProvider) ReadWalletEvidence(ctx context.Context, address string) (walletEvidenceRead, error) {
	var rows []struct {
		Address       string      `json:"address"`
		Category      string      `json:"category"`
		Amount        json.Number `json:"amount"`
		Confirmations json.Number `json:"confirmations"`
		TxID          string      `json:"txid"`
		Abandoned     bool        `json:"abandoned"`
	}
	if err := p.rpc.call(ctx, p.wallet, "listtransactions", []any{"*", bitcoinEvidenceEntries, 0}, &rows); err != nil {
		return walletEvidenceRead{}, err
	}
	read := walletEvidenceRead{Scope: "The 200 most recent wallet entries were requested; each outgoing destination is a separate entry.", Limited: len(rows) >= bitcoinEvidenceEntries}
	// Core returns the selected recent entries oldest first. Display the newest matches first.
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.Category != "send" || row.Address != address {
			continue
		}
		if !walletTxID(row.TxID) {
			read.Limited = true
			continue
		}
		coins := strings.TrimPrefix(string(row.Amount), "-")
		if !strings.HasPrefix(string(row.Amount), "-") {
			read.Limited = true // malformed send amount: do not show it as a match
			continue
		}
		atomic, err := parseAmount(coins, 8)
		if err != nil || atomic <= 0 {
			read.Limited = true
			continue
		}
		confs := evidenceConfs(row.Confirmations)
		if confs == "unknown" {
			read.Limited = true
		}
		status := "Wallet send"
		if row.Abandoned {
			status = "Abandoned"
		} else if strings.HasPrefix(confs, "-") {
			status = "Conflicted"
		} else if confs == "0" {
			status = "Unconfirmed"
		}
		read.Entries = append(read.Entries, WalletEvidenceEntry{TxID: row.TxID, Amount: amount(atomic, 8), Confirmations: confs, Status: status})
		if len(read.Entries) == walletEvidenceMaxMatches {
			read.Limited = true
			break
		}
	}
	return read, nil
}

// Pinned monero-wallet-rpc v0.18.5.1 filters confirmed out by height, but pending/failed are not height-filtered.
// The client's 16 MiB body cap and this bounded block window keep the read finite; old/rescanned records and
// outgoing records without destinations can therefore be absent from the matches shown.
func (p *moneroProvider) ReadWalletEvidence(ctx context.Context, address string) (walletEvidenceRead, error) {
	var tip struct {
		Height json.Number `json:"height"`
	}
	if err := p.wallet.call(ctx, "/json_rpc", "get_height", nil, &tip); err != nil {
		return walletEvidenceRead{}, err
	}
	height, err := tip.Height.Int64()
	if err != nil || height < 0 {
		return walletEvidenceRead{}, errors.New("monero wallet height unavailable")
	}
	minHeight := max(int64(0), height-moneroEvidenceBlockWindow)
	type transfer struct {
		TxID          string      `json:"txid"`
		Confirmations json.Number `json:"confirmations"`
		Destinations  []struct {
			Address string      `json:"address"`
			Amount  json.Number `json:"amount"`
		} `json:"destinations"`
	}
	var rows struct {
		Out     []transfer `json:"out"`
		Pending []transfer `json:"pending"`
		Failed  []transfer `json:"failed"`
	}
	if err := p.wallet.call(ctx, "/json_rpc", "get_transfers", map[string]any{
		"out": true, "pending": true, "failed": true, "filter_by_height": true,
		"min_height": minHeight, "max_height": height, "account_index": 0,
	}, &rows); err != nil {
		return walletEvidenceRead{}, err
	}
	read := walletEvidenceRead{Scope: "Confirmed outgoing entries from the most recent 21,600 wallet blocks, plus pending and failed outgoing entries (RPC response capped at 16 MiB).", Limited: true}
	omittedDestinations := false
	for _, group := range []struct {
		status string
		items  []transfer
	}{{"Pending outgoing", rows.Pending}, {"Failed outgoing", rows.Failed}, {"Confirmed outgoing", rows.Out}} {
		for _, row := range group.items {
			if len(row.Destinations) == 0 {
				omittedDestinations = true
				continue
			}
			if !walletTxID(row.TxID) {
				continue
			}
			for _, dest := range row.Destinations {
				if dest.Address != address {
					continue
				}
				atomic, err := dest.Amount.Int64()
				if err != nil || atomic <= 0 {
					continue
				}
				read.Entries = append(read.Entries, WalletEvidenceEntry{TxID: row.TxID, Amount: amount(atomic, 12), Confirmations: evidenceConfs(row.Confirmations), Status: group.status})
				if len(read.Entries) == walletEvidenceMaxMatches {
					if omittedDestinations {
						read.Scope += " Some wallet records omitted their destinations."
					}
					read.Scope += " More matching destinations may be omitted."
					return read, nil
				}
			}
		}
	}
	if omittedDestinations {
		read.Scope += " Some wallet records omitted their destinations."
	}
	return read, nil
}
