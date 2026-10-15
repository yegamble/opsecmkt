package market

// signerDown stops the fake signer (nothing listens on its socket: errSignerUnreachable) or starts it again.
func signerDown(f *fakeSigner, down bool) {
	if down {
		f.stop()
	} else {
		f.start()
	}
}

// signerGarbled makes the fake signer answer what the app does not understand (a refusal it has no class for), or
// answer properly again: like a signer that is down, a signer error that is not a pin mismatch.
func signerGarbled(f *fakeSigner, garbled bool) {
	f.set(func(f *fakeSigner) {
		f.raw = ""
		if garbled {
			f.raw = `{"error":"unavailable"}`
		}
	})
}

// solvable reports whether the fake watch wallet holds addr with its keys (a ranged import held it first).
func (x *escrowEnv) solvable(addr string) bool {
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	return x.core.rangedPubkeys[addr] != nil || x.core.watchPubkeys[addr] != nil
}
