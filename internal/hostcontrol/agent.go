package hostcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const rebootGrace = 30 * time.Second
const rebootCooldown = 10 * time.Minute
const LedgerFileName = "receipts.json"
const maxReceipts = 4096

const agentHelp = `usage: opsecmkt host-control-agent --deployment-dir DIR --app-uid UID --app-gid GID --unlock-ready-file FILE
  --socket /run/opsecmkt-host-control/control.sock
  --state-dir /var/lib/opsecmkt-host-control
Linux root only. Accept only the configured local application UID. A manually
installed policy acknowledges an unlock rehearsal; it is not proof of recovery.
No reboot can be requested until verified encrypted storage and policy are ready.
Accepted requests have a fixed 30-second grace and at least 10-minute cooldown.
Restarted agents never replay a pending request. No targets or commands accepted.
`

type agentConfig struct {
	socket, stateDir, deploymentDir, policyFile string
	appUID, appGID                              int
}

// Run serves the local root agent. verifyStorage must verify the preserved
// deployment's storage bindings; the agent separately verifies state and policy
// encryption. Nothing is installed or rebooted merely by calling Run.
func Run(ctx context.Context, args []string, out, errOut io.Writer, verifyStorage func(context.Context, string) error) error {
	if out == nil {
		out = io.Discard
	}
	f := flag.NewFlagSet("host-control-agent", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var cfg agentConfig
	f.StringVar(&cfg.socket, "socket", "/run/opsecmkt-host-control/control.sock", "local Unix socket")
	f.StringVar(&cfg.stateDir, "state-dir", "/var/lib/opsecmkt-host-control", "private persistent state")
	f.StringVar(&cfg.deploymentDir, "deployment-dir", "", "preserved production installation")
	f.StringVar(&cfg.policyFile, "unlock-ready-file", "", "root-owned unlock rehearsal policy")
	f.IntVar(&cfg.appUID, "app-uid", -1, "authorized application UID")
	f.IntVar(&cfg.appGID, "app-gid", -1, "application socket group")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = io.WriteString(out, agentHelp)
			return err
		}
		return errors.New("host control agent: invalid options")
	}
	if f.NArg() != 0 || cfg.appUID <= 0 || cfg.appGID <= 0 || uint64(cfg.appUID) > 1<<32-1 || uint64(cfg.appGID) > 1<<32-1 || verifyStorage == nil {
		return errors.New("host control agent: explicit positive application UID/GID and storage verifier are required")
	}
	for _, path := range []string{cfg.socket, cfg.stateDir, cfg.deploymentDir, cfg.policyFile} {
		if !safeAbsolute(path) || path == "/" {
			return errors.New("host control agent: clean absolute nonroot paths are required")
		}
	}
	if ctx.Err() != nil {
		return errors.New("host control agent: startup cancelled")
	}
	return runPlatformAgent(ctx, cfg, out, errOut, verifyStorage)
}

func safeAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

func validRequestID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

func validState(state string) bool {
	switch state {
	case "accepted", "executing", "failed", "cancelled", "abandoned", "boot-changed":
		return true
	}
	return false
}

type agentDependencies struct {
	context  context.Context
	now      func() time.Time
	ready    func(context.Context) string
	execute  func(context.Context) error
	schedule func(time.Duration, func()) func()
}

type controller struct {
	mu        sync.Mutex
	state     ledger
	store     *ledgerStore
	bootID    string
	appUID    uint32
	deps      agentDependencies
	pendingID string
	stopTimer func()
	closed    bool
	broken    bool
	shutdown  context.CancelFunc
}

func newController(store *ledgerStore, initial ledger, bootID string, appUID uint32, deps agentDependencies) (*controller, error) {
	if deps.context == nil {
		deps.context = context.Background()
	}
	controlContext, shutdown := context.WithCancel(deps.context)
	deps.context = controlContext
	c := &controller{store: store, state: initial, bootID: bootID, appUID: appUID, deps: deps, shutdown: shutdown}
	changed := false
	for id, record := range c.state.Records {
		if record.Receipt.State != "accepted" && record.Receipt.State != "executing" {
			continue
		}
		record.Receipt.State = "abandoned"
		if record.BootID != bootID {
			record.Receipt.State = "boot-changed"
		}
		c.state.Records[id] = record
		changed = true
	}
	if changed && store.save(c.state) != nil {
		shutdown()
		return nil, errors.New("host control agent: cannot recover durable receipt state")
	}
	return c, nil
}

type peerUIDKey struct{}

func (c *controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid, ok := r.Context().Value(peerUIDKey{}).(uint32)
	if !ok || uid != c.appUID {
		writeFailure(w, http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status" && r.URL.RawQuery == "":
		c.mu.Lock()
		result := c.status(r.Context())
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(result)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/reboot" && r.URL.RawQuery == "":
		if r.Header.Get("Content-Type") != "application/json" {
			writeFailure(w, http.StatusUnsupportedMediaType)
			return
		}
		var request Request
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
		if err != nil || decodeObject(data, &request) != nil {
			writeFailure(w, http.StatusBadRequest)
			return
		}
		receipt, fresh, code := c.reserve(r.Context(), request)
		if code != http.StatusOK && code != http.StatusAccepted {
			writeFailure(w, code)
			return
		}
		w.WriteHeader(code)
		if json.NewEncoder(w).Encode(receipt) != nil || http.NewResponseController(w).Flush() != nil {
			if fresh {
				c.cancel(receipt.RequestID, "abandoned")
			}
			return
		}
		// Starting the timer before this flush could reboot before the caller
		// receives its durable acknowledgement. Duplicates never arm a timer.
		if fresh {
			c.arm(receipt.RequestID)
		}
	default:
		writeFailure(w, http.StatusNotFound)
	}
}

func writeFailure(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, "{\"error\":\"host control refused or could not complete the request\"}\n")
}

