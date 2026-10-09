// Package trinetra: fleet_rpc.go is the master's side of an on-demand,
// request/response call to a connected child over the master-to-child
// stream: today the only method is "container_logs", used
// by replicaAPI.ContainerLogs (fleet_replica.go) so a remote node's Docker
// logs can be fetched exactly like a local one's, without a direct
// connection to the child at all.
//
// The request travels down as an "rpc" stream Frame; the child answers by
// POSTing to fleet.PathRPC (Shipper.PostRPCResult), which the master's
// fleet.Hub hands to rpcRegistry.Deliver via Hub.OnRPCResult. Hub.OnRPCResult's
// own doc comment is explicit that the id in that POST is untrusted input
// from the wire -- rpcRegistry is the consumer it warns must verify the id
// actually names a pending call sent to that exact node before trusting the
// body; see Deliver.
package trinetra

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// rpcCallTimeout is how long Call waits for a child's result before giving up.
var rpcCallTimeout = 10 * time.Second

// rpcSweepAfter/rpcMaxPendingPerNode are rpcRegistry's other two bounds
// an entry is swept once it has been sitting unanswered for
// this long past its own timeout (defense in depth -- Call already deletes
// its own entry the moment it times out; this only matters if that never
// happens, e.g. a future caller that doesn't wait), and no node may have
// more than this many calls pending at once.
var rpcSweepAfter = 60 * time.Second

const rpcMaxPendingPerNode = 32

var (
	// errNodeNotConnected is replicaAPI's exact wording for both the remote-ack and the
	// remote-RPC paths: the target node has no open stream connection right now.
	errNodeNotConnected = errors.New("node is not connected")
	// errRPCTimeout is Call's exact wording when no result arrives within rpcCallTimeout.
	errRPCTimeout = errors.New("node did not answer in 10s")
	// errTooManyPendingRPCs is Call's exact wording once a
	// node already has rpcMaxPendingPerNode calls outstanding.
	errTooManyPendingRPCs = errors.New("too many pending requests for this node")
)

// rpcFrameData is the "rpc" stream Frame's Data shape: {"id","method","args"}. args is
// opaque to the transport -- its shape depends entirely on method.
type rpcFrameData struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// rpcContainerLogsArgs is the "container_logs" method's args shape.
type rpcContainerLogsArgs struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

// rpcResultData is the body a child POSTs back via PostRPCResult:
// {"ok":true,"output":...} or {"ok":false,"error":...}.
type rpcResultData struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// pendingCall is one in-flight rpcRegistry entry.
type pendingCall struct {
	node    string
	created time.Time
	ch      chan rpcResultData
}

// rpcRegistry is the master's pending-RPC registry: id -> {node, created.
type rpcRegistry struct {
	now  func() time.Time
	logf func(format string, args ...any)

	mu      sync.Mutex
	pending map[string]*pendingCall

	rejectMu   sync.Mutex
	lastReject map[string]time.Time
}

// newRPCRegistry builds an rpcRegistry.
func newRPCRegistry(now func() time.Time, logf func(string, ...any)) *rpcRegistry {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &rpcRegistry{now: now, logf: logf, pending: map[string]*pendingCall{}, lastReject: map[string]time.Time{}}
}

// logRejectRateLimited logs one Deliver-rejection line for nodeID, at most once per second
// per node -- the same rate-limit shape as fleet.Hub's own logDrop.
func (r *rpcRegistry) logRejectRateLimited(nodeID, format string, args ...any) {
	now := r.now()
	r.rejectMu.Lock()
	last, seen := r.lastReject[nodeID]
	if seen && now.Sub(last) < time.Second {
		r.rejectMu.Unlock()
		return
	}
	r.lastReject[nodeID] = now
	r.rejectMu.Unlock()
	r.logf(format, args...)
}

// randomRPCID returns 16 random bytes, hex-encoded.
func randomRPCID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// sweepLocked drops every entry that expired (its own rpcCallTimeout has already passed)
// more than rpcSweepAfter ago.
func (r *rpcRegistry) sweepLocked() {
	cutoff := r.now().Add(-(rpcCallTimeout + rpcSweepAfter))
	for id, p := range r.pending {
		if p.created.Before(cutoff) {
			delete(r.pending, id)
		}
	}
}

// countForNodeLocked returns how many calls are currently pending for nodeID.
func (r *rpcRegistry) countForNodeLocked(nodeID string) int {
	n := 0
	for _, p := range r.pending {
		if p.node == nodeID {
			n++
		}
	}
	return n
}

// Call pushes an "rpc" frame for method/args to nodeID over hub and waits up to
// rpcCallTimeout for the child's result, delivered via Deliver.
func (r *rpcRegistry) Call(hub *fleet.Hub, nodeID, method string, args json.RawMessage) (rpcResultData, error) {
	if hub == nil || !hub.Connected(nodeID) {
		return rpcResultData{}, errNodeNotConnected
	}
	id, err := randomRPCID()
	if err != nil {
		return rpcResultData{}, err
	}
	p := &pendingCall{node: nodeID, created: r.now(), ch: make(chan rpcResultData, 1)}

	r.mu.Lock()
	r.sweepLocked()
	if r.countForNodeLocked(nodeID) >= rpcMaxPendingPerNode {
		r.mu.Unlock()
		return rpcResultData{}, errTooManyPendingRPCs
	}
	r.pending[id] = p
	r.mu.Unlock()

	data, err := json.Marshal(rpcFrameData{ID: id, Method: method, Args: args})
	if err != nil {
		r.remove(id)
		return rpcResultData{}, err
	}
	if !hub.Push(nodeID, fleet.Frame{Type: "rpc", Data: data}) {
		r.remove(id)
		return rpcResultData{}, errNodeNotConnected
	}

	t := time.NewTimer(rpcCallTimeout)
	defer t.Stop()
	select {
	case res := <-p.ch:
		return res, nil
	case <-t.C:
		r.remove(id)
		return rpcResultData{}, errRPCTimeout
	}
}

// remove drops id unconditionally (used when Call itself gives up on it,
// either before ever reaching the wire or on timeout).
func (r *rpcRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

// Deliver is wired as hub.OnRPCResult: nodeID is authenticated (mTLS), id and body come
// straight off the wire and are untrusted (see Hub.
func (r *rpcRegistry) Deliver(nodeID, id string, body []byte) {
	r.mu.Lock()
	p, ok := r.pending[id]
	if !ok {
		r.mu.Unlock()
		r.logRejectRateLimited(nodeID, "fleet: rpc result for unknown or already-completed id %s from node %s: rejected", id, nodeID)
		return
	}
	if p.node != nodeID {
		r.mu.Unlock()
		r.logRejectRateLimited(nodeID, "fleet: rpc result for id %s posted by node %s but was sent to node %s: rejected", id, nodeID, p.node)
		return
	}
	delete(r.pending, id) // single-use: any further result for id is now "unknown" above.
	r.mu.Unlock()

	var res rpcResultData
	if err := json.Unmarshal(body, &res); err != nil {
		r.logf("fleet: rpc result for id %s from node %s: invalid body: %v", id, nodeID, err)
		return
	}
	p.ch <- res // buffered 1: Call is either still waiting or has already timed out (never reads again); this never blocks.
}
