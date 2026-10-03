package market

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Escrow Phase 3, slice X-a (WR-escrow A-248, P3-R1): migration 084's Monero escrow schema, and Bitcoin escrow code
// that never touches a Monero escrow's rows. DB integration tests: no Monero wallet runs here, so the Monero rows are
// written by SQL as the later slices' watcher and checkout will write them; the Bitcoin escrow runs against the
// simulated Core of escrow_fake_test.go (simulated RPC, not chain evidence).

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

	// Escrows: the coin is the order's currency and never changes; a Bitcoin escrow has a market index and no slot, a
	// Monero escrow a slot (its own order's, at its address) and Monero refund and fee addresses.
	btcOrder := e.order(buyer, product, "BTC", stateAwaitingPayment)
	var btcEscrow int64
	if err := e.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qx84escrow',nextval('escrow_market_index'),'bcrt1qx84refund',0,'') RETURNING id", btcOrder).Scan(&btcEscrow); err != nil {
		t.Fatalf("a Bitcoin escrow as Phase 1 writes it: %v", err)
	}
	if got := agStrings(t, e, "SELECT coin||':'||(xmr_slot_id IS NULL) FROM escrows WHERE id=$1", btcEscrow); len(got) != 1 || got[0] != "BTC:true" {
		t.Fatalf("a Bitcoin escrow inserted as Phase 1 does: %v", got)
	}
	xmrOrder := e.order(buyer, product, "XMR", stateAwaitingPayment)
	xSlot := xmrSlot(t, e, w, 7)
	xAddr := agStrings(t, e, "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1 RETURNING address", xSlot, xmrOrder)[0]
	esc := "INSERT INTO escrows(order_id,coin,address,market_index,refund_address,fee_bps,fee_destination,xmr_slot_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)"
	otherBTC := e.order(buyer, product, "BTC", stateAwaitingPayment)
	xmrRefused(t, e, "a Bitcoin escrow of an XMR order", "the coin of an escrow is its order's currency", esc, xmrOrder, "BTC", "bcrt1qx84xmr", 7, "bcrt1qx84xmrrefund", 0, "", nil)
	xmrRefused(t, e, "a Monero escrow of a BTC order", "the coin of an escrow is its order's currency", esc, otherBTC, "XMR", xAddr, nil, xmrAddr("r1"), 0, "", xSlot)
	xmrRefused(t, e, "a Bitcoin escrow without a market index", "escrows_market_index_btc", esc, otherBTC, "BTC", "bcrt1qx84nomi", nil, "bcrt1qx84nomirefund", 0, "", nil)
	xmrRefused(t, e, "a Monero escrow with a market index", "escrows_market_index_btc", esc, xmrOrder, "XMR", xAddr, 99, xmrAddr("r1"), 0, "", xSlot)
	xmrRefused(t, e, "a Monero escrow without a slot", "escrows_xmr_slot_xmr", esc, xmrOrder, "XMR", xAddr, nil, xmrAddr("r1"), 0, "", nil)
	third := e.order(buyer, product, "XMR", stateAwaitingPayment)
	tSlot := xmrSlot(t, e, w, 11)
	tAddr := agStrings(t, e, "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1 RETURNING address", tSlot, third)[0]
	xmrRefused(t, e, "a Monero escrow on another order's slot", `constraint "escrows_xmr_slot"`, esc, xmrOrder, "XMR", tAddr, nil, xmrAddr("r1"), 0, "", tSlot)
	xmrRefused(t, e, "a Monero escrow away from its slot's address", `constraint "escrows_xmr_slot"`, esc, xmrOrder, "XMR", xmrAddr("elsewhere"), nil, xmrAddr("r1"), 0, "", xSlot)
	xmrRefused(t, e, "a Monero escrow refunding to a Bitcoin address", "escrows_xmr_addresses", esc, xmrOrder, "XMR", xAddr, nil, "bcrt1qx84xmrrefund", 0, "", xSlot)
	xmrRefused(t, e, "a Monero escrow's fee to a Bitcoin address", "escrows_xmr_addresses", esc, xmrOrder, "XMR", xAddr, nil, xmrAddr("r1"), 100, "bcrt1qx84fee", xSlot)
	xmrRefused(t, e, "a Bitcoin escrow on a slot", "escrows_xmr_slot_xmr", esc, otherBTC, "BTC", "bcrt1qx84slot", 98, "bcrt1qx84slotrefund", 0, "", xSlot)
	if _, err := e.DB.Exec(esc, xmrOrder, "XMR", xAddr, nil, xmrAddr("r1"), 100, xmrAddr("fee"), xSlot); err != nil {
		t.Fatalf("a Monero escrow with a market fee: %v", err)
	}
	// A Bitcoin escrow converted whole to a Monero one passes every CHECK and the slot's foreign key; only the coin rule
	// refuses it.
	convert := xmrSlot(t, e, w, 8)
	cAddr := agStrings(t, e, "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1 RETURNING address", convert, btcOrder)[0]
	xmrRefused(t, e, "a Bitcoin escrow's coin changed", "keeps its coin", "UPDATE escrows SET coin='XMR',market_index=NULL,xmr_slot_id=$2,address=$3,refund_address=$4 WHERE id=$1",
		btcEscrow, convert, cAddr, xmrAddr("convert-refund"))
	xmrRefused(t, e, "a Monero escrow's coin changed", "keeps its coin", "UPDATE escrows SET coin='BTC' WHERE order_id=$1", order)
	xmrRefused(t, e, "an escrow moved to another order", "keeps its coin", "UPDATE escrows SET order_id=$2 WHERE id=$1", btcEscrow, otherBTC)
	xmrRefused(t, e, "the currency of a Monero escrow order changed", "its currency, the escrow's coin, never changes", "UPDATE orders SET currency='BTC' WHERE id=$1", order)
	xmrRefused(t, e, "the currency of a Bitcoin escrow order changed", "its currency, the escrow's coin, never changes", "UPDATE orders SET currency='XMR' WHERE id=$1", btcOrder)
	if _, err := e.DB.Exec("UPDATE orders SET currency='BTC' WHERE id=$1", e.order(buyer, product, "XMR", stateDraft)); err != nil {
		t.Fatalf("the currency of an order without an escrow: %v", err)
	}
	xmrRefused(t, e, "a Monero escrow deleted", "escrow records are never deleted", "DELETE FROM escrows WHERE order_id=$1", order)
	xmrRefused(t, e, "a Bitcoin escrow deleted", "escrow records are never deleted", "DELETE FROM escrows WHERE id=$1", btcEscrow)

	// Each settlements table takes its own coin's escrows only.
	xmrRefused(t, e, "a Bitcoin settlement of a Monero escrow", "has a Monero escrow", "INSERT INTO settlements(order_id,kind) VALUES($1,'refund')", order)
	if _, err := e.DB.Exec("INSERT INTO settlements(order_id,kind) VALUES($1,'refund')", btcOrder); err != nil {
		t.Fatalf("a Bitcoin settlement of a Bitcoin escrow: %v", err)
	}
	xmrRefused(t, e, "a Bitcoin settlement moved to a Monero escrow", "has a Monero escrow", "UPDATE settlements SET order_id=$2 WHERE order_id=$1", btcOrder, order)
	xs := "INSERT INTO xmr_settlements(order_id,kind,destination,state) VALUES($1,$2,$3,$4)"
	xmrRefused(t, e, "a Monero settlement of a Bitcoin escrow", "has no Monero escrow", xs, btcOrder, "refund", xmrAddr("d1"), "requested")
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
	xmrRefused(t, e, "another order's release to the same destination", "xmr_settlements_release_destination", xs, other, "release", xmrAddr("claim-3"), "requested")
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
	agExec(e, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC',$2,0,'bcrt1qx84escrow',100000,3)", btcOrder, txidOf("x84-btc"))
	xmrRefused(t, e, "a Bitcoin row claimed by a Monero settlement", "payments_xmr_settlement", "UPDATE payments SET xmr_settlement_id=$2 WHERE order_id=$1", btcOrder, rel)
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
// payouts row is written for one, none is moved to one, and the send claim never claims one that existed before the
// order's escrow (as by hand).
func TestXMREscrowPayoutRefused(t *testing.T) {
	e := newTestApp(t)
	fake := newFakeProvider("XMR", 10)
	e.A.payments["XMR"] = fake
	buyer, _ := e.user("xpay_buyer", "buyer")
	vendor, _ := e.user("xpay_vendor", "vendor")
	product := e.product(vendor, "digital")
	w := xmrWallet(t, e, 1, "active")
	order, _, _ := xmrEscrow(t, e, w, 1, buyer, product, stateCompleted)
	payout := "INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'XMR',100000,'fake-testnet-vendor','pending')"
	xmrRefused(t, e, "a payout of a Monero escrow order", "is an escrow order", payout, order, vendor)
	pooled := e.order(buyer, product, "XMR", stateCompleted)
	agExec(e, payout, pooled, vendor)
	xmrRefused(t, e, "a payout moved to a Monero escrow order", "is an escrow order", "UPDATE payouts SET order_id=$2 WHERE order_id=$1", pooled, order)

	// A pending payout written before its order got a Monero escrow is never claimed; a pooled one in the same pass is.
	early := e.order(buyer, product, "XMR", stateCompleted)
	agExec(e, payout, early, vendor)
	slot := xmrSlot(t, e, w, 2)
	addr := agStrings(t, e, "UPDATE xmr_escrow_slots SET state='claimed',order_id=$2 WHERE id=$1 RETURNING address", slot, early)[0]
	agExec(e, "INSERT INTO escrows(order_id,coin,address,refund_address,fee_bps,fee_destination,xmr_slot_id) VALUES($1,'XMR',$2,$3,0,'',$4)", early, addr, xmrAddr("refund-early"), slot)
	if err := e.A.sendPayouts(context.Background(), fake, []string{early, pooled}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Sends()) != 1 || agInt(e, "SELECT count(*) FROM payouts WHERE order_id=$1 AND state='pending'", early) != 1 {
		t.Fatalf("%d sends; the Monero escrow order's payout must stay pending and the pooled one be sent", len(fake.Sends()))
	}
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
	agExec(e, "INSERT INTO payment_addresses(order_id,currency,address,provider,subaddr_index) VALUES($1,'XMR',$2,'monero',1)", pooled, pooledAddr)
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

// Bitcoin escrow code never reads or writes a Monero escrow's rows (P3-R1: every Bitcoin escrow query that could read
// a Monero row filters coin='BTC'), and works as before beside them. Monero escrow orders in every state a Bitcoin pass
// acts on (funded while awaiting payment with an old escrow, shipped past and near its release deadline with a restore
// recheck pending, closed with a late deposit and a recovery request, completed with a requested settlement) sit beside
// Bitcoin escrow orders that are funded, claimed, co-signed, broadcast and confirmed, and released at their deadline,
// with the daily check (strays and orphans) due. Afterwards no Monero row changed, no RPC named a Monero address, order
// or transaction, the order page shows nothing of the Bitcoin escrow (nor the pooled panel) for a Monero escrow order,
// and the vendor desk counts Bitcoin escrow orders only.
func TestEscrowBTCIgnoresXMRRows(t *testing.T) {
	x := newEscrowEnv(t)
	physical := x.physicalListing(x.vendor, 3)
	w := xmrWallet(t, x.testEnv, 1, "active")
	funded, fundedAddr, _ := xmrEscrow(t, x.testEnv, w, 1, x.buyer, x.product, stateAwaitingPayment)
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '20 days' WHERE order_id=$1", funded)
	agExec(x.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,$3,$4,100000,50)", funded, txidOf("xmr-fund"), xmrEscrowIdx(w, 1), fundedAddr)
	due, dueAddr, _ := xmrEscrow(t, x.testEnv, w, 2, x.buyer, physical, stateShipped)
	soon, soonAddr, _ := xmrEscrow(t, x.testEnv, w, 3, x.buyer, physical, stateShipped)
	agExec(x.testEnv, "UPDATE escrows SET release_days=14,dispatch_days=3,ship_by=now()-interval '10 days',release_at=now()-interval '1 hour' WHERE order_id=$1", due)
	agExec(x.testEnv, "UPDATE escrows SET release_days=14,dispatch_days=3,ship_by=now()-interval '10 days',release_at=now()+interval '24 hours' WHERE order_id=$1", soon)
	closed, closedAddr, _ := xmrEscrow(t, x.testEnv, w, 4, x.buyer, x.product, stateCancelled)
	agExec(x.testEnv, "UPDATE escrows SET recover_requested=now() WHERE order_id=$1", closed)
	agExec(x.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,after_close) VALUES($1,'XMR',$2,$3,$4,70000,50,true)", closed, txidOf("xmr-late"), xmrEscrowIdx(w, 4), closedAddr)
	done, doneAddr, _ := xmrEscrow(t, x.testEnv, w, 5, x.buyer, x.product, stateCompleted)
	agExec(x.testEnv, "INSERT INTO xmr_settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3)", done, xmrAddr("xmr-claim"), x.vendor)
	xmrOrders := []string{funded, due, soon, closed, done}
	monero := append([]string{fundedAddr, dueAddr, soonAddr, closedAddr, doneAddr, xmrAddr("xmr-claim"), txidOf("xmr-fund"), txidOf("xmr-late")}, xmrOrders...)
	// Every row of the Monero escrow orders, as the later slices will read them.
	snapshot := func() string {
		x.t.Helper()
		var parts []string
		for _, q := range []string{
			"SELECT to_jsonb(o) FROM orders o WHERE id=ANY($1) ORDER BY id",
			"SELECT to_jsonb(es) FROM escrows es WHERE order_id=ANY($1) ORDER BY id",
			"SELECT to_jsonb(s) FROM xmr_escrow_slots s ORDER BY id",
			"SELECT to_jsonb(xw) FROM xmr_escrow_wallets xw ORDER BY id",
			"SELECT to_jsonb(p) FROM payments p WHERE order_id=ANY($1) ORDER BY id",
			"SELECT to_jsonb(s) FROM xmr_settlements s ORDER BY id",
			"SELECT count(*)::text FROM settlements WHERE order_id=ANY($1)",
			"SELECT count(*)::text FROM order_events WHERE order_id=ANY($1)",
			"SELECT count(*)::text FROM payouts WHERE order_id=ANY($1)",
		} {
			args := []any{xmrOrders}
			if !strings.Contains(q, "$1") {
				args = nil
			}
			parts = append(parts, strings.Join(agStrings(x.t, x.testEnv, q, args...), "\n"))
		}
		for _, o := range xmrOrders {
			parts = append(parts, strconv.Itoa(agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE strpos(body,$1)>0", shortID(o))))
		}
		return strings.Join(parts, "\n--\n")
	}
	before := snapshot()

	// The Bitcoin escrow, beside them: a digital order funded, claimed, signed, co-signed, broadcast and confirmed; a
	// physical one shipped and released at its deadline, after a restore recheck and with the daily check due.
	claimOrder, claimID := x.confirmedSettlement("ign")
	order, rel := x.shippedPhysical(regtestAddr("ign-refund"), regtestAddr("ign-claim"), 100000)
	x.signAs(order, roleVendor, rel)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", releaseRecheckKey)
	agExec(x.testEnv, "UPDATE settings SET value='2000-01-01T00:00:00Z' WHERE key=$1", escrowStrayCheckSetting)
	x.poll()
	if x.state(order) != stateShipped || agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key=$1", releaseRecheckKey) != 0 {
		t.Fatalf("the restore recheck pass: order %s", x.state(order))
	}
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	x.poll()
	x.poll()
	if x.state(order) != stateCompleted || x.settleByID(rel).State != "broadcast" {
		t.Fatalf("the Bitcoin release at its deadline: order %s, release %+v", x.state(order), x.settleByID(rel))
	}
	if r := x.settleByID(claimID); r.State != "confirmed" || x.state(claimOrder) != stateCompleted {
		t.Fatalf("the Bitcoin claim: %+v", r)
	}
	// The buyer's notice of a release deadline under 72 hours (/orders) and the reminder: Bitcoin escrow copy, so none
	// for the Monero order whose deadline is a day away.
	if err := x.A.remindDeadlines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body := x.page("/orders", x.buyerSess); strings.Contains(body, "deadline-banner") {
		t.Error("/orders shows a Bitcoin release deadline banner for a Monero escrow order")
	}

	if after := snapshot(); after != before {
		t.Errorf("Bitcoin escrow passes changed Monero escrow rows:\nbefore\n%s\nafter\n%s", before, after)
	}
	x.core.mu.Lock()
	for _, call := range x.core.calls {
		for _, m := range monero {
			if strings.Contains(string(call.Params), m) {
				t.Errorf("RPC %s named %s: %.200s", call.Method, m, call.Params)
			}
		}
	}
	x.core.mu.Unlock()

	// The order page of a Monero escrow order: nothing of the Bitcoin escrow (panel, descriptor, PSBT and claim forms)
	// and not the pooled payment panel either, which would say the funds are in the market's pooled wallet.
	for _, o := range xmrOrders {
		for _, sess := range []string{x.buyerSess, x.vendorSess} {
			body := x.page("/order?id="+o, sess)
			for _, bad := range []string{"<h2>Bitcoin escrow", "/escrow-descriptor", "/escrow-psbt", `action="/escrow/`, "<h2>Payment", "<h2>No payment record", "no funds are held"} {
				if strings.Contains(body, bad) {
					t.Errorf("order page of Monero escrow order %s shows %q", shortID(o), bad)
				}
			}
		}
	}
	if w := x.do("GET", "/escrow-descriptor?id="+funded, x.buyerSess, nil); w.Code != 404 {
		t.Errorf("the Bitcoin escrow descriptor of a Monero escrow order: %d", w.Code)
	}
	// The Bitcoin escrow forms and downloads refuse a Monero escrow order before any check or RPC. legacy holds Bitcoin
	// settlement and fee-bump rows written before its Monero escrow existed (settlements_btc_only refuses only rows
	// written after), so for sign, bump, bump-sign and the PSBT download nothing but the Order.btcEscrow guard refuses;
	// decided is a resolved Monero escrow order whose changed decision would otherwise reach escrowSettleTerminal.
	legacy := x.testEnv.order(x.buyer, x.product, "XMR", stateCompleted)
	var legacyRel, legacyStray, legacyBump int64
	if err := x.DB.QueryRow(`INSERT INTO settlements(order_id,kind,state,destination,fee_rate,network_fee,input_total,psbt,txid,raw_tx,cosigned,final_at,broadcast_at)
		VALUES($1,'release','broadcast',$2,2,300,100000,'cHNidP8=',$3,'0200','market',now(),now()) RETURNING id`, legacy, regtestAddr("legacy-claim"), txidOf("legacy-rel")).Scan(&legacyRel); err != nil {
		t.Fatal(err)
	}
	if err := x.DB.QueryRow(`INSERT INTO settlements(order_id,kind,state,destination,fee_rate,network_fee,input_total,psbt,txid)
		VALUES($1,'stray_refund','awaiting_signature',$2,2,300,100000,'cHNidP8=',$3) RETURNING id`, legacy, regtestAddr("legacy-refund"), txidOf("legacy-stray")).Scan(&legacyStray); err != nil {
		t.Fatal(err)
	}
	if err := x.DB.QueryRow("INSERT INTO settlement_bumps(settlement_id,n,requested_by,fee_rate) VALUES($1,1,$2,5) RETURNING id", legacyRel, x.vendor).Scan(&legacyBump); err != nil {
		t.Fatal(err)
	}
	agExec(x.testEnv, "UPDATE settlement_bumps SET state='awaiting_signature',psbt='cHNidP8=',txid=$2,network_fee=900 WHERE id=$1", legacyBump, txidOf("legacy-bump"))
	xmrEscrowOf(t, x.testEnv, w, 6, legacy)
	decided, decidedAddr, _ := xmrEscrow(t, x.testEnv, w, 7, x.buyer, x.product, stateResolved)
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The licence key does not work at all.')", randomToken(), decided)
	agExec(x.testEnv, "UPDATE disputes SET outcome='release',status='Resolved — release to vendor' WHERE order_id=$1", decided)
	agExec(x.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'XMR',$2,$3,$4,100000,50)", decided, txidOf("xmr-decided"), xmrEscrowIdx(w, 7), decidedAddr)
	_, adminSess := x.testEnv.user("ign_admin_"+randomToken()[:6], "admin")
	guarded := func() string {
		return strings.Join(agStrings(t, x.testEnv, `SELECT 's'||id||state FROM settlements WHERE order_id=$1 UNION ALL SELECT 'b'||id||state FROM settlement_bumps WHERE settlement_id=$2
			UNION ALL SELECT 'd'||outcome FROM disputes WHERE order_id=$3 UNION ALL SELECT 'o'||state FROM orders WHERE id=$3 ORDER BY 1`, legacy, legacyRel, decided), ",")
	}
	guardedBefore, formsBefore := guarded(), snapshot()
	calls := len(x.core.methods())
	sid, bid := strconv.FormatInt(legacyStray, 10), strconv.FormatInt(legacyBump, 10)
	for _, f := range []struct {
		path string
		sess string
		form url.Values
	}{
		{"/escrow/claim", x.vendorSess, url.Values{"order_id": {done}, "address": {regtestAddr("ign-x")}, "password": {testPassword}}},
		{"/escrow/ship", x.vendorSess, url.Values{"order_id": {due}, "address": {regtestAddr("ign-y")}, "password": {testPassword}, "note": {"Sent."}}},
		{"/escrow/recover", x.buyerSess, url.Values{"order_id": {closed}, "password": {testPassword}}},
		{"/escrow/sign", x.buyerSess, url.Values{"order_id": {legacy}, "settlement_id": {sid}, "psbt": {"cHNidP8="}}},
		{"/escrow/bump", x.vendorSess, url.Values{"order_id": {legacy}, "settlement_id": {strconv.FormatInt(legacyRel, 10)}, "fee_rate": {"5"}, "password": {testPassword}}},
		{"/escrow/bump-sign", x.vendorSess, url.Values{"order_id": {legacy}, "bump_id": {bid}, "psbt": {"cHNidP8="}}},
		{"/escrow/decision", adminSess, url.Values{"order_id": {decided}, "outcome": {"refund"}, "reason": {"The buyer showed that the key never worked."}, "password": {testPassword}}},
	} {
		if w := x.do("POST", f.path, f.sess, f.form); w.Code != 404 || !strings.Contains(w.Body.String(), "Order not found") {
			t.Errorf("%s on a Monero escrow order: %d %.200s", f.path, w.Code, w.Body.String())
		}
	}
	for _, get := range []struct{ path, sess string }{
		{"/escrow-psbt?id=" + legacy + "&s=" + sid, x.buyerSess},
		{"/escrow-psbt?id=" + legacy + "&s=" + strconv.FormatInt(legacyRel, 10) + "&b=" + bid, x.vendorSess},
	} {
		if w := x.do("GET", get.path, get.sess, nil); w.Code != 404 {
			t.Errorf("GET %s of a Monero escrow order: %d %.200s", get.path, w.Code, w.Body.String())
		}
	}
	if n := len(x.core.methods()); n != calls {
		t.Errorf("the Bitcoin escrow forms made %d RPCs for Monero escrow orders", n-calls)
	}
	if after := snapshot(); after != formsBefore {
		t.Fatalf("Bitcoin escrow forms changed Monero escrow rows:\nbefore\n%s\nafter\n%s", formsBefore, after)
	}
	if after := guarded(); after != guardedBefore {
		t.Fatalf("Bitcoin escrow forms changed a Monero escrow order's rows: %s, was %s", after, guardedBefore)
	}

	// The vendor desk's escrow tile counts the vendor's Bitcoin escrow orders only.
	d := &PageData{User: x.loadUser(x.vendor)}
	if err := unsentPayoutsLoader(context.Background(), x.A, nil, d); err != nil {
		t.Fatal(err)
	}
	if want := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE coin='BTC'"); d.EscrowOrders != want || want == 0 {
		t.Fatalf("vendor desk counts %d escrow orders, want the %d Bitcoin ones", d.EscrowOrders, want)
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
			"SELECT to_jsonb(es) - 'coin' - 'xmr_slot_id' FROM escrows es ORDER BY id",
			"SELECT to_jsonb(p) - 'xmr_settlement_id' FROM payments p ORDER BY id",
			"SELECT to_jsonb(t) FROM settlements t ORDER BY id",
			"SELECT to_jsonb(t) FROM settlement_outputs t ORDER BY settlement_id,n",
			"SELECT to_jsonb(t) FROM settlement_bumps t ORDER BY id",
			"SELECT to_jsonb(t) FROM escrow_pubkeys t ORDER BY pubkey",
			"SELECT to_jsonb(t) FROM orders t ORDER BY id",
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
// Monero escrow's amount in XMR with its 12 decimals, never as Bitcoin.
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
		if strings.Contains(text, "BTC") || !strings.Contains(text, "1.234567890123 fakechain XMR") {
			t.Errorf("dispute preview of a Monero escrow order: %q", text)
		}
	}
	if body := e.do("GET", "/moderator", adminSess, nil).Body.String(); !strings.Contains(body, "1.234567890123 fakechain XMR") {
		t.Error("the moderation desk does not state the Monero escrow's amount in XMR")
	}
}
