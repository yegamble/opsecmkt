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
// draft on a digital listing with automatic delivery, or on a physical listing with a dispatch window (Phase 2,
// P2-R8), may pay into a 2-of-3 escrow once both the buyer's and the vendor's escrow
// keys are active (ES-9). A physical order's checkout also needs a vendor
// who can receive an encrypted shipping address (A-83, checked before any RPC), and its escrow row snapshots the
// release deadline length in force and the listing's dispatch window (escrows.release_days, dispatch_days).
//
// POST /orders/escrow takes the buyer's refund address, pinned for the order, and builds the escrow in the checkout
// transaction, after every check has passed (before deriving an address):
//  1. each party's next child key (/0/<i>, from 1; /0/0 is the practice child) and a fresh market child index from
//     the escrow_market_index counter;
//  2. Bitcoin Core expands the child keys statelessly: deriveaddresses of the extended-key descriptor, createpsbt
//     paying that address from a made-up outpoint (never signed or broadcast) and utxoupdatepsbt, which fills in
//     the witness script and each key's derivation (ES-3);
//  3. the witness script is checked (checkEscrowWitness): three distinct compressed keys, exactly one from the
//     pinned arbiter fingerprint, each party's key at its own origin;
//  4. the bare-key descriptor wsh(sortedmulti(2,<buyer>,<vendor>,<market>)) must have Core's checksum and derive the
//     same address;
//  5. no child key may be one an escrow on record holds or the orphan sweep reserved (escrow_pubkeys,
//     escrow_reserved_keys, A-366), and the watch-only wallet must not already watch the address (getaddressinfo,
//     A-359: an escrow a restored database no longer has); otherwise the next keys are tried (buildUnwatchedEscrow);
//  6. the escrow rows are written (uncommitted, so invisible) and the bare-key descriptor alone is imported into the
//     watch-only wallet; only then does the transaction commit and the address exist anywhere a page can show it.
//
// A failure at any step rolls the transaction back: no order change, no escrow row. A market index a failed build
// took is skipped, never reused; an import whose transaction then fails to commit stays in the watch-only wallet,
// watched but never shown (reconciliation lists such orphans, P1-d, and reserves their keys, P2-e). While the restore
// gate is set, or after a restore until the orphan sweep has run (escrow_sweep_required), no escrow is created at all
// (errEscrowRestoring, errEscrowSweep).
//
// The escrow's shape follows both parties' active keys (A-456a, rulings P4-R2, P4-R17, P4-R18): two Bitcoin Core keys make
// shape (a), above; two per-order wallet keys make shape (b); a mixed pair is refused, naming the vendor's key type. In
// shape (b) each party's key is the first receiving key (/0/0) of an order wallet built on the account key's child <i>,
// with i >= 2 from the same per-account counter (escrowNextChildSQL), and the market's is the escrow signer's account
// key's child <m>/0/0, m >= 2 from the market counter (cmd/escrow-signer signs only those). Go derives every key
// (escrow_bip32.go) and Core must agree before anything is stored (checkPerOrderEscrow): the three keys, the address,
// and the order wallet's sibling addresses /0/1..19 and /1/0..9, which are stored (escrow_siblings) and imported with the
// escrow as two ranged descriptors of the rooted per-order keys, so a deposit to one is seen and returned by the stray
// refund (escrow_watch.go), which spends a sibling's deposits with that sibling's own script (A-456b, siblingKeys).
// Every order wallet key (a tpub) is stored with its key (per_order_xpub: the buyer's and the vendor's since QA5W4A-5,
// the market's since QA5W5A-1), so it is rebuilt (perOrderXpub) whatever key a party has registered since and without
// the escrow signer, which is asked for its key at checkout and otherwise only to co-sign.
// Since A-456b a pair of per-order keys gets a per-order escrow whenever the escrow signer, which co-signs its
// settlements (escrow_cosign.go), answers with its pinned key; A-456a's checkout switch is gone.

