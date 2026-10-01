package market

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// Escrow checkout and creation (WR-escrow Phase 1, slice P1-b; ES-3, ES-5). With ESCROW_BTC=on, the buyer of a BTC
// draft on a digital listing with automatic delivery may pay into a 2-of-3 escrow instead of the pooled wallet,
// once both the buyer's and the vendor's escrow keys are active (ES-9). Every other order keeps the pooled flow.
//
// POST /orders/escrow takes the buyer's refund address, pinned for the order, and builds the escrow in the checkout
// transaction, after every check has passed (as /orders/pay asks the wallet for an address only then):
//  1. each party's next child key (/0/<i>, from 1; /0/0 is the practice child) and a fresh market child index from
//     the escrow_market_index counter;
//  2. Bitcoin Core expands the child keys statelessly: deriveaddresses of the extended-key descriptor, createpsbt
//     paying that address from a made-up outpoint (never signed or broadcast) and utxoupdatepsbt, which fills in
//     the witness script and each key's derivation (ES-3);
//  3. the witness script is checked (checkEscrowWitness): three distinct compressed keys, exactly one from the
//     pinned arbiter fingerprint, each party's key at its own origin;
//  4. the bare-key descriptor wsh(sortedmulti(2,<buyer>,<vendor>,<market>)) must have Core's checksum and derive the
//     same address;
//  5. the watch-only wallet must not already watch that address (getaddressinfo, A-359: an escrow a restored database
//     no longer has); if it does, the next child keys are tried (buildUnwatchedEscrow);
//  6. the escrow rows are written (uncommitted, so invisible) and the bare-key descriptor alone is imported into the
//     watch-only wallet; only then does the transaction commit and the address exist anywhere a page can show it.
//
// A failure at any step rolls the transaction back: no order change, no escrow row. A market index a failed build
// took is skipped, never reused; an import whose transaction then fails to commit stays in the watch-only wallet,
// watched but never shown (reconciliation lists such orphans, P1-d). While the restore gate is set, no escrow is
// created at all (errEscrowRestoring).

func init() {
	registerAction("/orders/escrow", actionSpec{OneAtATime: true, Off: escrowOff, Run: escrowCheckoutAction})
}

// errEscrowRefund and errEscrowRefundInvalid refuse a missing or invalid refund address on the escrow node's chain net.
func errEscrowRefund(net string) error {
	return fail(400, "Enter a refund address: a fresh "+net+" Bitcoin address from your own wallet. A refund of this escrow is paid there, and it cannot be changed later. Nothing was changed.")
}

func errEscrowRefundInvalid(net string) error {
	return fail(400, "The refund address is not a valid "+net+" Bitcoin address. Nothing was changed.")
}

var (
	errEscrowRefundUsed = fail(409, "That refund address was already used on this market. Give a fresh address from your wallet. Nothing was changed.")
	errEscrowOnlyBuyer  = fail(403, "Only the buyer can pay for this order.")
	errEscrowKind       = fail(409, "Bitcoin escrow is available only for digital listings with automatic delivery. Nothing was changed.")
	errEscrowCurrency   = fail(409, "Bitcoin escrow is for Bitcoin orders only. Nothing was changed.")
	errEscrowSelf       = fail(409, "You cannot buy your own listing with escrow: an escrow needs two parties' keys. Nothing was changed.")
	errEscrowBuyerKey   = fail(409, "Activate your Bitcoin escrow key first: register it and sign its practice transaction on your escrow key page. Nothing was changed.")
	errEscrowVendorKey  = fail(409, "The vendor has no active Bitcoin escrow key, so this order cannot use escrow. Nothing was changed.")
	errEscrowBuild      = fail(503, "The escrow could not be created: the market's Bitcoin node did not confirm it. Nothing was changed; try again later.")
	errEscrowKeyTaken   = fail(409, "The escrow could not be created: another escrow on this market already holds one of its keys. Nothing was changed. Trying again will not help; contact the market's staff.")
	// errEscrowRestoring refuses escrow checkout while the restore gate is set (A-359): the restored database does not
	// know the escrows created after its backup, so it could hand out their child keys again.
	errEscrowRestoring = fail(503, "Bitcoin escrow is paused while this market restores its records from a backup. Nothing was charged and your order is still a draft; try again later.")
	// errEscrowWatched refuses a checkout whose every composed address the watch-only wallet already watches (A-359).
	errEscrowWatched = fail(503, "The escrow could not be created: every escrow address the market composed for this order is already watched by its escrow wallet, which happens after the market restored its records from a backup. Nothing was charged and your order is still a draft; contact the market's staff.")
)

