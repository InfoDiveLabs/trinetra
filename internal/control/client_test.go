package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// shortSocketPath returns a short temp socket path: t.TempDir() plus a long test
// name and macOS $TMPDIR can exceed the ~104-byte sun_path limit.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw-ctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

var errWantedByTest = errors.New("no such alert: cpu")

// startTestServer runs a real Serve loop over a temp unix socket backed by fake,
// requiring token (empty means no auth), and returns the path and a cleanup func.
func startTestServer(t *testing.T, fake core.API, token string) string {
	t.Helper()
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go Serve(fake, ln, token)
	t.Cleanup(func() { ln.Close() })
	return path
}

func falsePtr() *bool {
	b := false
	return &b
}

func TestClientRoundTripsEveryMethod(t *testing.T) {
	fake := &fakeAPI{
		snapshot:    core.DashboardView{CPU: 42.5, Online: true, Cores: 8},
		monitoring:  core.MonitoringView{FailedUnits: []string{"nginx.service"}},
		series:      []core.SeriesPoint{{TS: 1, Min: 1, Avg: 2, Max: 3}},
		events:      []core.DownEventView{{Type: "net_down", Start: 10, End: 20, DurationSec: 10}},
		active:      []core.AlertRecord{{Key: "cpu", Severity: "warn", Kind: "threshold", Source: "cpu", Time: 5}},
		history:     []core.AlertRecord{{Key: "mem", Severity: "crit", Kind: "threshold", Source: "mem", Time: 6, Acked: true}},
		doctor:      core.DoctorReport{DockerAccess: "available=true method=socket", ThermalZones: 2},
		hostInfo:    core.HostInfoView{Hostname: "attic-pi", Kernel: "6.1.0", OS: "Debian 12", CPUModel: "Test CPU", CPUCores: 4, CPUThreads: 8, MemTotalBytes: 8 << 30, BootTime: 1000, UptimeSec: 3600, Disks: []core.HostDiskView{{Device: "sda", Model: "SSD", SizeBytes: 512 << 30, FSType: "ext4", Mount: "/"}}},
		ackAlertErr: nil,
	}
	fake.cfg = &config.Config{SampleInterval: 30, FastInterval: 5}
	fake.cfg.Collect.ContainerStats = falsePtr()
	// NetThroughput left nil deliberately.
	fake.enrollPIN = "424242"
	fake.enrollEnrolled = false
	fake.monitorTargets = []core.TargetView{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}}

	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	var _ core.API = (*Client)(nil)

	if got, err := client.Snapshot(); err != nil || !reflect.DeepEqual(got, fake.snapshot) {
		t.Errorf("Snapshot() = %+v, %v; want %+v, nil", got, err, fake.snapshot)
	}

	if got, err := client.Monitoring(); err != nil || !reflect.DeepEqual(got, fake.monitoring) {
		t.Errorf("Monitoring() = %+v, %v; want %+v, nil", got, err, fake.monitoring)
	}

	if got, err := client.Series("cpu", 1, 2, core.Res1m); err != nil || !reflect.DeepEqual(got, fake.series) {
		t.Errorf("Series() = %+v, %v; want %+v, nil", got, err, fake.series)
	}

	if got, err := client.Events(1, 2); err != nil || !reflect.DeepEqual(got, fake.events) {
		t.Errorf("Events() = %+v, %v; want %+v, nil", got, err, fake.events)
	}

	if got, err := client.ActiveAlerts(); err != nil || !reflect.DeepEqual(got, fake.active) {
		t.Errorf("ActiveAlerts() = %+v, %v; want %+v, nil", got, err, fake.active)
	}

	if got, err := client.AlertHistory(0, 10); err != nil || !reflect.DeepEqual(got, fake.history) {
		t.Errorf("AlertHistory() = %+v, %v; want %+v, nil", got, err, fake.history)
	}

	if got, err := client.Doctor(); err != nil || !reflect.DeepEqual(got, fake.doctor) {
		t.Errorf("Doctor() = %+v, %v; want %+v, nil", got, err, fake.doctor)
	}

	if got, err := client.HostInfo(); err != nil || !reflect.DeepEqual(got, fake.hostInfo) {
		t.Errorf("HostInfo() = %+v, %v; want %+v, nil", got, err, fake.hostInfo)
	}

	if pin, enrolled, err := client.EnrollmentPIN(context.Background()); err != nil || pin != fake.enrollPIN || enrolled != fake.enrollEnrolled {
		t.Errorf("EnrollmentPIN() = %q, %v, %v; want %q, %v, nil", pin, enrolled, err, fake.enrollPIN, fake.enrollEnrolled)
	}

	if got, err := client.MonitorTargets(context.Background()); err != nil || !reflect.DeepEqual(got, fake.monitorTargets) {
		t.Errorf("MonitorTargets() = %+v, %v; want %+v, nil", got, err, fake.monitorTargets)
	}

	// Config round trip must preserve *bool omitempty semantics.
	gotCfg, err := client.Config()
	if err != nil {
		t.Fatalf("Config() error: %v", err)
	}
	if !reflect.DeepEqual(gotCfg, fake.cfg) {
		t.Errorf("Config() = %+v, want %+v", gotCfg, fake.cfg)
	}
	if gotCfg.Collect.ContainerStats == nil || *gotCfg.Collect.ContainerStats != false {
		t.Errorf("Config().Collect.ContainerStats = %v, want explicit false", gotCfg.Collect.ContainerStats)
	}
	if gotCfg.Collect.NetThroughput != nil {
		t.Errorf("Config().Collect.NetThroughput = %v, want nil (unset)", gotCfg.Collect.NetThroughput)
	}

	// ApplyConfig: reaches the fake with the exact struct sent.
	newCfg := &config.Config{SampleInterval: 60, FastInterval: 10}
	if err := client.ApplyConfig(newCfg); err != nil {
		t.Fatalf("ApplyConfig() error: %v", err)
	}
	if fake.appliedConfig == nil || !reflect.DeepEqual(*fake.appliedConfig, *newCfg) {
		t.Errorf("fake.appliedConfig = %+v, want %+v", fake.appliedConfig, newCfg)
	}

	if err := client.AckAlert("cpu"); err != nil {
		t.Fatalf("AckAlert() error: %v", err)
	}
	if fake.ackedKey != "cpu" {
		t.Errorf("fake.ackedKey = %q, want %q", fake.ackedKey, "cpu")
	}

	if err := client.UnackAlert("mem"); err != nil {
		t.Fatalf("UnackAlert() error: %v", err)
	}
	if fake.unackedKey != "mem" {
		t.Errorf("fake.unackedKey = %q, want %q", fake.unackedKey, "mem")
	}

	if err := client.TestChannel("telegram"); err != nil {
		t.Fatalf("TestChannel() error: %v", err)
	}
	if fake.testedChannel != "telegram" {
		t.Errorf("fake.testedChannel = %q, want %q", fake.testedChannel, "telegram")
	}

	if err := client.ValidateChannel(config.ChannelConfig{Name: "hook", Type: "webhook"}); err != nil {
		t.Fatalf("ValidateChannel() error: %v", err)
	}
	if fake.validatedChannel.Name != "hook" {
		t.Errorf("fake.validatedChannel.Name = %q, want %q", fake.validatedChannel.Name, "hook")
	}

	if _, err := client.Subscribe(context.Background()); err == nil {
		t.Errorf("Subscribe() error = nil, want a streaming-unsupported error")
	}
}

