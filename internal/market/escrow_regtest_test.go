package market

import (
	"context"

	"fmt"
	"os"

	"testing"
	"time"
)

const (
	privateSignetChallenge   = "51"
	privateSignetLabel       = "private signet, not public"
	privateSignetMinerWallet = "private-signet-miner" // node A's wallet holding mature coins (TestEscrowPrivateSignetPrepare)
	signetMaxTries           = 2000000000             // generatetoaddress maxtries on signet: one int32 budget for the call's blocks
	signetMineTimeout        = 15 * time.Minute       // one generatetoaddress call on signet
)

// realChainNet is the chain the real-chain legs run on: "regtest", or "signet" (the private signet).
func realChainNet() string {
	if os.Getenv("ESCROW_REGTEST_CHAIN") == "signet" {
		return "signet"
	}
	return "regtest"
}

// realChainName names it in refusals and log lines.
func realChainName() string {
	if realChainNet() == "signet" {
		return "the private signet (signet_challenge 51; " + privateSignetLabel + ")"
	}
	return "regtest"
}

// checkRealChain refuses, before any wallet call, a node that is not on the legs' chain: on regtest the chain name, on
// the private signet the chain name and signet_challenge 51 (so public signet is refused).
func checkRealChain(t *testing.T, ctx context.Context, name string, c *rpcClient) {
	t.Helper()
	var info struct {
		Chain     string `json:"chain"`
		Challenge string `json:"signet_challenge"`
	}
	err := c.call(ctx, "", "getblockchaininfo", []any{}, &info)
	if err != nil || info.Chain != realChainNet() || realChainNet() == "signet" && info.Challenge != privateSignetChallenge {
		t.Fatalf("refusing: the %s node URL is not on %s (or is unreachable)", name, realChainName())
	}
}

// mineBlocks mines n blocks to addr with generatetoaddress on the node c reaches, on chain ("regtest" or "signet"), and
// returns their hashes; it is an error unless exactly n were mined. Regtest needs one call. On signet every block needs
// real proof of work: Core's default maxtries (1000000) returns [] without an error, and maxtries is one budget for
// all of a call's blocks, so each call passes signetMaxTries and asks for the blocks still missing, and each must mine
// at least one block and no more than it asked for: an empty answer is never taken as a block. A signet call may take
// minutes (seconds per block), so it waits up to signetMineTimeout; a regtest call waits up to regtestOperatorTimeout (QA4W5-7).
func mineBlocks(ctx context.Context, c *rpcClient, chain string, n int, addr string) ([]string, error) {
	var mined []string
	for len(mined) < n {
		want := n - len(mined)
		params, timeout := []any{want, addr}, regtestOperatorTimeout
		if chain == "signet" {
			params, timeout = append(params, signetMaxTries), signetMineTimeout
		}
		var got []string
		if err := c.callWithin(ctx, timeout, "", "generatetoaddress", params, &got); err != nil {
			return mined, err
		}
		if len(got) == 0 || len(got) > want {
			return mined, fmt.Errorf("generatetoaddress %d on %s mined %d blocks", want, chain, len(got))
		}
		mined = append(mined, got...)
		if chain != "signet" && len(mined) != n {
			return mined, fmt.Errorf("generatetoaddress %d on %s mined %d blocks", n, chain, len(mined))
		}
	}
	return mined, nil
}

// mustMine is mineBlocks on the legs' chain, failing the leg on any error.
func mustMine(t *testing.T, ctx context.Context, c *rpcClient, n int, addr string) []string {
	t.Helper()
	mined, err := mineBlocks(ctx, c, realChainNet(), n, addr)
	if err != nil {
		t.Fatalf("mining %d blocks: %v", n, err)
	}
	return mined
}

// privateSignetMiner is the operator client of node A, the private signet's other node.
func privateSignetMiner(t *testing.T, ctx context.Context) *rpcClient {
	t.Helper()
	miner, err := newRPCClient("private signet miner", os.Getenv("ESCROW_REGTEST_MINER_RPC_URL"), "1.0", false)
	if err != nil {
		t.Fatalf("%s: set ESCROW_REGTEST_MINER_RPC_URL (scripts/test-escrow-regtest.sh --private-signet)", privateSignetLabel)
	}
	checkRealChain(t, ctx, "miner", miner)
	return miner
}
