package control

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// helloMagic is the fixed Hello value both ends of the control socket must
// send in their handshake frame, alongside a matching ProtocolVersion.
const helloMagic = "serverwatch-control"

// helloTimeout and idleTimeout bound the two read deadlines handleConn
// enforces on each connection: helloTimeout for the opening hello frame,
// idleTimeout for every request frame after that. Without these, a
// connection that connects and then never speaks (accidentally or by a
// misbehaving/hostile peer) would tie up its goroutine forever. Unlike
// callTimeout (internal/control/client.go), these stay consts: Serve
// spawns a handleConn goroutine per connection that can outlive the test
// that started it, so a mutable package var here would be read and
// written from different goroutines with no synchronization between
// tests.
const (
	helloTimeout = 10 * time.Second
	idleTimeout  = 5 * time.Minute
)

// streamWriteTimeout bounds each stream-frame write in streamSubscribe. A
// streaming connection is expected to sit idle between events (unlike a
// request/response connection, bounded above by idleTimeout instead), but a
// write must still not block forever if the client stops reading -- a
// wedged or dead peer whose socket buffer has filled up would otherwise tie
// up this goroutine, and the api.Subscribe channel behind it, forever.
const streamWriteTimeout = 30 * time.Second

// emptyResult is the Result payload for a write method (ApplyConfig,
// AckAlert, UnackAlert, TestChannel, ValidateChannel): the call succeeded,
// there is nothing to return beyond ok=true.
var emptyResult = json.RawMessage("{}")

// Serve accepts connections on ln and handles each one (in its own
// goroutine) against api. token is the per-launch secret each client's
// hello must present (constant-time compared in handleConn); an empty
// token means no auth is required, which keeps the package's own
// round-trip tests simple -- production always sets one. Serve returns once
// Accept fails, which happens when ln is closed by the caller.
func Serve(api core.API, ln net.Listener, token string) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleConn(api, conn, token)
	}
}

// handleConn owns one client connection end to end: it validates the
// client's opening hello (protocol version, then token if one is
// configured), echoes its own, then services request frames against api
// until the peer disconnects or a frame can't be read (either case ends
// the connection quietly -- a peer going away mid-read is normal shutdown,
// not a protocol fault worth logging).
func handleConn(api core.API, conn net.Conn, token string) {
	defer conn.Close()

	r := bufio.NewReader(conn)

	if err := conn.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
		return
	}
	var clientHello hello
	if err := readFrame(r, &clientHello); err != nil {
		// The peer disconnected, or never completed the handshake before
		// helloTimeout expired; either way there is nothing to reply to.
		return
	}
	if clientHello.Hello != helloMagic || clientHello.Version != ProtocolVersion {
		_ = writeFrame(conn, response{
			OK: false,
			Error: fmt.Sprintf("control: unsupported hello %+v, want hello=%q version=%d",
				clientHello, helloMagic, ProtocolVersion),
		})
		return
	}
	if token != "" && subtle.ConstantTimeCompare([]byte(clientHello.Token), []byte(token)) != 1 {
		_ = writeFrame(conn, response{
			OK:    false,
			Error: "control: invalid token",
		})
		return
	}
	if err := writeFrame(conn, hello{Hello: helloMagic, Version: ProtocolVersion}); err != nil {
		return
	}

	for {
		if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			return
		}
		var req request
		if err := readFrame(r, &req); err != nil {
			// EOF, an idle connection past idleTimeout, or a partial/
			// unterminated final frame after the peer closed its write
			// side: end the connection, not an error.
			return
		}

		if req.Method == "Subscribe" {
			// Subscribe dedicates the rest of this connection's lifetime to
			// streaming (or, on error, to a single error response) -- it
			// never returns to this request/response loop, matching the
			// client's own design of opening a brand-new connection for
			// each subscription (client.go's Subscribe) rather than reusing
			// its mutex-serialized primary conn.
			streamSubscribe(api, conn, r, req.ID)
			return
		}

		result, callErr := dispatch(api, req.Method, req.Params)
		resp := response{ID: req.ID}
		if callErr != nil {
			resp.OK = false
			resp.Error = callErr.Error()
		} else {
			resp.OK = true
			resp.Result = result
		}

		if err := writeFrame(conn, resp); err != nil {
			return
		}
	}
}

