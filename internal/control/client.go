package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// callTimeout bounds how long Client.call waits for a response after
// writing its request. call holds the client mutex for the whole round
// trip, so without a deadline a single wedged server method would hang
// every caller sharing this Client forever. It is a package var rather
// than a const so a test can shrink it to keep a deadline-expiry test
// fast.
var callTimeout = 30 * time.Second

// Client is a core.API implementation backed by a control-socket connection:
// every method sends one request frame and waits for the matching response
// frame. A single Client is safe for concurrent use; call serializes access
// to the underlying connection so request/response pairs and monotonic ids
// never interleave across goroutines.
//
// The connection is self-healing. Any transport failure -- a write error, a
// read error/timeout, or a response whose id does not match the request --
// leaves the shared connection frame-misaligned (a late response, for
// instance, would be read by the NEXT call and mismatch its id, desyncing
// every call thereafter). call therefore closes and discards the connection
// on any such failure (poison), and the next call re-dials transparently.
// Without this a single slow daemon response would wedge a long-lived Client
// forever, since callers here (notably trinetra-web) hold one Client for
// the whole process lifetime with no reconnect of their own.
type Client struct {
	*clientConn
	// node, when non-empty, routes every call through this Client to that
	// fleet node instead of the daemon's own host (see ForNode). Views
	// share clientConn with the Client they came from, so closing one never
	// closes the connection every other view and the base Client depend on.
	node string
}

// clientConn is the connection state shared by a Client and every node view
// (Client.ForNode) built from it: the socket, its buffered reader, the
// dial parameters needed to reconnect, and the mutex/id-counter pair that
// serializes request/response pairs across every one of those views.
type clientConn struct {
	conn net.Conn
	r    *bufio.Reader

	// path and token are remembered from Dial so Subscribe (below) can open
	// its own dedicated connection, and so call can re-dial after a
	// poisoned connection is discarded. The primary conn above is
	// mutex-serialized for one-shot request/response calls, but a stream
	// sits in a long-lived read loop that would otherwise starve every
	// other caller sharing this Client.
	path  string
	token string

	mu     sync.Mutex
	nextID int
}

var _ core.API = (*Client)(nil)
var _ core.FleetProvider = (*Client)(nil)

// Dial connects to the control socket at path, exchanges the protocol hello
// with the server (presenting token, the per-launch secret the server was
// started with; pass "" when the server requires no auth), and returns a
// ready-to-use Client. It returns an error if the connection can't be
// established, the server's hello doesn't match ProtocolVersion, or the
// server rejected token: either failure surfaces the same way, since a
// wrong token makes the server close the connection after writing an error
// response instead of echoing a valid hello.
func Dial(path, token string) (*Client, error) {
	c := &Client{clientConn: &clientConn{path: path, token: token}}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

// ForNode returns a view of c whose calls are routed to fleet node id
// instead of c's own host (see clientConn's Node field on request/response,
// and resolveNode in server.go). The view shares c's clientConn -- the same
// socket, mutex and id counter -- so it never dials its own connection;
// closing it (Close, below) is a no-op, since the view does not own the
// connection. Passing core.SelfNodeID explicitly asks for this daemon's own
// host by the same routing path a real node id uses, rather than c's
// unrouted default.
func (c *Client) ForNode(id string) *Client {
	return &Client{clientConn: c.clientConn, node: id}
}

// Node implements core.FleetProvider by returning a node view (ForNode) of
// c. It never fails locally -- id is validated server-side, on the first
// call made through the returned API -- so a caller only learns of an
// unknown node (core.ErrNoSuchNode) once it actually calls a method.
func (c *Client) Node(id string) (core.API, error) { return c.ForNode(id), nil }

// Fleet implements core.FleetProvider: it returns a core.FleetAPI whose
// methods call the daemon's Fleet.* control-socket methods. The returned
// value carries its own unrouted Client view (node "") since Fleet.* methods
// always run against the master, never a specific node.
func (c *Client) Fleet() core.FleetAPI {
	return fleetClient{c: &Client{clientConn: c.clientConn}}
}

// connect dials the control socket and completes the hello handshake,
// setting c.conn/c.r on success and leaving them nil on failure. It is used
// both by Dial (constructing a fresh Client, no concurrent access yet) and
// by call to re-establish a poisoned connection (callers there hold c.mu),
// so the two paths handshake identically by construction.
//
// The handshake read is bounded by callTimeout: a reconnect against a daemon
// that is reachable at the socket layer but not answering (it never runs
// Accept, or is wedged before writing its hello) must fail fast rather than
// block every caller sharing this Client forever -- the same reasoning the
// per-call read deadline in call rests on. The original Dial had no such
// deadline because a brand-new process could afford to block on startup;
// a mid-life reconnect cannot.
func (c *Client) connect() error {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return err
	}
	r := bufio.NewReader(conn)

	if err := writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion, Token: c.token}); err != nil {
		conn.Close()
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(callTimeout)); err != nil {
		conn.Close()
		return err
	}
	var serverHello hello
	readErr := readFrame(r, &serverHello)
	conn.SetReadDeadline(time.Time{})
	if readErr != nil {
		conn.Close()
		return readErr
	}
	if serverHello.Hello != helloMagic || serverHello.Version != ProtocolVersion {
		conn.Close()
		return fmt.Errorf("control: unexpected server hello %+v, want hello=%q version=%d",
			serverHello, helloMagic, ProtocolVersion)
	}

	c.conn = conn
	c.r = r
	return nil
}

