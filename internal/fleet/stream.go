package fleet

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// pingInterval is how often the master sends a ping frame down an otherwise
// idle stream connection, keeping it (and anything in between) from being
// mistaken for dead -- and, via streamIdleTimeout in child.go, how the child
// judges a connection to have gone silent. It's read repeatedly by
// long-running background goroutines on both the master (the ping ticker)
// and the child (the read-idle watchdog), so it's an atomic.Int64
// (nanoseconds) rather than a plain var: a test shortening it needs that to
// be race-safe even while such a goroutine is already running, not merely
// safe at the moment the goroutine was started.
var pingInterval atomic.Int64

func init() { pingInterval.Store(int64(20 * time.Second)) }

// pingIntervalDuration reads the current ping interval.
func pingIntervalDuration() time.Duration { return time.Duration(pingInterval.Load()) }

// setPingInterval sets the ping interval and returns the previous value, so
// a test can restore it with `defer setPingInterval(setPingInterval(d))` or
// `old := setPingInterval(d); t.Cleanup(func() { setPingInterval(old) })`.
func setPingInterval(d time.Duration) time.Duration {
	return time.Duration(pingInterval.Swap(int64(d)))
}

// hubQueueSize bounds how many frames the master queues for one connected
// node before it starts dropping the newest ones.
const hubQueueSize = 256

// maxRPCResultBytes bounds a child's POSTed RPC result body.
const maxRPCResultBytes = 1 << 20

// nodeConn is one node's live stream connection.
type nodeConn struct {
	ch   chan Frame
	done chan struct{}
}

// Hub fans lease/receipt/silence/managed_config/rpc frames out to connected
// children over their stream connection (GET PathStream) and receives RPC
// results they POST back (PathRPC). At most one connection per node is kept:
// a newer connection replaces (and closes) an older one from the same node.
type Hub struct {
	mu    sync.Mutex
	conns map[string]*nodeConn

	onConnect   func(nodeID string)
	onRPCResult func(nodeID, id string, body []byte)

	logf func(format string, args ...any)

	dropMu   sync.Mutex
	lastDrop map[string]time.Time
}

// NewHub builds a Hub. A nil logf discards log lines.
func NewHub(logf func(string, ...any)) *Hub {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Hub{
		conns:       map[string]*nodeConn{},
		logf:        logf,
		lastDrop:    map[string]time.Time{},
		onConnect:   func(string) {},
		onRPCResult: func(string, string, []byte) {},
	}
}

// OnConnect sets the callback fired synchronously whenever a node's stream
// connects, before the handler starts delivering anything: a Push called
// from inside f is guaranteed to reach the new connection, which is how the
// master sends lease + silences + managed_config immediately on connect.
func (h *Hub) OnConnect(f func(nodeID string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onConnect = f
}

// OnRPCResult sets the callback fired when a child posts an RPC result via
// PathRPC. nodeID is authenticated (mTLS, via requireNode); id is whatever
// the child put in the URL, taken as-is.
//
// This layer does NOT verify that id was ever issued to nodeID as a pending
// RPC -- a child (compromised, buggy, or just racing a retry) can post any
// id it likes. The caller of OnRPCResult owns the pending-RPC registry and
// MUST check that id names an RPC it actually sent to this exact nodeID
// before trusting body; otherwise a node could spoof a result for an RPC it
// was never sent, or that was sent to a different node.
func (h *Hub) OnRPCResult(f func(nodeID, id string, body []byte)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onRPCResult = f
}

// connect registers nodeID's stream connection, replacing (and closing) any
// existing one for the same node, then fires OnConnect.
func (h *Hub) connect(nodeID string) *nodeConn {
	h.mu.Lock()
	if old, ok := h.conns[nodeID]; ok {
		close(old.done)
	}
	c := &nodeConn{ch: make(chan Frame, hubQueueSize), done: make(chan struct{})}
	h.conns[nodeID] = c
	onConnect := h.onConnect
	h.mu.Unlock()
	onConnect(nodeID)
	return c
}

// release removes nodeID's connection if c is still the current one. If a
// newer connection has already replaced it, this is a no-op: the newer
// connection owns the map entry now, and nodeID may still be reachable
// through it, so lastDrop must not be pruned in that case.
func (h *Hub) release(nodeID string, c *nodeConn) {
	h.mu.Lock()
	removed := h.conns[nodeID] == c
	if removed {
		delete(h.conns, nodeID)
	}
	h.mu.Unlock()
	if removed {
		h.pruneDrop(nodeID)
	}
}

// Push queues f for nodeID's connected stream and never blocks: it returns
// false if the node is not connected, or if its queue is full (the frame is
// dropped and logged, rate-limited to once per node per second).
func (h *Hub) Push(nodeID string, f Frame) bool {
	h.mu.Lock()
	c, ok := h.conns[nodeID]
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case c.ch <- f:
		return true
	default:
		h.logDrop(nodeID)
		return false
	}
}

