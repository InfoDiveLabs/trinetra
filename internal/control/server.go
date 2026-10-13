package control

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// helloMagic is the Hello value both ends must send, alongside a matching ProtocolVersion.
const helloMagic = "serverwatch-control"

// helloTimeout bounds the opening hello read and idleTimeout every request read after it,
// so a peer that connects and never speaks cannot tie up a goroutine.
const (
	helloTimeout = 10 * time.Second
	idleTimeout  = 5 * time.Minute
)

// streamWriteTimeout bounds each stream-frame write.
const streamWriteTimeout = 30 * time.Second

// emptyResult is the Result for write methods: success with nothing to return.
var emptyResult = json.RawMessage("{}")

// Serve accepts connections on ln and handles each in its own goroutine against api. token
// is the per-launch secret each hello must present (constant-time compared in handleConn).
func Serve(api core.API, ln net.Listener, token string) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleConn(api, conn, token)
	}
}

// handleConn validates the client's hello (version, then token), echoes its own, and
// services requests until the peer disconnects.
func handleConn(api core.API, conn net.Conn, token string) {
	defer conn.Close()

	r := bufio.NewReader(conn)

	if err := conn.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
		return
	}
	var clientHello hello
	if err := readFrame(r, &clientHello); err != nil {
		// Peer disconnected or missed helloTimeout; nothing to reply to.
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
			// EOF, idle past idleTimeout, or a partial final frame: end quietly.
			return
		}

		if req.Method == "Subscribe" {
			// Subscribe takes over the rest of the connection for streaming (or a single error
			// response) and never returns to this loop.
			if req.Node != "" && req.Node != core.SelfNodeID {
				_ = writeFrame(conn, response{ID: req.ID, Node: req.Node, OK: false, Error: "control: live event streams are not available for remote fleet nodes yet"})
				return
			}
			streamSubscribe(api, conn, r, req.ID)
			return
		}

		var result json.RawMessage
		var callErr error
		switch {
		case strings.HasPrefix(req.Method, "Fleet."):
			result, callErr = dispatchFleet(api, req.Method, req.Params)
		case strings.HasPrefix(req.Method, "StatusPage."):
			result, callErr = dispatchStatusPage(api, req.Method, req.Params)
		default:
			target, err := resolveNode(api, req.Node)
			if err != nil {
				callErr = err
			} else {
				result, callErr = dispatch(target, req.Method, req.Params)
			}
		}
		resp := response{ID: req.ID, Node: req.Node}
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

// streamSubscribe switches conn into Subscribe streaming mode.
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