func init() {
	registerAction("/orders/escrow", actionSpec{FormNonce: true, OneAtATime: true, Off: escrowOff, Run: escrowCheckoutAction})
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
	errEscrowKind       = fail(409, "Bitcoin escrow is available only for digital listings with automatic delivery and for physical listings with a dispatch window. Nothing was changed.")
	errEscrowCurrency   = fail(409, "Bitcoin escrow is for Bitcoin orders only. Nothing was changed.")
	errEscrowSelf       = fail(409, "You cannot buy your own listing with escrow: an escrow needs two parties' keys. Nothing was changed.")
	errEscrowBuyerKey   = fail(409, "Activate your Bitcoin escrow key first: register it and sign its practice transaction on your escrow key page. Nothing was changed.")
	errEscrowVendorKey  = fail(409, "The vendor has no active Bitcoin escrow key, so this order cannot use escrow. Nothing was changed.")
	// P4-R18: a Bitcoin Core wallet cannot sign a per-order key, nor a per-order wallet a Core child, so both parties'
	// keys must be of one kind. The listing and the vendor's page show the vendor's (A-455).
	errEscrowMixedVendorPerOrder = fail(409, "The vendor's Bitcoin escrow key is for a per-order wallet and yours is for Bitcoin Core. An escrow needs both keys to be of the same kind, so this order cannot use escrow. Nothing was changed.")
	errEscrowMixedVendorCore     = fail(409, "The vendor's Bitcoin escrow key is for Bitcoin Core and yours is for a per-order wallet. An escrow needs both keys to be of the same kind, so this order cannot use escrow. Nothing was changed.")
	errEscrowSigner              = fail(503, "The escrow could not be created: escrows with per-order wallets need the market's escrow signer, which is not available right now. Nothing was changed; try again later.")
	errEscrowBuild               = fail(503, "The escrow could not be created: the market's Bitcoin node did not confirm it. Nothing was changed; try again later.")
	errEscrowKeyTaken            = fail(409, "The escrow could not be created: another escrow on this market already holds one of its keys. Nothing was changed. Try again; if the same refusal comes back, contact the market's staff.")
	// QA5W3A-7: a party key that is the escrow signer's own key (isSignerKey) never joins an escrow.
	errEscrowBuyerSignerKey  = fail(409, "Your Bitcoin escrow key is the market's own escrow signer key, which can never be a party's key in an escrow. Register a key from your own wallet on your escrow key page. Nothing was changed.")
	errEscrowVendorSignerKey = fail(409, "The vendor's Bitcoin escrow key is the market's own escrow signer key, which can never be a party's key in an escrow, so this order cannot use escrow. Nothing was changed.")
	// errEscrowRestoring refuses escrow checkout while the restore gate is set (A-359): the restored database does not
	// know the escrows created after its backup, so it could hand out their child keys again.
	errEscrowRestoring = fail(503, "Bitcoin escrow is paused while this market restores its records from a backup. Nothing was charged and your order is still a draft; try again later.")
	// errEscrowSweep refuses escrow checkout after a restore until the orphan sweep has reserved the keys of the escrows
	// the restored database lost (P2-R10, A-366).
	errEscrowSweep = fail(503, "Bitcoin escrow is paused until this market has checked its escrow wallet after restoring its records from a backup. Nothing was charged and your order is still a draft; try again later.")
	// errEscrowWatched refuses a checkout whose every composed address the watch-only wallet already watches (A-359).
	errEscrowWatched = fail(503, "The escrow could not be created: every escrow address the market composed for this order is already watched by its escrow wallet, which happens after the market restored its records from a backup. Nothing was charged and your order is still a draft; contact the market's staff.")
	// errEscrowRescanning refuses a checkout whose import Core refused because the watch-only wallet is rescanning (an
	// operator's rescanblockchain, or the watch-only wallet repair's import after a restore, QA5W4A-3): a wait, said as
	// one (QA5W5A-2), not "the market's Bitcoin node did not confirm it".
	errEscrowRescanning = fail(503, "The escrow could not be created: the market's escrow wallet is rescanning the chain. Nothing was changed and your order is still a draft; try again in a few minutes.")
)

// escrowWatchedTries bounds how many composed escrow addresses one checkout tries while the watch-only wallet already
// watches each of them (A-359).
const escrowWatchedTries = 20

// escrowReservedTries bounds how many builds one checkout tries while a child key it composed is held by an escrow on
// record or reserved by the orphan sweep (A-366): after a restore, the lost escrows past the restore's margin of 50.
const escrowReservedTries = 100

// escrowCheckoutCtx marks the draft -> awaiting_payment transition of an escrow checkout: it needs the escrow node,
// not a pooled wallet for the currency (transition).
type escrowCheckoutCtx struct{}

func isEscrowCheckout(ctx context.Context) bool { return ctx.Value(escrowCheckoutCtx{}) != nil }

