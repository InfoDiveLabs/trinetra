// Package trinetra: fleet_rpc_child.go is the child's side of the master's
// on-demand RPC over the stream (see fleet_rpc.go for the master's side and
// the shared wire shapes). The only method is "container_logs".
package trinetra

import (
	"context"
	"encoding/json"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// rpcMaxLines bounds the "lines" a container_logs RPC will ever ask core.API.ContainerLogs
// for, regardless of what the master requested.
const rpcMaxLines = 2000

// rpcMaxOutputBytes bounds the RESULT this child will ever ship back over a
// single RPC: truncated from the start (the OLDEST lines
// are dropped, keeping the most recent output, which is what a log tail is
// almost always wanted for), with a marker line saying so.
const rpcMaxOutputBytes = 512 << 10

// rpcTruncatedMarker prefixes a container_logs result that had to be
// truncated to fit rpcMaxOutputBytes.
const rpcTruncatedMarker = "... (truncated to the last 512 KiB) ...\n"

// rpcPostTimeout bounds how long handleRPCFrame's goroutine will wait for PostRPCResult
// itself (a stream reconnect, not the RPC's own 10s master- side deadline).
const rpcPostTimeout = 30 * time.Second

// rpcMaxConcurrent bounds how many RPC executions this child runs at once each one shells
// out to `docker logs` (dispatchContainerLogs -> collectContainerLogs).
const rpcMaxConcurrent = 8

// rpcSem admits at most rpcMaxConcurrent concurrent RPC executions, used as a non-blocking
// semaphore (an empty struct sent into it "holds" a slot, received back out "releases" it).
var rpcSem = make(chan struct{}, rpcMaxConcurrent)

// handleRPCFrame is startChild's response to an "rpc" stream frame: it runs the requested
// method in its OWN goroutine -- never on the stream's read loop.
func handleRPCFrame(self core.API, sh *fleet.Shipper, logf func(format string, args ...any), f fleet.Frame) {
	if f.Type != "rpc" {
		return
	}
	var req rpcFrameData
	if json.Unmarshal(f.Data, &req) != nil || req.ID == "" {
		return
	}
	select {
	case rpcSem <- struct{}{}:
		go func() {
			defer func() { <-rpcSem }()
			runRPC(self, sh, logf, req)
		}()
	default:
		go postRPCBusy(sh, logf, req.ID)
	}
}

// postRPCBusy posts the fixed "node busy" rejection for an rpc frame that
// arrived while rpcMaxConcurrent executions were already running.
func postRPCBusy(sh *fleet.Shipper, logf func(string, ...any), id string) {
	body, err := json.Marshal(rpcResultData{OK: false, Error: "node busy"})
	if err != nil {
		logf("fleet: could not marshal the busy rpc result for %s: %v", id, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcPostTimeout)
	defer cancel()
	if err := sh.PostRPCResult(ctx, id, body); err != nil {
		logf("fleet: could not post the busy rpc result for %s: %v", id, err)
	}
}

// runRPC does the actual work for one "rpc" frame and posts its result.
func runRPC(self core.API, sh *fleet.Shipper, logf func(string, ...any), req rpcFrameData) {
	res := dispatchRPC(self, req)
	body, err := json.Marshal(res)
	if err != nil {
		logf("fleet: could not marshal rpc result for %s: %v", req.ID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcPostTimeout)
	defer cancel()
	if err := sh.PostRPCResult(ctx, req.ID, body); err != nil {
		logf("fleet: could not post rpc result for %s: %v", req.ID, err)
	}
}

// dispatchRPC runs req.Method against self, returning {"ok":false,"error":
// "unknown method"} for anything this child doesn't implement.
func dispatchRPC(self core.API, req rpcFrameData) rpcResultData {
	switch req.Method {
	case "container_logs":
		return dispatchContainerLogs(self, req.Args)
	default:
		return rpcResultData{OK: false, Error: "unknown method"}
	}
}

func dispatchContainerLogs(self core.API, rawArgs json.RawMessage) rpcResultData {
	if self == nil {
		return rpcResultData{OK: false, Error: "not available"}
	}
	var args rpcContainerLogsArgs
	if json.Unmarshal(rawArgs, &args) != nil {
		return rpcResultData{OK: false, Error: "bad args"}
	}
	lines := args.Lines
	if lines <= 0 || lines > rpcMaxLines {
		lines = rpcMaxLines
	}
	out, err := self.ContainerLogs(args.Name, lines)
	if err != nil {
		return rpcResultData{OK: false, Error: err.Error()}
	}
	return rpcResultData{OK: true, Output: truncateRPCOutput(out)}
}

// truncateRPCOutput bounds s to rpcMaxOutputBytes, dropping from the START
// (oldest content) and prefixing a marker line.
func truncateRPCOutput(s string) string {
	if len(s) <= rpcMaxOutputBytes {
		return s
	}
	keep := rpcMaxOutputBytes - len(rpcTruncatedMarker)
	if keep < 0 {
		keep = 0
	}
	return rpcTruncatedMarker + s[len(s)-keep:]
}
