package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// callTimeout bounds how long Client.call waits for a response. call holds the
// client mutex for the whole round trip, so without a deadline one wedged server
// method would hang every caller sharing the Client. A var so tests can shrink it.
var callTimeout = 30 * time.Second

// redialAfter is how long a connection may sit unused before call replaces
// it. It must stay below the server's idleTimeout: past that the daemon has
// already closed the connection, and the first call after a quiet spell
// would fail.
var redialAfter = idleTimeout / 2

// Client is a core.API implementation backed by a control-socket connection.
// It is safe for concurrent use; call serializes access so request/response
// pairs and ids never interleave.
//
// The connection is self-healing: any transport failure (write error, read
// error/timeout, mismatched response id) leaves the stream frame-misaligned, so
// call closes the connection (poison) and the next call re-dials. Without this a
// single slow daemon response would wedge a long-lived Client (trinetra-web holds
// one for the whole process) with no reconnect of its own.
type Client struct {
	*clientConn
	// node, when non-empty, routes every call to that fleet node instead of the
	// daemon's own host (see ForNode). Views share clientConn with their parent, so
	// closing one never closes the shared connection.
	node string
}

// clientConn is the connection state shared by a Client and its node views:
// the socket, reader, dial parameters for reconnecting, and the mutex/id pair
// that serializes round trips.
type clientConn struct {
	conn net.Conn
	r    *bufio.Reader

	// path and token are kept from Dial so Subscribe can open its own dedicated
	// connection and call can re-dial after a poisoned one is discarded. A stream
	// sits in a long-lived read loop that would starve callers on the primary conn.
	path  string
	token string

	mu       sync.Mutex
	nextID   int
	lastUsed time.Time
}

var _ core.API = (*Client)(nil)
var _ core.FleetProvider = (*Client)(nil)

// Dial connects to the control socket at path, exchanges the protocol hello and
// presents token (pass "" when the server requires no auth). A wrong token and a
// version mismatch fail the same way: the server writes an error response and
// closes instead of echoing a valid hello.
func Dial(path, token string) (*Client, error) {
	c := &Client{clientConn: &clientConn{path: path, token: token}}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

// ForNode returns a view of c whose calls are routed to fleet node id. The view
// shares c's clientConn, so Close on it is a no-op. core.SelfNodeID explicitly
// asks for the daemon's own host through the same routing path as a real node.
func (c *Client) ForNode(id string) *Client {
	return &Client{clientConn: c.clientConn, node: id}
}

// Node implements core.FleetProvider. It never fails locally: id is validated
// server-side on the first call (core.ErrNoSuchNode).
func (c *Client) Node(id string) (core.API, error) { return c.ForNode(id), nil }

// Fleet implements core.FleetProvider. Fleet.* methods always run against the
// master, so the returned API uses an unrouted view (node "").
func (c *Client) Fleet() core.FleetAPI {
	return fleetClient{c: &Client{clientConn: c.clientConn}}
}

// connect dials the socket and completes the hello handshake, leaving c.conn/c.r
// nil on failure. Used by Dial and by call to re-establish a poisoned connection
// (callers there hold c.mu). The handshake read is bounded by callTimeout so a
// reconnect to a daemon that accepts but never answers fails fast instead of
// blocking every caller.
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
	c.lastUsed = time.Now()
	c.r = r
	return nil
}

// poison closes the connection so the next call re-dials. Callers hold c.mu.
func (c *Client) poison() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.r = nil
	}
}

// Close closes the connection. It is a no-op on a node view, which shares the
// connection with the Client it came from.
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

// wireErrSentinels lists the core sentinel errors a control-socket caller may
// need to recover via errors.Is, checked by reconstructWireErr in order. A new
// sentinel wrapped by a Fleet.*/node method via fmt.Errorf("...: %w", s) must be
// added here, or callers silently lose errors.Is against it.
var wireErrSentinels = []error{core.ErrNoSuchNode, core.ErrNotMaster, core.ErrConflict, core.ErrNotFound, core.ErrStatusPageOnChild}

