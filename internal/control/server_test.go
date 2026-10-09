package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fakeAPI is a minimal core.API test double: read fields back one read method
// each, and write methods record their argument and return the configured error.
type fakeAPI struct {
	snapshot   core.DashboardView
	monitoring core.MonitoringView
	series     []core.SeriesPoint
	events     []core.DownEventView
	active     []core.AlertRecord
	history    []core.AlertRecord
	cfg        *config.Config
	doctor     core.DoctorReport
	hostInfo   core.HostInfoView

	enrollPIN      string
	enrollEnrolled bool
	// enrollErr, when set, is returned by EnrollmentPIN.
	enrollErr error

	monitorTargets []core.TargetView
	// monitorTargetsErr, when set, is returned by MonitorTargets.
	monitorTargetsErr error

	// updateStatus is returned by UpdateStatus and UpdateCheck on success; the
	// matching *Err fields make them fail.
	updateStatus    core.UpdateStatusView
	updateStatusErr error
	updateCheckErr  error
	// appliedUpdateVersion records UpdateApply's argument; updateApplyErr is its error.
	appliedUpdateVersion string
	updateApplyErr       error
	// updateRollbackCalled/updateRollbackErr cover UpdateRollback.
	updateRollbackCalled bool
	updateRollbackErr    error

	appliedConfig    *config.Config
	ackedKey         string
	unackedKey       string
	testedChannel    string
	validatedChannel config.ChannelConfig

	// ackAlertErr, when set, is returned by AckAlert.
	ackAlertErr error
	// validateChannelErr, when set, is returned by ValidateChannel.
	validateChannelErr error

	// subscribeCh, when non-nil, makes Subscribe return it instead of the default "not
	// implemented" error. subscribeCancelled, when also non-nil.
	subscribeCh        chan core.Event
	subscribeCancelled chan struct{}
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

func (f *fakeAPI) Config() (*config.Config, error)      { return f.cfg, nil }
func (f *fakeAPI) Doctor() (core.DoctorReport, error)   { return f.doctor, nil }
func (f *fakeAPI) HostInfo() (core.HostInfoView, error) { return f.hostInfo, nil }
func (f *fakeAPI) ContainerLogs(name string, lines int) (string, error) {
	return "logs of " + name, nil
}
func (f *fakeAPI) Version() (string, error) { return "v-test", nil }

func (f *fakeAPI) EnrollmentPIN(ctx context.Context) (string, bool, error) {
	return f.enrollPIN, f.enrollEnrolled, f.enrollErr
}

func (f *fakeAPI) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	return f.monitorTargets, f.monitorTargetsErr
}

func (f *fakeAPI) UpdateStatus() (core.UpdateStatusView, error) {
	return f.updateStatus, f.updateStatusErr
}

func (f *fakeAPI) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	return f.updateStatus, f.updateCheckErr
}

func (f *fakeAPI) UpdateApply(ctx context.Context, version string) error {
	f.appliedUpdateVersion = version
	return f.updateApplyErr
}

func (f *fakeAPI) UpdateRollback() error {
	f.updateRollbackCalled = true
	return f.updateRollbackErr
}

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

func (f *fakeAPI) ValidateChannel(cc config.ChannelConfig) error {
	f.validatedChannel = cc
	return f.validateChannelErr
}

func (f *fakeAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	if f.subscribeCh == nil {
		return nil, errors.New("fakeAPI: Subscribe not implemented")
	}
	go func() {
		<-ctx.Done()
		close(f.subscribeCancelled)
		// Unsubscribing closes the subscriber's channel, which ends streamSubscribe's
		// range loop instead of blocking forever.
		close(f.subscribeCh)
	}()
	return f.subscribeCh, nil
}

var _ core.API = (*fakeAPI)(nil)