// dispatch decodes params for method, calls the matching core.API method and marshals the
// result.
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

	case "Version":
		v, err := api.Version()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "ContainerLogs":
		var p struct {
			Name  string `json:"name"`
			Lines int    `json:"lines"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		v, err := api.ContainerLogs(p.Name, p.Lines)
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

	case "UpdateStatus":
		v, err := api.UpdateStatus()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "UpdateCheck":
		v, err := api.UpdateCheck(context.Background())
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "UpdateApply":
		var p struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.UpdateApply(context.Background(), p.Version); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "UpdateRollback":
		if err := api.UpdateRollback(); err != nil {
			return nil, err
		}
		return emptyResult, nil

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

// resolveNode maps a request's node field to the API serving it: "" or core.SelfNodeID is
// api itself, anything else needs api to be a core.FleetProvider.
func resolveNode(api core.API, node string) (core.API, error) {
	if node == "" || node == core.SelfNodeID {
		return api, nil
	}
	fp, ok := api.(core.FleetProvider)
	if !ok {
		return nil, errors.New("control: this daemon has no fleet support")
	}
	return fp.Node(node)
}

// dispatchFleet serves the Fleet.* methods against api's core.FleetAPI; write
// methods return emptyResult like dispatch's.
func dispatchFleet(api core.API, method string, params json.RawMessage) (json.RawMessage, error) {
	fp, ok := api.(core.FleetProvider)
	if !ok {
		return nil, errors.New("control: this daemon has no fleet support")
	}
	f := fp.Fleet()

	var p struct {
		ID              string               `json:"id"`
		Name            string               `json:"name"`
		Tags            []string             `json:"tags"`
		Deps            []string             `json:"deps"`
		Filter          core.NodeFilter      `json:"filter"`
		Spec            core.TokenSpec       `json:"spec"`
		IncFilter       core.IncidentFilter  `json:"inc_filter"`
		Actor           string               `json:"actor"`
		Key             string               `json:"key"`
		Limit           int                  `json:"limit"`
		Silence         core.Silence         `json:"silence"`
		Maintenance     core.Maintenance     `json:"maintenance"`
		Alerting        core.AlertingConfig  `json:"alerting"`
		TestAlert       core.TestAlert       `json:"test_alert"`
		ManagedFragment core.ManagedFragment `json:"managed_fragment"`
		Metric          string               `json:"metric"`
		Agg             core.Agg             `json:"agg"`
		From            int64                `json:"from"`
		To              int64                `json:"to"`
		Res             core.Resolution      `json:"res"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
	}

	switch method {
	case "Fleet.Status":
		v, err := f.Status()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.Nodes":
		v, err := f.Nodes(p.Filter)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.RenameNode":
		if err := f.RenameNode(p.ID, p.Name, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.SetNodeTags":
		if err := f.SetNodeTags(p.ID, p.Tags, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.SetNodeDeps":
		if err := f.SetNodeDeps(p.ID, p.Deps, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.RevokeNode":
		if err := f.RevokeNode(p.ID, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.RemoveNode":
		if err := f.RemoveNode(p.ID, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.Tokens":
		v, err := f.Tokens()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.CreateToken":
		v, err := f.CreateToken(p.Spec)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.DeleteToken":
		if err := f.DeleteToken(p.ID, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.Incidents":
		v, err := f.Incidents(p.IncFilter)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.Incident":
		v, err := f.Incident(p.ID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.AckIncident":
		if err := f.AckIncident(p.ID, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.Explain":
		v, err := f.Explain(p.Key)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.Audit":
		v, err := f.Audit(p.Limit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.Silences":
		v, err := f.Silences()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.CreateSilence":
		v, err := f.CreateSilence(p.Silence)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.ExpireSilence":
		if err := f.ExpireSilence(p.ID, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.Maintenances":
		v, err := f.Maintenances()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.SaveMaintenance":
		v, err := f.SaveMaintenance(p.Maintenance)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.DeleteMaintenance":
		if err := f.DeleteMaintenance(p.ID, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.Alerting":
		v, err := f.Alerting()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.SetAlerting":
		if err := f.SetAlerting(p.Alerting, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.RouteTest":
		v, err := f.RouteTest(p.TestAlert)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.RuleStates":
		v, err := f.RuleStates()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.Managed":
		v, err := f.Managed()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.SaveManaged":
		v, err := f.SaveManaged(p.ManagedFragment, p.Actor)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.DeleteManaged":
		if err := f.DeleteManaged(p.ID, p.Actor); err != nil {
			return nil, err
		}
		return emptyResult, nil

	case "Fleet.ManagedStatus":
		v, err := f.ManagedStatus()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)

	case "Fleet.FleetSeries":
		v, err := f.FleetSeries(p.Metric, p.Filter, p.Agg, p.From, p.To, p.Res)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	}
	return nil, fmt.Errorf("control: unknown method %q", method)
}

var errNoStatusPage = errors.New("status page not available on this daemon")

func dispatchStatusPage(api core.API, method string, params json.RawMessage) (json.RawMessage, error) {
	prov, ok := api.(core.StatusPageProvider)
	if !ok || prov.StatusPage() == nil {
		return nil, errNoStatusPage
	}
	sp := prov.StatusPage()
	var p struct {
		ID              string             `json:"id"`
		UpdateID        string             `json:"update_id"`
		Actor           string             `json:"actor"`
		Title           string             `json:"title"`
		Services        []string           `json:"services"`
		IncludeResolved bool               `json:"include_resolved"`
		Service         core.StatusService `json:"service"`
		Incident        core.NewIncident   `json:"incident"`
		Update          core.NewUpdate     `json:"update"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
	}
	var v any
	var err error
	switch method {
	case "StatusPage.Services":
		v, err = sp.Services()
	case "StatusPage.SetService":
		v, err = sp.SetService(p.Service, p.Actor)
	case "StatusPage.DeleteService":
		err = sp.DeleteService(p.ID, p.Actor)
	case "StatusPage.Evaluation":
		v, err = sp.Evaluation()
	case "StatusPage.Incidents":
		v, err = sp.Incidents(p.IncludeResolved)
	case "StatusPage.Incident":
		v, err = sp.Incident(p.ID)
	case "StatusPage.CreateIncident":
		v, err = sp.CreateIncident(p.Incident, p.Actor)
	case "StatusPage.PostUpdate":
		v, err = sp.PostUpdate(p.ID, p.Update, p.Actor)
	case "StatusPage.EditUpdate":
		v, err = sp.EditUpdate(p.ID, p.UpdateID, p.Update, p.Actor)
	case "StatusPage.EditIncident":
		v, err = sp.EditIncident(p.ID, p.Title, p.Services, p.Actor)
	case "StatusPage.DeleteIncident":
		err = sp.DeleteIncident(p.ID, p.Actor)
	case "StatusPage.Public":
		v, err = sp.Public()
	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
	if err != nil {
		return nil, err
	}
	if v == nil {
		return emptyResult, nil
	}
	b, err := json.Marshal(v)
	return b, err
}