// TestClientValidateChannelSurfacesError: over a real Serve loop, ValidateChannel
// must surface the server-side error (undeliverable channel, #79).
func TestClientValidateChannelSurfacesError(t *testing.T) {
	wantErr := `telegram channel "phone": chat_id not configured`
	fake := &fakeAPI{validateChannelErr: errors.New(wantErr)}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	cc := config.ChannelConfig{Name: "phone", Type: "telegram"}
	err = client.ValidateChannel(cc)
	if err == nil || err.Error() != wantErr {
		t.Fatalf("ValidateChannel() error = %v, want %q", err, wantErr)
	}
	if fake.validatedChannel.Name != "phone" {
		t.Errorf("fake.validatedChannel.Name = %q, want %q (request should still reach the server)", fake.validatedChannel.Name, "phone")
	}
}

func TestClientSurfacesMethodError(t *testing.T) {
	fake := &fakeAPI{ackAlertErr: errWantedByTest}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	err = client.AckAlert("cpu")
	if err == nil {
		t.Fatalf("AckAlert() error = nil, want %v", errWantedByTest)
	}
	if err.Error() != errWantedByTest.Error() {
		t.Errorf("AckAlert() error = %q, want %q", err.Error(), errWantedByTest.Error())
	}
	if fake.ackedKey != "cpu" {
		t.Errorf("fake.ackedKey = %q, want %q (dispatch should still call through)", fake.ackedKey, "cpu")
	}
}