// wireErr preserves a method error's exact text while unwrapping to the core
// sentinel it ended with.
type wireErr struct {
	msg    string
	target error
}

func (e *wireErr) Error() string { return e.msg }
func (e *wireErr) Unwrap() error { return e.target }

// reconstructWireErr rebuilds a method error from resp.Error (plain text only):
// a *wireErr unwrapping to a sentinel when the message ends with that sentinel's
// text (the shape fmt.Errorf("...: %w", s) produces), else errors.New. Keeps
// errors.Is(err, core.ErrNoSuchNode) working across the socket hop.
func reconstructWireErr(msg string) error {
	if msg == "" {
		return nil
	}
	for _, sentinel := range wireErrSentinels {
		if strings.HasSuffix(msg, sentinel.Error()) {
			return &wireErr{msg: msg, target: sentinel}
		}
	}
	return errors.New(msg)
}

// call sends one request frame and unmarshals the matching response into result
// (skipped if nil), holding mu for the whole round trip.
//
// A transport failure (dial, write, read/timeout, response id mismatch) poisons
// the connection, since the stream can no longer be trusted to be frame-aligned.
// A method-level error (ok=false) or an unmarshal failure consumed exactly one
// frame, so the stream stays aligned and the connection is kept.
func (c *Client) call(method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}

	if c.conn != nil && time.Since(c.lastUsed) > redialAfter {
		c.poison()
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
	// Reset the deadline so it applies only to this call, not the next caller.
	c.conn.SetReadDeadline(time.Time{})
	if readErr != nil {
		c.poison()
		return readErr
	}
	if resp.ID != id {
		c.poison()
		return fmt.Errorf("control: response id %d does not match request id %d", resp.ID, id)
	}
	c.lastUsed = time.Now()
	// A daemon that predates fleet routing ignores req.Node, answers with its own
	// host's data, and never echoes Node. Reject that before the ok check so its
	// valid answer is not mistaken for the requested node's.
	if c.node != "" && resp.Node != c.node {
		return errors.New("control: daemon does not support fleet node routing (upgrade trinetra)")
	}
	if !resp.OK {
		return reconstructWireErr(resp.Error)
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

// HostInfo implements core.API (#100).
func (c *Client) HostInfo() (core.HostInfoView, error) {
	var result core.HostInfoView
	err := c.call("HostInfo", struct{}{}, &result)
	return result, err
}

// Version implements core.API (#107).
func (c *Client) Version() (string, error) {
	var result string
	err := c.call("Version", struct{}{}, &result)
	return result, err
}

// ContainerLogs implements core.API: a `docker logs --tail` snapshot.
func (c *Client) ContainerLogs(name string, lines int) (string, error) {
	params := struct {
		Name  string `json:"name"`
		Lines int    `json:"lines"`
	}{Name: name, Lines: lines}
	var result string
	err := c.call("ContainerLogs", params, &result)
	return result, err
}

// EnrollmentPIN implements core.API.
func (c *Client) EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error) {
	var result enrollmentPINResult
	err = c.call("EnrollmentPIN", struct{}{}, &result)
	return result.PIN, result.Enrolled, err
}

// MonitorTargets implements core.API.
func (c *Client) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	var result []core.TargetView
	err := c.call("MonitorTargets", struct{}{}, &result)
	return result, err
}

// UpdateStatus implements core.API.
func (c *Client) UpdateStatus() (core.UpdateStatusView, error) {
	var result core.UpdateStatusView
	err := c.call("UpdateStatus", struct{}{}, &result)
	return result, err
}

// UpdateCheck implements core.API.
func (c *Client) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	var result core.UpdateStatusView
	err := c.call("UpdateCheck", struct{}{}, &result)
	return result, err
}

// UpdateApply implements core.API; an empty version means the channel's latest.
func (c *Client) UpdateApply(ctx context.Context, version string) error {
	params := struct {
		Version string `json:"version"`
	}{Version: version}
	return c.call("UpdateApply", params, nil)
}

