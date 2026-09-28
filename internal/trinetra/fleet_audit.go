// Package trinetra: fleet_audit.go is the fleet master's audit log: an
// append-only, 0600 record of every fleet mutation (rename, tag, revoke,
// remove, token create/delete, incident ack) with who did it.
package trinetra

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// auditLog appends AuditEntry lines to a single JSONL file.
type auditLog struct {
	mu   sync.Mutex
	path string
}

func newAuditLog(path string) *auditLog { return &auditLog{path: path} }

// Append records one audit entry. A nil *auditLog is a no-op (mirrors
// AlertLog's nil-degrades-gracefully convention), so tests and callers that
// have no audit log wired need not nil-check before calling.
func (a *auditLog) Append(actor, action, target, detail string, now int64) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if actor == "" {
		actor = "unknown"
	}
	b, err := json.Marshal(core.AuditEntry{TS: now, Actor: actor, Action: action, Target: target, Detail: detail})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	return appendSynced(a.path, append(b, '\n'))
}

// auditRecentChunkSize is how many bytes Recent reads back from wherever it
// currently is in the file on each backward step, doubling on every
// subsequent step (auditRecentChunkGrowth) -- a small first read satisfies
// the overwhelmingly common "give me the last 50" call in one or two seeks
// without ever reading the whole file, while a caller asking for a much
// larger limit against a small file still terminates in a handful of
// doublings rather than one read per line.
//
// A package var, not a const (mirrors sse.go's sseFallbackInterval for the
// identical reason): a test can shrink it to force Recent's multi-iteration
// chunk-growth path against a small fixture file, rather than needing a
// multi-megabyte file to observe more than one backward step.
var auditRecentChunkSize int64 = 64 * 1024

// auditRecentChunkGrowth is Recent's backward-read doubling factor (see
// auditRecentChunkSize's doc).
const auditRecentChunkGrowth = 4

// Recent returns the most recent audit entries, newest first, up to limit
// (<= 0 means unlimited, which still has to read the whole file -- there is
// no way to know "how far back is enough" without a limit). A missing file
// is not an error (nothing audited yet).
//
// Bounded read (C5 review carry-over): rather than scanning the file
// forward from byte 0 (the old implementation), this seeks backward from
// EOF in growing chunks (auditRecentChunkSize, doubling by
// auditRecentChunkGrowth each step) until it has accumulated at least limit
// COMPLETE lines or reached the start of the file -- so a 50-entry page
// view against a multi-hundred-thousand-line audit log reads a small
// bounded tail of it, not the entire file. A chunk boundary can split the
// line at its very start (offset 0 of the chunk), which this discards as a
// PARTIAL line and re-reads on the next, larger step that extends further
// back and re-covers that same byte range -- only the increasingly rare
// case of the very first (oldest) chunk read can permanently drop a leading
// partial line, and that only happens at the true start of the file, where
// there IS no earlier byte to complete it from (i.e. it isn't a valid JSONL
// line boundary at all, which can only happen if the file itself is
// corrupt/truncated -- the same class of "skip what doesn't parse" leniency
// bufio.Scanner-based parsing already had).
func (a *auditLog) Recent(limit int) ([]core.AuditEntry, error) {
	if a == nil {
		return nil, nil
	}
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	// parseLines turns buf's complete '\n'-terminated lines into entries,
	// newest-last (append order == file order for this buf), skipping any
	// blank or unparseable line exactly like the old scanner-based version
	// did.
	parseLines := func(buf []byte) []core.AuditEntry {
		var out []core.AuditEntry
		for _, line := range bytes.Split(buf, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var e core.AuditEntry
			if json.Unmarshal(line, &e) != nil {
				continue
			}
			out = append(out, e)
		}
		return out
	}

	if limit <= 0 {
		// Unlimited: there's no target line count to stop early at, so this
		// is the one case that still reads the whole file -- exactly the old
		// implementation's behavior, just expressed via parseLines.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		whole, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		out := parseLines(whole)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out, nil
	}

	var out []core.AuditEntry
	chunk := auditRecentChunkSize
	for {
		start := size - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, err
		}
		// Drop a partial line at the very front of this read (unless we've
		// already reached byte 0, where there's nothing earlier to complete
		// it from): the next, larger step re-reads this same tail from an
		// earlier start and parses it whole.
		if start > 0 {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil
			}
		}
		out = parseLines(buf)
		if len(out) >= limit || start == 0 {
			break
		}
		chunk *= auditRecentChunkGrowth
	}

	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