// TestClientEnrollmentPINSurfacesError: an EnrollmentPIN error (#90) must
// round-trip unchanged, not become a zero-value success.
func TestClientEnrollmentPINSurfacesError(t *testing.T) {
	wantErr := "trinetra: enrollment pin requires a running daemon; dial the control socket instead"
	fake := &fakeAPI{enrollErr: errors.New(wantErr)}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	pin, enrolled, err := client.EnrollmentPIN(context.Background())
	if err == nil || err.Error() != wantErr {
		t.Fatalf("EnrollmentPIN() error = %v, want %q", err, wantErr)
	}
	if pin != "" || enrolled {
		t.Errorf("EnrollmentPIN() = %q, %v on error, want zero values", pin, enrolled)
	}
}

// TestClientMonitorTargetsSurfacesError: a discovery error must round-trip
// unchanged.
func TestClientMonitorTargetsSurfacesError(t *testing.T) {
	wantErr := "discovery failed"
	fake := &fakeAPI{monitorTargetsErr: errors.New(wantErr)}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	got, err := client.MonitorTargets(context.Background())
	if err == nil || err.Error() != wantErr {
		t.Fatalf("MonitorTargets() error = %v, want %q", err, wantErr)
	}
	if len(got) != 0 {
		t.Errorf("MonitorTargets() = %+v on error, want empty", got)
	}
}

// TestClientCallTimesOutWhenServerNeverResponds: the per-call read deadline must
// fire when the server completes the handshake, reads the request, and never
// answers.
func TestClientCallTimesOutWhenServerNeverResponds(t *testing.T) {
	orig := callTimeout
	callTimeout = 100 * time.Millisecond
	t.Cleanup(func() { callTimeout = orig })

	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(conn)

		var clientHello hello
		if err := readFrame(r, &clientHello); err != nil {
			return
		}
		if err := writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion}); err != nil {
			return
		}

		var req request
		_ = readFrame(r, &req)
		// Never respond: the client's read deadline must fire.
	}()

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	start := time.Now()
	_, err = client.Snapshot()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Snapshot() error = nil, want a timeout error")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("Snapshot() error = %v, want a net.Error with Timeout() == true", err)
	}
	if elapsed > time.Second {
		t.Errorf("Snapshot() took %v, want it to return promptly after callTimeout (%v)", elapsed, callTimeout)
	}
}

// TestClientCallRejectsMismatchedResponseID: call refuses a response whose id
// differs from the request's.
func TestClientCallRejectsMismatchedResponseID(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)

		var clientHello hello
		if err := readFrame(r, &clientHello); err != nil {
			return
		}
		if err := writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion}); err != nil {
			return
		}

		var req request
		if err := readFrame(r, &req); err != nil {
			return
		}
		// Reply with a bogus id, not req.ID.
		_ = writeFrame(conn, response{ID: req.ID + 1, OK: true, Result: []byte("{}")})
	}()

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	_, err = client.Snapshot()
	if err == nil {
		t.Fatalf("Snapshot() error = nil, want an error for a mismatched response id")
	}
}

