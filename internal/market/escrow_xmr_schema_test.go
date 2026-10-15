package market

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"

	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// xmrEscrowIdx is the payments idx of a Monero escrow deposit to account of wallet (migration 084).
func xmrEscrowIdx(wallet, account int64) int64 { return 1<<62 + wallet<<32 + account }

// xmrAddr is a Monero-shaped address for label: 95 base58 characters, the shape migration 084 checks. It is not a
// valid Monero address (no checksum, no keys).
func xmrAddr(label string) string {
	const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	h := sha256.Sum256([]byte("xmr:" + label))
	b := []byte{'8'}
	for i := 0; len(b) < 95; i++ {
		b = append(b, base58[int(h[i%len(h)]^byte(i/len(h)))%len(base58)])
	}
	return string(b)
}

// xmrWallet inserts this month's escrow wallet numbered n in state, with its sealed seed: the row the escrow watcher
// writes once the wallet is created and sealed.
func xmrWallet(t *testing.T, e *testEnv, n int, state string) int64 {
	t.Helper()
	var id int64
	if err := e.DB.QueryRow(`INSERT INTO xmr_escrow_wallets(name,month,primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at,state)
		VALUES('escrow-'||to_char(now(),'YYYYMM')||'-'||lpad($1::text,2,'0'),date_trunc('month',now())::date,$2,1,'sealed seed',repeat('AB',20),now(),$3) RETURNING id`,
		strconv.Itoa(n), xmrAddr("wallet-"+strconv.Itoa(n)), state).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// xmrSlot inserts a free slot: account of wallet, at an address of its own.
func xmrSlot(t *testing.T, e *testEnv, wallet, account int64) int64 {
	t.Helper()
	var id int64
	if err := e.DB.QueryRow("INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) VALUES($1,$2,$3) RETURNING id",
		wallet, account, xmrAddr("slot-"+strconv.FormatInt(wallet, 10)+"-"+strconv.FormatInt(account, 10))).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// xmrEscrow is a new XMR order of product's in state with a Monero escrow, as checkout (X-c) will write it: a free slot
// (account of wallet) claimed for the order, then the escrow at the slot's address. It returns the order, the escrow
// address and the slot.
func xmrEscrow(t *testing.T, e *testEnv, wallet, account int64, buyer, product, state string) (string, string, int64) {
	t.Helper()
	order := e.order(buyer, product, "XMR", state)
	addr, slot := xmrEscrowOf(t, e, wallet, account, order)
	return order, addr, slot
}

// xmrEscrowOf gives the XMR order a Monero escrow on a new slot (account of wallet): its address and slot.
func xmrEscrowOf(t *testing.T, e *testEnv, wallet, account int64, order string) (string, int64) {
	t.Helper()
	slot := xmrSlot(t, e, wallet, account)
	var addr string
	if err := e.DB.QueryRow("UPDATE xmr_escrow_slots SET state='claimed',order_id=$2,updated=now() WHERE id=$1 AND state='free' RETURNING address", slot, order).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("INSERT INTO escrows(order_id,coin,address,refund_address,fee_bps,fee_destination,xmr_slot_id) VALUES($1,'XMR',$2,$3,0,'',$4)",
		order, addr, xmrAddr("refund-"+order), slot); err != nil {
		t.Fatal(err)
	}
	return addr, slot
}

// xmrRefused runs q in a transaction it rolls back, so that an accepted statement never masks the next case, and
// requires the database to refuse it with an error containing want.
func xmrRefused(t *testing.T, e *testEnv, what, want, q string, args ...any) {
	t.Helper()
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(q, args...); err == nil {
		t.Errorf("%s: accepted", what)
	} else if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: refused, but not by %s: %v", what, want, err)
	}
}

// Migration 084, by SQL: each CHECK and trigger of the Monero escrow tables and of the escrows, settlements, orders and
// payments changes, accepting what the later slices write and refusing everything else.
func TestXMREscrowMigration084(t *testing.T) {
	e := newTestApp(t)
	buyer, _ := e.user("x84_buyer", "buyer")
	vendor, _ := e.user("x84_vendor", "vendor")
	product := e.product(vendor, "digital")
	w := xmrWallet(t, e, 1, "active")

	// Wallets: name and month, address shape, the seal of every wallet that may hold coins, destruction no earlier than
	// 90 days after the last order closed, and an id that keeps the payments idx in a bigint.
	wallet := func(cols, vals string) string {
		return "INSERT INTO xmr_escrow_wallets(name,month," + cols + ") VALUES('escrow-'||to_char(now(),'YYYYMM')||'-09',date_trunc('month',now())::date," + vals + ")"
	}
	sealed := "primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at,state"
	sealedVals := func(addr, state string) string {
		return "'" + addr + "',1,'sealed seed',repeat('AB',20),now(),'" + state + "'"
	}
	creating := "INSERT INTO xmr_escrow_wallets(name,month) VALUES('escrow-'||to_char(now(),'YYYYMM')||'-07',date_trunc('month',now())::date)"
	if _, err := e.DB.Exec(creating); err != nil {
		t.Fatalf("a wallet being created: %v", err)
	}
	xmrRefused(t, e, "a second wallet with the same name", "xmr_escrow_wallets_name_key", creating)
	xmrRefused(t, e, "a wallet deleted", "escrow records are never deleted", "DELETE FROM xmr_escrow_wallets WHERE name LIKE '%-07'")
	for _, c := range []struct{ what, want, q string }{
		{"a malformed name", "xmr_escrow_wallets_name_check", "INSERT INTO xmr_escrow_wallets(name,month) VALUES('escrow-2026-10-1',date_trunc('month',now())::date)"},
		{"a name of another month", "xmr_escrow_wallets_name_month", "INSERT INTO xmr_escrow_wallets(name,month) VALUES('escrow-202601-01','2026-02-01')"},
		{"a month that is not a month's first day", "xmr_escrow_wallets_month_check", "INSERT INTO xmr_escrow_wallets(name,month) VALUES('escrow-202602-01','2026-02-02')"},
		{"a primary address of 94 characters", "xmr_escrow_wallets_primary_address_check", wallet(sealed, sealedVals(xmrAddr("w94")[:94], "sealed"))},
		{"an integrated primary address", "xmr_escrow_wallets_primary_address_check", wallet(sealed, sealedVals(xmrAddr("w106")+xmrAddr("x")[:11], "sealed"))},
		{"a primary address outside base58", "xmr_escrow_wallets_primary_address_check", wallet(sealed, sealedVals("0"+xmrAddr("w0")[1:], "sealed"))},
		{"an active wallet without its seal", "xmr_escrow_wallets_sealed", wallet("primary_address,restore_height,state", "'"+xmrAddr("w-a")+"',1,'active'")},
		{"a sealed wallet without its address", "xmr_escrow_wallets_sealed", wallet("restore_height,seed_sealed,seed_key_fpr,sealed_at,state", "1,'s',repeat('AB',20),now(),'sealed'")},
		{"a closing wallet without its seal", "xmr_escrow_wallets_sealed", wallet("primary_address,restore_height,state", "'"+xmrAddr("w-c")+"',1,'closing'")},
		{"a retained wallet without its seal", "xmr_escrow_wallets_sealed", wallet("primary_address,restore_height,state", "'"+xmrAddr("w-r")+"',1,'retained'")},
		{"a seal without its key", "xmr_escrow_wallets_seal", wallet("seed_sealed,sealed_at", "'s',now()")},
		{"a lowercase seal key fingerprint", "xmr_escrow_wallets_seed_key_fpr_check", wallet("seed_sealed,seed_key_fpr,sealed_at", "'s',repeat('ab',20),now()")},
		{"destroyed without destroyed_at", "xmr_escrow_wallets_destroyed", wallet("state,last_closed_at,destroy_after", "'destroyed',now()-interval '100 days',now()-interval '10 days'")},
		{"destroyed_at on a retained wallet", "xmr_escrow_wallets_destroyed", wallet(sealed+",destroyed_at", sealedVals(xmrAddr("w-d1"), "retained")+",now()")},
		{"destroyed with its seal", "xmr_escrow_wallets_destroyed", wallet(sealed+",last_closed_at,destroy_after,destroyed_at", sealedVals(xmrAddr("w-d2"), "destroyed")+",now()-interval '100 days',now()-interval '10 days',now()")},
		{"destroyed before destroy_after", "xmr_escrow_wallets_destroyed", wallet("state,last_closed_at,destroy_after,destroyed_at", "'destroyed',now()-interval '100 days',now()+interval '1 day',now()")},
		{"destroyed with no destroy_after", "xmr_escrow_wallets_destroyed", wallet("state,destroyed_at", "'destroyed',now()")},
		{"destroy_after under 90 days after the last close", "xmr_escrow_wallets_retention", wallet("last_closed_at,destroy_after", "now(),now()+interval '89 days'")},
		{"destroy_after with no last close", "xmr_escrow_wallets_retention", wallet("destroy_after", "now()")},
		{"a wallet id of 2^30", "xmr_escrow_wallets_id_check", "INSERT INTO xmr_escrow_wallets(id,name,month) VALUES(1073741824,'escrow-202601-01','2026-01-01')"},
	} {
		xmrRefused(t, e, c.what, c.want, c.q)
	}
	xmrRefused(t, e, "a second wallet with the same primary address", "xmr_escrow_wallets_primary_address_key", wallet(sealed, sealedVals(xmrAddr("wallet-1"), "sealed")))
	// A wallet keeps its name, address, restore height and seal; only destruction removes the seal, and a destroyed
	// wallet never changes.
	for _, c := range []struct{ what, set string }{
		{"a new seal", "seed_sealed='another seal'"},
		{"its seal removed without destruction", "seed_sealed=NULL,state='missing'"},
		{"another seal key", "seed_key_fpr=repeat('CD',20)"},
		{"another seal time", "sealed_at=now()-interval '1 day'"},
		{"another primary address", "primary_address='" + xmrAddr("w-other") + "'"},
		{"another restore height", "restore_height=2"},
		{"another name", "name='escrow-'||to_char(now(),'YYYYMM')||'-08'"},
	} {
		xmrRefused(t, e, "an active wallet given "+c.what, "keeps its name", "UPDATE xmr_escrow_wallets SET "+c.set+" WHERE id=$1", w)
	}
	retained := xmrWallet(t, e, 2, "retained")
	agExec(e, "UPDATE xmr_escrow_wallets SET last_closed_at=now()-interval '100 days',destroy_after=now()-interval '1 day' WHERE id=$1", retained)
	if _, err := e.DB.Exec("UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=now() WHERE id=$1", retained); err != nil {
		t.Fatalf("destroying a wallet past destroy_after: %v", err)
	}
	xmrRefused(t, e, "a destroyed wallet back to retained", "keeps its name", "UPDATE xmr_escrow_wallets SET state='retained',seed_sealed='s',destroyed_at=NULL WHERE id=$1", retained)
	xmrRefused(t, e, "a destroyed wallet touched", "keeps its name", "UPDATE xmr_escrow_wallets SET updated=now() WHERE id=$1", retained)
	// States move only forward, with the missing and orphan moves of P3-R9 and P3-R10.
	for _, to := range []string{"creating", "sealed", "retained", "orphan"} {
		xmrRefused(t, e, "an active wallet moved to "+to, "a wallet moves only forward", "UPDATE xmr_escrow_wallets SET state=$2 WHERE id=$1", w, to)
	}
	walker := xmrWallet(t, e, 4, "creating")
	for _, to := range []string{"sealed", "active", "missing", "active", "closing", "missing", "closing", "retained"} {
		if _, err := e.DB.Exec("UPDATE xmr_escrow_wallets SET state=$2 WHERE id=$1", walker, to); err != nil {
			t.Fatalf("a wallet moved to %s: %v", to, err)
		}
	}
	for _, to := range []string{"active", "closing", "sealed", "creating", "orphan"} {
		xmrRefused(t, e, "a retained wallet moved to "+to, "a wallet moves only forward", "UPDATE xmr_escrow_wallets SET state=$2 WHERE id=$1", walker, to)
	}
	orphan := xmrWallet(t, e, 6, "orphan")
	xmrRefused(t, e, "an orphan wallet made active", "a wallet moves only forward", "UPDATE xmr_escrow_wallets SET state='active' WHERE id=$1", orphan)
	agExec(e, "UPDATE xmr_escrow_wallets SET state='retained' WHERE id=$1", orphan)
	// A creating row whose name create_wallet finds already taken becomes an orphan (X-c, P3-R10), never sealed or used.
	taken := xmrWallet(t, e, 42, "creating")
	agExec(e, "UPDATE xmr_escrow_wallets SET state='orphan' WHERE id=$1", taken)
	xmrRefused(t, e, "an orphan wallet back to creating", "a wallet moves only forward", "UPDATE xmr_escrow_wallets SET state='creating' WHERE id=$1", taken)
	// The last close and the destruction time only move later, and are never cleared.
	agExec(e, "UPDATE xmr_escrow_wallets SET last_closed_at=now()-interval '100 days',destroy_after=now()-interval '1 day' WHERE id=$1", walker)
	for _, c := range []struct{ what, set string }{
		{"an earlier last close", "last_closed_at=now()-interval '101 days'"},
		{"its last close cleared", "last_closed_at=NULL,destroy_after=NULL"},
		{"an earlier destruction time", "destroy_after=now()-interval '2 days'"},
		{"its destruction time cleared", "destroy_after=NULL"},
	} {
		xmrRefused(t, e, "a wallet given "+c.what, "only move later", "UPDATE xmr_escrow_wallets SET "+c.set+" WHERE id=$1", walker)
	}
	if _, err := e.DB.Exec("UPDATE xmr_escrow_wallets SET last_closed_at=now()-interval '50 days',destroy_after=now()+interval '41 days' WHERE id=$1", walker); err != nil {
		t.Fatalf("a later last close and destruction time: %v", err)
	}

	// Slots: account 0 is never a slot; one account and one address per slot; a slot is created free and claimed, for an
	// order, only from an active wallet; a claimed or retired slot never changes.
	sealedWallet := xmrWallet(t, e, 3, "sealed")
	slot := "INSERT INTO xmr_escrow_slots(wallet_id,account_index,address,state,order_id) VALUES($1,$2,$3,$4,$5)"
	free := xmrSlot(t, e, w, 1)
	xmrRefused(t, e, "account 0", "xmr_escrow_slots_account_index_check", slot, w, 0, xmrAddr("s0"), "free", nil)
	xmrRefused(t, e, "account 2^32", "xmr_escrow_slots_account_index_check", slot, w, int64(1)<<32, xmrAddr("s32"), "free", nil)
	xmrRefused(t, e, "a second slot of one account", "xmr_escrow_slots_wallet_id_account_index_key", slot, w, 1, xmrAddr("s1b"), "free", nil)
	xmrRefused(t, e, "a second slot at one address", "xmr_escrow_slots_address_key", slot, w, 2, xmrAddr("slot-"+strconv.FormatInt(w, 10)+"-1"), "free", nil)
	xmrRefused(t, e, "a slot at a malformed address", "xmr_escrow_slots_address_check", slot, w, 2, xmrAddr("s2")[:94], "free", nil)
	pooled := e.order(buyer, product, "XMR", stateAwaitingPayment)
	xmrRefused(t, e, "a slot inserted claimed", "only checkout claims it", slot, w, 2, xmrAddr("s2"), "claimed", pooled)
	xmrRefused(t, e, "a free slot naming an order", "xmr_escrow_slots_claimed", slot, w, 2, xmrAddr("s2"), "free", pooled)
	xmrRefused(t, e, "a slot claimed with no order", "xmr_escrow_slots_claimed", "UPDATE xmr_escrow_slots SET state='claimed' WHERE id=$1", free)
	notActive := xmrSlot(t, e, sealedWallet, 1)
	xmrRefused(t, e, "a slot of a sealed wallet claimed", "only from an active wallet", "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1", notActive, pooled)
	closingSlot := xmrSlot(t, e, xmrWallet(t, e, 5, "closing"), 1)
	xmrRefused(t, e, "a slot of a closing wallet claimed", "only from an active wallet", "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1", closingSlot, pooled)
	xmrRefused(t, e, "a slot deleted", "escrow records are never deleted", "DELETE FROM xmr_escrow_slots WHERE id=$1", free)
	order, addr, claimed := xmrEscrow(t, e, w, 5, buyer, product, stateAwaitingPayment)
	for _, c := range []struct{ what, set string }{
		{"freed", "state='free',order_id=NULL"},
		{"retired", "state='retired',order_id=NULL"},
		{"given to another order", "order_id='" + pooled + "'"},
		{"moved to another account", "account_index=6"},
		{"moved to another address", "address='" + xmrAddr("s-moved") + "'"},
	} {
		xmrRefused(t, e, "a claimed slot "+c.what, "never changes", "UPDATE xmr_escrow_slots SET "+c.set+" WHERE id=$1", claimed)
	}
	agExec(e, "UPDATE xmr_escrow_slots SET state='retired' WHERE id=$1", notActive)
	xmrRefused(t, e, "a retired slot claimed", "never changes", "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1", notActive, pooled)
	xmrRefused(t, e, "a second slot claimed for one order", "xmr_escrow_slots_order_id_key", "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1", free, order)

	// Monero escrows keep their order and slot, and Bitcoin financial rows are refused.
	xmrRefused(t, e, "a new Bitcoin order", "orders_monero_only",
		"INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES($1,$2,$3,'BTC',100000,'awaiting_payment')", randomToken(), buyer, product)
	xmrOrder := e.order(buyer, product, "XMR", stateAwaitingPayment)
	xSlot := xmrSlot(t, e, w, 7)
	xAddr := agStrings(t, e, "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1 RETURNING address", xSlot, xmrOrder)[0]
	esc := "INSERT INTO escrows(order_id,coin,address,market_index,refund_address,fee_bps,fee_destination,xmr_slot_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)"
	xmrRefused(t, e, "a Bitcoin escrow of an XMR order", "the coin of an escrow is its order's currency", esc, xmrOrder, "BTC", "bcrt1qx84xmr", 7, "bcrt1qx84xmrrefund", 0, "", nil)
	xmrRefused(t, e, "a Monero escrow with a market index", "escrows_market_index_btc", esc, xmrOrder, "XMR", xAddr, 99, xmrAddr("r1"), 0, "", xSlot)
	xmrRefused(t, e, "a Monero escrow without a slot", "escrows_xmr_slot_xmr", esc, xmrOrder, "XMR", xAddr, nil, xmrAddr("r1"), 0, "", nil)
	third := e.order(buyer, product, "XMR", stateAwaitingPayment)
	tSlot := xmrSlot(t, e, w, 11)
	tAddr := agStrings(t, e, "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1 RETURNING address", tSlot, third)[0]
	xmrRefused(t, e, "a Monero escrow on another order's slot", `constraint "escrows_xmr_slot"`, esc, xmrOrder, "XMR", tAddr, nil, xmrAddr("r1"), 0, "", tSlot)
	xmrRefused(t, e, "a Monero escrow away from its slot's address", `constraint "escrows_xmr_slot"`, esc, xmrOrder, "XMR", xmrAddr("elsewhere"), nil, xmrAddr("r1"), 0, "", xSlot)
	xmrRefused(t, e, "a Monero escrow refunding to a Bitcoin address", "escrows_xmr_addresses", esc, xmrOrder, "XMR", xAddr, nil, "bcrt1qx84xmrrefund", 0, "", xSlot)
	xmrRefused(t, e, "a Monero escrow's fee to a Bitcoin address", "escrows_xmr_addresses", esc, xmrOrder, "XMR", xAddr, nil, xmrAddr("r1"), 100, "bcrt1qx84fee", xSlot)
	if _, err := e.DB.Exec(esc, xmrOrder, "XMR", xAddr, nil, xmrAddr("r1"), 100, xmrAddr("fee"), xSlot); err != nil {
		t.Fatalf("a Monero escrow with a market fee: %v", err)
	}

	xmrRefused(t, e, "a Monero escrow's coin changed", "keeps its coin", "UPDATE escrows SET coin='BTC' WHERE order_id=$1", order)
	xmrRefused(t, e, "an escrow moved to another order", "keeps its coin", "UPDATE escrows SET order_id=$2 WHERE order_id=$1", order, third)
	xmrRefused(t, e, "the currency of a Monero escrow order changed", "its currency, the escrow's coin, never changes", "UPDATE orders SET currency='BTC' WHERE id=$1", order)
	xmrRefused(t, e, "a draft changed to Bitcoin", "orders_monero_only", "UPDATE orders SET currency='BTC' WHERE id=$1", e.order(buyer, product, "XMR", stateDraft))
	xmrRefused(t, e, "a Monero escrow deleted", "escrow records are never deleted", "DELETE FROM escrows WHERE order_id=$1", order)

	// Each settlements table takes its own coin's escrows only.
	xmrRefused(t, e, "a Bitcoin settlement of a Monero escrow", "has a Monero escrow", "INSERT INTO settlements(order_id,kind) VALUES($1,'refund')", order)

	xs := "INSERT INTO xmr_settlements(order_id,kind,destination,state) VALUES($1,$2,$3,$4)"
	xmrRefused(t, e, "a Monero settlement of an order with no escrow", "has no Monero escrow", xs, pooled, "refund", xmrAddr("d1"), "requested")
	xmrRefused(t, e, "a Monero settlement to a Bitcoin address", "xmr_settlements_destination_check", xs, order, "refund", "bcrt1qx84dest", "requested")
	xmrRefused(t, e, "a Monero settlement to an integrated address", "xmr_settlements_destination_check", xs, order, "refund", xmrAddr("d106")+xmrAddr("y")[:11], "requested")
	xmrRefused(t, e, "a Monero settlement inserted built", "inserted as requested", xs, order, "refund", xmrAddr("d1"), "built")
	xmrRefused(t, e, "a Monero settlement to a 94-character address", "xmr_settlements_destination_check", xs, order, "refund", xmrAddr("d94")[:94], "requested")

	// Building, storing and following a release: the transaction is stored only once the order's state fits the kind
	// (migration 081's function), is frozen once stored, and the settlement moves only forward.
	var rel int64
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3) RETURNING id", order, xmrAddr("claim-1"), vendor).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	storeTx := func(id int64, label string) string {
		return "UPDATE xmr_settlements SET state='built',tx_hash='" + txidOf(label) + "',tx_metadata='ab01',tx_blob='cd02',fee=30000000,input_total=100000," +
			"key_images=ARRAY['" + txidOf(label+"-ki") + "'] WHERE id=" + strconv.FormatInt(id, 10)
	}
	xmrRefused(t, e, "built without a transaction", "xmr_settlements_built", "UPDATE xmr_settlements SET state='built' WHERE id=$1", rel)
	for _, st := range []string{stateAwaitingPayment, statePaid, stateShipped, stateCancelled} {
		agExec(e, "UPDATE orders SET state=$2 WHERE id=$1", order, st)
		xmrRefused(t, e, "a release's transaction stored on an order in state "+st, "only once its order's state fits its kind", storeTx(rel, "rel"))
	}
	agExec(e, "UPDATE orders SET state='disputed' WHERE id=$1", order)
	agExec(e, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The licence key does not work at all.')", randomToken(), order)
	agExec(e, "UPDATE disputes SET outcome='refund' WHERE order_id=$1", order)
	agExec(e, "UPDATE orders SET state='resolved' WHERE id=$1", order)
	xmrRefused(t, e, "a release's transaction stored on an order decided for the buyer", "only once its order's state fits its kind", storeTx(rel, "rel"))
	agExec(e, "UPDATE disputes SET outcome='release' WHERE order_id=$1", order)
	xmrRefused(t, e, "a transaction without its key images", "xmr_settlements_tx", "UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab',tx_blob='cd',fee=1,input_total=2 WHERE id=$1", rel, txidOf("nk"))
	xmrRefused(t, e, "a transaction with no key images", "xmr_settlements_key_images_check", strings.Replace(storeTx(rel, "rel"), "ARRAY['"+txidOf("rel-ki")+"']", "'{}'", 1))
	xmrRefused(t, e, "a transaction with a malformed key image", "xmr_settlements_key_images_check", strings.Replace(storeTx(rel, "rel"), txidOf("rel-ki"), "abc", 1))
	xmrRefused(t, e, "a transaction stored as requested", "only when it is built from requested", strings.Replace(storeTx(rel, "rel"), "state='built'", "state='requested'", 1))
	xmrRefused(t, e, "a settlement inserted with a transaction", "xmr_settlements_unbuilt", `INSERT INTO xmr_settlements(order_id,kind,destination,tx_hash,tx_metadata,tx_blob,fee,input_total,key_images)
		VALUES($1,'stray_refund',$2,$3,'ab','cd',1,2,ARRAY[$3])`, order, xmrAddr("refund-"+order), txidOf("ins"))
	if _, err := e.DB.Exec(storeTx(rel, "rel")); err != nil {
		t.Fatalf("a release's transaction stored on an order decided for the vendor: %v", err)
	}
	for _, c := range []struct{ what, set string }{
		{"its transaction hash", "tx_hash='" + txidOf("rel-2") + "'"},
		{"its metadata", "tx_metadata='ab02'"},
		{"its blob", "tx_blob='cd03'"},
		{"its fee", "fee=40000000"},
		{"its input total", "input_total=200000"},
		{"its key images", "key_images=ARRAY['" + txidOf("other-ki") + "']"},
		{"its transaction removed", "tx_hash=NULL,tx_metadata=NULL,tx_blob=NULL,fee=NULL,input_total=NULL,key_images=NULL,state='failed'"},
	} {
		xmrRefused(t, e, "a stored transaction given "+c.what, "never rebuilt or changed", "UPDATE xmr_settlements SET "+c.set+" WHERE id=$1", rel)
	}
	xmrRefused(t, e, "another destination", "keeps its order, kind and destination", "UPDATE xmr_settlements SET destination=$2 WHERE id=$1", rel, xmrAddr("claim-x"))
	xmrRefused(t, e, "another kind", "keeps its order, kind and destination", "UPDATE xmr_settlements SET kind='refund' WHERE id=$1", rel)
	xmrRefused(t, e, "built back to requested", "cannot move from built to requested", "UPDATE xmr_settlements SET state='requested' WHERE id=$1", rel)
	xmrRefused(t, e, "built to superseded", "cannot move from built to superseded", "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", rel)
	xmrRefused(t, e, "built to confirmed, skipping relayed", "cannot move from built to confirmed", "UPDATE xmr_settlements SET state='confirmed',relayed_at=now(),confirmed_at=now() WHERE id=$1", rel)
	xmrRefused(t, e, "relayed without relayed_at", "xmr_settlements_relayed", "UPDATE xmr_settlements SET state='relayed' WHERE id=$1", rel)
	for _, step := range []string{"state='relayed',relayed_at=now()", "state='confirmed',confirmed_at=now()", "state='relayed'", "state='confirmed'", "reorg_safe_at=now()"} {
		if _, err := e.DB.Exec("UPDATE xmr_settlements SET "+step+" WHERE id=$1", rel); err != nil {
			t.Fatalf("release %s (forward, or back to relayed after a reorganisation): %v", step, err)
		}
		if step == "state='relayed',relayed_at=now()" {
			xmrRefused(t, e, "relayed back to built", "cannot move from relayed to built", "UPDATE xmr_settlements SET state='built' WHERE id=$1", rel)
			xmrRefused(t, e, "relayed to superseded", "cannot move from relayed to superseded", "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", rel)
		}
	}
	xmrRefused(t, e, "confirmed to superseded", "cannot move from confirmed to superseded", "UPDATE xmr_settlements SET state='superseded',reorg_safe_at=NULL WHERE id=$1", rel)
	xmrRefused(t, e, "reorg_safe_at on a relayed settlement", "xmr_settlements_reorg_safe", "UPDATE xmr_settlements SET state='relayed' WHERE id=$1", rel)
	xmrRefused(t, e, "confirmed back to built", "cannot move from confirmed to built", "UPDATE xmr_settlements SET state='built' WHERE id=$1", rel)
	agExec(e, "UPDATE xmr_settlements SET state='external',reorg_safe_at=NULL WHERE id=$1", rel)
	for _, st := range []string{"relayed", "confirmed", "failed", "requested", "superseded"} {
		xmrRefused(t, e, "external to "+st, "cannot move from external", "UPDATE xmr_settlements SET state='"+st+"' WHERE id=$1", rel)
	}
	xmrRefused(t, e, "a settlement deleted", "escrow records are never deleted", "DELETE FROM xmr_settlements WHERE id=$1", rel)

	// One live release or refund per order, one stray refund in progress, one built transaction not yet relayed, and a
	// release destination paid once.
	xmrRefused(t, e, "a second release", "xmr_settlements_one_outcome", xs, order, "release", xmrAddr("claim-2"), "requested")
	xmrRefused(t, e, "a refund beside the release", "xmr_settlements_one_outcome", xs, order, "refund", xmrAddr("d1"), "requested")
	done, _, _ := xmrEscrow(t, e, w, 9, buyer, product, stateCompleted)
	var claim int64
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2) RETURNING id", done, xmrAddr("claim-3")).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	other, _, _ := xmrEscrow(t, e, w, 10, buyer, product, stateCompleted)
	xmrRefused(t, e, "the same vendor's release to the same destination", "escrow_external_addresses_pkey", xs, other, "release", xmrAddr("claim-3"), "requested")
	var stray int64
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'stray_refund',$2) RETURNING id", done, xmrAddr("refund-"+done)).Scan(&stray); err != nil {
		t.Fatal(err)
	}
	xmrRefused(t, e, "a second stray refund in progress", "xmr_settlements_one_open_stray", xs, done, "stray_refund", xmrAddr("refund-"+done), "requested")
	xmrRefused(t, e, "a stray refund superseded", "xmr_settlements_superseded", "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", stray)
	agExec(e, storeTx(claim, "claim"))
	xmrRefused(t, e, "a second built transaction of one order", "xmr_settlements_one_built", storeTx(stray, "stray"))
	agExec(e, "UPDATE xmr_settlements SET state='relayed',relayed_at=now() WHERE id=$1", claim)
	if _, err := e.DB.Exec(storeTx(stray, "stray")); err != nil {
		t.Fatalf("the stray refund built once the release was relayed: %v", err)
	}

	// A changed decision supersedes an unbuilt release; the superseded row has no transaction and never comes back.
	var refund int64
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'refund',$2) RETURNING id", other, xmrAddr("refund-"+other)).Scan(&refund); err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", refund)
	if _, err := e.DB.Exec(xs, other, "release", xmrAddr("claim-4"), "requested"); err != nil {
		t.Fatalf("a release once the refund was superseded: %v", err)
	}
	xmrRefused(t, e, "superseded back to requested", "cannot move from superseded to requested", "UPDATE xmr_settlements SET state='requested' WHERE id=$1", refund)
	cancelled, _, _ := xmrEscrow(t, e, w, 12, buyer, product, stateCancelled)
	var dropped int64
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'refund',$2) RETURNING id", cancelled, xmrAddr("refund-"+cancelled)).Scan(&dropped); err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", dropped)
	xmrRefused(t, e, "a superseded settlement's transaction stored", "only when it is built from requested", strings.Replace(storeTx(dropped, "sup"), "state='built',", "", 1))
	// A transaction is stored only by the move requested -> built, never on a row that failed or became external unbuilt.
	for _, st := range []string{"failed", "external"} {
		var unbuilt int64
		if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'stray_refund',$2) RETURNING id", cancelled, xmrAddr("refund-"+cancelled)).Scan(&unbuilt); err != nil {
			t.Fatal(err)
		}
		agExec(e, "UPDATE xmr_settlements SET state=$2 WHERE id=$1", unbuilt, st)
		xmrRefused(t, e, "a transaction stored on an unbuilt "+st+" settlement", "only when it is built from requested", strings.Replace(storeTx(unbuilt, "unbuilt-"+st), "state='built',", "", 1))
		xmrRefused(t, e, "an unbuilt "+st+" settlement superseded", "cannot move from "+st+" to superseded", "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", unbuilt)
	}

	// The administrator's re-request (O-1, A-401): a built settlement that failed is superseded, keeping its stored
	// transaction, and in the same transaction a new row names it in supersedes, with the same order, kind and
	// destination; the new row's transaction spends exactly the stored key images (in any order). No row is superseded
	// twice, and only a superseded row that was built is re-requested.
	admin, _ := e.user("x84_admin", "admin")
	again, _, _ := xmrEscrow(t, e, w, 13, buyer, product, stateCompleted)
	keyImages := func(labels ...string) string {
		var q []string
		for _, l := range labels {
			q = append(q, "'"+txidOf(l)+"'")
		}
		return "ARRAY[" + strings.Join(q, ",") + "]"
	}
	build := func(id int64, label, images string) string {
		return "UPDATE xmr_settlements SET state='built',tx_hash='" + txidOf(label) + "',tx_metadata='ab01',tx_blob='cd01',fee=30000000,input_total=100000,key_images=" +
			images + " WHERE id=" + strconv.FormatInt(id, 10)
	}
	var first int64
	if err := e.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3) RETURNING id", again, xmrAddr("claim-6"), vendor).Scan(&first); err != nil {
		t.Fatal(err)
	}
	agExec(e, build(first, "again-1", keyImages("ki-1", "ki-2")))
	agExec(e, "UPDATE xmr_settlements SET state='failed' WHERE id=$1", first)
	reRequest := "INSERT INTO xmr_settlements(order_id,kind,destination,requested_by,supersedes) VALUES($1,$2,$3,$4,$5)"
	xmrRefused(t, e, "a re-request of a settlement not superseded", "keeps its order, kind and destination", reRequest, again, "release", xmrAddr("claim-6"), admin, first)
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var second int64
	if _, err = tx.Exec("UPDATE xmr_settlements SET state='superseded' WHERE id=$1", first); err == nil {
		err = tx.QueryRow(reRequest+" RETURNING id", again, "release", xmrAddr("claim-6"), admin, first).Scan(&second)
	}
	if err != nil {
		tx.Rollback()
		t.Fatalf("a failed built release superseded and re-requested in one transaction: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	xmrRefused(t, e, "a re-request of an unbuilt superseded settlement", "keeps its order, kind and destination", reRequest, other, "refund", xmrAddr("refund-"+other), admin, refund)
	xmrRefused(t, e, "a re-request that names another settlement", "keeps its order, kind and destination", "UPDATE xmr_settlements SET supersedes=NULL WHERE id=$1", second)
	xmrRefused(t, e, "a re-request spending other key images", "spends exactly its key images", build(second, "again-2", keyImages("ki-1", "ki-3")))
	xmrRefused(t, e, "a re-request spending some of the key images", "spends exactly its key images", build(second, "again-2", keyImages("ki-1")))
	xmrRefused(t, e, "a re-request spending more key images", "spends exactly its key images", build(second, "again-2", keyImages("ki-1", "ki-2", "ki-3")))
	if _, err = e.DB.Exec(build(second, "again-2", keyImages("ki-2", "ki-1"))); err != nil {
		t.Fatalf("the re-request built with the stored key images: %v", err)
	}
	agExec(e, "UPDATE xmr_settlements SET state='failed' WHERE id=$1", second)
	agExec(e, "UPDATE xmr_settlements SET state='superseded' WHERE id=$1", second)
	xmrRefused(t, e, "a re-request to another destination", "keeps its order, kind and destination", reRequest, again, "release", xmrAddr("claim-7"), admin, second)
	xmrRefused(t, e, "a re-request of another kind", "keeps its order, kind and destination", reRequest, again, "stray_refund", xmrAddr("claim-6"), admin, second)
	xmrRefused(t, e, "a second re-request of one settlement", "xmr_settlements_supersedes_key", reRequest, again, "release", xmrAddr("claim-6"), admin, first)
	if _, err = e.DB.Exec(reRequest, again, "release", xmrAddr("claim-6"), admin, second); err != nil {
		t.Fatalf("a re-request of the re-request: %v", err)
	}

	// Outputs are written while their settlement is requested, then never changed.
	var plan int64
	if err := e.DB.QueryRow("SELECT id FROM xmr_settlements WHERE order_id=$1 AND kind='release'", other).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	out := "INSERT INTO xmr_settlement_outputs(settlement_id,n,role,address,amount) VALUES($1,$2,$3,$4,$5)"
	if _, err := e.DB.Exec(out, plan, 0, "claimant", xmrAddr("claim-4"), 90000); err != nil {
		t.Fatalf("a requested release's output: %v", err)
	}
	xmrRefused(t, e, "an output to a Bitcoin address", "xmr_settlement_outputs_address_check", out, plan, 1, "market_fee", "bcrt1qx84fee", 1000)
	xmrRefused(t, e, "a second claimant output", "xmr_settlement_outputs_settlement_id_role_key", out, plan, 1, "claimant", xmrAddr("claim-5"), 1000)
	xmrRefused(t, e, "an output of zero", "xmr_settlement_outputs_amount_check", out, plan, 1, "market_fee", xmrAddr("fee"), 0)
	xmrRefused(t, e, "an output changed", "written only while", "UPDATE xmr_settlement_outputs SET amount=1 WHERE settlement_id=$1", plan)
	xmrRefused(t, e, "an output removed", "written only while", "DELETE FROM xmr_settlement_outputs WHERE settlement_id=$1", plan)
	xmrRefused(t, e, "an output of a built settlement", "written only while", out, claim, 1, "market_fee", xmrAddr("fee"), 1000)
	xmrRefused(t, e, "a stored transaction recorded twice", "xmr_settlements_tx_hash_key", storeTx(plan, "claim"))

	// payments: a Monero settlement claims only Monero escrow rows, and an escrow-range idx names its own order's account.
	agExec(e, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,$3,$4,100000,12)", order, txidOf("x84-dep"), xmrEscrowIdx(w, 5), addr)
	xmrRefused(t, e, "a Bitcoin payment", "payments_monero_only", "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC',$2,0,'retired-btc-address',100000,3)", order, txidOf("x84-btc"))
	if _, err := e.DB.Exec("UPDATE payments SET xmr_settlement_id=$2 WHERE order_id=$1", order, rel); err != nil {
		t.Fatalf("a Monero escrow row claimed by its settlement: %v", err)
	}
	dep := "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,$3,$4,100000,12)"
	xmrRefused(t, e, "a deposit naming another order's account", "escrow account; it is never recorded", dep, done, txidOf("x84-2"), xmrEscrowIdx(w, 5), addr)
	xmrRefused(t, e, "a deposit naming another wallet's account", "escrow account; it is never recorded", dep, order, txidOf("x84-3"), xmrEscrowIdx(sealedWallet, 5), addr)
	xmrRefused(t, e, "a deposit to a pooled order in the escrow range", "escrow account; it is never recorded", dep, pooled, txidOf("x84-4"), xmrEscrowIdx(w, 1), xmrAddr("p"))
	xmrRefused(t, e, "a deposit moved to another order", "escrow account; it is never recorded", "UPDATE payments SET order_id=$2 WHERE order_id=$1 AND currency='XMR'", order, done)
	xmrRefused(t, e, "a Monero escrow deposit keyed on its minor index", "escrow account; it is never recorded", dep, order, txidOf("x84-5"), 0, addr)
	if _, err := e.DB.Exec(dep, pooled, txidOf("x84-5"), 0, xmrAddr("p")); err != nil {
		t.Fatalf("a pooled Monero deposit at minor index 0 of the same transaction: %v", err)
	}
}

// Migration 074's payouts_not_escrow keys on escrows, so it covers a Monero escrow order too (ES-1, invariant 4): no
// payouts row is written for one and none is moved to one. Migration 108 refuses every new payouts row (A-492), so 074
// is checked beneath it with 108's triggers off, rolled back.
func TestXMREscrowPayoutRefused(t *testing.T) {
	e := newTestApp(t)
	buyer, _ := e.user("xpay_buyer", "buyer")
	vendor, _ := e.user("xpay_vendor", "vendor")
	product := e.product(vendor, "digital")
	w := xmrWallet(t, e, 1, "active")
	order, _, _ := xmrEscrow(t, e, w, 1, buyer, product, stateCompleted)
	payout := "INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'XMR',100000,'historical-vendor','pending')"
	historical := e.order(buyer, product, "XMR", stateCompleted)
	xmrRefused(t, e, "a payout of an order without escrow", "takes no new rows", payout, historical, vendor)
	withPooledHistoryWritable(t, e, func(tx *sql.Tx) {
		if _, err := tx.Exec(payout, order, vendor); err == nil || !strings.Contains(err.Error(), "is an escrow order") {
			t.Errorf("a payout of a Monero escrow order, 108 off: %v", err)
		}
	})
	withPooledHistoryWritable(t, e, func(tx *sql.Tx) {
		if _, err := tx.Exec(payout, historical, vendor); err != nil {
			t.Fatalf("a payout of an order without escrow, 108 off: %v", err)
		}
		if _, err := tx.Exec("UPDATE payouts SET order_id=$2 WHERE order_id=$1", historical, order); err == nil || !strings.Contains(err.Error(), "is an escrow order") {
			t.Errorf("a payout moved to a Monero escrow order, 108 off: %v", err)
		}
	})
}

// The payments idx of a Monero escrow deposit (2^62 + wallet_id·2^32 + account_index) never meets a pooled Monero
// deposit's (its subaddress minor index), so one transaction paying a pooled order and two escrow accounts is three
// ledger rows, each its own order's, written by the ledger writer both watchers use. A deposit recorded for another
// order's account is refused, and the other order's row is left as it was (never merged); so is an escrow deposit keyed
// on its minor index, under which two accounts' outputs of one transaction (each to its account's address 0) would be
// one row.
func TestXMREscrowIdxNoCollision(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	buyer, _ := e.user("xidx_buyer", "buyer")
	vendor, _ := e.user("xidx_vendor", "vendor")
	product := e.product(vendor, "digital")
	w := xmrWallet(t, e, 1, "active")
	a, addrA, _ := xmrEscrow(t, e, w, 1, buyer, product, stateAwaitingPayment)
	b, addrB, _ := xmrEscrow(t, e, w, 2, buyer, product, stateAwaitingPayment)
	pooled := e.order(buyer, product, "XMR", stateAwaitingPayment)
	pooledAddr := xmrAddr("pooled-" + pooled)
	tx := txidOf("xidx-one-tx")
	// The pooled watcher writes the pooled output (minor index 1), the escrow watcher the two escrow accounts' outputs.
	if err := e.A.applyIncoming(ctx, "XMR", []Incoming{{Address: pooledAddr, TxID: tx, Index: 1, Amount: 100000, Confirmations: 1}},
		[]string{pooledAddr}, map[string]string{pooledAddr: pooled}); err != nil {
		t.Fatal(err)
	}
	escrowOf := map[string]string{addrA: a, addrB: b}
	escrowIn := []Incoming{{Address: addrA, TxID: tx, Index: xmrEscrowIdx(w, 1), Amount: 100000, Confirmations: 1},
		{Address: addrB, TxID: tx, Index: xmrEscrowIdx(w, 2), Amount: 100000, Confirmations: 1}}
	if err := e.A.applyIncoming(ctx, "XMR", escrowIn, []string{addrA, addrB}, escrowOf); err != nil {
		t.Fatalf("two escrow accounts and a pooled subaddress in one transaction: %v", err)
	}
	rows := func() string {
		return strings.Join(agStrings(t, e, "SELECT order_id||':'||idx||':'||confirmations FROM payments WHERE txid=$1 ORDER BY idx", tx), " ")
	}
	want := pooled + ":1:1 " + a + ":" + strconv.FormatInt(xmrEscrowIdx(w, 1), 10) + ":1 " + b + ":" + strconv.FormatInt(xmrEscrowIdx(w, 2), 10) + ":1"
	if got := rows(); got != want {
		t.Fatalf("ledger rows of one transaction:\n got %s\nwant %s", got, want)
	}
	// Seen again with more confirmations: each row is updated in place, never merged or duplicated.
	escrowIn[0].Confirmations, escrowIn[1].Confirmations = 3, 3
	if err := e.A.applyIncoming(ctx, "XMR", escrowIn, []string{addrA, addrB}, escrowOf); err != nil {
		t.Fatal(err)
	}
	if got := rows(); got != pooled+":1:1 "+a+":"+strconv.FormatInt(xmrEscrowIdx(w, 1), 10)+":3 "+b+":"+strconv.FormatInt(xmrEscrowIdx(w, 2), 10)+":3" {
		t.Fatalf("ledger rows after a second read: %s", got)
	}
	// An escrow output read for the wrong order (account 1's output mapped to order b) is refused, naming the rule, and
	// order a's row is left as it was.
	before := rows()
	err := e.A.applyIncoming(ctx, "XMR", []Incoming{{Address: addrB, TxID: tx, Index: xmrEscrowIdx(w, 1), Amount: 100000, Confirmations: 9}},
		[]string{addrB}, map[string]string{addrB: b})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.ConstraintName != "payments_xmr_escrow_idx" {
		t.Fatalf("an escrow output recorded for another order: %v", err)
	}
	if got := rows(); got != before {
		t.Fatalf("a refused collision changed the ledger:\n got %s\nwant %s", got, before)
	}
	// Keyed on the minor index instead (0: every escrow address is its account's address 0), the second account's
	// output would update the first's row; the database refuses an escrow order's row under any key but its account's.
	err = e.A.applyIncoming(ctx, "XMR", []Incoming{{Address: addrA, TxID: txidOf("xidx-minor"), Index: 0, Amount: 100000, Confirmations: 1}},
		[]string{addrA}, map[string]string{addrA: a})
	if !errors.As(err, &pe) || pe.ConstraintName != "payments_xmr_escrow_idx" {
		t.Fatalf("an escrow output keyed on its minor index: %v", err)
	}
}

// Migration 084 on a database that already holds Phase 1 and Phase 2 rows (Bitcoin escrows, digital and physical, with
// keys, settlements in every final state, outputs, signatures, a fee bump and claimed payments): it applies, every row
// reads back unchanged (each escrow is coin 'BTC' with no slot), and the Bitcoin rules still hold.
func TestXMREscrowMigration084Upgrade(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var through117 []migration
	for _, m := range list {
		if m.Version < 118 {
			through117 = append(through117, m)
		}
	}
	list = through117 // Historical084 behavior is tested before the Monero-only118 migration.
	var upTo083 []migration
	for _, m := range list {
		if m.Version < 84 {
			upTo083 = append(upTo083, m)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, upTo083)
	})
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role) VALUES('ub','up_buyer','x','buyer'),('uv','up_vendor','x','vendor')",
		"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,dispatch_days) VALUES('pd','uv','D','D','Hardware','Worldwide','digital',100000,1,5,NULL),('pp','uv','P','P','Hardware','Worldwide','physical',100000,1,5,3)",
		`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('o1','ub','pd','BTC',100000,'completed'),('o2','ub','pp','BTC',100000,'shipped'),
			('o3','ub','pd','BTC',100000,'cancelled'),('o4','ub','pd','XMR',100000,'completed')`,
		"INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at) VALUES('ub','s',repeat('a',64),'a1b2c3d4','/48h/1h/0h/2h',now()),('uv','s',repeat('b',64),'b1b2c3d4','/48h/1h/0h/2h',now())",
		`INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination,release_days,dispatch_days,ship_by,release_at,recover_requested) VALUES
			('o1','bcrt1qupescrow1',nextval('escrow_market_index'),'bcrt1quprefund1',250,'bcrt1qupfee',NULL,NULL,NULL,NULL,now()),
			('o2','bcrt1qupescrow2',nextval('escrow_market_index'),'bcrt1quprefund2',0,'',14,3,now()-interval '2 days',now()+interval '12 days',NULL),
			('o3','bcrt1qupescrow3',nextval('escrow_market_index'),'bcrt1quprefund3',0,'',NULL,NULL,NULL,NULL,NULL)`,
		`INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix)
			SELECT '02'||repeat('1'||id::text,32),id,'buyer','ub',repeat('a',64),'a1b2c3d4','/48h/1h/0h/2h','/0/'||id FROM escrows`,
		`INSERT INTO settlements(order_id,kind,state,destination,fee_rate,network_fee,input_total,psbt,txid,signer,signed_at,raw_tx,cosigned,final_at,broadcast_at,confirmed_at,reorg_safe_at) VALUES
			('o1','release','confirmed','bcrt1qupclaim1',2,300,100000,'cHNidP8',repeat('e1',32),'vendor',now(),'0200','market',now(),now(),now(),now()),
			('o2','release','building','bcrt1qupclaim2',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL),
			('o3','refund','broadcast','bcrt1quprefund3',2,300,100000,'cHNidP8',repeat('e3',32),'buyer',now(),'0201','market',now(),now(),NULL,NULL)`,
		"INSERT INTO settlement_outputs(settlement_id,n,role,address,sats) SELECT id,0,'claimant',destination,99700 FROM settlements WHERE state='building'",
		"UPDATE settlements SET state='awaiting_signature',psbt='cHNidP8',txid=repeat('e2',32),fee_rate=2,network_fee=300,input_total=100000 WHERE state='building'",
		"INSERT INTO settlement_bumps(settlement_id,n,requested_by,fee_rate) SELECT id,1,'ub',5 FROM settlements WHERE order_id='o3'",
		`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited,settlement_id) SELECT s.order_id,'BTC',repeat('d'||right(s.order_id,1),32),0,es.address,100000,6,true,s.id
			FROM settlements s JOIN escrows es ON es.order_id=s.order_id`,
		"INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES('o4','XMR',repeat('f4',32),3,'pooled-xmr-subaddress',100000,12)",
		"INSERT INTO escrow_orphans(address,order_id,received) VALUES('bcrt1quporphan','',5000)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dump := func() string {
		t.Helper()
		var out []string
		for _, q := range []string{
			"SELECT to_jsonb(es) - 'coin' - 'xmr_slot_id' - 'fee_descriptor' - 'fee_index' FROM escrows es ORDER BY id", // and the columns 109 adds
			"SELECT to_jsonb(p) - 'xmr_settlement_id' - 'escrow_too_few_by' FROM payments p ORDER BY id",                // the columns 084 and 087 add
			"SELECT to_jsonb(t) FROM settlements t ORDER BY id",
			"SELECT to_jsonb(t) FROM settlement_outputs t ORDER BY settlement_id,n",
			"SELECT to_jsonb(t) FROM settlement_bumps t ORDER BY id",
			"SELECT to_jsonb(t) FROM escrow_pubkeys t ORDER BY pubkey",
			"SELECT to_jsonb(t) - 'listing_title' - 'listing_description' - 'listing_kind' - 'financial_closed_at' - 'financial_checked_at' - 'financial_retirement_checked_at' - 'private_expired_at' FROM orders t ORDER BY id", // columns added after the Phase 1/2 rows
			"SELECT to_jsonb(t) FROM escrow_orphans t",
		} {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				if err = rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				out = append(out, s)
			}
			rows.Close()
		}
		return strings.Join(out, "\n")
	}
	before := dump()
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, list) })
	var inventedSnapshots int
	if err = db.QueryRow("SELECT count(*) FROM orders WHERE listing_title IS NOT NULL OR listing_description IS NOT NULL OR listing_kind IS NOT NULL").Scan(&inventedSnapshots); err != nil || inventedSnapshots != 0 {
		t.Fatalf("historical listing terms were invented during upgrade: %d %v", inventedSnapshots, err)
	}
	var inventedRetention int
	if err = db.QueryRow("SELECT count(*) FROM orders WHERE financial_closed_at IS NOT NULL OR financial_checked_at IS NOT NULL OR financial_retirement_checked_at IS NOT NULL OR private_expired_at IS NOT NULL").Scan(&inventedRetention); err != nil || inventedRetention != 0 {
		t.Fatalf("historical financial closure or expiry was invented during upgrade: %d %v", inventedRetention, err)
	}
	if after := dump(); after != before {
		t.Fatalf("migration 084 changed Phase 1 and 2 rows:\nbefore\n%s\nafter\n%s", before, after)
	}
	var coins string
	if err = db.QueryRow("SELECT string_agg(coin||':'||(xmr_slot_id IS NULL)||':'||(market_index IS NOT NULL),',' ORDER BY id) FROM escrows").Scan(&coins); err != nil || coins != "BTC:true:true,BTC:true:true,BTC:true:true" {
		t.Fatalf("existing escrows after 084: %q %v", coins, err)
	}
	// The Bitcoin rules on the existing rows are unchanged: a final transaction never changes, a confirmed settlement
	// may go back to broadcast after a reorganisation, an awaiting one is party-signed, and a new escrow takes the next
	// market index.
	for _, c := range []struct {
		q  string
		ok bool
	}{
		{"UPDATE settlements SET raw_tx='0299' WHERE order_id='o1'", false},
		{"UPDATE settlements SET state='broadcast',reorg_safe_at=NULL WHERE order_id='o1'", true},
		{"UPDATE settlements SET state='party_signed',signer='vendor',signed_at=now() WHERE order_id='o2'", true},
		{"UPDATE escrows SET release_at=now() WHERE order_id='o2'", false},
		{"INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('o5','ub','pd','BTC',100000,'awaiting_payment')", true},
		{"INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES('o5','bcrt1qupescrow5',nextval('escrow_market_index'),'bcrt1quprefund5',0,'')", true},
	} {
		if _, err := db.Exec(c.q); (err == nil) != c.ok {
			t.Errorf("%s: %v (want accepted %v)", c.q, err, c.ok)
		}
	}
}

