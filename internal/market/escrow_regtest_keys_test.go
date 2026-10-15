package market

import (
	"context"

	"testing"
	"time"
)

const regtestOperatorTimeout = 120 * time.Second

// regtestOperator checks that every URL in urls reports the legs' chain (checkRealChain, before any wallet call) and
// returns a helper that calls Core as the operator user (full rights, standing in for the operator's own bitcoin-cli),
// and that user's client (for mustMine and matureFunds).
func regtestOperator(t *testing.T, opURL string, urls map[string]string) (func(wallet, method string, out any, params ...any), *rpcClient) {
	t.Helper()
	ctx := context.Background()
	op, err := newRPCClient("regtest operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	urls["operator"] = opURL
	for name, u := range urls {
		c, err := newRPCClient("regtest "+name, u, "1.0", false)
		if err != nil {
			t.Fatal(err)
		}
		checkRealChain(t, ctx, name, c)
	}
	return func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := op.callWithin(ctx, regtestOperatorTimeout, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}, op
}

func errorCauseOrNone(err error) string {
	if err == nil {
		return "no error"
	}
	return watcherCause(err)
}
