# InfluxDB — Study Notes

## Core Concept

InfluxDB is a **time-series database** — every record has a timestamp, and the database
is optimized for queries like "give me all CPU readings for node X in the last hour".

Unlike SQL where you define schemas upfront, InfluxDB creates structure automatically
from the data you write.

---

## Data Model

```
Measurement  ←→  SQL table name
Tags         ←→  indexed string columns (for filtering/grouping)
Fields       ←→  numeric value columns (the actual measurements)
Timestamp    ←→  required on every row (nanosecond precision)
```

Example point in this project:
```
measurement: vm_metrics
tags:    vm_id=rancher-desktop  hostname=lima-rancher-desktop
fields:  cpu_pct=9.8            mem_pct=76.2
time:    1775453494311600000    (nanoseconds)
```

### Tags vs Fields — Critical Distinction

| | Tags | Fields |
|---|---|---|
| Type | String only | Numeric (float, int, bool) |
| Indexed? | Yes | No |
| Use for | Filtering, grouping | Actual measurements |
| Example | `vm_id`, `hostname` | `cpu_pct`, `mem_pct` |

**Never put numeric measurements in tags** — they're stored as strings and not indexed
for range queries. `cpu_pct=9.8` as a tag means you can only do exact match, not
"cpu > 80%".

**Never put high-cardinality data in tags** — e.g. timestamps, UUIDs. Each unique tag
combination creates a "series" in InfluxDB's index. Too many series = performance problems.

---

## Write API — Blocking vs Non-Blocking

The InfluxDB Go client offers two write modes:

### WriteAPI (non-blocking) — used in this project

```go
writeAPI := client.WriteAPI(org, bucket)
writeAPI.WritePoint(point)   // returns immediately, buffered internally
```

- `WritePoint` returns immediately — the actual HTTP write happens in the background
- The client batches multiple points and sends them together (more efficient)
- Errors come back on `writeAPI.Errors()` channel (must drain it or errors are silent)
- **Must call `Flush()` before shutdown** — otherwise buffered points in memory are lost

```go
// correct shutdown sequence
writeAPI.Flush()    // send all buffered points to InfluxDB now
client.Close()      // close the HTTP connection
```

### WriteAPIBlocking

```go
writeAPI := client.WriteAPIBlocking(org, bucket)
err := writeAPI.WritePoint(ctx, point)   // blocks until InfluxDB confirms
```

- Simpler error handling — error returned directly from `WritePoint`
- Lower throughput — one HTTP round-trip per write
- No need for `Flush()` — every write is confirmed before returning

### When to use which

| Scenario | Choice |
|---|---|
| High-frequency writes (metrics pipeline) | Non-blocking WriteAPI |
| Low-frequency, must confirm each write | WriteAPIBlocking |
| This project (consumer writing metrics) | Non-blocking WriteAPI |

---

## Flush() — When and Why

`Flush()` forces the non-blocking WriteAPI to immediately send all buffered points.

**When to call it:**
1. **Before shutdown** — `defer writeAPI.Flush()` or in `Close()` — prevents data loss
2. **After a logical batch** — e.g. after flushing a 1-hour window, ensure all points
   are persisted before committing the Kafka offset

```go
func (w *Writer) Close() {
    w.writeApi.Flush()   // 1. send buffered points
    w.client.Close()     // 2. close connection
}
```

If you only call `client.Close()` without `Flush()`, any points sitting in the buffer
that haven't been sent yet are silently dropped.

---

## Flux — Query Language

InfluxDB 2.x uses **Flux** (not SQL, not InfluxQL from v1).

```flux
from(bucket: "metrics")
  |> range(start: -1h)                          // last 1 hour
  |> filter(fn: (r) => r._measurement == "vm_metrics")
  |> filter(fn: (r) => r.vm_id == "rancher-desktop")
  |> filter(fn: (r) => r._field == "cpu_pct")
```

