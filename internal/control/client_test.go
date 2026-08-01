package control

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// shortSocketPath returns a temp-dir socket path independent of the test
// name: a plain t.TempDir() nests under a per-test-name directory that,
// combined with a long test name and a long $TMPDIR (common on macOS,
// e.g. under /var/folders/...), can exceed the ~104-byte sun_path limit
// unix domain sockets are bound by. os.MkdirTemp with a short, fixed
// prefix keeps the path well under that limit regardless of the calling
// test's name.
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

// startTestServer stands up a real Serve loop over a temp unix socket
// backed by fake, and returns the socket path plus a cleanup func.
func startTestServer(t *testing.T, fake core.API) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go Serve(fake, ln)
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
		ackAlertErr: nil,
	}
	fake.cfg = &config.Config{SampleInterval: 30, FastInterval: 5}
	fake.cfg.Collect.ContainerStats = falsePtr()
	// NetThroughput left nil deliberately.

	path := startTestServer(t, fake)

	client, err := Dial(path)
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

	// Config round trip: must preserve the *bool omitempty semantics --
	// ContainerStats explicitly false, NetThroughput left nil.
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

	if _, err := client.Subscribe(context.Background()); err == nil {
		t.Errorf("Subscribe() error = nil, want a streaming-unsupported error")
	}
}

func TestClientSurfacesMethodError(t *testing.T) {
	fake := &fakeAPI{ackAlertErr: errWantedByTest}
	path := startTestServer(t, fake)

	client, err := Dial(path)
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

// TestClientCallTimesOutWhenServerNeverResponds proves the per-call read
// deadline in Client.call fires instead of hanging forever when the server
// accepts the connection, completes the hello handshake, reads the
// request, and then never writes a response -- e.g. a wedged server
// method. Without a deadline this would hang the test (and, in production,
// every caller sharing the Client, since call holds the mutex across the
// read).
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
		// Deliberately never write a response: the client's read deadline
		// must fire instead of this call hanging forever.
	}()

	client, err := Dial(path)
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

// TestClientCallRejectsMismatchedResponseID proves Client.call refuses a
// response whose id does not match the request it just sent, rather than
// silently handing the caller a result meant for a different call.
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

	client, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	_, err = client.Snapshot()
	if err == nil {
		t.Fatalf("Snapshot() error = nil, want an error for a mismatched response id")
	}
}