// poison closes and discards the current connection so the next call
// re-dials. Callers hold c.mu. It is idempotent (a nil conn is a no-op) and
// is invoked on every transport failure in call, where the connection can no
// longer be trusted to be frame-aligned.
func (c *Client) poison() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.r = nil
	}
}

// Close closes the underlying connection. A node view (ForNode) does not own
// the connection -- it shares clientConn with the Client it came from, which
// may still be in use -- so Close on a view is a no-op.
func (c *Client) Close() error {
	if c.node != "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// call sends one request frame for method with params, waits for the
// matching response, and unmarshals its result into result (skipped if
// result is nil). It holds mu for the duration of the round trip so ids and
// frames from concurrent callers never interleave on the single connection.
//
// If the connection was poisoned by a prior transport failure (c.conn is
// nil), call re-dials first; a dial/handshake failure surfaces to the caller
// and leaves the connection poisoned for the next attempt. Any transport
// failure DURING the round trip -- a write error, a read error/timeout, or a
// response id that does not match the request just sent -- poisons the
// connection before returning, since after any of these the stream can no
// longer be trusted to be frame-aligned (a late or dropped response would
// desync every subsequent call on the same connection). A method-level error
// (the server answered, with ok=false) is NOT a transport failure: the
// stream is still aligned, so the connection is kept and the error surfaces
// unchanged. Likewise a result that fails to unmarshal: exactly one response
// frame was consumed, so the stream stays aligned and only the caller's
// decode fails.
func (c *Client) call(method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}

	if c.conn == nil {
		if err := c.connect(); err != nil {
			return err
		}
	}

	c.nextID++
	id := c.nextID

	req := request{ID: id, Method: method, Params: rawParams, Node: c.node}
	if err := writeFrame(c.conn, req); err != nil {
		c.poison()
		return err
	}

	if err := c.conn.SetReadDeadline(time.Now().Add(callTimeout)); err != nil {
		c.poison()
		return err
	}
	var resp response
	readErr := readFrame(c.r, &resp)
	// Reset the deadline unconditionally so it applies only to this call,
	// not to whatever the next caller does with the shared connection.
	c.conn.SetReadDeadline(time.Time{})
	if readErr != nil {
		c.poison()
		return readErr
	}
	if resp.ID != id {
		c.poison()
		return fmt.Errorf("control: response id %d does not match request id %d", resp.ID, id)
	}
	// An old daemon that predates fleet routing has no idea req.Node exists:
	// it answers every call with its own host's data and never echoes Node
	// back. Catch that here, before the ok/error check below, so such a
	// daemon's (perfectly valid, ok=true) answer about itself is never
	// mistaken for the requested node's data.
	if c.node != "" && resp.Node != c.node {
		return errors.New("control: daemon does not support fleet node routing (upgrade trinetra)")
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

func (c *Client) Snapshot() (core.DashboardView, error) {
	var result core.DashboardView
	err := c.call("Snapshot", struct{}{}, &result)
	return result, err
}

func (c *Client) Monitoring() (core.MonitoringView, error) {
	var result core.MonitoringView
	err := c.call("Monitoring", struct{}{}, &result)
	return result, err
}

func (c *Client) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	params := struct {
		Metric string `json:"metric"`
		From   int64  `json:"from"`
		To     int64  `json:"to"`
		Res    int    `json:"res"`
	}{Metric: metric, From: from, To: to, Res: int(res)}
	var result []core.SeriesPoint
	err := c.call("Series", params, &result)
	return result, err
}

func (c *Client) Events(from, to int64) ([]core.DownEventView, error) {
	params := struct {
		From int64 `json:"from"`
		To   int64 `json:"to"`
	}{From: from, To: to}
	var result []core.DownEventView
	err := c.call("Events", params, &result)
	return result, err
}

func (c *Client) ActiveAlerts() ([]core.AlertRecord, error) {
	var result []core.AlertRecord
	err := c.call("ActiveAlerts", struct{}{}, &result)
	return result, err
}

func (c *Client) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	params := struct {
		Since int64 `json:"since"`
		Limit int   `json:"limit"`
	}{Since: since, Limit: limit}
	var result []core.AlertRecord
	err := c.call("AlertHistory", params, &result)
	return result, err
}

