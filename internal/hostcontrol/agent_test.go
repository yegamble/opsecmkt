package hostcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testBoot = "00112233-4455-6677-8899-aabbccddeeff"
const nextBoot = "11223344-5566-7788-99aa-bbccddeeff00"
const testID = "00112233445566778899aabbccddeeff"
const nextID = "112233445566778899aabbccddeeff00"

type harness struct {
	c                     *controller
	store                 *ledgerStore
	now                   time.Time
	ready                 string
	readyChecks, executes int
	executionError        error
	timers                []func()
	delays                []time.Duration
}

func testFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("invalid test state")
	}
	return nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{store: &ledgerStore{dir: t.TempDir(), checkFile: testFile}, now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC), ready: "ready"}
	initial, err := h.store.load("/srv/preserved", true)
	if err != nil {
		t.Fatal(err)
	}
	h.c, err = newController(h.store, initial, testBoot, 1000, agentDependencies{
		now: func() time.Time { return h.now },
		ready: func(context.Context) string {
			h.readyChecks++
			return h.ready
		},
		execute: func(context.Context) error { h.executes++; return h.executionError },
		schedule: func(delay time.Duration, action func()) func() {
			h.timers, h.delays = append(h.timers, action), append(h.delays, delay)
			return func() {}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) request(id string) Request {
	return Request{Version: ProtocolVersion, Action: RebootAction, RequestID: id, RequestedAt: h.now}
}

func (h *harness) post(request Request) *httptest.ResponseRecorder {
	data, _ := json.Marshal(request)
	return h.postRaw(string(data), 1000)
}

func (h *harness) postRaw(body string, uid uint32) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/reboot", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), peerUIDKey{}, uid))
	response := httptest.NewRecorder()
	h.c.ServeHTTP(response, req)
	return response
}

func TestReceiptDurableAndResponseFlushedBeforeTimer(t *testing.T) {
	h := newHarness(t)
	response := httptest.NewRecorder()
	h.c.deps.schedule = func(delay time.Duration, _ func()) func() {
		if !response.Flushed {
			t.Fatal("timer armed before HTTP acknowledgement flushed")
		}
		durable, err := h.store.load("/srv/preserved", false)
		if err != nil || durable.Records[testID].Receipt.State != "accepted" {
			t.Fatal("timer armed without durable receipt")
		}
		if delay != 30*time.Second {
			t.Fatalf("grace changed: %v", delay)
		}
		return func() {}
	}
	data, _ := json.Marshal(h.request(testID))
	req := httptest.NewRequest(http.MethodPost, "/v1/reboot", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), peerUIDKey{}, uint32(1000)))
	h.c.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted || !response.Flushed || h.executes != 0 {
		t.Fatalf("unexpected acceptance: %d %v %d", response.Code, response.Flushed, h.executes)
	}
	var receipt Receipt
	if json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.RequestID != testID || receipt.State != "accepted" || !receipt.RebootAt.Equal(receipt.AcceptedAt.Add(30*time.Second)) {
		t.Fatal("invalid acknowledgement")
	}
}

func TestOnlyExactAuthorizedLocalUIDCanRequest(t *testing.T) {
	h := newHarness(t)
	data, _ := json.Marshal(h.request(testID))
	for _, uid := range []uint32{0, 999, 1001} {
		if response := h.postRaw(string(data), uid); response.Code != http.StatusForbidden {
			t.Fatalf("unauthorized peer accepted: %d", uid)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/reboot", bytes.NewReader(data))
	response := httptest.NewRecorder()
	h.c.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(h.timers) != 0 || len(h.c.state.Records) != 0 {
		t.Fatal("missing peer proof reached reboot scheduling")
	}
}

func TestStrictFreshBoundedRequestValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"version": func(r *Request) { r.Version = 2 }, "action": func(r *Request) { r.Action = "reboot-payments-host" },
		"id": func(r *Request) { r.RequestID = "invalid" }, "uppercase": func(r *Request) { r.RequestID = strings.ToUpper(testID) },
		"missing time": func(r *Request) { r.RequestedAt = time.Time{} },
		"stale":        func(r *Request) { r.RequestedAt = r.RequestedAt.Add(-61 * time.Second) },
		"future":       func(r *Request) { r.RequestedAt = r.RequestedAt.Add(6 * time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			r := h.request(testID)
			mutate(&r)
			if response := h.post(r); response.Code != http.StatusBadRequest || len(h.timers) != 0 {
				t.Fatalf("invalid request accepted: %s %d", name, response.Code)
			}
		})
	}
	for _, body := range []string{`{}`, `{"extra":"field"}`, `{} {}`, `{"version":1,"version":1}`, strings.Repeat(" ", 2049) + `{}`} {
		h := newHarness(t)
		if response := h.postRaw(body, 1000); response.Code != http.StatusBadRequest || len(h.timers) != 0 {
			t.Fatal("malformed or oversized body accepted")
		}
	}
}

