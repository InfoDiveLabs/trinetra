# Storage design (future-safe time-series)

Status: **implemented (v0.2 / Epic #44)** (the "Tiered sampling + efficient storage" epic).
This supersedes the current JSONL sample store for time-series data.

## Goals

1. **Efficient** for high-frequency numeric data (5s resource sampling) — no reflection, small on disk, fast range scans.
2. **Future-safe** — the storage *engine* can change (binary → SQLite → remote TSDB) without changing any caller, and the on-disk *format* can evolve without orphaning old data.
3. **Stdlib-only** for the default backend; still human-inspectable via an export command.
4. **Right tool per data kind** — time-series gets a real store; small mutable state stays JSON.

## What is / isn't time-series

- **Time-series (this design):** `samples` (per-metric numeric points) and `downtime` events.
- **Small mutable state (stays JSON, atomic rewrite):** `config.json`, `status.json`, `baseline.json`, `alerts.json`. These are tiny, read/written whole, and benefit from being human-readable. No reason to binary-encode them.

## Core abstraction — the swappable seam (future-safety #1)

All callers depend on an interface, never on a file format:

```go
type SampleStore interface {
    Append(ts int64, m MetricSet) error          // write one timestamped sample
    Query(metric string, from, to int64, res Resolution) ([]Point, error)
    AppendEvent(DownEvent) error
    Events(from, to int64) ([]DownEvent, error)
    Prune(now int64) error                        // apply retention policy
    Close() error
}
```

- `Point` = `{TS int64; Min, Avg, Max float64}` (raw points set Min=Avg=Max=value).
- Backends are selected by config (`storage.backend`), so a future `sqlite` or `remote` backend is a drop-in. Default: `tsfile` (below).
- The daemon, handlers, and digests only ever see `SampleStore` — this is the single change that makes everything else replaceable.

## Default backend: `tsfile` (columnar, per-series, binary)

**Per-series files** — one append-only file per metric series and resolution:

```
/var/lib/serverwatch/ts/
  raw/cpu.tsd   raw/mem.tsd   raw/swap.tsd   raw/load1.tsd ...
  1m/cpu.tsd    1m/mem.tsd    ...
  1h/cpu.tsd    ...            (future; not in v1)
  events.tsd
```

Adding a **new metric = a new file** — no format change, no schema migration. This is the main extensibility win over a fixed-width single-record layout (where adding a column changes every record).

**File format (self-describing + versioned — future-safety #2):**

```
Header (fixed):
  magic      [4]byte   "SWTS"
  version    uint16    format version (readers switch on this)
  recordLen  uint16    bytes per record (lets old readers skip unknown-size records)
  resolution uint32    seconds per point (0 = raw/irregular)
  reserved   [4]byte
Records (append-only, sorted by ts):
  ts   int64          unix seconds
  min  float64
  avg  float64
  max  float64        (raw: min=avg=max)
```

- **Range query** = binary-search the sorted records by `ts` (fixed `recordLen` makes offsets O(1)), then scan. No full-file parse.
- **Append** = seek to end, write one record. O(1), no rewrite.
- **Corruption tolerance** = a torn final record (size < recordLen) is ignored on read; the header `recordLen` lets a newer format be read by an older binary by skipping.
- **Versioning** = bump `version` for layout changes; keep readers for old versions; `serverwatch migrate` rewrites in place when worthwhile.

## Resolutions, downsampling & retention (future-safety #3)

- **raw** — 5s resource points; short retention (`storage.raw_retention`, default 48h).
- **1m** — min/avg/max rollup; 30-day retention (`storage.rollup_retention`, default 30d).
- **1h** — reserved for future long-term (months/years); same mechanism, just another resolution dir.
- A **downsampler** rolls raw → 1m on the slow tick; `Query` picks the **coarsest resolution** that satisfies the requested range (recent = raw, long = 1m), so longer retention never means bigger scans.
- Retention is **per-resolution policy**, so extending history later is a config change, not a redesign.

## Metric identity

Metrics are addressed by a **stable string id** (`cpu`, `mem`, `swap`, `load1`, `disk:/`, `temp`, …) that maps 1:1 to a series file. IDs are the same keys already used by the anomaly engine's `Check.Key`, so discovery-driven metrics (per-disk, per-container) get their own series automatically.

## Tooling & migration

- `serverwatch dump [--metric X] [--since ...] [--format csv|json]` — export any series for humans/graphing (keeps the `cat`-friendliness we'd lose going binary).
- `serverwatch migrate` — one-shot import of the legacy `samples/*.jsonl` + `downtime.jsonl` into the new store; idempotent; old files archived, not deleted.
- Format `version` in every header means a future layout change ships with a reader for the old version + an opt-in `migrate`.

## Future backends (enabled by the interface, not built now)

- **`sqlite`** — if rich ad-hoc queries are wanted (pulls a pure-Go SQLite dep; relaxes the stdlib rule — deliberate opt-in).
- **`remote`** — Prometheus remote-write / VictoriaMetrics for dashboards + multi-host, behind the same `SampleStore`.

Both are out of scope for the first cut; the point of the interface is that neither requires touching the daemon, handlers, or digests.

## Config keys (all CLI-managed)

```
storage.backend           tsfile            # tsfile | (future: sqlite, remote)
storage.raw_retention     48h
storage.rollup_retention  720h              # 30d
```
