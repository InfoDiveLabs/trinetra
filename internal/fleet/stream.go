package fleet

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// pingInterval is how often the master pings an idle stream, so the child's
// read-idle watchdog (streamIdleTimeout) does not take it for dead. An
// atomic.Int64 (nanoseconds) because background goroutines on both sides read
// it while a test shortens it.
var pingInterval atomic.Int64

func init() { pingInterval.Store(int64(20 * time.Second)) }

// pingIntervalDuration reads the current ping interval.
func pingIntervalDuration() time.Duration { return time.Duration(pingInterval.Load()) }

// setPingInterval sets the ping interval and returns the previous value so a
// test can restore it.
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

// OnConnect sets the callback fired synchronously when a node's stream connects,
// before delivery starts, so a Push from inside f reaches the new connection.
// The master uses this to send lease, silences and managed_config on connect.
func (h *Hub) OnConnect(f func(nodeID string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onConnect = f
}

// OnRPCResult sets the callback fired when a child posts an RPC result via
// PathRPC. nodeID is authenticated (mTLS); id is whatever the child put in the
// URL. This layer does NOT verify id was issued to nodeID, so the caller owns
// the pending-RPC registry and MUST check that id names an RPC sent to this
// exact node before trusting body, or a node could spoof results.
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

// release removes nodeID's connection if c is still current. If a newer
// connection replaced it, this is a no-op so lastDrop is not pruned for a node
// that is still reachable.
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

// Push queues f for nodeID's stream without blocking. It returns false if the
// node is not connected or its queue is full (the frame is dropped and logged,
// at most once per node per second).
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

// pruneDrop forgets nodeID's drop-log rate-limit state so lastDrop stays bounded.
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

// CloseAll closes every connected node's stream, as Disconnect does for one.
// Used on graceful stop: Shutdown does not cancel a running handler's request
// context, so handleStream would otherwise outlive Shutdown's deadline.
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

// handleStream serves GET PathStream: one JSON Frame per line (ndjson), flushed
// per frame, until the client disconnects or the node's connection is replaced
// or disconnected. The server-wide Read/WriteTimeout would kill a connection
// this long-lived, so both deadlines are cleared for this request.
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

// handleRPC serves POST PathRPC+"{id}". The node id comes from requireNode
// (mTLS); id itself is untrusted, see Hub.OnRPCResult.
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