func (c *Client) Config() (*config.Config, error) {
	var result config.Config
	if err := c.call("Config", struct{}{}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) Doctor() (core.DoctorReport, error) {
	var result core.DoctorReport
	err := c.call("Doctor", struct{}{}, &result)
	return result, err
}

// HostInfo implements core.API: fetches the static host inventory (#100) over
// the socket.
func (c *Client) HostInfo() (core.HostInfoView, error) {
	var result core.HostInfoView
	err := c.call("HostInfo", struct{}{}, &result)
	return result, err
}

// Version implements core.API: fetches the core daemon's build-stamped version
// (#107) over the socket.
func (c *Client) Version() (string, error) {
	var result string
	err := c.call("Version", struct{}{}, &result)
	return result, err
}

// ContainerLogs implements core.API: fetches a `docker logs --tail` snapshot
// for the named container over the socket.
func (c *Client) ContainerLogs(name string, lines int) (string, error) {
	params := struct {
		Name  string `json:"name"`
		Lines int    `json:"lines"`
	}{Name: name, Lines: lines}
	var result string
	err := c.call("ContainerLogs", params, &result)
	return result, err
}

// EnrollmentPIN implements core.API: it sends the request and unmarshals the
// server's enrollmentPINResult (protocol.go) into pin/enrolled.
func (c *Client) EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error) {
	var result enrollmentPINResult
	err = c.call("EnrollmentPIN", struct{}{}, &result)
	return result.PIN, result.Enrolled, err
}

// MonitorTargets implements core.API: it sends the request and unmarshals
// the server's []core.TargetView result directly (no wrapper struct, same
// as Monitoring/ActiveAlerts).
func (c *Client) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	var result []core.TargetView
	err := c.call("MonitorTargets", struct{}{}, &result)
	return result, err
}

func (c *Client) ApplyConfig(cfg *config.Config) error {
	params := struct {
		Config config.Config `json:"config"`
	}{Config: *cfg}
	return c.call("ApplyConfig", params, nil)
}

func (c *Client) AckAlert(key string) error {
	params := struct {
		Key string `json:"key"`
	}{Key: key}
	return c.call("AckAlert", params, nil)
}

func (c *Client) UnackAlert(key string) error {
	params := struct {
		Key string `json:"key"`
	}{Key: key}
	return c.call("UnackAlert", params, nil)
}

func (c *Client) TestChannel(name string) error {
	params := struct {
		Name string `json:"name"`
	}{Name: name}
	return c.call("TestChannel", params, nil)
}

// ValidateChannel implements core.API: it sends cc to the server and
// surfaces whatever error api.ValidateChannel returned (buildNotifier's
// error for an undeliverable channel, or nil).
func (c *Client) ValidateChannel(cc config.ChannelConfig) error {
	params := struct {
		Channel config.ChannelConfig `json:"channel"`
	}{Channel: cc}
	return c.call("ValidateChannel", params, nil)
}

