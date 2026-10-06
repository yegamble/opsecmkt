package market

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
)

// Broadcast honesty (A-441, P4-R7 (a)(b)).
//
// Bitcoin Core's -privatebroadcast with a proxy that does not answer (Tor down, say) accepts sendrawtransaction, answers
// the txid and keeps the transaction in a private queue: it never reaches the node's mempool, and a resend is idempotent
// (probed on Core 31.1 regtest by the A-255/A-249 council). The escrow code records such a transaction as broadcast and
// sends it again on every pass, so without more the market and its staff would believe it was sent.
//
//   - The stall alert, in every mode: a settlement in state broadcast none of whose transactions (its own and its fee
//     bumps', settlementAttempts) the watch-only wallet has seen escrowStallBlocks blocks after it was broadcast raises
//     one staff notice (flagOrder: an order note starting escrowStallMark and a notification to each moderator and
//     administrator), and staff see a "queued or stuck: not seen by the network" line on /admin under Payment providers
//     and on the order page, until the wallet sees one of them: then one order note starting escrowSeenMark clears it.
//     No settlement state is added (migration 074's CHECK): the state stays broadcast and the stored transaction is
//     sent again on each pass exactly as before. Which alert is open is read from those two notes (escrowStalledSQL);
//     the height from which a transaction went unseen is kept in memory only, so after a restart the count starts again
//     at the tip the first pass reads (the alert can come up to escrowStallBlocks blocks later, never twice).
//   - The Tor-mode gate (APP_MODE=tor only): once per escrow watcher pass, getnetworkinfo must show a proxy for both
//     ipv4 and ipv6 (-proxy sets them; -onion alone does not, and onion "reachable" reflects configuration, not
//     liveness, so it is not read) and a version of at least 310100 (Bitcoin Core 31.1, the first with
//     -privatebroadcast). Otherwise, or when getnetworkinfo fails, nothing is sent: no sendrawtransaction is issued and
//     no settlement changes state; one log line per change and a line on /admin say what to configure. Clearnet mode
//     is unaffected. The gate checks configuration; the stall alert covers liveness (a proxy that is configured but
//     down).

// escrowStallBlocks is how many blocks a broadcast settlement's transaction may go unseen by the watch-only wallet
// before staff are told. A transaction the node accepted into its mempool is in the wallet at once, and one sent
// privately comes back from the network within minutes; six blocks (about an hour on a public test network, the
// customary depth for a settled payment, and twice the default PAYMENT_CONFIRMATIONS_BTC) is well past either, yet
// short enough for staff to act the same hour.
const escrowStallBlocks = 6

// The order notes that open and close a stall alert (they start with these; strpos=1). Each names the settlement's
// transaction ID, which tells two settlements of one order apart.
const (
	escrowStallMark = "Queued or stuck: not seen by the network."
	escrowSeenMark  = "Seen by the network:"
)

// escrowStalledSQL is true for the settlements row s while its stall alert is open: the latest of its two notes is the
// stall note. Callers add s.state='broadcast'.
const escrowStalledSQL = `COALESCE((SELECT strpos(e.note,'` + escrowStallMark + `')=1 FROM order_events e WHERE e.order_id=s.order_id AND strpos(e.note,s.txid)>0
	AND (strpos(e.note,'` + escrowStallMark + `')=1 OR strpos(e.note,'` + escrowSeenMark + `')=1) ORDER BY e.id DESC LIMIT 1),false)`

// escrowUnseen is, per broadcast settlement whose transaction the watch-only wallet has not seen, the tip height from
// which it went unseen (in memory only; see the file comment).
type escrowUnseen struct {
	mu    sync.Mutex
	since map[int64]int64
}

// broadcastAt records that settlement id was sent at the tip the current pass read.
func (a *App) broadcastAt(id int64) {
	if tip := a.escrowTip.Load(); tip > 0 {
		a.escrowUnseen.mu.Lock()
		if a.escrowUnseen.since == nil {
			a.escrowUnseen.since = map[int64]int64{}
		}
		a.escrowUnseen.since[id] = tip
		a.escrowUnseen.mu.Unlock()
	}
}

// noteBroadcastSeen applies what trackBroadcast found: seen is whether the watch-only wallet knows any of the
// settlement's transactions. Unseen for escrowStallBlocks blocks opens the stall alert once; seen while it is open
// closes it. Nothing else about the settlement changes.
func (a *App) noteBroadcastSeen(ctx context.Context, id int64, seen bool) error {
	tip := a.escrowTip.Load()
	a.escrowUnseen.mu.Lock()
	if a.escrowUnseen.since == nil {
		a.escrowUnseen.since = map[int64]int64{}
	}
	if seen {
		delete(a.escrowUnseen.since, id)
	} else if _, ok := a.escrowUnseen.since[id]; !ok && tip > 0 {
		a.escrowUnseen.since[id] = tip
	}
	since, ok := a.escrowUnseen.since[id]
	a.escrowUnseen.mu.Unlock()
	if !seen && (!ok || tip-since < escrowStallBlocks) {
		return nil
	}
	// Only the escrow watcher writes these notes, under its advisory lock, so reading the alert's state needs no row lock.
	var orderID, kind, txid string
	var open bool
	err := a.db.QueryRowContext(ctx, "SELECT s.order_id,s.kind,s.txid,"+escrowStalledSQL+" FROM settlements s WHERE s.id=$1 AND s.state='broadcast'", id).Scan(&orderID, &kind, &txid, &open)
	// Seen with the alert open closes it; unseen with none open opens it; anything else changes nothing.
	if err == sql.ErrNoRows || err == nil && seen != open {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	what := settlementKind(kind)
	if seen {
		note := escrowSeenMark + " the market's watch-only wallet now has the transaction of the escrow settlement (" + what + ", transaction " + txid + "); the queued-or-stuck alert is cleared."
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", orderID, note); err != nil {
			return err
		}
		log.Printf("escrow: settlement of order %s: its transaction is seen by the network; the queued-or-stuck alert is cleared", shortID(orderID))
		return tx.Commit()
	}
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", orderID))
	if err != nil {
		return err
	}
	note := escrowStallMark + " The escrow settlement (" + what + ") was broadcast as transaction " + txid + ", but " + strconv.Itoa(escrowStallBlocks) +
		" blocks later the market's watch-only wallet has not seen it (it is not in the escrow node's mempool or in a block): the node may have queued it" +
		" behind a proxy that does not answer, or has no peers. The settlement stays broadcast and the market sends it again on each pass; staff check the escrow node."
	if err = a.flagOrder(ctx, tx, &o, note, ""); err != nil {
		return err
	}
	log.Printf("escrow: settlement of order %s: queued or stuck, not seen by the network after %d blocks; staff notified", shortID(orderID), escrowStallBlocks)
	return tx.Commit()
}

