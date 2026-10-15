package market

import (
	"sync/atomic"

	"time"
)

const slowNodeHold = 5 * time.Second

// slowImport makes the next importdescriptors call (the last RPC of an escrow checkout, made with every row lock held)
// take slowNodeHold; entered receives once that call has started. Later imports answer at once.
func (x *escrowEnv) slowImport() <-chan struct{} {
	entered := make(chan struct{}, 1)
	var used atomic.Bool
	x.core.mu.Lock()
	x.core.beforeImport = func() {
		if used.Swap(true) {
			return
		}
		entered <- struct{}{}
		time.Sleep(slowNodeHold)
	}
	x.core.mu.Unlock()
	return entered
}