func (h *Hub) logDrop(nodeID string) {
	now := time.Now()
	h.dropMu.Lock()
	last, seen := h.lastDrop[nodeID]
	if seen && now.Sub(last) < time.Second {
		h.dropMu.Unlock()
		return
	}
	h.lastDrop[nodeID] = now
	h.dropMu.Unlock()
	h.logf("fleet: dropped a stream frame for node %s: queue full", nodeID)
}

// pruneDrop forgets nodeID's drop-log rate-limit state, so lastDrop does not
// grow without bound for a fleet with high node churn.
func (h *Hub) pruneDrop(nodeID string) {
	h.dropMu.Lock()
	delete(h.lastDrop, nodeID)
	h.dropMu.Unlock()
}

// Connected reports whether nodeID currently has an open stream connection.
func (h *Hub) Connected(nodeID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.conns[nodeID]
	return ok
}

// Disconnect closes nodeID's stream connection, if any. Used by revoke/remove.
func (h *Hub) Disconnect(nodeID string) {
	h.mu.Lock()
	c, ok := h.conns[nodeID]
	if ok {
		delete(h.conns, nodeID)
	}
	h.mu.Unlock()
	if ok {
		close(c.done)
		h.pruneDrop(nodeID)
	}
}

// CloseAll closes every currently connected node's stream connection, the
// same way Disconnect does for one. Used by the master's own graceful
// stop: http.Server.Shutdown does not cancel a still-running handler's
// request context on its own, so handleStream's long-lived select loop
// would otherwise only end once the client disconnects or the process
// exits -- Shutdown's deadline would elapse without ever actually closing
// a live connection (final-review transport I2).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	conns := h.conns
	h.conns = map[string]*nodeConn{}
	h.mu.Unlock()
	for nodeID, c := range conns {
		close(c.done)
		h.pruneDrop(nodeID)
	}
}

func (h *Hub) fireRPCResult(nodeID, id string, body []byte) {
	h.mu.Lock()
	f := h.onRPCResult
	h.mu.Unlock()
	f(nodeID, id, body)
}

// handleStream serves GET PathStream: one JSON Frame per line
// (application/x-ndjson), flushed after every frame, until the client
// disconnects (observed via r.Context()) or this node's connection is
// replaced or explicitly disconnected. The master's server-wide
// Read/WriteTimeout would otherwise kill a connection this long-lived, so
// both deadlines are cleared for this request specifically.
func (m *Master) handleStream(w http.ResponseWriter, r *http.Request, nodeID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	conn := m.cfg.Hub.connect(nodeID)
	defer m.cfg.Hub.release(nodeID, conn)
	m.cfg.Logf("fleet: node %s stream connected", nodeID)

	enc := json.NewEncoder(w)
	ticker := time.NewTicker(pingIntervalDuration())
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.done:
			return
		case f := <-conn.ch:
			if err := enc.Encode(f); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if err := enc.Encode(Frame{Type: "ping"}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleRPC serves POST PathRPC+"{id}": the child posts the result of an RPC
// the master pushed over the stream. The node id comes from requireNode
// (mTLS), never from the body or the URL. id itself, however, is untrusted:
// this handler does not check that id was ever issued to nodeID as a
// pending RPC -- see the warning on Hub.OnRPCResult, whose caller must.
func (m *Master) handleRPC(w http.ResponseWriter, r *http.Request, nodeID string) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing rpc id", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRPCResultBytes+1))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxRPCResultBytes {
		http.Error(w, "rpc result too large", http.StatusRequestEntityTooLarge)
		return
	}
	m.cfg.Hub.fireRPCResult(nodeID, id, body)
	w.WriteHeader(http.StatusNoContent)
}
