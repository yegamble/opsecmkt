package market

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// The Bitcoin escrow market fee (WR-escrow Phase 1, slice P1-c2; ES-5, the A-246 carry-over). An administrator sets
// it on /admin: basis points of the price and the address the fee output pays (settings escrow_btc_fee_bps and
// escrow_btc_fee_address, password step-up, audited without the address). Until an administrator saves it, the
// startup settings ESCROW_BTC_FEE_BPS and ESCROW_BTC_FEE_ADDRESS apply. Each escrow snapshots the fee and its
// destination at checkout (escrows.fee_bps, fee_destination), so a change affects only later escrows.
//
// The destination is a fixed address, allowed on test networks only (regtest, signet, testnet4), or a watch-only
// descriptor giving a fresh fee address per settlement (escrow_fee_desc.go), which a mainnet market requires (A-460; the
// owner's decision ES-5). The setting stores a descriptor sealed; the audit row never names either.

const (
	escrowFeeBpsSetting  = "escrow_btc_fee_bps"
	escrowFeeAddrSetting = "escrow_btc_fee_address"
)

func init() {
	registerAction("/admin/escrow-fee", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Off: escrowOff, Run: escrowFeeAction})
	registerLoader("admin", escrowFeeLoader)
}

// escrowFee is the market fee in force: the administrator's setting, or the startup setting when none was saved.
func escrowFee(ctx context.Context, q rowQuerier, e *escrowBTC) (int64, string, error) {
	bps, addr := e.feeBps, e.feeAddress
	var b, a sql.NullString
	err := q.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=$1),(SELECT value FROM settings WHERE key=$2)`, escrowFeeBpsSetting, escrowFeeAddrSetting).Scan(&b, &a)
	if err != nil {
		return 0, "", err
	}
	if b.Valid {
		if bps, err = strconv.ParseInt(b.String, 10, 64); err != nil || bps < 0 || bps > 10000 {
			return 0, "", fail(503, "The market fee setting is unreadable. Nothing was changed.")
		}
		addr = a.String
	}
	return bps, addr, nil // insertEscrow snapshots no destination for a fee of 0
}

// escrowFeeLoader shows the fee in force and its form on /admin while ESCROW_BTC is on.
func escrowFeeLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if !a.escrowConfigured || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	v := &EscrowFeeView{Available: a.escrow.Load() != nil, Live: a.live}
	e := a.escrow.Load()
	if e == nil { // the startup setting, as startEscrow forms it (feeDestinationForm)
		addr, _, err := a.feeDestinationForm(a.escrowNetwork(), a.escrowCfg.feeAddress, "")
		if err != nil || a.escrowCfg.feeAddress == "" {
			addr = ""
		}
		e = &escrowBTC{feeBps: a.escrowCfg.feeBps, feeAddress: addr}
	}
	bps, addr, err := escrowFee(ctx, a.db, e)
	if err != nil {
		return err
	}
	v.Bps, v.Percent = strconv.FormatInt(bps, 10), strconv.FormatFloat(float64(bps)/100, 'f', -1, 64)
	if isSealedFeeDescriptor(addr) {
		v.Descriptor = "unreadable (was APP_SECRET_KEY changed?)"
		if k, err := a.openFeeDescriptor(addr); err == nil {
			v.Descriptor = k.Fingerprint
		}
	} else {
		v.Address = addr
	}
	high, err := highestFeeIndex(ctx, a.db)
	if err != nil {
		return err
	}
	if high >= 0 {
		v.HighestIndex = strconv.FormatInt(high, 10)
	}
	d.EscrowFee = v
	return nil
}

// escrowFeeAction saves the market fee: basis points from 0 to 10000 and, above 0, a destination: the fee wallet's
// watch-only descriptor (sealed; required on a mainnet market, A-460), or on a test network an address the node accepts
// that the market has not used as an escrow, refund, deposit or settlement address. The administrator confirms with
// their password (and authenticator code when enrolled); the audit row names the old and new basis points, never the
// destination (ES-10).
func escrowFeeAction(c *actionCtx) (actionResult, error) {
	bps, err := strconv.ParseInt(strings.TrimSpace(c.Form.Get("fee_bps")), 10, 64)
	if err != nil || bps < 0 || bps > 10000 {
		return actionResult{}, fail(400, "Enter the market fee as a whole number of basis points from 0 to 10000 (250 = 2.5%). Nothing was changed.")
	}
	addr, plain := "", false
	if bps > 0 {
		in := c.Form.Get("fee_address")
		var err error
		switch addr, plain, err = c.A.feeDestinationForm(c.A.escrowNetwork(), in, ""); {
		case errors.Is(err, errFeePlainOnMainnet):
			return actionResult{}, fail(400, "This market runs on Bitcoin mainnet, so the market fee must go to a watch-only descriptor, which gives a fresh fee address for every settlement: enter the fee wallet's receiving descriptor, wpkh([fingerprint/path]xpub/0/*). Nothing was changed.")
		case errors.Is(err, errFeeDescriptor):
			return actionResult{}, fail(400, "The fee descriptor must be the fee wallet's receiving descriptor, wpkh([fingerprint/path]key/0/*), or its key with its origin, with a public key of this market's network. Nothing was changed.")
		case err != nil && isFeeDescriptorInput(in):
			return actionResult{}, err
		case err != nil:
			return actionResult{}, fail(400, "Enter the "+c.A.escrowNetwork()+" Bitcoin address the market fee is paid to, or the fee wallet's watch-only descriptor. Nothing was changed.")
		}
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		e := c.A.escrow.Load()
		if e == nil {
			return actionResult{}, errEscrowDown
		}
		ctx := c.Ctx()
		if !plain && addr != "" {
			k, err := c.A.openFeeDescriptor(addr)
			if err != nil {
				return actionResult{}, err
			}
			if k.Fingerprint == e.fingerprint {
				return actionResult{}, fail(400, "The fee descriptor is the market's arbiter key. Give the key of a wallet used only for the market fee. Nothing was changed.")
			}
		}
		if plain {
			valid, err := e.validAddress(ctx, addr)
			if err != nil {
				return actionResult{}, fail(503, "The fee address could not be checked: the market's Bitcoin node did not answer. Nothing was changed; try again later.")
			}
			if !valid {
				return actionResult{}, fail(400, "The fee address is not a valid "+c.A.escrowNetwork()+" Bitcoin address. Nothing was changed.")
			}
		}
		for _, key := range []string{escrowFeeBpsSetting, escrowFeeAddrSetting} {
			if err := lockSetting(ctx, c.Tx, key); err != nil {
				return actionResult{}, err
			}
		}
		oldBps, oldAddr, err := escrowFee(ctx, c.Tx, e)
		if err != nil {
			return actionResult{}, err
		}
		if oldBps == bps && (oldAddr == addr || c.A.sameFeeDescriptor(oldAddr, addr)) {
			return actionResult{}, fail(409, "The market fee is already that; nothing was changed.")
		}
		if plain {
			var used bool
			if err = c.Tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM escrows WHERE address=$1 OR refund_address=$1)
				OR EXISTS(SELECT 1 FROM escrow_siblings WHERE address=$1)
				OR EXISTS(SELECT 1 FROM settlements WHERE destination=$1)
				OR EXISTS(SELECT 1 FROM retention_escrow_retired_addresses WHERE coin='BTC' AND address_hash=encode(sha256(convert_to($1,'UTF8')),'hex'))`, addr).Scan(&used); err != nil {
				return actionResult{}, err
			}
			if used {
				return actionResult{}, fail(409, "That address was already used on this market as an escrow, refund, deposit or settlement address. Give an address used only for the market fee. Nothing was changed.")
			}
		}
		if err = setSetting(c, escrowFeeBpsSetting, strconv.FormatInt(bps, 10)); err != nil {
			return actionResult{}, err
		}
		if err = setSetting(c, escrowFeeAddrSetting, addr); err != nil {
			return actionResult{}, err
		}
		audit := "Changed the Bitcoin escrow market fee (" + c.A.escrowNetwork() + "): " + strconv.FormatInt(oldBps, 10) + " → " + strconv.FormatInt(bps, 10) + " basis points"
		if oldAddr != addr && !c.A.sameFeeDescriptor(oldAddr, addr) {
			audit += "; fee destination changed"
		}
		return actionResult{Redirect: "/admin", Notice: "escrow-fee", Audit: audit}, nil
	})
}

// sameFeeDescriptor reports two sealed fee descriptors of one key (each seal has its own nonce).
func (a *App) sameFeeDescriptor(x, y string) bool {
	if !isSealedFeeDescriptor(x) || !isSealedFeeDescriptor(y) {
		return false
	}
	kx, errX := a.openFeeDescriptor(x)
	ky, errY := a.openFeeDescriptor(y)
	return errX == nil && errY == nil && kx.Expression() == ky.Expression()
}