// escrowAddressForm is the offline check of an address the market will pay or refuse (a refund or fee address): 14 to
// 90 ASCII letters and digits, so it can never close or extend the addr(…) descriptor the node checks it with; bech32
// in lower case, the one spelling the market stores and compares. An address with the human-readable part of chain's
// segwit addresses (the chain table, escrowChains: bcrt on regtest, tb on signet and testnet4) in any case is lowered
// (QA4W1-4: a TB1… address in capitals settles on signet as a BCRT1… one does on regtest); anything else is kept as
// given and left to the node. chain "" (ESCROW_BTC_FEE_ADDRESS, read before any node answers) lowers nothing; startEscrow
// forms that address again with the node's chain.
func escrowAddressForm(chain, s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 14 || len(s) > 90 || strings.ContainsFunc(s, func(r rune) bool { return !isASCIIAlnum(r) }) {
		return "", false
	}
	if hrp := escrowChains[chain]; hrp != "" {
		if lower := strings.ToLower(s); strings.HasPrefix(lower, hrp+"1") {
			s = lower
		}
	}
	return s, true
}

// escrowParty is one party key of an escrow being built: the account key, its owner and the child suffix used. A
// per-order (shape (b)) key also carries Go's derivation: the order wallet's key (the account key's child <Index>) and
// its first receiving key (/0/0), the escrow's key.
type escrowParty struct {
	Party, UserID, KeyHMAC string
	Key                    escrowKey
	Suffix                 string
	Index                  int64  // the child index in Suffix (/0/<Index>, or /<Index>/0/0 in shape (b))
	Layout                 string // layoutCore (shape (a)) or layoutPerOrder (shape (b))
	Order, Leaf            bip32Key
}