func (c *controller) status(ctx context.Context) Status {
	result := Status{Reason: "state-unavailable"}
	if last, ok := c.state.Records[c.state.LastID]; ok {
		receipt := last.Receipt
		result.Last = &receipt
	}
	if pending, ok := c.state.Records[c.pendingID]; ok && c.pendingID != "" {
		receipt := pending.Receipt
		result.Pending, result.Reason = &receipt, "reboot-pending"
		return result
	}
	if c.broken || c.closed || c.deps.context.Err() != nil || len(c.state.Records) >= maxReceipts {
		return result
	}
	if result.Last != nil && c.deps.now().Before(result.Last.AcceptedAt.Add(rebootCooldown)) {
		result.Reason = "cooldown"
		return result
	}
	result.Reason = c.deps.ready(ctx)
	result.Ready = result.Reason == "ready"
	return result
}

func (c *controller) reserve(ctx context.Context, request Request) (Receipt, bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.Version != ProtocolVersion || request.Action != RebootAction || !validRequestID(request.RequestID) || request.RequestedAt.IsZero() {
		return Receipt{}, false, http.StatusBadRequest
	}
	if previous, ok := c.state.Records[request.RequestID]; ok {
		return previous.Receipt, false, http.StatusOK
	}
	now := c.deps.now().UTC()
	age := now.Sub(request.RequestedAt)
	if age < -5*time.Second || age > time.Minute {
		return Receipt{}, false, http.StatusBadRequest
	}
	if c.broken || c.closed || len(c.state.Records) >= maxReceipts {
		return Receipt{}, false, http.StatusServiceUnavailable
	}
	if c.pendingID != "" || c.status(ctx).Reason != "ready" {
		return Receipt{}, false, http.StatusConflict
	}
	if ctx.Err() != nil {
		return Receipt{}, false, http.StatusServiceUnavailable
	}
	receipt := Receipt{RequestID: request.RequestID, State: "accepted", AcceptedAt: now, RebootAt: now.Add(rebootGrace)}
	next := cloneLedger(c.state)
	next.LastID = request.RequestID
	next.Records[request.RequestID] = savedReceipt{Receipt: receipt, BootID: c.bootID}
	if c.store.save(next) != nil {
		c.broken = true
		return Receipt{}, false, http.StatusServiceUnavailable
	}
	c.state, c.pendingID = next, request.RequestID
	return receipt, true, http.StatusAccepted
}

func (c *controller) arm(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.broken || c.deps.context.Err() != nil || c.pendingID != id || c.stopTimer != nil {
		return
	}
	record := c.state.Records[id]
	delay := record.Receipt.RebootAt.Sub(c.deps.now())
	// RebootAt is an earliest time. Slow persistence/response flushing must
	// never consume the operator's full grace after the acknowledgement.
	if delay < rebootGrace {
		delay = rebootGrace
	}
	c.stopTimer = c.deps.schedule(delay, func() { c.perform(id) })
}

func (c *controller) cancel(id, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changeState(id, state)
}

func (c *controller) changeState(id, state string) bool {
	if c.pendingID != id {
		return false
	}
	next := cloneLedger(c.state)
	record := next.Records[id]
	record.Receipt.State = state
	next.Records[id] = record
	if c.store.save(next) != nil {
		c.broken = true
		return false
	}
	c.state = next
	if state != "executing" {
		c.pendingID = ""
		c.stopTimer = nil
	}
	return true
}

func (c *controller) perform(id string) {
	c.mu.Lock()
	if c.closed || c.broken || c.deps.context.Err() != nil || c.pendingID != id || c.state.Records[id].Receipt.State != "accepted" {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(c.deps.context, 10*time.Second)
	ready := c.deps.ready(ctx)
	if ctx.Err() != nil {
		ready = "storage-unverified"
	}
	cancel()
	if ready != "ready" {
		c.changeState(id, "cancelled")
		c.mu.Unlock()
		return
	}
	if !c.changeState(id, "executing") {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	ctx, cancel = context.WithTimeout(c.deps.context, 15*time.Second)
	err := c.deps.execute(ctx)
	cancel()
	if err != nil {
		c.cancel(id, "failed")
	}
}

func (c *controller) close() {
	// Cancellation must reach a readiness check already holding the mutex.
	// Otherwise stopping the agent could lose a race to its delayed executor.
	c.shutdown()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.stopTimer != nil {
		c.stopTimer()
	}
	if c.pendingID != "" && c.state.Records[c.pendingID].Receipt.State == "accepted" {
		c.changeState(c.pendingID, "abandoned")
	}
}
