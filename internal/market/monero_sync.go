package market

import (
	"errors"
	"os"
	"strings"
	"time"
)

// The Monero daemon sync guard (A-449, P4-R15), shared by the Monero escrow watcher (escrow_xmr_watch.go) and the pooled
// Monero check (payments_monero.go), as bitcoin_sync.go is for Bitcoin (A-439). A monerod that lost its peers can keep
// reporting synchronized with target_height not above height while deposits sent since are invisible to it, and a
// watcher that trusts it expires orders that were paid (A-439 found the pooled Monero check read only target_height).
// The rule is stateless (nothing is stored, unlike a high-water mark, which stalls when a chain is legitimately
// replaced: A-147): a daemon is not synced when get_info reports synchronized false or a target_height above its
// height, or, on a public network type (anything but fakechain), no peers or a top block (get_last_block_header) older
// than MONERO_MAX_TIP_AGE. No peers means get_info's offline flag, or no connections (outgoing plus incoming) on an
// unrestricted RPC. A restricted RPC (--restricted-rpc: the bundled monero service, and most remote nodes) reports both
// counts as 0 whatever its peers (monerod 0.18.5.1, probed with a peered stagenet pair, A-449), so there the counts are
// not read: offline and the tip's age cover a daemon cut off from the network (one that lost its peers stops receiving
// blocks). A lone fakechain daemon has no peers and an old tip by design, so the fakechain is exempt from both checks,
// and get_last_block_header is never called on it.

// moneroNode is what the guard reads: get_info, and on a public network type get_last_block_header's timestamp.
type moneroNode struct {
	NetType              string
	Synchronized         bool
	Height, TargetHeight int64
	Offline, Restricted  bool  // get_info's offline and restricted
	Connections          int64 // outgoing_connections_count + incoming_connections_count (hidden, 0, on a restricted RPC)
	TipTime              int64 // the top block's timestamp (Unix seconds), read on public network types only; -1 unknown
}

// The reason classes of a Monero daemon that is not synced: fixed text, shown on /admin and in one log line per change.
// A daemon with no connections shares Bitcoin's class (notSyncedPeers).
const (
	xmrNotSyncedFlag   = "daemon not synchronized"
	xmrNotSyncedTarget = "target height above height"
	xmrNotSyncedTip    = "tip older than MONERO_MAX_TIP_AGE"
	xmrNotSyncedTipBad = "tip time unreadable"
)

// notSynced is the reason class the daemon is not synced for at now, or "" when it is synced.
func (n moneroNode) notSynced(now time.Time, maxAge time.Duration) string {
	switch {
	case !n.Synchronized:
		return xmrNotSyncedFlag
	case n.TargetHeight > n.Height:
		return xmrNotSyncedTarget
	case n.NetType == "fakechain":
		return ""
	case n.Offline, !n.Restricted && n.Connections <= 0:
		return notSyncedPeers
	case n.TipTime < 0:
		return xmrNotSyncedTipBad
	case now.Sub(time.Unix(n.TipTime, 0)) > maxAge:
		return xmrNotSyncedTip
	}
	return ""
}

// parseMoneroMaxTipAge reads MONERO_MAX_TIP_AGE: a Go duration from 20m to 24h, 1h when blank (BITCOIN_MAX_TIP_AGE's
// parsing; a Monero block is due every 2 minutes, so 20 minutes is ten blocks missed).
func parseMoneroMaxTipAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 20*time.Minute || d > 24*time.Hour {
		return 0, errors.New("MONERO_MAX_TIP_AGE must be a duration from 20m to 24h, e.g. 1h: a Monero daemon on a public network whose top block is older counts as not synced")
	}
	return d, nil
}

// moneroMaxTipAge is MONERO_MAX_TIP_AGE for the guard. It is validated at startup (moneroFromEnv,
// escrowXMRConfigFromEnv); 1h if it is not valid.
func moneroMaxTipAge() time.Duration {
	d, err := parseMoneroMaxTipAge(os.Getenv("MONERO_MAX_TIP_AGE"))
	if err != nil {
		return time.Hour
	}
	return d
}