// escrowParties locks the buyer's and the vendor's key rows (in account order, so two checkouts never deadlock) and
// returns each party's key with its next child suffix (escrowNextChildSQL). The count keys off the account, never the
// HMAC, so an APP_SECRET_KEY rotation never hands out a child key twice (chair's ruling (a) on QA25); the key rows
// locked FOR UPDATE serialise it. Both keys must have one layout (P4-R18): a mixed pair is refused naming the vendor's.
func (a *App) escrowParties(ctx context.Context, tx *sql.Tx, o *Order) (buyer, vendor escrowParty, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at IS NOT NULL,layout FROM user_escrow_keys
		WHERE user_id IN ($1,$2) ORDER BY user_id FOR UPDATE`, o.BuyerID, o.VendorID)
	if err != nil {
		return buyer, vendor, err
	}
	type row struct {
		sealed, hmac, fp, path, layout string
		active                         bool
	}
	found := map[string]row{}
	for rows.Next() {
		var uid string
		var r row
		if err = rows.Scan(&uid, &r.sealed, &r.hmac, &r.fp, &r.path, &r.active, &r.layout); err != nil {
			rows.Close()
			return buyer, vendor, err
		}
		found[uid] = r
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return buyer, vendor, err
	}
	if r, ok := found[o.BuyerID]; !ok || !r.active {
		return buyer, vendor, errEscrowBuyerKey
	}
	if r, ok := found[o.VendorID]; !ok || !r.active {
		return buyer, vendor, errEscrowVendorKey
	}
	layout := found[o.BuyerID].layout
	switch v := found[o.VendorID].layout; {
	case v != layout && v == layoutPerOrder:
		return buyer, vendor, errEscrowMixedVendorPerOrder
	case v != layout:
		return buyer, vendor, errEscrowMixedVendorCore
	}
	// The one shared counter (P4-R2): never below 2 for a per-order key.
	floor := int64(1)
	if layout == layoutPerOrder {
		floor = 2
	}
	party := func(uid, name string, refused error) (escrowParty, error) {
		r := found[uid]
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
		if err = tx.QueryRowContext(ctx, escrowNextChildSQL, uid, floor).Scan(&next); err != nil {
			return escrowParty{}, err
		}
		p := escrowParty{Party: name, UserID: uid, KeyHMAC: r.hmac, Key: key, Index: next - 1, Layout: layout}
		if p.next() != nil {
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

// escrowNextChildSQL is an account's next escrow child index ($1 the user ID, $2 the lowest index the layout takes: 1
// for a Bitcoin Core key, 2 for a per-order wallet key): one more than the highest its escrows used under any origin
// and in either shape (A-314: a counter per origin restarted at /0/1 when a key's origin changed, a child key an
// existing escrow already held; P4-R2: one counter for /0/<i> and /<i>/0/0), never below the floor a restore set
// (escrow_index_floors, P2-R10) and never below $2. scripts/restore.sh's floor reads the highest index the same way.
const escrowNextChildSQL = `SELECT GREATEST(COALESCE(max(CASE WHEN derivation_suffix LIKE '/0/%' THEN split_part(derivation_suffix,'/',3)
		ELSE split_part(derivation_suffix,'/',2) END::bigint),0)+1,
	COALESCE((SELECT next_index FROM escrow_index_floors WHERE user_id=$1),1),$2::bigint) FROM escrow_pubkeys
	WHERE user_id=$1 AND (derivation_suffix ~ '^/0/[1-9][0-9]{0,9}$' OR derivation_suffix ~ '^/[1-9][0-9]{0,9}/0/0$')`

// escrowCheckoutAction: the buyer moves a draft to awaiting_payment into a new escrow (see the file comment). The
// refusals that need no node come first and make no RPC.
func escrowCheckoutAction(c *actionCtx) (actionResult, error) {
	a, ctx := c.A, c.Ctx()
	e := a.escrow.Load()
	if e == nil {
		return actionResult{}, errEscrowDown
	}
	refund, ok := escrowAddressForm(e.network, c.Form.Get("refund_address"))
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
	// After a restore, until the orphan sweep has reserved the lost escrows' keys (A-366): no node call either.
	if sweep, err := escrowSweepRequired(ctx, c.Tx); err != nil {
		return actionResult{}, err
	} else if sweep {
		return actionResult{}, errEscrowSweep
	}
	if err = lockNewIntake(c, o.BuyerID, o.VendorID, false); err != nil {
		return actionResult{}, err
	}
	// The buyer's cap on orders awaiting payment, serialised on the buyer's row as in payAction.
	var open int
	if err = c.Tx.QueryRowContext(ctx, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND state='awaiting_payment'", c.User.ID).Scan(&open); err != nil {
		return actionResult{}, err
	}
	if open >= maxAwaitingPayment {
		return actionResult{}, fail(409, "You already have "+strconv.Itoa(maxAwaitingPayment)+" orders awaiting payment. Pay or cancel one before requesting another payment address.")
	}
	if err = lockPaymentIntake(c, o.Currency, kindEscrow); err != nil {
		return actionResult{}, err
	}
	eligible, price, dispatchDays, err := escrowEligible(ctx, c.Tx, &o)
	if err != nil {
		return actionResult{}, err
	} else if !eligible {
		return actionResult{}, errEscrowKind
	} else if price < payoutFloor(o.Currency, "release") {
		message, err := subFloorListingForProduct(ctx, c.Tx, o.ProductID, o.Currency)
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{}, fail(400, message)
	}
	// A physical order: the buyer must be able to send the vendor an encrypted shipping address (A-83, as payAction
	// checks), and the escrow snapshots the release deadline length in force with the listing's dispatch window. Both
	// are settled before the node is asked anything.
	releaseDays := 0
	if o.Kind == "physical" {
		if ok, err := userCanReceiveEncrypted(ctx, c.Tx, o.VendorID); err != nil {
			return actionResult{}, err
		} else if !ok {
			return actionResult{}, fail(409, "Escrow unavailable. "+noVendorKeyReason+" Nothing was changed.")
		}
		if releaseDays, err = escrowReleaseDays(ctx, c.Tx); err != nil {
			return actionResult{}, err
		}
	} else {
		dispatchDays = 0 // a digital escrow has no dispatch window or release deadline
	}
	buyer, vendor, err := a.escrowParties(ctx, c.Tx, &o)
	if err != nil {
		return actionResult{}, err
	}
	// Shape (b): the market's key is the escrow signer's (P4-R17), read from the signer and checked against its pins.
	var signer *escrowKey
	if buyer.Layout == layoutPerOrder {
		k, err := a.signerAccount(ctx, c.Tx)
		if err != nil {
			log.Printf("escrow: per-order escrow for order %s not built: escrow signer: %s", shortID(o.ID), errorClassOf(err))
			return actionResult{}, errEscrowSigner
		}
		// A party key that is the signer's (registered before the signer's pins existed; isSignerKey refuses it at
		// registration since QA5W3A-7) would give the signer's seed two of the three keys: refused, saying so.
		signerMAC := a.escrowXPubHMAC(k.ChainCode, k.PubKey)
		for _, p := range []escrowParty{buyer, vendor} {
			if p.Key.Fingerprint == k.Fingerprint || a.escrowXPubHMAC(p.Key.ChainCode, p.Key.PubKey) == signerMAC {
				log.Printf("escrow: per-order escrow for order %s not built: the %s's escrow key is the escrow signer's", shortID(o.ID), p.Party)
				if p.Party == "buyer" {
					return actionResult{}, errEscrowBuyerSignerKey
				}
				return actionResult{}, errEscrowVendorSignerKey
			}
		}
		signer = &k
	}
	valid, err := e.validAddress(ctx, refund)
	if err != nil {
		log.Printf("escrow: refund address not checked: %s", errorClassOf(err))
		return actionResult{}, errEscrowBuild
	}
	if !valid {
		return actionResult{}, errEscrowRefundInvalid(c.A.escrowNetwork())
	}
	// Never an address the market already used (ES-5): an escrow address or a per-order escrow's sibling address
	// (QA5W3A-4), another escrow's refund address, a settlement destination (a vendor's claim), a fee destination
	// snapshotted or configured, or a pooled deposit address.
	feeBps, feeAddr, err := escrowFee(ctx, c.Tx, e)
	if err != nil {
		return actionResult{}, err
	}
	var used bool
	if err = c.Tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM escrows WHERE address=$1 OR refund_address=$1 OR fee_destination=$1) OR $1=$2
		OR EXISTS(SELECT 1 FROM escrow_siblings WHERE address=$1)
		OR EXISTS(SELECT 1 FROM payment_addresses WHERE address=$1) OR EXISTS(SELECT 1 FROM settlements WHERE destination=$1)`, refund, feeAddr).Scan(&used); err != nil {
		return actionResult{}, err
	}
	if used {
		return actionResult{}, errEscrowRefundUsed
	}
	if _, err = c.Tx.ExecContext(ctx, "UPDATE orders o SET amount=p.btc FROM products p WHERE o.id=$1 AND o.state='draft' AND p.id=o.product_id", o.ID); err != nil {
		return actionResult{}, err
	}
	moved, err := a.transition(withCheckoutPrice(context.WithValue(ctx, escrowCheckoutCtx{}, true)), c.Tx, o.ID, stateDraft, stateAwaitingPayment, c.User, "Escrow address requested")
	if err != nil {
		return actionResult{}, err
	}
	sats, err := parseAmount(moved.Amount, 8)
	if err != nil {
		return actionResult{}, err
	}
	built, err := a.buildUnwatchedEscrow(ctx, c.Tx, e, o.ID, sats, buyer, vendor, signer)
	if err != nil {
		return actionResult{}, err
	}
	if err = insertEscrow(ctx, c.Tx, o.ID, refund, feeBps, feeAddr, releaseDays, dispatchDays, built); err != nil {
		return actionResult{}, err
	}
	// The import is the last step before commit: the address is shown only once the watch-only wallet watches it (in
	// shape (b), with the order wallet's ranged descriptors and each sibling address, labelled, in one call).
	imports := append([]escrowImport{{Desc: built.Descriptor, Timestamp: "now", Label: "escrow:" + o.ID}}, built.Ranged...)
	var siblings []string
	for _, s := range built.Siblings {
		siblings = append(siblings, s.Address)
	}
	imports = append(imports, siblingImports(o.ID, siblings, "now")...)
	if err = e.importDescriptors(ctx, e.watchName, imports); err != nil {
		log.Printf("escrow: escrow for order %s not imported: %s", shortID(o.ID), errorClassOf(err))
		var re *escrowRPCError
		if errors.As(err, &re) && re.Class == escrowClassRescanning {
			return actionResult{}, errEscrowRescanning // QA5W5A-2: a wait, not a node that disagreed
		}
		return actionResult{}, errEscrowBuild
	}
	return actionResult{Redirect: orderPage(o.ID), Notice: "escrow-created", Audit: "Created Bitcoin escrow (" + c.A.escrowNetwork() + ") for order " + shortID(o.ID)}, nil
}

