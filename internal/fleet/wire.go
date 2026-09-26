package fleet

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Record kinds. Durable kinds travel through the outbox with a seq; the live
// snapshot is not a Record (see LiveUpdate).
const (
	KindSamples   = "samples"
	KindDownEvent = "downevent"
	KindAlert     = "alert"
)

// HTTP endpoints served by the master.
const (
	PathJoin     = "/fleet/v1/join"
	PathRenew    = "/fleet/v1/renew"
	PathIngest   = "/fleet/v1/ingest"
	PathBackfill = "/fleet/v1/backfill"
	PathLive     = "/fleet/v1/live"
	PathStream   = "/fleet/v1/stream" // GET, master-to-child push
	PathRPC      = "/fleet/v1/rpc/"   // POST /fleet/v1/rpc/{id}
)

// HeaderSentAt carries the child's send time (unix seconds) for skew tracking.
const HeaderSentAt = "X-SW-Sent-At"

// Batch limits.
const (
	MaxBatchRecords   = 5000
	MaxBatchBytes     = 1 << 20
	maxDecodedBatch   = 16 << 20
	maxDecodedRecords = 2 * MaxBatchRecords
)

// Frame is one push on the master-to-child stream (PathStream): the response
// body is application/x-ndjson, one JSON-encoded Frame per line, flushed as
// soon as it is written.
type Frame struct {
	Type string          `json:"type"` // lease|receipt|ack|unack|silences|managed_config|rpc|revoked|ping
	Data json.RawMessage `json:"data,omitempty"`
}

// Record is one durable telemetry item. Seq is 0 for unsequenced backfill.
type Record struct {
	Seq  uint64          `json:"seq,omitempty"`
	Kind string          `json:"kind"`
	TS   int64           `json:"ts"`
	Data json.RawMessage `json:"data"`
}

// SamplesData is the payload of a KindSamples record: either raw values
// (Res "") or 1m rollups (Res "1m", only sent by gap repair).
type SamplesData struct {
	TS      int64                  `json:"ts"`
	Res     string                 `json:"res,omitempty"`
	Metrics map[string]float64     `json:"m,omitempty"`
	Rollups map[string]RollupPoint `json:"r,omitempty"`
}

// RollupPoint is one 1m bucket.
type RollupPoint struct {
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// DownEventData mirrors trinetra.DownEvent's JSON shape.
type DownEventData struct {
	Type        string `json:"type"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DurationSec int64  `json:"duration_sec"`
}

// OutboxStats describes a child's spool, reported on every live update.
type OutboxStats struct {
	Bytes           int64  `json:"bytes"`
	Unacked         uint64 `json:"unacked"`
	NextSeq         uint64 `json:"next_seq"`
	AckedSeq        uint64 `json:"acked_seq"`
	OldestUnackedTS int64  `json:"oldest_unacked_ts,omitempty"`
	Gaps            int    `json:"gaps,omitempty"`
}

// LiveUpdate is the latest-wins "now" view a child posts every fast tick.
type LiveUpdate struct {
	SentAt     int64           `json:"sent_at"`
	Version    string          `json:"version"`
	Snapshot   json.RawMessage `json:"snapshot"`
	AlertState json.RawMessage `json:"alert_state,omitempty"`
	HostInfo   json.RawMessage `json:"hostinfo,omitempty"`
	Outbox     OutboxStats     `json:"outbox"`
}

// IngestResponse acknowledges everything up to AckedSeq as durable.
type IngestResponse struct {
	AckedSeq   uint64 `json:"acked_seq"`
	ServerTime int64  `json:"server_time"`
}

// JoinRequest is posted (without a client cert) to PathJoin.
type JoinRequest struct {
	Token      string          `json:"token"`
	CSR        string          `json:"csr"`
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	HostInfo   json.RawMessage `json:"hostinfo,omitempty"`
	PrevNodeID string          `json:"prev_node_id,omitempty"`
	PrevSig    string          `json:"prev_sig,omitempty"`
}

// JoinResponse carries the new identity. Name is the name actually stored
// in the registry, which may differ from the requested name if it collided
// (case-insensitively) with an existing node -- Registry.Add suffixes it
// "-2", "-3", ... to keep names unique (review round 2, item b), and the
// child prints this one, not the one it asked for.
type JoinResponse struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Cert   string `json:"cert"`
	CA     string `json:"ca"`
}

// RenewRequest/RenewResponse rotate a child's key and cert over mTLS.
type RenewRequest struct {
	CSR string `json:"csr"`
}
type RenewResponse struct {
	Cert string `json:"cert"`
}

// EncodeBatch gzips recs as newline-delimited JSON.
func EncodeBatch(recs []Record) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for i := range recs {
		if err := enc.Encode(&recs[i]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeBatch reverses EncodeBatch with hard limits against oversized or
// hostile input.
func DecodeBatch(r io.Reader) ([]Record, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("fleet: batch not gzip: %w", err)
	}
	defer zr.Close()
	lr := &io.LimitedReader{R: zr, N: maxDecodedBatch + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 64<<10), maxDecodedBatch)
	var out []Record
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		if len(out) >= maxDecodedRecords {
			return nil, errors.New("fleet: batch has too many records")
		}
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("fleet: bad record: %w", err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("fleet: read batch: %w", err)
	}
	if lr.N <= 0 {
		return nil, errors.New("fleet: batch too large")
	}
	return out, nil
}