func TestDuplicateRequestsNeverScheduleAgainIncludingAfterRestart(t *testing.T) {
	h := newHarness(t)
	r := h.request(testID)
	if h.post(r).Code != http.StatusAccepted || h.post(r).Code != http.StatusOK || len(h.timers) != 1 {
		t.Fatal("duplicate scheduled a second timer")
	}
	initial, err := h.store.load("/srv/preserved", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, boot := range []string{testBoot, nextBoot} {
		restarted, err := newController(h.store, cloneLedger(initial), boot, 1000, h.c.deps)
		if err != nil {
			t.Fatal(err)
		}
		h.now = h.now.Add(time.Hour)
		receipt, fresh, code := restarted.reserve(context.Background(), r)
		want := "abandoned"
		if boot != testBoot {
			want = "boot-changed"
		}
		if code != http.StatusOK || fresh || receipt.State != want || len(h.timers) != 1 || h.executes != 0 {
			t.Fatalf("restart replayed: %s %d %v %s", boot, code, fresh, receipt.State)
		}
	}
}

func TestOnePendingAndTenMinuteCooldown(t *testing.T) {
	h := newHarness(t)
	h.executionError = errors.New("synthetic executor failed")
	if h.post(h.request(testID)).Code != http.StatusAccepted || h.post(h.request(nextID)).Code != http.StatusConflict {
		t.Fatal("multiple pending requests accepted")
	}
	h.now = h.now.Add(30 * time.Second)
	h.timers[0]() // This dependency is always fake; no OS executor is called.
	if h.executes != 1 || h.c.state.Records[testID].Receipt.State != "failed" {
		t.Fatal("fake execution failure not recorded")
	}
	h.now = h.now.Add(9*time.Minute + 29*time.Second)
	if h.post(h.request(nextID)).Code != http.StatusConflict {
		t.Fatal("cooldown shorter than ten minutes")
	}
	h.now = h.now.Add(time.Second)
	if h.post(h.request(nextID)).Code != http.StatusAccepted || len(h.timers) != 2 || h.delays[1] != 30*time.Second {
		t.Fatal("subsequent request did not receive its own grace timer")
	}
}

func TestPolicyAndStorageRecheckedImmediatelyBeforeExecution(t *testing.T) {
	for _, reason := range []string{"storage-unverified", "unlock-not-rehearsed", "policy-unavailable"} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			if h.post(h.request(testID)).Code != http.StatusAccepted {
				t.Fatal("acceptance failed")
			}
			h.ready = reason
			h.now = h.now.Add(30 * time.Second)
			h.timers[0]()
			if h.executes != 0 || h.readyChecks != 2 || h.c.state.Records[testID].Receipt.State != "cancelled" {
				t.Fatal("revoked readiness reached executor")
			}
		})
	}
}

func TestUnreadyOrUnavailableStateNeverSchedules(t *testing.T) {
	h := newHarness(t)
	h.ready = "unlock-not-rehearsed"
	if h.post(h.request(testID)).Code != http.StatusConflict || len(h.timers) != 0 {
		t.Fatal("unrehearsed request accepted")
	}
	h.ready = "ready"
	h.store.checkFile = func(string) error { return errors.New("private path must never be reported") }
	response := h.post(h.request(testID))
	if response.Code != http.StatusServiceUnavailable || len(h.timers) != 0 || strings.Contains(response.Body.String(), "private path") || !h.c.broken {
		t.Fatal("state persistence failure did not fail closed")
	}
}

func TestAcceptedRequestDoesNotReplayAfterAgentShutdown(t *testing.T) {
	h := newHarness(t)
	if h.post(h.request(testID)).Code != http.StatusAccepted {
		t.Fatal("acceptance failed")
	}
	h.c.close()
	h.now = h.now.Add(30 * time.Second)
	h.timers[0]()
	if h.executes != 0 || h.c.state.Records[testID].Receipt.State != "abandoned" {
		t.Fatal("stopped agent executed a reboot")
	}
}