// TestDialWithMatchingTokenRoundTrips: a matching token completes the handshake.
func TestDialWithMatchingTokenRoundTrips(t *testing.T) {
	fake := &fakeAPI{snapshot: core.DashboardView{CPU: 7}}
	path := startTestServer(t, fake, "secret")

	client, err := Dial(path, "secret")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	got, err := client.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	if !reflect.DeepEqual(got, fake.snapshot) {
		t.Errorf("Snapshot() = %+v, want %+v", got, fake.snapshot)
	}
}

// TestDialWithWrongTokenRejected: a wrong token fails Dial itself, since the
// rejection frame does not parse as a valid hello.
func TestDialWithWrongTokenRejected(t *testing.T) {
	fake := &fakeAPI{snapshot: core.DashboardView{CPU: 7}}
	path := startTestServer(t, fake, "secret")

	if _, err := Dial(path, "wrong"); err == nil {
		t.Fatalf("Dial() error = nil, want an error for a wrong token")
	}
}

// TestDialWithEmptyTokenRejectedWhenAuthConfigured: an empty token must not skip
// the check.
func TestDialWithEmptyTokenRejectedWhenAuthConfigured(t *testing.T) {
	fake := &fakeAPI{snapshot: core.DashboardView{CPU: 7}}
	path := startTestServer(t, fake, "secret")

	if _, err := Dial(path, ""); err == nil {
		t.Fatalf("Dial() error = nil, want an error for a missing token")
	}
}

// TestClientSubscribeReceivesPublishedEvents: Subscribe opens its own connection
// and delivers server-side events on the returned channel.
func TestClientSubscribeReceivesPublishedEvents(t *testing.T) {
	fake := &fakeAPI{subscribeCh: make(chan core.Event, 4), subscribeCancelled: make(chan struct{})}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	want := core.Event{Kind: "alert_fire", Source: "cpu", Severity: "warn", Title: "cpu high", Time: 42}
	fake.subscribeCh <- want

	select {
	case got, ok := <-events:
		if !ok {
			t.Fatal("events channel closed before delivering the published event")
		}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the published event")
	}
}

// TestClientSubscribeCtxCancelUnsubscribesAndClosesChannel: cancelling the ctx
// makes the server unsubscribe and closes the returned channel.
func TestClientSubscribeCtxCancelUnsubscribesAndClosesChannel(t *testing.T) {
	fake := &fakeAPI{subscribeCh: make(chan core.Event), subscribeCancelled: make(chan struct{})}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	events, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()

	select {
	case <-fake.subscribeCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("server never cancelled api.Subscribe's ctx after the client's ctx was cancelled")
	}

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("events channel delivered a value instead of closing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events channel never closed after ctx cancel")
	}
}

// TestClientNormalCallWorksWhileSubscriptionActive: the dedicated stream
// connection must not block calls on the primary connection.
func TestClientNormalCallWorksWhileSubscriptionActive(t *testing.T) {
	fake := &fakeAPI{
		snapshot:           core.DashboardView{CPU: 7},
		subscribeCh:        make(chan core.Event, 1),
		subscribeCancelled: make(chan struct{}),
	}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := client.Subscribe(ctx); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	got, err := client.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() while a subscription is active: %v", err)
	}
	if !reflect.DeepEqual(got, fake.snapshot) {
		t.Errorf("Snapshot() = %+v, want %+v", got, fake.snapshot)
	}
}

