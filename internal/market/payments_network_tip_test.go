package market

import (
	"context"
	"strings"
	"testing"
)

type namedTestNetworkProvider struct {
	*fakeProvider
	network string
}

func (p namedTestNetworkProvider) Network() string { return p.network }

// A-147: a different test chain may have a much shorter history, but a node
// rolling back within the new chain must still hold the watcher until catch-up.
func TestWatcherTipResetsOnlyWhenProviderNetworkChanges(t *testing.T) {
	for _, tc := range []struct {
		name, currency, old, next string
		noTip                     bool
	}{
		{"BTC/reported", "BTC", "testnet3", "testnet4", false},
		{"BTC/unreported", "BTC", "testnet3", "testnet4", true},
		{"XMR/reported", "XMR", "stagenet", "testnet", false},
		{"XMR/unreported", "XMR", "stagenet", "testnet", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPayEnv(t)
			wallet := p.fake
			if tc.currency == "XMR" {
				wallet = newFakeProvider("XMR", 3)
			}
			p.A.payments[tc.currency] = namedTestNetworkProvider{wallet, tc.old}
			wallet.SetHeight(4800000)
			p.poll()
			if n := p.count("SELECT tip_height FROM payment_status WHERE currency=$1 AND network=$2", tc.currency, tc.old); n != 4800000 {
				t.Fatalf("old chain highest tip = %d", n)
			}

			p.A.payments[tc.currency] = namedTestNetworkProvider{wallet, tc.next}
			if err := p.A.checkNetworkHistory(context.Background()); err != nil {
				t.Fatalf("valid test-network switch refused: %v", err)
			}
			if tc.noTip {
				// Some provider checks succeed without a reported height. The
				// status writer must drop the old network's tip on that pass.
				wallet.SetHeight(0)
				p.poll()
				if n := p.count("SELECT count(*) FROM payment_status WHERE currency=$1 AND network=$2 AND tip_height IS NULL", tc.currency, tc.next); n != 1 {
					t.Fatal("network changed without a tip but old high-water mark remained")
				}
			}
			wallet.SetHeight(250000)
			if tc.currency == "BTC" {
				wallet.Deposit(p.addr, "a147-network-switch", 0, 100000, 3)
			}
			p.poll()
			if n := p.count("SELECT tip_height FROM payment_status WHERE currency=$1 AND network=$2", tc.currency, tc.next); n != 250000 {
				t.Fatalf("new chain highest tip = %d, want 250000", n)
			}
			if tc.currency == "BTC" && p.state(p.order) != statePaid {
				t.Fatal("new-chain deposit was not read")
			}

			wallet.SetHeight(249999)
			if tc.currency == "BTC" {
				wallet.SetConfirmations("a147-network-switch", 0)
			}
			p.poll()
			if n := p.count("SELECT tip_height FROM payment_status WHERE currency=$1", tc.currency); n != 250000 {
				t.Fatalf("same-chain rollback reduced high-water mark to %d", n)
			}
			if reason := p.str("SELECT last_error FROM payment_status WHERE currency=$1", tc.currency); !strings.Contains(reason, "below the highest tip") {
				t.Fatalf("same-chain rollback not held: %q", reason)
			}
			if tc.currency == "BTC" && p.count("SELECT confirmations FROM payments WHERE txid='a147-network-switch'") != 3 {
				t.Fatal("rollback read the stale wallet and changed the deposit")
			}
			wallet.SetHeight(250001)
			p.poll()
			if tc.currency == "BTC" && p.count("SELECT confirmations FROM payments WHERE txid='a147-network-switch'") != 0 {
				t.Fatal("caught-up wallet did not update the deposit")
			}
		})
	}
}

// QA10W-2: migration 056 marked only negative depths as already announced.
// A flagged credited deposit at depth zero had also been reported before upgrade.
func TestMigration096PreservesReportedZeroConfirmationEpisode(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "a147-depth-zero")
	if _, err := p.DB.Exec("UPDATE payments SET confirmations=0,flagged=true,regress_notice=false WHERE txid='a147-depth-zero'"); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		txid              string
		confs             int
		credited, flagged bool
	}{
		{"a147-unflagged", 0, true, false},
		{"a147-uncredited", 0, false, true},
		{"a147-depth-one", 1, true, true},
	} {
		if _, err := p.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited,flagged)
			VALUES($1,'BTC',$2,0,$3,100000,$4,$5,$6)`, p.order, row.txid, p.addr, row.confs, row.credited, row.flagged); err != nil {
			t.Fatal(err)
		}
	}
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list {
		if m.Version != 96 {
			continue
		}
		found = true
		for range 2 { // an already-applied migration must remain harmless
			if _, err := p.DB.Exec(m.SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("migration 096 missing")
	}
	for txid, want := range map[string]bool{
		"a147-depth-zero": true, "a147-unflagged": false,
		"a147-uncredited": false, "a147-depth-one": false,
	} {
		if got := p.str("SELECT regress_notice::text FROM payments WHERE txid=$1", txid); (got == "true") != want {
			t.Fatalf("%s regress_notice=%s, want %v", txid, got, want)
		}
	}
	if _, err := p.DB.Exec("DELETE FROM payments WHERE txid<>$1", "a147-depth-zero"); err != nil {
		t.Fatal(err)
	}
	p.fake.SetConfirmations("a147-depth-zero", 0)
	p.poll()
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Credited TESTNET deposit %Moderator review required.'", p.order); n != 0 {
		t.Fatalf("previously reported zero-confirmation deposit announced again: %d", n)
	}
}
