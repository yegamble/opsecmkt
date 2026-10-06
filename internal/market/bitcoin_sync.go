package market

import (
	"errors"
	"os"
	"strings"
	"time"
)

// The Bitcoin node sync guard (A-439, P4-R3), shared by the escrow watcher (escrow_watch.go) and the pooled Bitcoin
// check (payments_bitcoin.go). Core's initialblockdownload latches false: a node that lost its peers keeps reporting
// IBD false and headers = blocks while deposits sent since are invisible to it, and a watcher that trusts it expires
// orders that were paid. The rule is stateless (nothing is stored, unlike a high-water mark, which stalls when a chain
// is legitimately replaced: A-147): a node is not synced when it is in initial block download, or its headers are
// ahead of its blocks, or, on a public chain (anything but regtest), it has no peers or its tip block is older than
// BITCOIN_MAX_TIP_AGE. A lone regtest node has no peers and an old tip by design, so regtest is exempt from both.

// bitcoinNode is what the guard reads: getblockchaininfo, and getnetworkinfo's connections (read on public chains
// only, so a regtest rpcwhitelist written before A-439 keeps working).
type bitcoinNode struct {
	Chain           string `json:"chain"`
	IBD             bool   `json:"initialblockdownload"`
	Headers         int64  `json:"headers"`
	Blocks          int64  `json:"blocks"`
	Time            int64  `json:"time"` // the tip block's time (Unix seconds); Core 31.1 reports it in getblockchaininfo
	SignetChallenge string `json:"signet_challenge"`
	Connections     int64  `json:"-"` // -1: getnetworkinfo failed, so the peer count is unknown
}

// The reason classes of a node that is not synced: fixed text, shown on /admin and in one log line per change.
const (
	notSyncedIBD         = "initial block download"
	notSyncedHeaders     = "headers ahead of blocks"
	notSyncedPeers       = "no peers"
	notSyncedPeersUnread = "peer count unreadable"
	notSyncedTip         = "tip older than BITCOIN_MAX_TIP_AGE"
)

// notSynced is the reason class the node is not synced for at now, or "" when it is synced.
func (n bitcoinNode) notSynced(now time.Time, maxAge time.Duration) string {
	switch {
	case n.IBD:
		return notSyncedIBD
	case n.Headers > n.Blocks:
		return notSyncedHeaders
	case n.Chain == "regtest":
		return ""
	case n.Connections < 0:
		return notSyncedPeersUnread
	case n.Connections == 0:
		return notSyncedPeers
	case now.Sub(time.Unix(n.Time, 0)) > maxAge:
		return notSyncedTip
	}
	return ""
}

// parseMaxTipAge reads BITCOIN_MAX_TIP_AGE: a Go duration from 30m to 24h, 2h when blank. Above 24h the check adds
// nothing to Core's own initial block download test before it latches.
func parseMaxTipAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 2 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 30*time.Minute || d > 24*time.Hour {
		return 0, errors.New("BITCOIN_MAX_TIP_AGE must be a duration from 30m to 24h, e.g. 2h: a public-chain Bitcoin node whose tip block is older counts as not synced")
	}
	return d, nil
}

// maxTipAge is BITCOIN_MAX_TIP_AGE for the guard. It is validated at startup (bitcoinFromEnv, escrowConfigFromEnv);
// 2h if it is not valid.
func maxTipAge() time.Duration {
	d, err := parseMaxTipAge(os.Getenv("BITCOIN_MAX_TIP_AGE"))
	if err != nil {
		return 2 * time.Hour
	}
	return d
}
