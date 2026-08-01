package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// Client is a core.API implementation backed by a control-socket connection:
// every method sends one request frame and waits for the matching response
// frame. A single Client is safe for concurrent use; call serializes access
// to the underlying connection so request/response pairs and monotonic ids
// never interleave across goroutines.
type Client struct {
	conn net.Conn
	r    *bufio.Reader

	mu     sync.Mutex
	nextID int
}

var _ core.API = (*Client)(nil)

// Dial connects to the control socket at path, exchanges the protocol hello
// with the server, and returns a ready-to-use Client. It returns an error if
// the connection can't be established or the server's hello doesn't match
// ProtocolVersion.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	c := &Client{conn: conn, r: bufio.NewReader(conn)}

	if err := writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion}); err != nil {
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

	var resp response
	if err := readFrame(c.r, &resp); err != nil {
		return err
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

// Subscribe always returns an error: streaming over the control socket is
// not implemented until S5. It calls through to the server so a mock/fake
// exercising the wire protocol observes the same behavior a real server
// would produce.
func (c *Client) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	err := c.call("Subscribe", struct{}{}, nil)
	if err == nil {
		err = errors.New("control: streaming not supported over the control socket yet")
	}
	return nil, err
}
