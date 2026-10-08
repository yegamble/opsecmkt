package market

import (
	"strconv"
	"testing"
	"time"
)

func TestWriteLimiterIndependentOfBlockingAuthenticationCapacity(t *testing.T) {
	a := &App{limits: make(map[string]bucket)}
	until := time.Now().Add(10 * time.Minute)
	for i := range 4096 {
		a.limits["auth:blocked:"+strconv.Itoa(i)] = bucket{Count: 10, Limit: 10, Until: until}
	}
	if a.allow("auth:new", 10) {
		t.Fatal("saturated authentication table admitted a new key")
	}
	if !a.allowWrite("buyer") {
		t.Fatal("blocking authentication entries refused the buyer's write")
	}
	if !a.allowWriteBudget("upload:buyer", 20) {
		t.Fatal("blocking authentication entries refused the buyer's upload budget")
	}
	if len(a.limits) != 4096 || len(a.writeLimits) != 2 || a.writeLimits["buyer"].Count != 1 {
		t.Fatalf("tables shared capacity: auth=%d write=%d", len(a.limits), len(a.writeLimits))
	}
}

func TestWriteLimiterSaturatedEntriesDoNotResetAndExpiryReclaimsCapacity(t *testing.T) {
	a := &App{limits: make(map[string]bucket), writeLimits: make(map[string]bucket)}
	until := time.Now().Add(10 * time.Minute)
	for i := range 4096 {
		a.writeLimits["blocked:"+strconv.Itoa(i)] = bucket{Count: 100, Limit: 100, Until: until}
	}
	if a.allowWrite("new") || a.allowWrite("blocked:0") {
		t.Fatal("saturated write table admitted a new or blocked account")
	}
	if len(a.writeLimits) != 4096 || a.writeLimits["blocked:0"].Count < 100 {
		t.Fatal("saturated write table evicted or reset a blocking counter")
	}
	if !a.allow("auth:new", 10) {
		t.Fatal("saturated writes refused an authentication attempt")
	}
	a.writeLimits["blocked:1"] = bucket{Count: 100, Limit: 100, Until: time.Now().Add(-time.Second)}
	if !a.allowWrite("new") || len(a.writeLimits) != 4096 {
		t.Fatal("expired write budget was not reclaimed")
	}
	if _, ok := a.writeLimits["blocked:1"]; ok {
		t.Fatal("expired write key remains in table")
	}
}