// buildUnwatchedEscrow builds the order's escrow (buildEscrow) at a fresh market index and checks, before anything is
// written or imported, that none of its child keys is held by an escrow on record or reserved by the orphan sweep
// (A-366) and that the watch-only wallet does not already watch its address (A-359). A restored database knows nothing
// of the escrows created after its backup: each party's next child index (one more than the highest its escrows on
// record used, or the floor the restore set) and the market counter are back where the backup left them.
//
// A key on record or reserved (escrow_pubkeys, escrow_reserved_keys: one query, no RPC) would link this escrow on chain
// to the escrow that used it, whatever address it is composed into: only the colliding party's index moves on by one
// (for the market key, the next market index) and the build is tried again without asking the wallet, at most
// escrowReservedTries times; then the checkout is refused (errEscrowKeyTaken). Normal operation finds no such key and
// makes no extra node call.
//
// When the wallet watches the composed address (the same buyer and vendor would get an escrow address handed out after
// the backup, and their deposits would merge), both party indices and the market index move on by one and the build
// is tried again, at most escrowWatchedTries times; then the checkout is refused with a fixed 503 (errEscrowWatched).
//
// A skipped market index is never used again (a sequence); skipped party indices lie below the ones the escrow stores,
// which the next escrow counts from. After the last try nothing is written or imported.
//
// With signer set the escrow is shape (b) (A-456a): the market's key is signer's child <m>/0/0, where the market index m
// skips 0, 1 and the practice index 2^31-1 (and any index whose per-order keys BIP32 calls invalid), and Go's
// derivation must agree with Core's (checkPerOrderEscrow) before anything else is asked.
func (a *App) buildUnwatchedEscrow(ctx context.Context, tx *sql.Tx, e *escrowBTC, orderID string, sats int64, buyer, vendor escrowParty, signer *escrowKey) (*builtEscrow, error) {
	var market escrowParty
	nextMarket := true
	for watched, reserved := 0, 0; ; {
		if nextMarket {
			var err error
			if market, err = nextMarketParty(ctx, tx, e, signer); err != nil {
				return nil, err
			}
		}
		marketFP := e.fingerprint
		if signer != nil {
			marketFP = signer.Fingerprint
		}
		built, err := e.buildEscrowWith(ctx, orderID, sats, buyer, vendor, market, marketFP)
		if err == nil && signer != nil {
			err = e.checkPerOrderEscrow(ctx, built)
		}
		if err != nil {
			log.Printf("escrow: escrow for order %s not built: %s", shortID(orderID), errorClassOf(err))
			return nil, errEscrowBuild
		}
		held, err := heldEscrowKeys(ctx, tx, built.Keys)
		if err != nil {
			return nil, err
		}
		if len(held) > 0 {
			if reserved++; reserved == escrowReservedTries {
				log.Printf("escrow: escrow for order %s not built: every child key tried is already held by an escrow or reserved (%d tries)", shortID(orderID), escrowReservedTries)
				return nil, errEscrowKeyTaken
			}
			log.Printf("escrow: escrow for order %s: a child key it composed is already held by an escrow or reserved by the orphan sweep (a restored database?); trying that key's next index", shortID(orderID))
			nextMarket = held["market"]
			for _, p := range []*escrowParty{&buyer, &vendor} {
				if held[p.Party] {
					if err = p.next(); err != nil {
						return nil, err
					}
				}
			}
			continue
		}
		known, err := e.watchKnows(ctx, built.Address)
		if err != nil {
			log.Printf("escrow: escrow for order %s not built: %s", shortID(orderID), errorClassOf(err))
			return nil, errEscrowBuild
		}
		if !known {
			return built, nil
		}
		if watched++; watched == escrowWatchedTries {
			log.Printf("escrow: escrow for order %s not built: every address tried is already in the watch-only wallet (%d tries)", shortID(orderID), escrowWatchedTries)
			return nil, errEscrowWatched
		}
		log.Printf("escrow: escrow for order %s: its address is already in the watch-only wallet (a restored database?); trying the next child keys", shortID(orderID))
		nextMarket = true
		for _, p := range []*escrowParty{&buyer, &vendor} {
			if err = p.next(); err != nil {
				return nil, err
			}
		}
	}
}

