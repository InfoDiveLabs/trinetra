package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func decodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return nil
	}
	return json.Unmarshal(params, v)
}

// dispatchPluginHost serves one Plugin.* call for the plugin named by grant.
func dispatchPluginHost(host plugin.Host, grant Grant, method string, params json.RawMessage) (json.RawMessage, error) {
	if host == nil {
		return nil, plugin.ErrUnavailable
	}
	id := grant.PluginID
	switch method {
	case plugin.MethodHello:
		var p plugin.HelloRequest
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		v, err := host.Hello(id, p)
		return marshalResult(v, err)
	case plugin.MethodGetConfig:
		v, err := host.GetConfig(id)
		return marshalResult(v, err)
	case plugin.MethodReportStatus:
		var p plugin.StatusReport
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return emptyOrErr(host.ReportStatus(id, p))
	case plugin.MethodLog:
		var p plugin.LogEntry
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return emptyOrErr(host.Log(id, p))
	case plugin.MethodPublishSeries:
		var p plugin.PublishRequest
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		v, err := host.PublishSeries(id, p)
		return marshalResult(v, err)
	case plugin.MethodRaiseAlert:
		var p plugin.AlertRequest
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return emptyOrErr(host.RaiseAlert(id, p))
	case plugin.MethodResolveAlert:
		var p plugin.ResolveRequest
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return emptyOrErr(host.ResolveAlert(id, p))
	case plugin.MethodSnapshot:
		v, err := host.Snapshot(id)
		return marshalResult(v, err)
	case plugin.MethodSeries:
		var p plugin.SeriesRequest
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		v, err := host.Series(id, p)
		return marshalResult(v, err)
	case plugin.MethodActiveAlerts:
		v, err := host.ActiveAlerts(id)
		return marshalResult(v, err)
	case plugin.MethodContainers:
		v, err := host.Containers(id)
		return marshalResult(v, err)
	case plugin.MethodContainerLogs:
		var p plugin.ContainerLogsRequest
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		v, err := host.ContainerLogs(id, p)
		return marshalResult(v, err)
	case plugin.MethodFleetNodes:
		v, err := host.FleetNodes(id)
		return marshalResult(v, err)
	}
	return nil, fmt.Errorf("control: unknown method %q", method)
}

func marshalResult(v any, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func emptyOrErr(err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	return emptyResult, nil
}

// streamWatchConfig switches conn into config streaming for Plugin.WatchConfig.
func streamWatchConfig(host plugin.Host, id string, conn net.Conn, r *bufio.Reader, reqID int) {
	if host == nil {
		_ = writeFrame(conn, response{ID: reqID, Error: plugin.ErrUnavailable.Error()})
		return
	}
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

	ch, err := host.WatchConfig(ctx, id)
	if err != nil {
		_ = writeFrame(conn, response{ID: reqID, Error: err.Error()})
		return
	}
	if err := writeFrame(conn, response{ID: reqID, OK: true}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case cfg, ok := <-ch:
			if !ok {
				return
			}
			result, err := json.Marshal(cfg)
			if err != nil {
				return
			}
			if err := conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
				return
			}
			if err := plugin.WriteFrame(conn, response{ID: plugin.StreamID, OK: true, Result: result}); err != nil {
				return
			}
		}
	}
}