// escrowWatchedTries bounds how many composed escrow addresses one checkout tries while the watch-only wallet already
// watches each of them (A-359).
const escrowWatchedTries = 20

// escrowCheckoutCtx marks the draft -> awaiting_payment transition of an escrow checkout: it needs the escrow node,
// not a pooled wallet for the currency (transition).
type escrowCheckoutCtx struct{}

func isEscrowCheckout(ctx context.Context) bool { return ctx.Value(escrowCheckoutCtx{}) != nil }

// escrowAddressForm is the offline check of an address the market will pay or refuse (a refund or fee address): 14 to
// 90 ASCII letters and digits, so it can never close or extend the addr(…) descriptor the node checks it with; bech32
// in lower case, the one spelling the market stores and compares.
func escrowAddressForm(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 14 || len(s) > 90 || strings.ContainsFunc(s, func(r rune) bool { return !isASCIIAlnum(r) }) {
		return "", false
	}
	if lower := strings.ToLower(s); strings.HasPrefix(lower, "bcrt1") {
		s = lower
	}
	return s, true
}

// escrowParty is one party key of an escrow being built: the account key, its owner and the child suffix used.
type escrowParty struct {
	Party, UserID, KeyHMAC string
	Key                    escrowKey
	Suffix                 string
	Index                  int64 // the child index in Suffix (/0/<Index>)
}

// escrowParties locks the buyer's and the vendor's key rows (in account order, so two checkouts never deadlock) and
// returns each party's key with its next child suffix: one more than the highest this account's escrows used under
// any origin, never below 1 (A-314: a counter per origin restarted at /0/1 when a key's origin changed, a child key
// an existing escrow already held). The count keys off the account, never the HMAC, so an APP_SECRET_KEY rotation
// never hands out a child key twice (chair's ruling (a) on QA25); the key rows locked FOR UPDATE serialise it.
func (a *App) escrowParties(ctx context.Context, tx *sql.Tx, o *Order) (buyer, vendor escrowParty, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at IS NOT NULL FROM user_escrow_keys
		WHERE user_id IN ($1,$2) ORDER BY user_id FOR UPDATE`, o.BuyerID, o.VendorID)
	if err != nil {
		return buyer, vendor, err
	}
	type row struct {
		sealed, hmac, fp, path string
		active                 bool
	}
	found := map[string]row{}
	for rows.Next() {
		var uid string
		var r row
		if err = rows.Scan(&uid, &r.sealed, &r.hmac, &r.fp, &r.path, &r.active); err != nil {
			rows.Close()
			return buyer, vendor, err
		}
		found[uid] = r
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return buyer, vendor, err
	}
	party := func(uid, name string, refused error) (escrowParty, error) {
		r, ok := found[uid]
		if !ok || !r.active {
			return escrowParty{}, refused
		}
		xpub, err := a.open("escrow-xpub", r.sealed)
		if err != nil {
			log.Print(errKeySealed)
			if name == "buyer" {
				return escrowParty{}, fail(409, errEscrowKeyUnreadable)
			}
			return escrowParty{}, refused
		}
		key, err := parseEscrowKey("[" + r.fp + r.path + "]" + xpub)
		if err != nil {
			return escrowParty{}, refused
		}
		var next int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(split_part(derivation_suffix,'/',3)::bigint),0)+1 FROM escrow_pubkeys
			WHERE user_id=$1 AND derivation_suffix ~ '^/0/[1-9][0-9]{0,9}$'`, uid).Scan(&next); err != nil {
			return escrowParty{}, err
		}
		p := escrowParty{Party: name, UserID: uid, KeyHMAC: r.hmac, Key: key, Suffix: escrowChildSuffix(next), Index: next}
		if checkEscrowOrigin(key, key.PathText, p.Suffix) != nil {
			return escrowParty{}, refused
		}
		return p, nil
	}
	if buyer, err = party(o.BuyerID, "buyer", errEscrowBuyerKey); err != nil {
		return buyer, vendor, err
	}
	vendor, err = party(o.VendorID, "vendor", errEscrowVendorKey)
	return buyer, vendor, err
}

