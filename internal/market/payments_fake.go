package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// fakeProvider is a deterministic in-memory test-network wallet. It is constructed only by Go tests;
// there is no environment switch that enables it in a running server.
type fakeProvider struct {
	currency string
	confs    int

	mu      sync.Mutex
	outputs []Incoming
	sends   []fakeSend
	sendErr error
	// sendHook, when set, runs inside Send before recording (tests use it to observe concurrency).
	sendHook func()
	// sendCtxErrs records ctx.Err() as Send saw it after sendHook (shutdown must not cancel a send).
	sendCtxErrs []error
	// incomingErr fails Incoming (wallet outage); checkErr and syncing drive Check.
	incomingErr, checkErr error
	syncing               bool
	checks                int
}

type fakeSend struct {
	To     string
	Amount int64
	TxID   string
}

func newFakeProvider(currency string, confs int) *fakeProvider {
	return &fakeProvider{currency: currency, confs: confs}
}

func (f *fakeProvider) Currency() string   { return f.currency }
func (f *fakeProvider) Network() string    { return "fake" }
func (f *fakeProvider) Confirmations() int { return f.confs }

func (f *fakeProvider) NewAddress(_ context.Context, orderID string) (string, error) {
	return "fake-testnet-" + orderID[:min(8, len(orderID))], nil
}

// Deposit adds an output paying addr.
func (f *fakeProvider) Deposit(addr, txid string, index, amount, confirmations int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs = append(f.outputs, Incoming{Address: addr, TxID: txid, Index: index, Amount: amount, Confirmations: confirmations})
}

// SetConfirmations updates every output of txid (negative = conflicted).
func (f *fakeProvider) SetConfirmations(txid string, confirmations int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.outputs {
		if f.outputs[i].TxID == txid {
			f.outputs[i].Confirmations = confirmations
		}
	}
}

// Drop removes txid from the wallet view, as a wallet does for a replaced or evicted transaction.
func (f *fakeProvider) Drop(txid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.outputs[:0]
	for _, o := range f.outputs {
		if o.TxID != txid {
			kept = append(kept, o)
		}
	}
	f.outputs = kept
}

// Check reports the scripted wallet state (see providerChecker).
func (f *fakeProvider) Check(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.syncing, f.checkErr
}

// SetWallet scripts the wallet: an Incoming error, a Check error and the syncing flag.
func (f *fakeProvider) SetWallet(incomingErr, checkErr error, syncing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incomingErr, f.checkErr, f.syncing = incomingErr, checkErr, syncing
}

func (f *fakeProvider) Incoming(_ context.Context, addresses []string) ([]Incoming, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incomingErr != nil {
		return nil, f.incomingErr
	}
	want := map[string]bool{}
	for _, a := range addresses {
		want[a] = true
	}
	var out []Incoming
	for _, o := range f.outputs {
		if want[o.Address] {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeProvider) Send(ctx context.Context, to string, amount int64) (string, error) {
	if f.sendHook != nil {
		f.sendHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCtxErrs = append(f.sendCtxErrs, ctx.Err())
	if f.sendErr != nil {
		return "", f.sendErr
	}
	if !f.valid(to) {
		return "", errors.New("fake wallet: invalid address")
	}
	s := fakeSend{To: to, Amount: amount, TxID: fmt.Sprintf("fake-payout-%d", len(f.sends)+1)}
	f.sends = append(f.sends, s)
	return s.TxID, nil
}

func (f *fakeProvider) Sends() []fakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSend(nil), f.sends...)
}

func (f *fakeProvider) valid(addr string) bool {
	return strings.HasPrefix(addr, "fake-testnet-") && len(addr) <= 64
}

func (f *fakeProvider) ValidAddress(addr string) bool { return f.valid(addr) }