// TestClientReconnectsAfterTransportFailure: after a transport failure poisons
// the connection, the next call must reconnect and succeed. Regression for
// issue #105: trinetra-web holds one long-lived Client, and one late response
// used to desync it until a core restart, leaving the dashboard all-zero.
//
// The server hands the first connection a mismatched id and drops it, then
// serves later connections normally.
func TestClientReconnectsAfterTransportFailure(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	want := core.DashboardView{CPU: 55, Cores: 8, Online: true}

	go func() {
		first := true
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			corrupt := first
			first = false
			go func(conn net.Conn, corrupt bool) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				var h hello
				if err := readFrame(r, &h); err != nil {
					return
				}
				if err := writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion}); err != nil {
					return
				}
				for {
					var req request
					if err := readFrame(r, &req); err != nil {
						return
					}
					id := req.ID
					if corrupt {
						// Desync this connection like a late response would: wrong id, then drop.
						id = req.ID + 1
					}
					b, _ := json.Marshal(want)
					if err := writeFrame(conn, response{ID: id, OK: true, Result: b}); err != nil {
						return
					}
					if corrupt {
						return
					}
				}
			}(conn, corrupt)
		}
	}()

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// First call lands on the desynced connection: it must fail.
	if _, err := client.Snapshot(); err == nil {
		t.Fatal("first Snapshot() error = nil, want a transport error from the desynced connection")
	}

	// Second call must transparently reconnect and succeed.
	got, err := client.Snapshot()
	if err != nil {
		t.Fatalf("second Snapshot() after reconnect: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Snapshot() after reconnect = %+v, want %+v", got, want)
	}
}

// coreErrSentinelTexts extracts the message of every exported
// "var ErrXxx = errors.New(...)" in internal/core, so the sentinel coverage test
// checks wireErrSentinels against core itself rather than a second hand-kept list.
func coreErrSentinelTexts(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("../core/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files matched ../core/*.go -- wrong working directory for this test?")
	}
	texts := map[string]string{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					if !name.IsExported() || !strings.HasPrefix(name.Name, "Err") {
						continue
					}
					call, ok := vs.Values[i].(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						continue
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "New" {
						continue
					}
					if pkgIdent, ok := sel.X.(*ast.Ident); !ok || pkgIdent.Name != "errors" {
						continue
					}
					lit, ok := call.Args[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					texts[name.Name] = s
				}
			}
		}
	}
	return texts
}

// TestWireErrSentinelsCoverEveryCoreSentinel: every exported core.Err* sentinel
// declared via errors.New must appear in wireErrSentinels, or errors.Is is
// silently lost over the control socket.
func TestWireErrSentinelsCoverEveryCoreSentinel(t *testing.T) {
	want := coreErrSentinelTexts(t)
	if len(want) == 0 {
		t.Fatal("found no core.Err* sentinel declarations to check -- coreErrSentinelTexts's AST scan is probably broken")
	}
	have := map[string]bool{}
	for _, s := range wireErrSentinels {
		have[s.Error()] = true
	}
	for name, text := range want {
		if !have[text] {
			t.Errorf("core.%s (%q) is not in wireErrSentinels -- a control-socket caller's errors.Is(err, core.%s) would silently stop working for a %%w-wrapped instance of it", name, text, name)
		}
	}
}

// serveClosingAfterOne answers one request per connection and then closes
// it, the way the daemon drops a connection that sat past idleTimeout.
func serveClosingAfterOne(t *testing.T, want core.DashboardView) (path string, accepts *atomic.Int32) {
	t.Helper()
	path = shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	accepts = new(atomic.Int32)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				var h hello
				if readFrame(r, &h) != nil || writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion}) != nil {
					return
				}
				var req request
				if readFrame(r, &req) != nil {
					return
				}
				b, _ := json.Marshal(want)
				_ = writeFrame(conn, response{ID: req.ID, OK: true, Result: b})
			}(conn)
		}
	}()
	return path, accepts
}

func TestClientRedialsAfterIdleClose(t *testing.T) {
	orig := redialAfter
	redialAfter = 50 * time.Millisecond
	t.Cleanup(func() { redialAfter = orig })

	want := core.DashboardView{CPU: 12, Cores: 64, Online: true}
	path, accepts := serveClosingAfterOne(t, want)
	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Snapshot(); err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	got, err := client.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after an idle gap failed instead of re-dialing: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if n := accepts.Load(); n != 2 {
		t.Errorf("connections = %d, want 2", n)
	}
}