// escrowStalls lists, for /admin, the broadcast settlements whose stall alert is open.
func (a *App) escrowStalls(ctx context.Context) ([]EscrowStallView, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT s.order_id,s.kind,s.txid FROM settlements s WHERE s.state='broadcast' AND "+escrowStalledSQL+" ORDER BY s.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EscrowStallView
	for rows.Next() {
		var order, kind string
		var v EscrowStallView
		if err = rows.Scan(&order, &kind, &v.TxID); err != nil {
			return nil, err
		}
		v.Order, v.What = shortID(order), settlementKind(kind)
		out = append(out, v)
	}
	return out, rows.Err()
}

// errBroadcastHeld is sendStored's answer while the Tor-mode gate holds broadcasts: nothing was sent. Its callers check
// broadcastHold first and wait quietly; this keeps any other path from sending.
var errBroadcastHeld = errors.New("escrow: broadcasts wait for the Tor-mode check of the escrow node (A-441)")

// The Tor-mode gate's reasons (fixed text, for the log and /admin).
const (
	torGateUnread   = "the escrow node not checked yet"
	torGateNoProxy  = "no proxy for IPv4 or IPv6"
	torGateNoIPv4   = "no proxy for IPv4"
	torGateNoIPv6   = "no proxy for IPv6"
	torGateOldCore  = "Bitcoin Core older than 31.1"
	torGateMinCore  = 310100 // getnetworkinfo's version of Bitcoin Core 31.1.0
	torGateFailedAs = "getnetworkinfo failed: "
)

// escrowNetInfo is getnetworkinfo as escrow reads it: the version, the peer count (the sync guard, A-439) and each
// network's proxy (the Tor-mode gate). "reachable" is deliberately not read: it reflects configuration, not liveness.
type escrowNetInfo struct {
	Version     int64 `json:"version"`
	Connections int64 `json:"connections"`
	Networks    []struct {
		Name  string `json:"name"`
		Proxy string `json:"proxy"`
	} `json:"networks"`
}

// torGate is why the Tor-mode gate holds broadcasts for a getnetworkinfo answer (or its failure), "" when the node
// sends through a proxy for both IPv4 and IPv6 on Bitcoin Core 31.1 or later.
func torGate(info escrowNetInfo, err error) string {
	if err != nil {
		return torGateFailedAs + errorClassOf(err)
	}
	proxied := map[string]bool{}
	for _, n := range info.Networks {
		proxied[n.Name] = strings.TrimSpace(n.Proxy) != ""
	}
	var why []string
	switch {
	case !proxied["ipv4"] && !proxied["ipv6"]:
		why = append(why, torGateNoProxy)
	case !proxied["ipv4"]:
		why = append(why, torGateNoIPv4)
	case !proxied["ipv6"]:
		why = append(why, torGateNoIPv6)
	}
	if info.Version < torGateMinCore {
		why = append(why, torGateOldCore)
	}
	return strings.Join(why, "; ")
}

// broadcastHold is why escrow broadcasts wait ("" = they may be sent): in APP_MODE=tor, what the Tor-mode gate found in
// this pass (torGateUnread before any pass checked it); in clearnet mode always "".
func (a *App) broadcastHold() string {
	if a.mode != "tor" {
		return ""
	}
	why, ok := a.escrowTorGate.Load().(string)
	if !ok {
		return torGateUnread
	}
	return why
}

// noteTorGate records why (a torGate reason, "" = sends) as the Tor-mode gate's state for this pass and /admin, and logs
// one fixed line when it changes. In memory only.
func (a *App) noteTorGate(why string) {
	prev, _ := a.escrowTorGate.Swap(why).(string)
	switch {
	case prev == why:
	case why == "":
		log.Print("escrow: APP_MODE=tor: the escrow node sends through a proxy for IPv4 and IPv6 on Bitcoin Core 31.1 or later; escrow broadcasts resumed")
	default:
		log.Printf("escrow: APP_MODE=tor: escrow broadcasts wait (%s); start the escrow node with -proxy=<Tor SOCKS address> on Bitcoin Core 31.1 or later, never -connect or -onlynet", why)
	}
}
