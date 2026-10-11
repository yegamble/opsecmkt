package market

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLateDepositFlagNotesAreBoundedAndIndexOutputs(t *testing.T) {
	for _, currency := range []string{"BTC", "XMR"} {
		t.Run(currency, func(t *testing.T) {
			e := newTestApp(t)
			buyer, _ := e.user("flagbuyer_"+randomToken()[:6], "buyer")
			vendor, _ := e.user("flagvendor_"+randomToken()[:6], "vendor")
			mod, _ := e.user("flagmod_"+randomToken()[:6], "moderator")
			product := e.product(vendor, "digital")
			order := e.order(buyer, product, currency, stateCompleted)
			addr := "escrow-" + order[:12]
			var wallet int64
			if currency == "BTC" {
				agExec(e, `INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,$2,nextval('escrow_market_index'),$3,0,'')`, order, addr, "refund-"+order[:12])
			} else {
				wallet = xmrWallet(t, e, 1, "active")
				addr, _ = xmrEscrowOf(t, e, wallet, 1, order)
			}
			const txid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const outputs = 18
			deposit := func(n int) (string, int64) {
				if currency == "XMR" {
					return txidOf("bounded XMR note " + strconv.Itoa(n)), xmrEscrowIdx(wallet, 1)
				}
				return txid, int64(n)
			}
			for n := range outputs {
				id, idx := deposit(n)
				agExec(e, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,after_close)
					VALUES($1,$2,$3,$4,$5,5000,3,true)`, order, currency, id, idx, addr)
			}
			tx, err := e.DB.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			o, err := scanOrder(tx.QueryRowContext(context.Background(), orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", order))
			if err != nil {
				t.Fatal(err)
			}
			auditBefore := agInt(e, "SELECT count(*) FROM audit_events")
			if err = e.A.flagLateDeposits(context.Background(), tx, &o, 3); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}

			notes := agStrings(t, e, "SELECT note FROM order_events WHERE order_id=$1 AND actor_id IS NULL ORDER BY id", order)
			if len(notes) < 2 {
				t.Fatalf("%d outputs were not split into bounded notes: %d", outputs, len(notes))
			}
			joined := strings.Join(notes, "\n")
			for _, note := range notes {
				if n := utf8.RuneCountInString(note); n > maxNoteChars {
					t.Fatalf("generated note has %d characters (limit %d): %s", n, maxNoteChars, note)
				}
				if !strings.HasSuffix(note, "Moderator review required.") {
					t.Fatalf("generated note does not date a flag: %q", note)
				}
			}
			// Literal expectations, not lateDepositName itself: a Bitcoin escrow output is named by its own output index, and
			// a Monero escrow deposit by its account component (the wallet bits stay out of the note), each exactly once.
			seen := map[string]bool{}
			for n := range outputs {
				id, _ := deposit(n)
				want := "deposit " + id[:20] + "… output " + strconv.Itoa(n) + " (0.00005 BTC)"
				if currency == "XMR" {
					want = "deposit " + id[:20] + "… account 1 (0.000000005 XMR)"
				}
				if c := strings.Count(joined, want); c != 1 {
					t.Errorf("deposit %d: %q appears %d times in the notes, want once", n, want, c)
				}
				seen[want] = true
			}
			if len(seen) != outputs {
				t.Errorf("%d outputs are named with only %d distinct labels", outputs, len(seen))
			}
			if currency == "XMR" && strings.Contains(joined, "output ") {
				t.Errorf("a Monero escrow note names an output index: %s", joined)
			}
			if n := agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order); n != outputs {
				t.Fatalf("flagged %d of %d outputs", n, outputs)
			}
			if n := agInt(e, `SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,'Payment review needed for order '||left($2,8))=1`, mod, order); n != 1 {
				t.Fatalf("staff notices were not episode-bounded: %d", n)
			}
			if n := agInt(e, "SELECT count(*) FROM audit_events"); n != auditBefore {
				t.Fatalf("flagging changed audit history from %d to %d rows", auditBefore, n)
			}
		})
	}
}

func TestPaymentReviewFlagTimeUsesIndexedOutputAndLegacyNotes(t *testing.T) {
	e := newTestApp(t)
	buyer, _ := e.user("flagdatebuyer_"+randomToken()[:6], "buyer")
	vendor, _ := e.user("flagdatevendor_"+randomToken()[:6], "vendor")
	_, mod := e.user("flagdatemod_"+randomToken()[:6], "moderator")
	product := e.product(vendor, "digital")
	order := e.order(buyer, product, "BTC", stateCompleted)
	agExec(e, `INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,$2,nextval('escrow_market_index'),$3,0,'')`, order, "escrow-"+order[:12], "refund-"+order[:12])
	txid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	legacyTx := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, p := range []struct {
		txid string
		idx  int
	}{{txid, 3}, {txid, 9}, {legacyTx, 1}} {
		agExec(e, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,after_close,flagged)
			VALUES($1,'BTC',$2,$3,'test-address',5000,3,true,true)`, order, p.txid, p.idx)
	}
	addNote := func(note, at string) {
		agExec(e, `INSERT INTO order_events(order_id,from_state,to_state,actor_id,note,created)
			VALUES($1,$2,$2,NULL,$3,$4::timestamptz)`, order, stateCompleted, note, at)
	}
	addNote("BTC "+txid+" legacy after-close deposit. Moderator review required.", "2025-01-01 00:00:00+00")
	addNote("BTC deposit "+truncate(txid, 20)+" output 3 (0.00005 BTC) arrived after close. Moderator review required.", "2025-04-01 00:00:00+00")
	addNote("BTC deposit "+truncate(txid, 20)+" output 9 (0.00005 BTC) arrived after close. Moderator review required.", "2026-01-01 00:00:00+00")
	addNote("BTC pooled deposit "+truncate(txid, 20)+":3 (0.00005 BTC) is still unconfirmed. It was not paid out. Moderator review required.", "2025-06-01 00:00:00+00")
	// Index 30 must not advance index 3's flag time by matching its prefix.
	addNote("BTC pooled deposit "+truncate(txid, 20)+":30 (0.00005 BTC) is still unconfirmed. It was not paid out. Moderator review required.", "2026-02-01 00:00:00+00")
	addNote("BTC "+truncate(legacyTx, 20)+" old-format deposit. Moderator review required.", "2024-02-03 00:00:00+00")

	d := &PageData{User: &User{ID: mod, Role: "moderator"}}
	if err := loadPaymentReviews(context.Background(), e.A, nil, d); err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, p := range d.PaymentReviews {
		got[p.Index] = p.Flagged
	}
	if got[3] != "2025-06-01 00:00 UTC" || got[9] != "2026-01-01 00:00 UTC" || got[1] != "2024-02-03 00:00 UTC" {
		t.Fatalf("payment flag dates did not resolve per indexed output and legacy note: %#v", got)
	}
}