// next moves the party to its next child index: for a per-order key, the next one whose order wallet key and first
// receiving key BIP32 can derive (perOrderKeys; an invalid child has a probability of about 2^-127), with Go's
// derivation of both.
func (p *escrowParty) next() error {
	for {
		p.Index++
		p.Suffix = escrowChildSuffix(p.Index)
		if p.Layout == layoutPerOrder {
			p.Suffix = perOrderSuffix(p.Index)
		}
		if checkEscrowOrigin(p.Key, p.Key.PathText, p.Suffix) != nil {
			return errEscrowBuild
		}
		if p.Layout != layoutPerOrder {
			return nil
		}
		var ok bool
		if p.Order, p.Leaf, ok = perOrderKeys(p.Key, uint32(p.Index)); ok {
			return nil
		}
		log.Printf("escrow: per-order index %d of a %s key is not derivable (BIP32); using the next", p.Index, p.Party)
	}
}

// nextMarketParty is the market's key for the next escrow at a fresh market index: in shape (a) (signer nil) the arbiter
// key's child /0/<index>; in shape (b) the escrow signer's account key's child <index>/0/0, skipping the indices 0 and 1
// (P4-R2), any whose keys BIP32 cannot derive, and refusing (errEscrowBuild) the practice index 2^31-1 and anything
// beyond, which no unhardened step reaches.
func nextMarketParty(ctx context.Context, tx *sql.Tx, e *escrowBTC, signer *escrowKey) (escrowParty, error) {
	for {
		var index int64
		if err := tx.QueryRowContext(ctx, "SELECT nextval('escrow_market_index')").Scan(&index); err != nil {
			return escrowParty{}, err
		}
		if signer == nil {
			return escrowParty{Party: "market", Key: e.arbiterKey, Suffix: escrowChildSuffix(index), Index: index, Layout: layoutCore}, nil
		}
		if index >= escrowPracticeIndex {
			log.Print("escrow: no per-order market index is left below the reserved practice index 2^31-1; per-order escrows cannot be created")
			return escrowParty{}, errEscrowBuild
		}
		if index < 2 {
			continue
		}
		if order, leaf, ok := perOrderKeys(*signer, uint32(index)); ok {
			return escrowParty{Party: "market", Key: *signer, Suffix: perOrderSuffix(index), Index: index, Layout: layoutPerOrder, Order: order, Leaf: leaf}, nil
		}
	}
}

