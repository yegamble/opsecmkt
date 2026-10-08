package market

import (
	"context"
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
			const txid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const outputs = 18
			for idx := range outputs {
				agExec(e, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,after_close)
					VALUES($1,$2,$3,$4,'test-address',5000,3,true)`, order, currency, txid, idx)
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
			if err = e.A.flagLateDeposits(context.Background(), tx, &o, 3, false); err != nil {
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
			for idx := range outputs {
				label := lateDepositName(txid, int64(idx), 5000, currency, false)
				if !strings.Contains(joined, label) {
					t.Errorf("output %d is not distinctly named: %q", idx, label)
				}
			}
			if n := agInt(e, `SELECT count(*) FROM payments p WHERE p.order_id=$1 AND p.flagged AND EXISTS (
				SELECT 1 FROM order_events ev WHERE ev.order_id=p.order_id AND ev.actor_id IS NULL
				AND ev.note LIKE '%Moderator review required.%' AND strpos(ev.note,' '||CASE WHEN length(p.txid)<=20 THEN p.txid ELSE left(p.txid,20)||'…' END||' ')>0)`, order); n != outputs {
				t.Fatalf("flag timestamp notes match %d of %d outputs", n, outputs)
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