// dialTestConn runs handleConn on one end of a net.Pipe with fake as the API and returns
// the other end plus a reader, past a valid hello. done closes when handleConn returns.
func dialTestConn(t *testing.T, fake core.API) (net.Conn, *bufio.Reader, <-chan struct{}) {
	t.Helper()
	server, client := net.Pipe()

	done := make(chan struct{})
	go func() {
		handleConn(fake, server, "")
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

// TestHandleConnValidateChannelPropagatesError: an api.ValidateChannel error
// comes back as ok=false and the channel argument reaches the API.
func TestHandleConnValidateChannelPropagatesError(t *testing.T) {
	fake := &fakeAPI{validateChannelErr: errors.New(`telegram channel "phone": chat_id not configured`)}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	cc := config.ChannelConfig{Name: "phone", Type: "telegram"}
	resp := sendRequest(t, client, r, 4, "ValidateChannel", map[string]any{"channel": cc})

	if resp.OK {
		t.Fatalf("resp.OK = true, want false (error propagation)")
	}
	if resp.Error != `telegram channel "phone": chat_id not configured` {
		t.Errorf("resp.Error = %q, want the buildNotifier-style chat_id error", resp.Error)
	}
	if fake.validatedChannel.Name != "phone" {
		t.Errorf("fake.validatedChannel.Name = %q, want %q (dispatch should still call through)", fake.validatedChannel.Name, "phone")
	}

	client.Close()
	<-done
}

// TestHandleConnEnrollmentPINRoundTrip (#90): pin/enrolled come back unmodified.
func TestHandleConnEnrollmentPINRoundTrip(t *testing.T) {
	fake := &fakeAPI{enrollPIN: "424242", enrollEnrolled: false}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	resp := sendRequest(t, client, r, 5, "EnrollmentPIN", struct{}{})

	if !resp.OK {
		t.Fatalf("resp.OK = false, want true (error: %s)", resp.Error)
	}
	var got enrollmentPINResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got.PIN != "424242" || got.Enrolled != false {
		t.Errorf("got %+v, want {PIN:424242 Enrolled:false}", got)
	}

	client.Close()
	<-done
}

// TestHandleConnEnrollmentPINPropagatesError: an EnrollmentPIN error comes back
// as ok=false, not a zero-value success.
func TestHandleConnEnrollmentPINPropagatesError(t *testing.T) {
	fake := &fakeAPI{enrollErr: errors.New("trinetra: enrollment pin requires a running daemon")}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	resp := sendRequest(t, client, r, 6, "EnrollmentPIN", struct{}{})

	if resp.OK {
		t.Fatalf("resp.OK = true, want false (error propagation)")
	}
	if resp.Error != "trinetra: enrollment pin requires a running daemon" {
		t.Errorf("resp.Error = %q, want the fake's enrollErr text", resp.Error)
	}

	client.Close()
	<-done
}

// TestHandleConnMonitorTargetsRoundTrip: the target list comes back unmodified.
func TestHandleConnMonitorTargetsRoundTrip(t *testing.T) {
	fake := &fakeAPI{monitorTargets: []core.TargetView{
		{ID: "disk:/", Kind: "disk", Display: "/", Available: true},
	}}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	resp := sendRequest(t, client, r, 7, "MonitorTargets", struct{}{})

	if !resp.OK {
		t.Fatalf("resp.OK = false, want true (error: %s)", resp.Error)
	}
	var got []core.TargetView
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(got) != 1 || got[0] != fake.monitorTargets[0] {
		t.Errorf("got %+v, want %+v", got, fake.monitorTargets)
	}

	client.Close()
	<-done
}

// TestHandleConnMonitorTargetsPropagatesError covers MonitorTargets errors.
func TestHandleConnMonitorTargetsPropagatesError(t *testing.T) {
	fake := &fakeAPI{monitorTargetsErr: errors.New("discovery failed")}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	resp := sendRequest(t, client, r, 8, "MonitorTargets", struct{}{})

	if resp.OK {
		t.Fatalf("resp.OK = true, want false (error propagation)")
	}
	if resp.Error != "discovery failed" {
		t.Errorf("resp.Error = %q, want the fake's monitorTargetsErr text", resp.Error)
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

// TestStreamSubscribeSendsEventsInOrderAfterAck: after the ack, each event from
// the Subscribe channel arrives as a stream frame (ID streamID) in order.
func TestStreamSubscribeSendsEventsInOrderAfterAck(t *testing.T) {
	fake := &fakeAPI{subscribeCh: make(chan core.Event, 4), subscribeCancelled: make(chan struct{})}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	raw, err := json.Marshal(struct{}{})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if err := writeFrame(client, request{ID: 9, Method: "Subscribe", Params: raw}); err != nil {
		t.Fatalf("writeFrame(subscribe request): %v", err)
	}

	var ack response
	if err := readFrame(r, &ack); err != nil {
		t.Fatalf("readFrame(ack): %v", err)
	}
	if !ack.OK || ack.ID != 9 {
		t.Fatalf("ack = %+v, want {ID:9 OK:true}", ack)
	}

	want := []core.Event{
		{Kind: "alert_fire", Source: "cpu", Severity: "warn", Title: "cpu high", Time: 1},
		{Kind: "snapshot", Time: 2},
		{Kind: "alert_recover", Source: "cpu", Severity: "warn", Title: "cpu high", Time: 3},
	}
	for _, ev := range want {
		fake.subscribeCh <- ev
	}

	for i, w := range want {
		var frame response
		if err := readFrame(r, &frame); err != nil {
			t.Fatalf("readFrame(event %d): %v", i, err)
		}
		if frame.ID != streamID || !frame.OK {
			t.Fatalf("frame %d = %+v, want {ID:%d OK:true}", i, frame, streamID)
		}
		var got core.Event
		if err := json.Unmarshal(frame.Result, &got); err != nil {
			t.Fatalf("unmarshal event %d: %v", i, err)
		}
		if got != w {
			t.Errorf("event %d = %+v, want %+v", i, got, w)
		}
	}

	client.Close()
	<-done
}

// TestStreamSubscribeDisconnectCancelsContext: the ctx passed to api.Subscribe must derive
// from the connection's lifetime.
func TestStreamSubscribeDisconnectCancelsContext(t *testing.T) {
	fake := &fakeAPI{subscribeCh: make(chan core.Event), subscribeCancelled: make(chan struct{})}
	client, r, done := dialTestConn(t, fake)

	raw, err := json.Marshal(struct{}{})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if err := writeFrame(client, request{ID: 1, Method: "Subscribe", Params: raw}); err != nil {
		t.Fatalf("writeFrame(subscribe request): %v", err)
	}
	var ack response
	if err := readFrame(r, &ack); err != nil {
		t.Fatalf("readFrame(ack): %v", err)
	}
	if !ack.OK {
		t.Fatalf("ack.OK = false, want true (error: %s)", ack.Error)
	}

	client.Close()

	select {
	case <-fake.subscribeCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("api.Subscribe's ctx was never cancelled after the client disconnected")
	}

	<-done
}

func TestHandleConnWrongVersionHelloRejected(t *testing.T) {
	fake := &fakeAPI{}
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		handleConn(fake, server, "")
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

	// The server must close the connection after rejecting the hello.
	var again response
	if err := readFrame(r, &again); err == nil {
		t.Errorf("expected a read error after hello rejection, got a frame: %+v", again)
	}

	<-done
}

// TestHandleConnRightTokenAccepted: the configured token completes the handshake.
func TestHandleConnRightTokenAccepted(t *testing.T) {
	fake := &fakeAPI{snapshot: core.DashboardView{CPU: 1}}
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		handleConn(fake, server, "secret")
		close(done)
	}()

	if err := writeFrame(client, hello{Hello: helloMagic, Version: ProtocolVersion, Token: "secret"}); err != nil {
		t.Fatalf("writeFrame(hello): %v", err)
	}
	r := bufio.NewReader(client)
	var gotHello hello
	if err := readFrame(r, &gotHello); err != nil {
		t.Fatalf("readFrame(server hello): %v", err)
	}
	if gotHello.Hello != helloMagic || gotHello.Version != ProtocolVersion {
		t.Fatalf("server hello = %+v, want valid ack", gotHello)
	}

	resp := sendRequest(t, client, r, 1, "Snapshot", struct{}{})
	if !resp.OK {
		t.Fatalf("resp.OK = false, want true (error: %s)", resp.Error)
	}

	client.Close()
	<-done
}

// TestHandleConnWrongTokenRejected: a wrong token is rejected at the handshake
// and the connection closed, like a version mismatch.
func TestHandleConnWrongTokenRejected(t *testing.T) {
	fake := &fakeAPI{}
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		handleConn(fake, server, "secret")
		close(done)
	}()

	if err := writeFrame(client, hello{Hello: helloMagic, Version: ProtocolVersion, Token: "wrong"}); err != nil {
		t.Fatalf("writeFrame(hello): %v", err)
	}

	r := bufio.NewReader(client)
	var resp response
	if err := readFrame(r, &resp); err != nil {
		t.Fatalf("readFrame(rejection): %v", err)
	}
	if resp.OK {
		t.Fatalf("resp.OK = true, want false for a wrong-token hello")
	}
	if resp.Error == "" {
		t.Errorf("resp.Error is empty, want a token-mismatch message")
	}

	var again response
	if err := readFrame(r, &again); err == nil {
		t.Errorf("expected a read error after token rejection, got a frame: %+v", again)
	}

	<-done
}

// TestHandleConnEmptyTokenRejectedWhenAuthConfigured: omitting the token cannot
// skip the check.
func TestHandleConnEmptyTokenRejectedWhenAuthConfigured(t *testing.T) {
	fake := &fakeAPI{}
	server, client := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		handleConn(fake, server, "secret")
		close(done)
	}()

	if err := writeFrame(client, hello{Hello: helloMagic, Version: ProtocolVersion}); err != nil {
		t.Fatalf("writeFrame(hello): %v", err)
	}

	r := bufio.NewReader(client)
	var resp response
	if err := readFrame(r, &resp); err != nil {
		t.Fatalf("readFrame(rejection): %v", err)
	}
	if resp.OK {
		t.Fatalf("resp.OK = true, want false for a missing-token hello")
	}

	<-done
}

func TestApplyConfigAndConfigRoundTrip(t *testing.T) {
	fake := &fakeAPI{cfg: &config.Config{SampleInterval: 30, FastInterval: 5}}
	client, r, done := dialTestConn(t, fake)
	defer client.Close()

	// Config: the raw config.Config, preserving zero/omitted fields.
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

	// ApplyConfig: the fake receives the unmarshaled struct.
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