// streamSubscribe switches conn into Subscribe streaming mode for one
// request. It calls api.Subscribe first -- deciding whether the client gets
// a stream at all -- and only once that succeeds does it write the ack
// response and start relaying events; on error it writes a single ok=false
// response instead, the same shape every other dispatch error takes, so a
// caller that gets no live daemon behind api (fileAPI, or an inprocAPI built
// without a bus) sees a normal error rather than a stream that silently
// never sends anything.
//
// The ctx passed to api.Subscribe is tied to conn's lifetime, not to
// anything with its own timeout: a background goroutine does nothing but
// read from conn (via r, which after this point belongs solely to that
// goroutine -- the caller stops reading once it hands off here) so that the
// moment the client disconnects, a read on conn returns EOF or another
// error and ctx is cancelled. That is what makes api.Subscribe's own
// unsubscribe fire instead of leaking a subscriber -- and this goroutine --
// on the daemon side forever. The normal idleTimeout read deadline does not
// apply here: a streaming connection is expected to sit idle between
// events, unlike a request/response connection.
func streamSubscribe(api core.API, conn net.Conn, r *bufio.Reader, reqID int) {
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := r.ReadByte(); err != nil {
				cancel()
				return
			}
		}
	}()

	ch, err := api.Subscribe(ctx)
	if err != nil {
		_ = writeFrame(conn, response{ID: reqID, OK: false, Error: err.Error()})
		return
	}

	if err := writeFrame(conn, response{ID: reqID, OK: true}); err != nil {
		return
	}

	for ev := range ch {
		result, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if err := conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
			return
		}
		if err := writeFrame(conn, response{ID: streamID, OK: true, Result: result}); err != nil {
			return
		}
	}
}

// dispatch decodes params for method, calls the matching core.API method on
// api, and marshals its result. Read methods return the method's DTO; write
// methods (ApplyConfig, AckAlert, UnackAlert, TestChannel, ValidateChannel)
// return emptyResult and surface only the error. Config/ApplyConfig carry the raw
// config.Config value (not a display-formatted projection) so
// Collect.*bool's omitempty semantics survive the round trip. An
// unrecognized method name returns an error. Subscribe never reaches here --
// handleConn intercepts it before calling dispatch, since it switches the
// connection into streaming mode (streamSubscribe above) instead of
// producing one normal response.
func dispatch(api core.API, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "Snapshot":
		v, err := api.Snapshot()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Monitoring":
		v, err := api.Monitoring()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Series":
		var p struct {
			Metric string `json:"metric"`
			From   int64  `json:"from"`
			To     int64  `json:"to"`
			Res    int    `json:"res"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		v, err := api.Series(p.Metric, p.From, p.To, core.Resolution(p.Res))
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Events":
		var p struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		v, err := api.Events(p.From, p.To)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "ActiveAlerts":
		v, err := api.ActiveAlerts()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "AlertHistory":
		var p struct {
			Since int64 `json:"since"`
			Limit int   `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		v, err := api.AlertHistory(p.Since, p.Limit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Config":
		v, err := api.Config()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Doctor":
		v, err := api.Doctor()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "HostInfo":
		v, err := api.HostInfo()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "EnrollmentPIN":
		pin, enrolled, err := api.EnrollmentPIN(context.Background())
		if err != nil {
			return nil, err
		}
		return json.Marshal(enrollmentPINResult{PIN: pin, Enrolled: enrolled})

	case "MonitorTargets":
		v, err := api.MonitorTargets(context.Background())
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "ApplyConfig":
		var p struct {
			Config config.Config `json:"config"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.ApplyConfig(&p.Config); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "AckAlert":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.AckAlert(p.Key); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "UnackAlert":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.UnackAlert(p.Key); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "TestChannel":
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.TestChannel(p.Name); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "ValidateChannel":
		var p struct {
			Channel config.ChannelConfig `json:"channel"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.ValidateChannel(p.Channel); err != nil {
			return nil, err
		}
		return emptyResult, nil

	default:
		return nil, fmt.Errorf("control: unknown method %q", method)
	}
}
