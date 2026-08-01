package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// fakeAPI is a minimal core.API test double for handleConn/dispatch tests:
// each read field backs exactly one read method's return value, and each
// write method records its argument and returns the configured error (nil
// unless the test sets one), so a test can prove both that a write reached
// the API and that its error propagates back over the wire.
type fakeAPI struct {
	snapshot   core.DashboardView
	monitoring core.MonitoringView
	series     []core.SeriesPoint
	events     []core.DownEventView
	active     []core.AlertRecord
	history    []core.AlertRecord
	cfg        *config.Config
	doctor     core.DoctorReport

	appliedConfig *config.Config
	ackedKey      string
	unackedKey    string
	testedChannel string

	// ackAlertErr, when set, is returned by AckAlert -- used to prove a
	// method error propagates back over the wire as ok=false.
	ackAlertErr error
}

func (f *fakeAPI) Snapshot() (core.DashboardView, error)    { return f.snapshot, nil }
func (f *fakeAPI) Monitoring() (core.MonitoringView, error) { return f.monitoring, nil }

func (f *fakeAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	return f.series, nil
}

func (f *fakeAPI) Events(from, to int64) ([]core.DownEventView, error) {
	return f.events, nil
}

func (f *fakeAPI) ActiveAlerts() ([]core.AlertRecord, error) { return f.active, nil }

func (f *fakeAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return f.history, nil
}

func (f *fakeAPI) Config() (*config.Config, error)    { return f.cfg, nil }
func (f *fakeAPI) Doctor() (core.DoctorReport, error) { return f.doctor, nil }

func (f *fakeAPI) ApplyConfig(c *config.Config) error {
	f.appliedConfig = c
	return nil
}

func (f *fakeAPI) AckAlert(key string) error {
	f.ackedKey = key
	return f.ackAlertErr
}

func (f *fakeAPI) UnackAlert(key string) error {
	f.unackedKey = key
	return nil
}

func (f *fakeAPI) TestChannel(name string) error {
	f.testedChannel = name
	return nil
}

func (f *fakeAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	return nil, errors.New("fakeAPI: Subscribe not implemented")
}

var _ core.API = (*fakeAPI)(nil)

// dialTestConn starts handleConn on one end of an in-memory net.Pipe (via
// net.Conn's dedicated pipe helper) with fake as the backing API and hands
// the test the other end plus a reader for it, already past a valid hello
// handshake. It returns a done channel closed once handleConn returns.
func dialTestConn(t *testing.T, fake core.API) (net.Conn, *bufio.Reader, <-chan struct{}) {
	t.Helper()
	server, client := net.Pipe()

	done := make(chan struct{})
	go func() {
		handleConn(fake, server)
		close(done)
	}()

	if err := writeFrame(client, hello{Hello: "serverwatch-control", Version: ProtocolVersion}); err != nil {
		t.Fatalf("writeFrame(hello): %v", err)
	}

	r := bufio.NewReader(client)
	var gotHello hello
	if err := readFrame(r, &gotHello); err != nil {
		t.Fatalf("readFrame(server hello): %v", err)
	}
	if gotHello.Hello != "serverwatch-control" || gotHello.Version != ProtocolVersion {
		t.Fatalf("server hello = %+v, want valid ack", gotHello)
	}

	return client, r, done
}

func sendRequest(t *testing.T, client net.Conn, r *bufio.Reader, id int, method string, params any) response {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := request{ID: id, Method: method, Params: raw}
	if err := writeFrame(client, req); err != nil {
		t.Fatalf("writeFrame(request): %v", err)
	}
	var resp response
	if err := readFrame(r, &resp); err != nil {
		t.Fatalf("readFrame(response): %v", err)
	}
	return resp
}

