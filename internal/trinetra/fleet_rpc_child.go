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

// rpcMaxConcurrent bounds how many RPC executions this child runs at once
// (round-1 review fix, IMPORTANT 2): each one shells out to `docker logs`
// (dispatchContainerLogs -> collectContainerLogs), so a master pushing rpc
// frames faster than they complete -- buggy or hostile -- must not be able
// to spawn an unbounded pile of concurrent subprocesses/goroutines. Beyond
// this many already running, a new rpc frame is refused immediately with
// {"ok":false,"error":"node busy"} rather than queued or run anyway.
const rpcMaxConcurrent = 8

// rpcSem admits at most rpcMaxConcurrent concurrent RPC executions, used as
// a non-blocking semaphore (an empty struct sent into it "holds" a slot,
// received back out "releases" it). A package-level var, like rpcCallTimeout
// (fleet_rpc.go), so a test can swap in a fresh one instead of contending
// with whatever a previous test left in flight; exactly one child role runs
// per daemon process, so a single package-level semaphore is the correct
// scope in production.
var rpcSem = make(chan struct{}, rpcMaxConcurrent)

// handleRPCFrame is startChild's response to an "rpc" stream frame: it runs
// the requested method in its OWN goroutine -- never on the stream's read
// loop, which (like onStreamFrame) must never block -- and posts the result
// back via sh.PostRPCResult. self may be nil defensively (mirrors
// applyAckFrame); sh is used only to post the result, never to read from
// the stream.
//
// If rpcMaxConcurrent executions are already running, this refuses the new
// one immediately (still off the read loop: posting the "busy" result is
// itself a network call, via PostRPCResult) rather than queuing it or
// blocking the read loop waiting for a slot to free up.
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