// UpdateRollback implements core.API.
func (c *Client) UpdateRollback() error {
	return c.call("UpdateRollback", struct{}{}, nil)
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

// ValidateChannel implements core.API.
func (c *Client) ValidateChannel(cc config.ChannelConfig) error {
	params := struct {
		Channel config.ChannelConfig `json:"channel"`
	}{Channel: cc}
	return c.call("ValidateChannel", params, nil)
}

// Subscribe implements core.API over its own DEDICATED connection so the
// long-lived stream never blocks (or is blocked by) the mutex-serialized primary
// conn. Cancelling ctx or the server ending the stream closes that connection
// and the returned channel.
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

	// Forces dc closed as soon as ctx is done, unblocking the reader; exits without
	// doing so if the reader finishes first, so it never outlives the subscription.
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

// fleetClient implements core.FleetAPI by sending Fleet.* requests on c, which
// always carries node "" since they run against the master.
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

func (f fleetClient) RenameNode(id, name, actor string) error {
	return f.c.call("Fleet.RenameNode", map[string]any{"id": id, "name": name, "actor": actor}, nil)
}

func (f fleetClient) SetNodeTags(id string, tags []string, actor string) error {
	return f.c.call("Fleet.SetNodeTags", map[string]any{"id": id, "tags": tags, "actor": actor}, nil)
}

func (f fleetClient) SetNodeDeps(id string, deps []string, actor string) error {
	return f.c.call("Fleet.SetNodeDeps", map[string]any{"id": id, "deps": deps, "actor": actor}, nil)
}

func (f fleetClient) RevokeNode(id, actor string) error {
	return f.c.call("Fleet.RevokeNode", map[string]any{"id": id, "actor": actor}, nil)
}

func (f fleetClient) RemoveNode(id, actor string) error {
	return f.c.call("Fleet.RemoveNode", map[string]any{"id": id, "actor": actor}, nil)
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

func (f fleetClient) DeleteToken(id, actor string) error {
	return f.c.call("Fleet.DeleteToken", map[string]any{"id": id, "actor": actor}, nil)
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

func (f fleetClient) Silences() ([]core.Silence, error) {
	var v []core.Silence
	err := f.c.call("Fleet.Silences", struct{}{}, &v)
	return v, err
}

func (f fleetClient) CreateSilence(s core.Silence) (core.Silence, error) {
	var v core.Silence
	err := f.c.call("Fleet.CreateSilence", map[string]any{"silence": s}, &v)
	return v, err
}

func (f fleetClient) ExpireSilence(id, actor string) error {
	return f.c.call("Fleet.ExpireSilence", map[string]any{"id": id, "actor": actor}, nil)
}

func (f fleetClient) Maintenances() ([]core.Maintenance, error) {
	var v []core.Maintenance
	err := f.c.call("Fleet.Maintenances", struct{}{}, &v)
	return v, err
}

func (f fleetClient) SaveMaintenance(m core.Maintenance) (core.Maintenance, error) {
	var v core.Maintenance
	err := f.c.call("Fleet.SaveMaintenance", map[string]any{"maintenance": m}, &v)
	return v, err
}

func (f fleetClient) Alerting() (core.AlertingConfig, error) {
	var v core.AlertingConfig
	err := f.c.call("Fleet.Alerting", struct{}{}, &v)
	return v, err
}

func (f fleetClient) SetAlerting(cfg core.AlertingConfig, actor string) error {
	return f.c.call("Fleet.SetAlerting", map[string]any{"alerting": cfg, "actor": actor}, nil)
}

func (f fleetClient) RouteTest(alert core.TestAlert) (core.RouteDecision, error) {
	var v core.RouteDecision
	err := f.c.call("Fleet.RouteTest", map[string]any{"test_alert": alert}, &v)
	return v, err
}

func (f fleetClient) RuleStates() ([]core.RuleState, error) {
	var v []core.RuleState
	err := f.c.call("Fleet.RuleStates", struct{}{}, &v)
	return v, err
}

func (f fleetClient) DeleteMaintenance(id, actor string) error {
	return f.c.call("Fleet.DeleteMaintenance", map[string]any{"id": id, "actor": actor}, nil)
}

func (f fleetClient) Managed() ([]core.ManagedFragment, error) {
	var v []core.ManagedFragment
	err := f.c.call("Fleet.Managed", struct{}{}, &v)
	return v, err
}

func (f fleetClient) SaveManaged(frag core.ManagedFragment, actor string) (core.ManagedFragment, error) {
	var v core.ManagedFragment
	err := f.c.call("Fleet.SaveManaged", map[string]any{"managed_fragment": frag, "actor": actor}, &v)
	return v, err
}

func (f fleetClient) DeleteManaged(id, actor string) error {
	return f.c.call("Fleet.DeleteManaged", map[string]any{"id": id, "actor": actor}, nil)
}

func (f fleetClient) ManagedStatus() ([]core.ManagedStatus, error) {
	var v []core.ManagedStatus
	err := f.c.call("Fleet.ManagedStatus", struct{}{}, &v)
	return v, err
}

func (f fleetClient) FleetSeries(metric string, filter core.NodeFilter, agg core.Agg, from, to int64, res core.Resolution) ([]core.FleetSeriesPoint, error) {
	var v []core.FleetSeriesPoint
	err := f.c.call("Fleet.FleetSeries", map[string]any{
		"metric": metric, "filter": filter, "agg": agg, "from": from, "to": to, "res": res,
	}, &v)
	return v, err
}

// StatusPage returns the status-page API over the socket (issue #157).
func (c *Client) StatusPage() core.StatusPageAPI {
	return statusPageClient{c: &Client{clientConn: c.clientConn}}
}

type statusPageClient struct{ c *Client }

var _ core.StatusPageAPI = statusPageClient{}

func (s statusPageClient) Services() (out []core.StatusService, err error) {
	err = s.c.call("StatusPage.Services", nil, &out)
	return
}
func (s statusPageClient) SetService(svc core.StatusService, actor string) (out core.StatusService, err error) {
	err = s.c.call("StatusPage.SetService", map[string]any{"service": svc, "actor": actor}, &out)
	return
}
func (s statusPageClient) DeleteService(id, actor string) error {
	return s.c.call("StatusPage.DeleteService", map[string]any{"id": id, "actor": actor}, nil)
}
func (s statusPageClient) Evaluation() (out []core.ServiceEvaluation, err error) {
	err = s.c.call("StatusPage.Evaluation", nil, &out)
	return
}
func (s statusPageClient) Incidents(includeResolved bool) (out []core.StatusIncident, err error) {
	err = s.c.call("StatusPage.Incidents", map[string]any{"include_resolved": includeResolved}, &out)
	return
}
func (s statusPageClient) Incident(id string) (out core.StatusIncident, err error) {
	err = s.c.call("StatusPage.Incident", map[string]any{"id": id}, &out)
	return
}
func (s statusPageClient) CreateIncident(in core.NewIncident, actor string) (out core.StatusIncident, err error) {
	err = s.c.call("StatusPage.CreateIncident", map[string]any{"incident": in, "actor": actor}, &out)
	return
}
func (s statusPageClient) PostUpdate(id string, u core.NewUpdate, actor string) (out core.StatusIncident, err error) {
	err = s.c.call("StatusPage.PostUpdate", map[string]any{"id": id, "update": u, "actor": actor}, &out)
	return
}
func (s statusPageClient) EditUpdate(id, updateID string, u core.NewUpdate, actor string) (out core.StatusIncident, err error) {
	err = s.c.call("StatusPage.EditUpdate", map[string]any{"id": id, "update_id": updateID, "update": u, "actor": actor}, &out)
	return
}
func (s statusPageClient) EditIncident(id, title string, services []string, actor string) (out core.StatusIncident, err error) {
	err = s.c.call("StatusPage.EditIncident", map[string]any{"id": id, "title": title, "services": services, "actor": actor}, &out)
	return
}
func (s statusPageClient) DeleteIncident(id, actor string) error {
	return s.c.call("StatusPage.DeleteIncident", map[string]any{"id": id, "actor": actor}, nil)
}
func (s statusPageClient) Public() (out core.PublicStatus, err error) {
	err = s.c.call("StatusPage.Public", nil, &out)
	return
}
