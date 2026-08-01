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

	"serverwatch/internal/config"
	"serverwatch/internal/core"
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
type Client struct {
	conn net.Conn
	r    *bufio.Reader

	// path and token are remembered from Dial so Subscribe (below) can open
	// its own dedicated connection: the primary conn above is
	// mutex-serialized for one-shot request/response calls, but a stream
	// sits in a long-lived read loop that would otherwise starve every
	// other caller sharing this Client.
	path  string
	token string

	mu     sync.Mutex
	nextID int
}

var _ core.API = (*Client)(nil)

// Dial connects to the control socket at path, exchanges the protocol hello
// with the server (presenting token, the per-launch secret the server was
// started with; pass "" when the server requires no auth), and returns a
// ready-to-use Client. It returns an error if the connection can't be
// established, the server's hello doesn't match ProtocolVersion, or the
// server rejected token: either failure surfaces the same way, since a
// wrong token makes the server close the connection after writing an error
// response instead of echoing a valid hello.
func Dial(path, token string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	c := &Client{conn: conn, r: bufio.NewReader(conn), path: path, token: token}

	if err := writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion, Token: token}); err != nil {
		conn.Close()
		return nil, err
	}
	var serverHello hello
	if err := readFrame(c.r, &serverHello); err != nil {
		conn.Close()
		return nil, err
	}
	if serverHello.Hello != helloMagic || serverHello.Version != ProtocolVersion {
		conn.Close()
		return nil, fmt.Errorf("control: unexpected server hello %+v, want hello=%q version=%d",
			serverHello, helloMagic, ProtocolVersion)
	}

	return c, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// call sends one request frame for method with params, waits for the
// matching response, and unmarshals its result into result (skipped if
// result is nil). It holds mu for the duration of the round trip so ids and
// frames from concurrent callers never interleave on the single connection.
func (c *Client) call(method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req := request{ID: id, Method: method, Params: rawParams}
	if err := writeFrame(c.conn, req); err != nil {
		return err
	}

	if err := c.conn.SetReadDeadline(time.Now().Add(callTimeout)); err != nil {
		return err
	}
	var resp response
	readErr := readFrame(c.r, &resp)
	// Reset the deadline unconditionally so it applies only to this call,
	// not to whatever the next caller does with the shared connection.
	c.conn.SetReadDeadline(time.Time{})
	if readErr != nil {
		return readErr
	}
	if resp.ID != id {
		return fmt.Errorf("control: response id %d does not match request id %d", resp.ID, id)
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