// escrowCheckoutAction: the buyer moves a draft to awaiting_payment into a new escrow (see the file comment). The
// refusals that need no node come first and make no RPC.
func escrowCheckoutAction(c *actionCtx) (actionResult, error) {
	a, ctx := c.A, c.Ctx()
	e := a.escrow.Load()
	if e == nil {
		return actionResult{}, errEscrowDown
	}
	refund, ok := escrowAddressForm(c.Form.Get("refund_address"))
	if !ok {
		return actionResult{}, errEscrowRefund(c.A.escrowNetwork())
	}
	if err := waitForWalletLocks(ctx, c.Tx); err != nil {
		return actionResult{}, err
	}
	o, err := partyOrder(c, c.Form.Get("order_id"))
	if err != nil {
		return actionResult{}, err
	}
	switch {
	case o.BuyerID != c.User.ID:
		return actionResult{}, errEscrowOnlyBuyer
	case o.State != stateDraft:
		return actionResult{}, errConflict
	case o.Currency != "BTC":
		return actionResult{}, errEscrowCurrency
	case o.VendorID == o.BuyerID:
		return actionResult{}, errEscrowSelf
	}
	// While the restore gate is set no escrow is created: the order stays a draft and nothing is asked of the node.
	if gate, err := restoreGateSet(ctx, c.Tx); err != nil {
		return actionResult{}, err
	} else if gate {
		return actionResult{}, errEscrowRestoring
	}
	if ok, err := sellerIsVendor(c, o.ProductID); err != nil {
		return actionResult{}, err
	} else if !ok {
		return actionResult{}, fail(409, sellerNotVendor+" Cancel this draft.")
	}
	// The buyer's cap on orders awaiting payment, serialised on the buyer's row as in payAction.
	var open int
	if _, err = c.Tx.ExecContext(ctx, "SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE", c.User.ID); err != nil {
		return actionResult{}, err
	}
	if err = c.Tx.QueryRowContext(ctx, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND state='awaiting_payment'", c.User.ID).Scan(&open); err != nil {
		return actionResult{}, err
	}
	if open >= maxAwaitingPayment {
		return actionResult{}, fail(409, "You already have "+strconv.Itoa(maxAwaitingPayment)+" orders awaiting payment. Pay or cancel one before requesting another payment address.")
	}
	if err = lockPaymentIntake(c, o.Currency); err != nil {
		return actionResult{}, err
	}
	if eligible, price, err := escrowEligible(ctx, c.Tx, &o); err != nil {
		return actionResult{}, err
	} else if !eligible {
		return actionResult{}, errEscrowKind
	} else if price < payoutFloor(o.Currency, "release") {
		return actionResult{}, fail(400, subFloorListing(o.Currency))
	}
	buyer, vendor, err := a.escrowParties(ctx, c.Tx, &o)
	if err != nil {
		return actionResult{}, err
	}
	valid, err := e.validAddress(ctx, refund)
	if err != nil {
		log.Printf("escrow: refund address not checked: %s", errorClassOf(err))
		return actionResult{}, errEscrowBuild
	}
	if !valid {
		return actionResult{}, errEscrowRefundInvalid(c.A.escrowNetwork())
	}
	// Never an address the market already used (ES-5): an escrow address, another escrow's refund address, a
	// settlement destination (a vendor's claim), a fee destination snapshotted or configured, or a pooled deposit
	// address.
	feeBps, feeAddr, err := escrowFee(ctx, c.Tx, e)
	if err != nil {
		return actionResult{}, err
	}
	var used bool
	if err = c.Tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM escrows WHERE address=$1 OR refund_address=$1 OR fee_destination=$1) OR $1=$2
		OR EXISTS(SELECT 1 FROM payment_addresses WHERE address=$1) OR EXISTS(SELECT 1 FROM settlements WHERE destination=$1)`, refund, feeAddr).Scan(&used); err != nil {
		return actionResult{}, err
	}
	if used {
		return actionResult{}, errEscrowRefundUsed
	}
	if _, err = c.Tx.ExecContext(ctx, "UPDATE orders o SET amount=p.btc FROM products p WHERE o.id=$1 AND o.state='draft' AND p.id=o.product_id", o.ID); err != nil {
		return actionResult{}, err
	}
	moved, err := a.transition(context.WithValue(ctx, escrowCheckoutCtx{}, true), c.Tx, o.ID, stateDraft, stateAwaitingPayment, c.User, "Escrow address requested")
	if err != nil {
		return actionResult{}, err
	}
	sats, err := parseAmount(moved.Amount, 8)
	if err != nil {
		return actionResult{}, err
	}
	built, err := a.buildUnwatchedEscrow(ctx, c.Tx, e, o.ID, sats, buyer, vendor)
	if err != nil {
		return actionResult{}, err
	}
	if err = insertEscrow(ctx, c.Tx, o.ID, refund, feeBps, feeAddr, built); err != nil {
		return actionResult{}, err
	}
	// The import is the last step before commit: the address is shown only once the watch-only wallet watches it.
	if err = e.importDescriptors(ctx, e.watchName, []escrowImport{{Desc: built.Descriptor, Timestamp: "now", Label: "escrow:" + o.ID}}); err != nil {
		log.Printf("escrow: escrow for order %s not imported: %s", shortID(o.ID), errorClassOf(err))
		return actionResult{}, errEscrowBuild
	}
	return actionResult{Redirect: orderPage(o.ID), Notice: "escrow-created", Audit: "Created Bitcoin escrow (" + c.A.escrowNetwork() + ") for order " + shortID(o.ID)}, nil
}

// buildUnwatchedEscrow builds the order's escrow (buildEscrow) at a fresh market index and checks, before anything is
// written or imported, that the watch-only wallet does not already watch its address (A-359). A restored database
// knows nothing of the escrows created after its backup: each party's next child index (one more than the highest
// its escrows on record used) and the market counter are back where the backup left them, so the same buyer and
// vendor would get an escrow address handed out after the backup, and their deposits would merge. When the wallet
// watches the composed address, both party indices and the market index move on by one and the build is tried
// again, at most escrowWatchedTries times. A skipped market index is never used again (a sequence); skipped party
// indices lie below the ones the escrow stores, which the next escrow counts from. After the last try nothing is
// written or imported and the checkout is refused with a fixed 503. Only the composed address is checked: a child key
// a lost escrow used, combined with other keys into another address, is not detected (docs/testnet-runbook.md).
func (a *App) buildUnwatchedEscrow(ctx context.Context, tx *sql.Tx, e *escrowBTC, orderID string, sats int64, buyer, vendor escrowParty) (*builtEscrow, error) {
	for try := 1; ; try++ {
		var marketIndex int64
		if err := tx.QueryRowContext(ctx, "SELECT nextval('escrow_market_index')").Scan(&marketIndex); err != nil {
			return nil, err
		}
		built, err := e.buildEscrow(ctx, orderID, sats, buyer, vendor, marketIndex)
		var known bool
		if err == nil {
			known, err = e.watchKnows(ctx, built.Address)
		}
		if err != nil {
			log.Printf("escrow: escrow for order %s not built: %s", shortID(orderID), errorClassOf(err))
			return nil, errEscrowBuild
		}
		if !known {
			return built, nil
		}
		if try == escrowWatchedTries {
			log.Printf("escrow: escrow for order %s not built: every address tried is already in the watch-only wallet (%d tries)", shortID(orderID), escrowWatchedTries)
			return nil, errEscrowWatched
		}
		log.Printf("escrow: escrow for order %s: its address is already in the watch-only wallet (a restored database?); trying the next child keys", shortID(orderID))
		for _, p := range []*escrowParty{&buyer, &vendor} {
			p.Index++
			p.Suffix = escrowChildSuffix(p.Index)
			if checkEscrowOrigin(p.Key, p.Key.PathText, p.Suffix) != nil {
				return nil, errEscrowBuild
			}
		}
	}
}

// escrowEligible reports whether o's listing takes escrow (a digital listing whose content is delivered
// automatically, as autoDeliveryHook would, and not taken down) and its BTC price.
func escrowEligible(ctx context.Context, q rowQuerier, o *Order) (bool, int64, error) {
	var content string
	var takenDown bool
	var price int64
	if err := q.QueryRowContext(ctx, "SELECT delivery_content,admin_archive_reason IS NOT NULL,btc FROM products WHERE id=$1", o.ProductID).Scan(&content, &takenDown, &price); err != nil {
		return false, 0, err
	}
	ok := o.Kind == "digital" && strings.TrimSpace(content) != "" && utf8.RuneCountInString(content) <= maxDeliveryChars && !takenDown
	return ok, price, nil
}

// builtEscrow is a checked escrow, not yet imported: its address, the bare-key descriptor with its checksum (what is
// imported) and each party's child key with the party it belongs to.
type builtEscrow struct {
	Address, Descriptor string
	MarketIndex         int64
	Parties             []escrowParty
	Keys                map[string][]byte
}

// buildEscrow composes and checks the escrow of one order (steps 2 to 4 of the file comment). Its RPCs are
// deriveaddresses, createpsbt, utxoupdatepsbt, getdescriptorinfo and deriveaddresses; none touches a wallet.
func (e *escrowBTC) buildEscrow(ctx context.Context, orderID string, sats int64, buyer, vendor escrowParty, marketIndex int64) (*builtEscrow, error) {
	market := escrowParty{Party: "market", Key: e.arbiterKey, Suffix: "/0/" + strconv.FormatInt(marketIndex, 10)}
	parties := []escrowParty{buyer, vendor, market}
	var exprs []string
	var want []escrowExpectKey
	for _, p := range parties {
		steps, err := parseOriginPath(p.Suffix)
		if err != nil || len(steps) != 2 {
			return nil, errEscrowOrigin
		}
		exprs = append(exprs, p.Key.Expression()+p.Suffix)
		want = append(want, escrowExpectKey{Party: p.Party, Fingerprint: p.Key.Fingerprint, Path: slices.Concat(p.Key.Path, steps)})
	}
	extended := withChecksum("wsh(sortedmulti(2," + strings.Join(exprs, ",") + "))")
	addrs, err := e.deriveAddresses(ctx, extended, -1)
	if err != nil {
		return nil, err
	}
	if len(addrs) != 1 {
		return nil, &escrowRPCError{Method: "deriveaddresses", Class: "bad-reply"}
	}
	// A made-up outpoint paying the escrow: createpsbt gives the address's scriptPubKey, and with it as the input's
	// witness UTXO utxoupdatepsbt fills in the witness script and derivations. Nothing is signed or broadcast.
	prev := sha256.Sum256([]byte("escrow-build:" + orderID))
	unsigned, err := e.createPSBT(ctx, []escrowOutpoint{{TxID: hex.EncodeToString(prev[:]), Vout: 0}}, []escrowPayment{{Address: addrs[0], Sats: sats}})
	if err != nil {
		return nil, err
	}
	p, err := decodePSBT(unsigned)
	if err != nil || len(p.Tx.In) != 1 || len(p.Tx.Out) != 1 || p.Tx.Out[0].Value != sats {
		return nil, &escrowRPCError{Method: "createpsbt", Class: "bad-reply"}
	}
	spk := p.Tx.Out[0].Script
	utxo := binary.LittleEndian.AppendUint64(nil, uint64(sats))
	utxo = append(append(utxo, compactSize(len(spk))...), spk...)
	withUTXO, err := p.withInputField(0, []byte{0x01}, utxo)
	if err != nil {
		return nil, err
	}
	filled, err := e.utxoUpdatePSBT(ctx, encodePSBT(withUTXO), []string{extended})
	if err != nil {
		return nil, err
	}
	fp, err := decodePSBT(filled)
	if err != nil || fp.TxID() != p.TxID() || len(fp.Inputs) != 1 {
		return nil, &escrowRPCError{Method: "utxoupdatepsbt", Class: "bad-reply"}
	}
	keys, err := checkEscrowWitness(fp.Inputs[0], spk, e.fingerprint, want)
	if err != nil {
		return nil, err
	}
	bare := "wsh(sortedmulti(2," + hex.EncodeToString(keys["buyer"]) + "," + hex.EncodeToString(keys["vendor"]) + "," + hex.EncodeToString(keys["market"]) + "))"
	public, err := e.descriptorInfo(ctx, bare)
	if err != nil {
		return nil, err
	}
	if public != withChecksum(bare) {
		return nil, &escrowRPCError{Method: "getdescriptorinfo", Class: "checksum-mismatch"}
	}
	bareAddrs, err := e.deriveAddresses(ctx, public, -1)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(bareAddrs, addrs) {
		return nil, &escrowRPCError{Method: "deriveaddresses", Class: "address-mismatch"}
	}
	return &builtEscrow{Address: addrs[0], Descriptor: public, MarketIndex: marketIndex, Parties: parties, Keys: keys}, nil
}

// insertEscrow writes the escrow row (the pinned refund address and the fee snapshot) and its three child keys, each
// party's with its own origin (ruling (b): origin_path the exact account origin, derivation_suffix below it).
func insertEscrow(ctx context.Context, tx *sql.Tx, orderID, refund string, feeBps int64, feeDest string, b *builtEscrow) error {
	if feeBps == 0 {
		feeDest = ""
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, orderID, b.Address, b.MarketIndex, refund, feeBps, feeDest).Scan(&id)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "escrows_refund_address_key" {
		return errEscrowRefundUsed // another checkout pinned it first
	}
	if err != nil {
		return err
	}
	for _, p := range b.Parties {
		var uid, h any
		if p.Party != "market" {
			if checkEscrowOrigin(p.Key, p.Key.PathText, p.Suffix) != nil {
				return errEscrowOrigin
			}
			uid, h = p.UserID, p.KeyHMAC
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, hex.EncodeToString(b.Keys[p.Party]), id, p.Party, uid, h, p.Key.Fingerprint, p.Key.PathText, p.Suffix); err != nil {
			if errors.As(err, &pe) && pe.Code == "23505" {
				// A child key another escrow already holds: a fixed line, never the database's text (which names the key).
				log.Printf("escrow: escrow for order %s not built: a child key is already held by another escrow", shortID(orderID))
				return errEscrowKeyTaken
			}
			return err
		}
	}
	return nil
}
