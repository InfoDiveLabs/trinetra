// Package trinetra: fleet_rpc_child.go is the child's side of the master's
// on-demand RPC over the stream (task 9, see fleet_rpc.go for the master's
// side and the wire shapes both sides share): today the only method is
// "container_logs".
package trinetra

import (
	"context"
	"encoding/json"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// rpcMaxLines bounds the "lines" a container_logs RPC will ever ask
// core.API.ContainerLogs for (task-9 ruling), regardless of what the master
// requested: a compromised or misbehaving master cannot make a child read
// an unbounded amount of log data.
const rpcMaxLines = 2000

// rpcMaxOutputBytes bounds the RESULT this child will ever ship back over a
// single RPC (task-9 ruling): truncated from the start (the OLDEST lines
// are dropped, keeping the most recent output, which is what a log tail is
// almost always wanted for), with a marker line saying so.
const rpcMaxOutputBytes = 512 << 10

// rpcTruncatedMarker prefixes a container_logs result that had to be
// truncated to fit rpcMaxOutputBytes.
const rpcTruncatedMarker = "... (truncated to the last 512 KiB) ...\n"

// rpcPostTimeout bounds how long handleRPCFrame's goroutine will wait for
// PostRPCResult itself (a stream reconnect, not the RPC's own 10s master-
// side deadline): generous enough that a slow-but-working link still gets
// the result through, short enough that a goroutine can never accumulate
// forever if the link is actually down for good.
const rpcPostTimeout = 30 * time.Second

// handleRPCFrame is startChild's response to an "rpc" stream frame: it runs
// the requested method in its OWN goroutine -- never on the stream's read
// loop, which (like onStreamFrame) must never block -- and posts the result
// back via sh.PostRPCResult. self may be nil defensively (mirrors
// applyAckFrame); sh is used only to post the result, never to read from
// the stream.
func handleRPCFrame(self core.API, sh *fleet.Shipper, logf func(format string, args ...any), f fleet.Frame) {
	if f.Type != "rpc" {
		return
	}
	var req rpcFrameData
	if json.Unmarshal(f.Data, &req) != nil || req.ID == "" {
		return
	}
	go runRPC(self, sh, logf, req)
}

// runRPC does the actual work for one "rpc" frame and posts its result.
// Any error posting the result (e.g. the link is down) is logged, not
// retried: the master's own 10s wait will have already given up by the
// time a retry could plausibly land, and the master's own sweep reclaims
// the pending slot regardless.
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
// "unknown method"} for anything this child doesn't implement (task-9
// ruling).
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
// (oldest content) and prefixing a marker line, per the task-9 ruling.
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
