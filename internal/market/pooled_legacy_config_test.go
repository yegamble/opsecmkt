package market

import (
	"context"
	"strings"
	"testing"
)

func TestObsoletePooledEnvironmentIsRefusedWithoutEchoingValues(t *testing.T) {
	base := map[string]string{
		"APP_SECRET_KEY": strings.Repeat("a", 64),
	}
	get := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	for _, name := range []string{"BITCOIN_RPC_URL", "BITCOIN_WALLET", "MONERO_WALLET_RPC_URL", "MONERO_RPC_URL"} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			secret := "http://user:do-not-print@wallet.example:1234"
			values[name] = secret
			_, err := loadStartupConfig(get(values), "clearnet")
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("obsolete pooled setting %s was not refused: %v", name, err)
			}
			if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "no database changes were made") {
				t.Fatalf("refusal exposed a value or omitted the no-change guarantee: %v", err)
			}
		})
	}
}

func TestNewRefusesObsoletePooledEnvironmentBeforeDatabaseAndRPC(t *testing.T) {
	t.Setenv("APP_MODE", "clearnet")
	t.Setenv("APP_SECRET_KEY", testSetupToken)
	t.Setenv("SETUP_TOKEN", "")
	t.Setenv("DATABASE_URL", "")
	secret := "http://user:private-rpc-password@wallet.example:1234"
	t.Setenv("BITCOIN_RPC_URL", secret)
	app, err := New(context.Background(), false)
	if app != nil || err == nil || !strings.Contains(err.Error(), "BITCOIN_RPC_URL") || strings.Contains(err.Error(), secret) {
		t.Fatalf("New result = (%v, %v), want a value-free pooled configuration refusal before database setup", app, err)
	}
}

func TestSharedEscrowEnvironmentIsAccepted(t *testing.T) {
	values := map[string]string{
		"APP_SECRET_KEY":            testSetupToken,
		"DATABASE_URL":              "postgres://market:example@db/market",
		"ESCROW_BTC_RPC_URL":        "http://escrow-user:secret@bitcoin:38332",
		"ESCROW_XMR_WALLET_RPC_URL": "http://escrow-wallet:secret@monero:18083",
		"BITCOIN_CHAIN":             "signet",
		"MONERO_NETWORK":            "stagenet",
		"BITCOIN_MAX_TIP_AGE":       "2h",
		"MONERO_MAX_TIP_AGE":        "2h",
		"PAYMENT_CONFIRMATIONS_BTC": "3",
		"PAYMENT_CONFIRMATIONS_XMR": "10",
		"PAYMENT_EXPIRY":            "24h",
		"PAYMENT_POLL_INTERVAL":     "30s",
	}
	_, err := loadStartupConfig(func(name string) string { return values[name] }, "clearnet")
	if err != nil {
		t.Fatalf("shared escrow configuration was refused: %v", err)
	}
}