// heldEscrowKeys reports which parties' child keys of a built escrow an escrow on record holds or the orphan sweep
// reserved (A-366).
func heldEscrowKeys(ctx context.Context, tx *sql.Tx, keys map[string][]byte) (map[string]bool, error) {
	party := map[string]string{}
	var hexes []string
	for p, k := range keys {
		h := hex.EncodeToString(k)
		party[h] = p
		hexes = append(hexes, h)
	}
	found, err := collectStrings(ctx, tx, "SELECT pubkey FROM escrow_pubkeys WHERE pubkey=ANY($1) UNION SELECT pubkey FROM escrow_reserved_keys WHERE pubkey=ANY($1)", hexes)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, k := range found {
		held[party[k]] = true
	}
	return held, nil
}

// escrowEligible reports whether o's listing takes escrow, its price in o's currency (BTC or XMR, in satoshi or
// piconero) and its dispatch window in days (0 = none). A digital listing takes escrow when its content is delivered
// automatically (as autoDeliveryHook would) and it is not taken down; a physical listing, when it has a dispatch window
// and is not taken down.
func escrowEligible(ctx context.Context, q rowQuerier, o *Order) (bool, int64, int, error) {
	var content string
	var takenDown bool
	var price int64
	var dispatch sql.NullInt32
	if err := q.QueryRowContext(ctx, "SELECT delivery_content,admin_archive_reason IS NOT NULL,CASE WHEN $2='XMR' THEN xmr ELSE btc END,dispatch_days FROM products WHERE id=$1",
		o.ProductID, o.Currency).Scan(&content, &takenDown, &price, &dispatch); err != nil {
		return false, 0, 0, err
	}
	ok := !takenDown && (o.Kind == "digital" && strings.TrimSpace(content) != "" && utf8.RuneCountInString(content) <= maxDeliveryChars ||
		o.Kind == "physical" && dispatch.Valid)
	return ok, price, int(dispatch.Int32), nil
}

// builtEscrow is a checked escrow, not yet imported: its address, the bare-key descriptor with its checksum (what is
// imported) and each party's child key with the party it belongs to. A shape-(b) escrow also has the order wallet's
// sibling addresses and the ranged descriptors that cover them (checkPerOrderEscrow), imported with it.
type builtEscrow struct {
	Address, Descriptor string
	MarketIndex         int64
	Parties             []escrowParty
	Keys                map[string][]byte
	Siblings            []escrowSibling
	Ranged              []escrowImport
}

// buildEscrow composes and checks the shape-(a) escrow of one order with the arbiter key's child /0/<marketIndex>.
func (e *escrowBTC) buildEscrow(ctx context.Context, orderID string, sats int64, buyer, vendor escrowParty, marketIndex int64) (*builtEscrow, error) {
	market := escrowParty{Party: "market", Key: e.arbiterKey, Suffix: escrowChildSuffix(marketIndex), Index: marketIndex, Layout: layoutCore}
	return e.buildEscrowWith(ctx, orderID, sats, buyer, vendor, market, e.fingerprint)
}

