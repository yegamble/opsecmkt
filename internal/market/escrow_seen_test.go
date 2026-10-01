package market

import (
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-344: the exact stored transaction is already in the mempool or a block when the market comes to complete a
// party-signed settlement (the parties exchanged signatures and broadcast it themselves, or a restored database holds
// an older party-signed row). The watch-only wallet names it by its txid, so it is checked and recorded as the
// settlement's final transaction and followed as broadcast to its confirmation, never marked external. Another
// transaction spending the escrow is external. Fake escrow Core (simulated RPC, not chain evidence).

// partiesBroadcast puts the settlement's final transaction, finalized from the given signatures on every input, in the
// fake node and wallet with confs confirmations, its inputs spent: what the parties' own broadcast would do.
func (x *escrowEnv) partiesBroadcast(id int64, confs int64, sigs ...partySigs) (txid, raw string) {
	x.t.Helper()
	var stored string
	if err := x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored); err != nil {
		x.t.Fatal(err)
	}
	sp, err := decodePSBT(stored)
	if err != nil {
		x.t.Fatal(err)
	}
	both, err := withPartySigs(sp, sigs...)
	if err != nil {
		x.t.Fatal(err)
	}
	pk, err := decodePSBT(both)
	if err != nil {
		x.t.Fatal(err)
	}
	raw, ok := fakeFinalTx(pk)
	if !ok {
		x.t.Fatal("the signatures do not finalize")
	}
	b, _ := hex.DecodeString(raw)
	final, err := parseSignedTx(b)
	if err != nil {
		x.t.Fatal(err)
	}
	txid = final.TxID()
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	if x.core.broadcast == nil {
		x.core.broadcast, x.core.txConfs = map[string]string{}, map[string]int64{}
	}
	if x.core.spent == nil {
		x.core.spent = map[string]bool{}
	}
	x.core.broadcast[txid], x.core.txConfs[txid] = raw, confs
	for _, in := range final.In {
		id := in.TxID
		slices.Reverse(id[:])
		x.core.spent[hex.EncodeToString(id[:])+":"+strconv.FormatInt(int64(in.Vout), 10)] = true
	}
	return txid, raw
}

// storedSigs reads party's stored signatures on settlement id.
func (x *escrowEnv) storedSigs(id int64, party string) partySigs {
	x.t.Helper()
	p := partySigs{Party: party}
	for _, r := range agStrings(x.t, x.testEnv, "SELECT pubkey||' '||signature FROM settlement_signatures WHERE settlement_id=$1 AND party=$2 ORDER BY input_n", id, party) {
		f := strings.Fields(r)
		pub, _ := hex.DecodeString(f[0])
		sig, _ := hex.DecodeString(f[1])
		p.Sigs = append(p.Sigs, psbtSig{pub, sig})
	}
	return p
}

func TestEscrowPartiesBroadcastBeforeTheWatcher(t *testing.T) {
	x := newEscrowEnv(t)
	for _, c := range []struct {
		name  string
		confs int64
	}{{"in the mempool", 0}, {"mined", 1}} {
		t.Run(c.name, func(t *testing.T) {
			order, id := x.built(regtestAddr("refund-seen-"+c.name), regtestAddr("claim-seen-"+c.name), 100000)
			x.signAs(order, roleVendor, id)
			x.signAs(order, roleBuyer, id)
			txid, raw := x.partiesBroadcast(id, c.confs, x.storedSigs(id, roleVendor), x.storedSigs(id, roleBuyer))
			if txid != x.settleByID(id).TxID {
				t.Fatal("the parties' transaction is not the stored one")
			}
			signs, sends := x.core.count("walletprocesspsbt"), x.sendsOf(raw)
			x.poll()
			r := x.settleByID(id)
			if r.State != "broadcast" || r.Raw != raw || r.Cosigned != "parties" || r.Error != "" || r.SpentBy != "" {
				t.Fatalf("after the pass: %+v", r)
			}
			if x.core.count("walletprocesspsbt") != signs || x.sendsOf(raw) != sends {
				t.Fatal("the market signed or sent a transaction the parties had already broadcast")
			}
			if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order) != 0 ||
				agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE $1", "%"+shortID(order)+"%did not make%") != 0 {
				t.Fatal("flagged as spent outside the market")
			}
			ev := strings.Join(x.events(order), "\n")
			if !strings.Contains(ev, "was already on the regtest network when the market came to complete it") || !strings.Contains(ev, "the market did not sign or broadcast it") ||
				!strings.Contains(ev, "The buyer and the vendor both signed it") || strings.Contains(ev, "did not make") {
				t.Fatalf("order notes:\n%s", ev)
			}
			x.core.setTxConfs(txid, 3, "")
			x.poll()
			if r := x.settleByID(id); r.State != "confirmed" {
				t.Fatalf("at 3 confirmations: %+v", r)
			}
			x.noPayouts(order)
		})
	}

	// A restored database's party-signed row whose transaction the market had co-signed after the backup: the stored
	// signatures do not show who signed besides the vendor, so it is recorded as found on the network.
	order, id := x.built(regtestAddr("refund-seen-market"), regtestAddr("claim-seen-market"), 100000)
	x.signAs(order, roleVendor, id)
	market, _ := hex.DecodeString(x.keyHex(order, "market"))
	var stored string
	x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored)
	sp, _ := decodePSBT(stored)
	ms := partySigs{Party: "market"}
	for range sp.Inputs {
		ms.Sigs = append(ms.Sigs, psbtSig{market, fakeSig(market, sp.TxID(), 1)})
	}
	_, raw := x.partiesBroadcast(id, 2, x.storedSigs(id, roleVendor), ms)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" || r.Raw != raw || r.Cosigned != "network" {
		t.Fatalf("market-co-signed transaction found on the network: %+v", r)
	}
	if ev := strings.Join(x.events(order), "\n"); !strings.Contains(ev, "who signed it besides the vendor is not recorded") {
		t.Fatalf("order notes:\n%s", ev)
	}

	// The stored txid, but witnesses the market's checks refuse (signatures not SIGHASH_ALL): neither recorded as the
	// settlement's final transaction nor called external; it waits, with the class on the order page and in the log.
	orderN, idN := x.built(regtestAddr("refund-seen-none"), regtestAddr("claim-seen-none"), 100000)
	x.signAs(orderN, roleVendor, idN)
	x.signAs(orderN, roleBuyer, idN)
	none := func(p partySigs) partySigs {
		q := partySigs{Party: p.Party}
		for _, s := range p.Sigs {
			q.Sigs = append(q.Sigs, psbtSig{s.PubKey, fakeSig(s.PubKey, x.settleByID(idN).TxID, 2)})
		}
		return q
	}
	x.partiesBroadcast(idN, 1, none(x.storedSigs(idN, roleVendor)), none(x.storedSigs(idN, roleBuyer)))
	x.poll()
	if r := x.settleByID(idN); r.State != "party_signed" || r.Raw != "" || r.Error != finalWitness {
		t.Fatalf("stored txid with SIGHASH_NONE witnesses: %+v", r)
	}

	// Another transaction spent the escrow: external, flagged.
	order2, id2 := x.built(regtestAddr("refund-seen-other"), regtestAddr("claim-seen-other"), 100000)
	x.signAs(order2, roleVendor, id2)
	x.signAs(order2, roleBuyer, id2)
	x.core.spend(txidOf(order2), 0)
	x.poll()
	if r := x.settleByID(id2); r.State != "external" {
		t.Fatalf("spent by another transaction: %+v", r)
	}
}