// Subscribe implements core.API by opening its own DEDICATED connection --
// a second Dial to the same path/token remembered from c's own Dial -- so
// the long-lived stream it reads from never blocks (or is blocked by) the
// mutex-serialized primary conn other calls on c share. It sends one
// Subscribe request on that connection, reads the server's ack, then hands
// back a channel fed by a background goroutine that decodes each stream
// frame (server.go's streamSubscribe: response{ID: streamID, ...}) into a
// core.Event. Cancelling ctx, or the server ending the stream (a read
// error, e.g. because the daemon shut down), closes the dedicated
// connection and the returned channel; a second goroutine exists solely to
// force that closure on ctx.Done without leaking once the stream ends for
// some other reason.
func (c *Client) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	if c.node != "" && c.node != core.SelfNodeID {
		return nil, errors.New("control: live event streams are not available for remote fleet nodes yet")
	}
	dc, err := Dial(c.path, c.token)
	if err != nil {
		return nil, err
	}

	dc.nextID++
	id := dc.nextID
	req := request{ID: id, Method: "Subscribe", Params: json.RawMessage("{}")}
	if err := writeFrame(dc.conn, req); err != nil {
		dc.Close()
		return nil, err
	}

	var ack response
	if err := readFrame(dc.r, &ack); err != nil {
		dc.Close()
		return nil, err
	}
	if ack.ID != id {
		dc.Close()
		return nil, fmt.Errorf("control: subscribe ack id %d does not match request id %d", ack.ID, id)
	}
	if !ack.OK {
		dc.Close()
		return nil, errors.New(ack.Error)
	}

	out := make(chan core.Event)
	stopped := make(chan struct{})

	// This goroutine's only purpose is forcing dc closed the moment ctx is
	// done, unblocking the reader goroutine's in-flight (or next) read; it
	// exits without doing that once the reader goroutine finishes on its
	// own (stream/connection ended for some other reason), so it never
	// outlives the subscription it belongs to.
	go func() {
		select {
		case <-ctx.Done():
			dc.Close()
		case <-stopped:
		}
	}()

	go func() {
		defer close(out)
		defer dc.Close()
		defer close(stopped)
		for {
			var resp response
			if err := readFrame(dc.r, &resp); err != nil {
				return
			}
			if resp.ID != streamID || !resp.OK {
				continue
			}
			var ev core.Event
			if err := json.Unmarshal(resp.Result, &ev); err != nil {
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

// fleetClient implements core.FleetAPI over a control socket: each method
// sends the matching Fleet.* request (server.go's dispatchFleet) on c, which
// always carries node "" -- Fleet.* methods run against the master, never a
// specific node, regardless of which view of a Client Fleet() was called on.
type fleetClient struct{ c *Client }

func (f fleetClient) Status() (core.FleetStatus, error) {
	var v core.FleetStatus
	err := f.c.call("Fleet.Status", struct{}{}, &v)
	return v, err
}

func (f fleetClient) Nodes(filter core.NodeFilter) ([]core.NodeSummary, error) {
	var v []core.NodeSummary
	err := f.c.call("Fleet.Nodes", map[string]any{"filter": filter}, &v)
	return v, err
}

func (f fleetClient) RenameNode(id, name string) error {
	return f.c.call("Fleet.RenameNode", map[string]any{"id": id, "name": name}, nil)
}

func (f fleetClient) SetNodeTags(id string, tags []string) error {
	return f.c.call("Fleet.SetNodeTags", map[string]any{"id": id, "tags": tags}, nil)
}

func (f fleetClient) RevokeNode(id string) error {
	return f.c.call("Fleet.RevokeNode", map[string]any{"id": id}, nil)
}

func (f fleetClient) RemoveNode(id string) error {
	return f.c.call("Fleet.RemoveNode", map[string]any{"id": id}, nil)
}

func (f fleetClient) Tokens() ([]core.TokenView, error) {
	var v []core.TokenView
	err := f.c.call("Fleet.Tokens", struct{}{}, &v)
	return v, err
}

func (f fleetClient) CreateToken(s core.TokenSpec) (core.CreatedToken, error) {
	var v core.CreatedToken
	err := f.c.call("Fleet.CreateToken", map[string]any{"spec": s}, &v)
	return v, err
}

func (f fleetClient) DeleteToken(id string) error {
	return f.c.call("Fleet.DeleteToken", map[string]any{"id": id}, nil)
}

func (f fleetClient) Incidents(filter core.IncidentFilter) ([]core.Incident, error) {
	var v []core.Incident
	err := f.c.call("Fleet.Incidents", map[string]any{"inc_filter": filter}, &v)
	return v, err
}

func (f fleetClient) Incident(id string) (core.Incident, error) {
	var v core.Incident
	err := f.c.call("Fleet.Incident", map[string]any{"id": id}, &v)
	return v, err
}

func (f fleetClient) AckIncident(id, actor string) error {
	return f.c.call("Fleet.AckIncident", map[string]any{"id": id, "actor": actor}, nil)
}

func (f fleetClient) Explain(key string) ([]core.IncidentEvent, error) {
	var v []core.IncidentEvent
	err := f.c.call("Fleet.Explain", map[string]any{"key": key}, &v)
	return v, err
}

func (f fleetClient) Audit(limit int) ([]core.AuditEntry, error) {
	var v []core.AuditEntry
	err := f.c.call("Fleet.Audit", map[string]any{"limit": limit}, &v)
	return v, err
}