func TestHandleConnSnapshotRoundTrip(t *testing.T) {
	fake := &fakeAPI{snapshot: core.DashboardView{CPU: 42.5, Online: true, Cores: 8}}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	resp := sendRequest(t, client, r, 1, "Snapshot", struct{}{})

	if !resp.OK {
		t.Fatalf("resp.OK = false, want true (error: %s)", resp.Error)
	}
	if resp.ID != 1 {
		t.Errorf("resp.ID = %d, want 1", resp.ID)
	}
	var got core.DashboardView
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !reflect.DeepEqual(got, fake.snapshot) {
		t.Errorf("got %+v, want %+v", got, fake.snapshot)
	}

	client.Close()
	<-done
}

func TestHandleConnMethodErrorPropagates(t *testing.T) {
	fake := &fakeAPI{ackAlertErr: errors.New("no such alert: cpu")}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	resp := sendRequest(t, client, r, 2, "AckAlert", map[string]string{"key": "cpu"})

	if resp.OK {
		t.Fatalf("resp.OK = true, want false (error propagation)")
	}
	if resp.Error != "no such alert: cpu" {
		t.Errorf("resp.Error = %q, want %q", resp.Error, "no such alert: cpu")
	}
	if fake.ackedKey != "cpu" {
		t.Errorf("fake.ackedKey = %q, want %q (dispatch should still call through)", fake.ackedKey, "cpu")
	}

	client.Close()
	<-done
}

func TestHandleConnSubscribeReturnsError(t *testing.T) {
	fake := &fakeAPI{}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	resp := sendRequest(t, client, r, 3, "Subscribe", struct{}{})

	if resp.OK {
		t.Fatalf("resp.OK = true, want false")
	}
	if resp.Error == "" {
		t.Errorf("resp.Error = %q, want a non-empty streaming-unsupported message", resp.Error)
	}

	client.Close()
	<-done
}

func TestHandleConnWrongVersionHelloRejected(t *testing.T) {
	fake := &fakeAPI{}
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		handleConn(fake, server)
		close(done)
	}()

	if err := writeFrame(client, hello{Hello: "serverwatch-control", Version: ProtocolVersion + 1}); err != nil {
		t.Fatalf("writeFrame(hello): %v", err)
	}

	r := bufio.NewReader(client)
	var resp response
	if err := readFrame(r, &resp); err != nil {
		t.Fatalf("readFrame(rejection): %v", err)
	}
	if resp.OK {
		t.Fatalf("resp.OK = true, want false for a version-mismatched hello")
	}
	if resp.Error == "" {
		t.Errorf("resp.Error is empty, want a version-mismatch message")
	}

	// The server must close the connection after rejecting the hello: a
	// further read should fail rather than hang or yield a real response.
	var again response
	if err := readFrame(r, &again); err == nil {
		t.Errorf("expected a read error after hello rejection, got a frame: %+v", again)
	}

	<-done
}

func TestApplyConfigAndConfigRoundTrip(t *testing.T) {
	fake := &fakeAPI{cfg: &config.Config{SampleInterval: 30, FastInterval: 5}}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	// Config: read back the raw config.Config, preserving zero/omitted
	// fields the way configForDisplay would not.
	resp := sendRequest(t, client, r, 4, "Config", struct{}{})
	if !resp.OK {
		t.Fatalf("Config resp.OK = false, want true (error: %s)", resp.Error)
	}
	var gotCfg config.Config
	if err := json.Unmarshal(resp.Result, &gotCfg); err != nil {
		t.Fatalf("unmarshal Config result: %v", err)
	}
	if !reflect.DeepEqual(gotCfg, *fake.cfg) {
		t.Errorf("Config got %+v, want %+v", gotCfg, *fake.cfg)
	}

	// ApplyConfig: the fake should receive the unmarshaled struct.
	newCfg := config.Config{SampleInterval: 60, FastInterval: 10}
	resp = sendRequest(t, client, r, 5, "ApplyConfig", map[string]config.Config{"config": newCfg})
	if !resp.OK {
		t.Fatalf("ApplyConfig resp.OK = false, want true (error: %s)", resp.Error)
	}
	if fake.appliedConfig == nil || !reflect.DeepEqual(*fake.appliedConfig, newCfg) {
		t.Errorf("fake.appliedConfig = %+v, want %+v", fake.appliedConfig, newCfg)
	}

	client.Close()
	<-done
}