func TestShutdownDuringFinalReadinessCannotReachExecutor(t *testing.T) {
	h := newHarness(t)
	if h.post(h.request(testID)).Code != http.StatusAccepted {
		t.Fatal("acceptance failed")
	}
	h.c.deps.ready = func(context.Context) string {
		h.c.shutdown() // Simulate service termination during its last check.
		return "ready"
	}
	h.now = h.now.Add(30 * time.Second)
	h.timers[0]()
	if h.executes != 0 || h.c.state.Records[testID].Receipt.State != "cancelled" {
		t.Fatal("agent termination lost race to delayed reboot")
	}
}

type failedFlush struct{ *httptest.ResponseRecorder }

func (failedFlush) FlushError() error { return errors.New("synthetic write failure") }

func TestFlushFailureDoesNotArmTimer(t *testing.T) {
	h := newHarness(t)
	data, _ := json.Marshal(h.request(testID))
	req := httptest.NewRequest(http.MethodPost, "/v1/reboot", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), peerUIDKey{}, uint32(1000)))
	h.c.ServeHTTP(failedFlush{httptest.NewRecorder()}, req)
	if len(h.timers) != 0 || h.c.state.Records[testID].Receipt.State != "abandoned" {
		t.Fatal("timer armed after failed acknowledgement")
	}
}

type advancingFlush struct {
	*httptest.ResponseRecorder
	advance func()
}

func (w advancingFlush) FlushError() error {
	w.ResponseRecorder.Flush()
	w.advance()
	return nil
}

func TestSlowFlushCannotConsumePostAcknowledgementGrace(t *testing.T) {
	h := newHarness(t)
	data, _ := json.Marshal(h.request(testID))
	req := httptest.NewRequest(http.MethodPost, "/v1/reboot", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), peerUIDKey{}, uint32(1000)))
	writer := advancingFlush{ResponseRecorder: httptest.NewRecorder(), advance: func() { h.now = h.now.Add(time.Minute) }}
	h.c.ServeHTTP(writer, req)
	if len(h.timers) != 1 || h.delays[0] < rebootGrace || !writer.Flushed || h.executes != 0 {
		t.Fatal("slow flush allowed immediate reboot after acknowledgement")
	}
}

func TestReceiptCapacityFailsClosedWithoutExpiringIDs(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxReceipts; i++ {
		id := fmt.Sprintf("%032x", i)
		h.c.state.Records[id] = savedReceipt{Receipt: Receipt{RequestID: id, State: "abandoned", AcceptedAt: h.now.Add(-time.Hour)}, BootID: testBoot}
	}
	if _, _, code := h.c.reserve(context.Background(), h.request(testID)); code != http.StatusServiceUnavailable || h.c.status(context.Background()).Ready || len(h.c.state.Records) != maxReceipts {
		t.Fatal("receipt capacity expired old IDs or allowed another request")
	}
}

func TestReadyPolicyCannotInitializeMissingReceiptHistory(t *testing.T) {
	store := &ledgerStore{dir: t.TempDir(), checkFile: testFile}
	if _, err := store.load("/srv/preserved", false); err == nil {
		t.Fatal("ready policy recreated missing idempotency history")
	}
	if _, err := os.Lstat(filepath.Join(store.dir, LedgerFileName)); !os.IsNotExist(err) {
		t.Fatal("missing-history refusal wrote a ledger")
	}
	if _, err := store.load("/srv/preserved", true); err != nil {
		t.Fatal("initial unready setup could not create history")
	}
	if _, err := store.load("/srv/other-install", false); err == nil {
		t.Fatal("another deployment adopted receipt history")
	}
}

func TestCorruptOrSymlinkReceiptHistoryRefused(t *testing.T) {
	store := &ledgerStore{dir: t.TempDir(), checkFile: testFile}
	path := filepath.Join(store.dir, LedgerFileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"records":{}} trailing`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load("/srv/preserved", true); err == nil {
		t.Fatal("corrupt receipt history accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store.dir, "other"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load("/srv/preserved", true); err == nil {
		t.Fatal("symlink receipt history accepted")
	}
}

func TestHelpAndInvalidAgentOptionsCannotReachExecutor(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"--help"}, &output, &output, nil); err != nil || !strings.Contains(output.String(), "30-second") {
		t.Fatal("agent help unavailable")
	}
	for _, args := range [][]string{{}, {"--command", "private-value-must-not-echo"}, {"--app-uid", "0", "--app-gid", "1"}} {
		output.Reset()
		err := Run(context.Background(), args, &output, &output, nil)
		if err == nil || strings.Contains(err.Error()+output.String(), "private-value-must-not-echo") {
			t.Fatal("invalid agent options accepted or leaked")
		}
	}
}