The `|>` is a pipe operator — same idea as Unix pipes. Each step transforms the data.

Key concepts:
- `_measurement` — the measurement name (like table)
- `_field` — the field name (like column)
- `_value` — the field value
- `_time` — the timestamp
- Tags appear as regular columns

---

## p95 — Why Percentiles Matter More Than Averages

**p95 (95th percentile):** sort all values, take the value 95% of the way through.
Means "95% of readings were below this number."

```
Example CPU readings over 1 hour (simplified):
[5, 7, 8, 8, 9, 10, 11, 12, 45, 98]  ← sorted

avg = 21.3%   → "CPU is fine"
p95 = 45%     → "CPU spikes to 45% for 5% of the time" ← what causes user issues
max = 98%     → "hit 98% once" (blip or real problem?)
```

Averages hide spikes. p95 captures the "tail" experience — what most users see when
things get slow. It is the standard SRE metric for latency and resource utilization.

**p95 cannot be recomputed after raw data is deleted.** You must have all raw values
at computation time. This is why the consumer pre-computes it during the 1-hour window
before raw points expire.

---

## InfluxDB in This Project

### Two measurements — raw and hourly

```
vm_metrics          (raw)     — written every 15s per node
vm_metrics_hourly   (summary) — written once per hour per vm_id
```

| | vm_metrics | vm_metrics_hourly |
|---|---|---|
| Written by | consumer on each Kafka message | consumer at hour boundary |
| Retention | 7 days (then deleted) | forever (long-term trends) |
| Use for | "what is CPU right now?" | "what was avg/p95 CPU last month?" |
| Fields | cpu_pct, mem_pct | cpu_avg, cpu_min, cpu_max, cpu_p95, mem_* |

### Retention strategy — downsampling

```
Day 1–7:   both raw and hourly exist
Day 8+:    only hourly survives — raw is auto-deleted by retention policy

Query "last 6 hours"  → use vm_metrics        (granular, real-time)
Query "last 6 months" → use vm_metrics_hourly  (raw is gone, summary lives on)
```

This is the standard **downsampling** pattern used by Datadog, Prometheus (recording
rules), and all production metrics pipelines. Raw data is expensive; summaries are cheap.

### Schema

```
Measurement: vm_metrics
Tags:    vm_id, hostname
Fields:  cpu_pct, mem_pct
Time:    nanosecond Unix timestamp

Measurement: vm_metrics_hourly
Tags:    vm_id
Fields:  cpu_avg, cpu_min, cpu_max, cpu_p95
         mem_avg, mem_min, mem_max, mem_p95
Time:    timestamp of the hour boundary
```

### Write flow

```
agent scrapes k8s every 15s
  └── produces JSON to Kafka
        └── consumer reads Kafka message
              ├── Write(metric)          → vm_metrics (raw, immediate)
              └── append to cpuWindows/memWindows (in-memory accumulator)

consumer every minute checks clock
  └── if hour changed:
        └── stats(cpuWindows[vmID])     → avg/min/max/p95
              └── WriteHourlySummary()  → vm_metrics_hourly
```

### Verifying writes

Open InfluxDB UI at `http://localhost:8086` (admin / adminpassword):
- Data Explorer → bucket: `metrics` → measurement: `vm_metrics`
- Or use Flux in the query editor:
```flux
from(bucket: "metrics")
  |> range(start: -1h)
  |> filter(fn: (r) => r._measurement == "vm_metrics")
```

---

## Key Differences from SQL

| Concept | SQL | InfluxDB |
|---|---|---|
| Schema | Defined upfront (DDL) | Auto-created on first write |
| Primary key | User-defined | Timestamp + tag set |
| Query language | SQL | Flux |
| Best for | Relational, joins | Time-range queries, metrics |
| Compression | Standard | High (delta encoding for timestamps) |
| Retention | Manual DELETE | Built-in retention policies |