// The resolve form's statement of an escrow dispute's outcomes (escrowDisputePreview, on the moderation desk) states a
// Monero escrow's amount in XMR with its 12 decimals, never as Bitcoin. ESCROW_XMR is off here, so no network is known
// and none is named (A-472; with one known the amount names it: TestXMREscrowDisputeAndAdminNameNetwork).
func TestXMREscrowDisputePreview(t *testing.T) {
	e := newTestApp(t)
	buyer, _ := e.user("xdp_buyer", "buyer")
	vendor, _ := e.user("xdp_vendor", "vendor")
	_, adminSess := e.user("xdp_admin", "admin")
	w := xmrWallet(t, e, 1, "active")
	order, addr, _ := xmrEscrow(t, e, w, 1, buyer, e.product(vendor, "digital"), stateDisputed)
	agExec(e, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The licence key does not work at all.')", randomToken(), order)
	agExec(e, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,$3,$4,1234567890123,12)", order, txidOf("xdp"), xmrEscrowIdx(w, 1), addr)
	o, err := scanOrder(e.DB.QueryRow(orderQuery+" WHERE o.id=$1", order))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.A.escrowDisputePreview(context.Background(), &o)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{p.Release, p.Refund} {
		if strings.Contains(text, "BTC") || !strings.Contains(text, "1.234567890123 XMR") {
			t.Errorf("dispute preview of a Monero escrow order: %q", text)
		}
	}
	if body := e.do("GET", "/moderator", adminSess, nil).Body.String(); !strings.Contains(body, "1.234567890123 XMR") {
		t.Error("the moderation desk does not state the Monero escrow's amount in XMR")
	}
}

// Migration 086, by SQL (A-419): xmr_escrow_wallets.destroy_started_at is set once, only on a retained wallet past
// destroy_after (at a time no earlier than it), never on insert; once set it never changes or clears, and the wallet
// moves only to destroyed or missing, or from missing back to retained (it opened again with the market's password).
func TestXMREscrowMigration086(t *testing.T) {
	e := newTestApp(t)
	const started = "starts its destruction only while retained past destroy_after"
	const kept = "keeps the start of its destruction and moves only to destroyed or missing, or from missing back to retained"
	const start = "UPDATE xmr_escrow_wallets SET destroy_started_at=now() WHERE id=$1"
	const pastDue = "UPDATE xmr_escrow_wallets SET last_closed_at=now()-interval '100 days',destroy_after=now()-interval '1 day' WHERE id=$1"
	xmrRefused(t, e, "a wallet inserted with its destruction started", "inserted with no destruction started",
		"INSERT INTO xmr_escrow_wallets(name,month,destroy_started_at) VALUES('escrow-'||to_char(now(),'YYYYMM')||'-09',date_trunc('month',now())::date,now())")
	for n, state := range []string{"sealed", "active", "closing", "orphan"} {
		xmrRefused(t, e, "started on a wallet "+state, started, start, xmrWallet(t, e, n+1, state))
	}
	retained := xmrWallet(t, e, 5, "retained")
	xmrRefused(t, e, "started with no destroy_after", started, start, retained)
	agExec(e, pastDue, retained)
	xmrRefused(t, e, "started at a time before destroy_after", started, "UPDATE xmr_escrow_wallets SET destroy_started_at=now()-interval '2 days' WHERE id=$1", retained)
	xmrRefused(t, e, "started while moving to missing", started, "UPDATE xmr_escrow_wallets SET state='missing',destroy_started_at=now() WHERE id=$1", retained)
	missing := xmrWallet(t, e, 6, "retained")
	agExec(e, pastDue, missing)
	agExec(e, "UPDATE xmr_escrow_wallets SET state='missing' WHERE id=$1", missing)
	xmrRefused(t, e, "started on a missing wallet", started, start, missing)

	agExec(e, start, retained)
	agExec(e, "UPDATE xmr_escrow_wallets SET updated=now() WHERE id=$1", retained)
	xmrRefused(t, e, "its start moved", kept, "UPDATE xmr_escrow_wallets SET destroy_started_at=destroy_started_at+interval '1 second' WHERE id=$1", retained)
	xmrRefused(t, e, "its start cleared", kept, "UPDATE xmr_escrow_wallets SET destroy_started_at=NULL WHERE id=$1", retained)
	agExec(e, "UPDATE xmr_escrow_wallets SET state='missing' WHERE id=$1", retained)
	for _, to := range []string{"sealed", "active", "closing"} {
		xmrRefused(t, e, "a missing wallet whose destruction started moved to "+to, kept, "UPDATE xmr_escrow_wallets SET state=$2 WHERE id=$1", retained, to)
	}
	xmrRefused(t, e, "back to retained with its start cleared", kept, "UPDATE xmr_escrow_wallets SET state='retained',destroy_started_at=NULL WHERE id=$1", retained)
	agExec(e, "UPDATE xmr_escrow_wallets SET state='retained' WHERE id=$1", retained)
	agExec(e, "UPDATE xmr_escrow_wallets SET state='missing' WHERE id=$1", retained)
	agExec(e, "UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=now() WHERE id=$1", retained)
	direct := xmrWallet(t, e, 7, "retained")
	agExec(e, pastDue, direct)
	agExec(e, start, direct)
	agExec(e, "UPDATE xmr_escrow_wallets SET state='destroyed',seed_sealed=NULL,destroyed_at=now() WHERE id=$1", direct)
	if n := agInt(e, "SELECT count(*) FROM xmr_escrow_wallets WHERE id IN ($1,$2) AND state='destroyed' AND destroy_started_at IS NOT NULL", retained, direct); n != 2 {
		t.Fatalf("%d wallets destroyed with their destruction's start kept", n)
	}
}