// buildEscrowWith composes and checks the escrow of one order (steps 2 to 4 of the file comment) from the parties' and
// the market's keys; marketFP is the fingerprint the market's key derives from (the arbiter's, or in shape (b) the escrow
// signer's). Its RPCs are deriveaddresses, createpsbt, utxoupdatepsbt, getdescriptorinfo and deriveaddresses; none
// touches a wallet.
func (e *escrowBTC) buildEscrowWith(ctx context.Context, orderID string, sats int64, buyer, vendor, market escrowParty, marketFP string) (*builtEscrow, error) {
	parties := []escrowParty{buyer, vendor, market}
	var exprs []string
	var want []escrowExpectKey
	for _, p := range parties {
		steps, err := parseOriginPath(p.Suffix)
		if err != nil || len(steps) != 2 && len(steps) != 3 || len(steps) == 3 != (p.Layout == layoutPerOrder) {
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
	keys, err := checkEscrowWitness(fp.Inputs[0], spk, marketFP, want)
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
	return &builtEscrow{Address: addrs[0], Descriptor: public, MarketIndex: market.Index, Parties: parties, Keys: keys}, nil
}

// insertEscrow writes the escrow row (the pinned refund address, the fee snapshot and, for a physical order, the
// release deadline length and dispatch window; 0 = none) and its three child keys, each party's with its own origin
// (ruling (b): origin_path the exact account origin, derivation_suffix below it).
func insertEscrow(ctx context.Context, tx *sql.Tx, orderID, refund string, feeBps int64, feeDest string, releaseDays, dispatchDays int, b *builtEscrow) error {
	if feeBps == 0 {
		feeDest = ""
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO escrows(order_id,coin,address,market_index,refund_address,fee_bps,fee_destination,release_days,dispatch_days)
		VALUES($1,'BTC',$2,$3,$4,$5,$6,NULLIF($7,0),NULLIF($8,0)) RETURNING id`, orderID, b.Address, b.MarketIndex, refund, feeBps, feeDest, releaseDays, dispatchDays).Scan(&id)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "escrows_refund_address_key" {
		return errEscrowRefundUsed // another checkout pinned it first
	}
	if err != nil {
		return err
	}
	for _, p := range b.Parties {
		var uid, h, xpub any
		if p.Party != "market" {
			if checkEscrowOrigin(p.Key, p.Key.PathText, p.Suffix) != nil {
				return errEscrowOrigin
			}
			uid, h = p.UserID, p.KeyHMAC
		}
		// Shape (b): each order wallet key (the ranged descriptors name it rooted), cross-checked with Core with them
		// (checkPerOrderEscrow), and checked again here against the key stored beside it (QA5W4A-5). The market's
		// (QA5W5A-1) must also come from the escrow signer's key that this checkout read and checked against the pins
		// (signerAccount): its origin fingerprint is the pinned one, and it is that key's child <m>.
		if p.Layout == layoutPerOrder {
			if p.Party == "market" {
				var pinned string
				if err = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')", escrowSignerFingerprintPin).Scan(&pinned); err != nil {
					return err
				}
				order, used, err := deriveOrderKey(p.Key, uint32(p.Index))
				if pinned == "" || p.Key.Fingerprint != pinned || err != nil || used != uint32(p.Index) || order.Serialize(versionTpub) != p.Order.Serialize(versionTpub) {
					return errPerOrderMismatch
				}
			}
			s := perOrderStored{Party: p.Party, PubKey: hex.EncodeToString(b.Keys[p.Party]), Suffix: p.Suffix,
				Xpub: sql.NullString{String: p.Order.Serialize(versionTpub), Valid: true}}
			if _, err := s.orderKey(); err != nil {
				return errPerOrderMismatch
			}
			xpub = s.Xpub.String
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix,per_order_xpub)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, hex.EncodeToString(b.Keys[p.Party]), id, p.Party, uid, h, p.Key.Fingerprint, p.Key.PathText, p.Suffix, xpub); err != nil {
			if errors.As(err, &pe) && pe.Code == "23505" {
				// A child key another escrow already holds: a fixed line, never the database's text (which names the key).
				log.Printf("escrow: escrow for order %s not built: a child key is already held by another escrow", shortID(orderID))
				return errEscrowKeyTaken
			}
			return err
		}
	}
	// Shape (b): the order wallet's sibling addresses, which the watcher reads with the escrow's (A-456a).
	for _, s := range b.Siblings {
		if _, err = tx.ExecContext(ctx, "INSERT INTO escrow_siblings(escrow_id,branch,idx,address) VALUES($1,$2,$3,$4)", id, s.Branch, s.Index, s.Address); err != nil {
			return err
		}
	}
	return nil
}
