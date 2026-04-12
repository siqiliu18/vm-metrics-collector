# Grafana — Study Notes

## What is Grafana?

Grafana is a **dashboard and visualization tool**. It does not store data — it connects
to data sources (InfluxDB, Prometheus, PostgreSQL, etc.) and renders charts from queries.

```
InfluxDB (stores data)
    ↓  Flux query
Grafana (renders charts)
    ↓  HTTP
Browser (you see graphs)
```

In this project:
- InfluxDB stores `vm_metrics` and `vm_metrics_hourly`
- Grafana queries InfluxDB directly via Flux
- You view dashboards at `http://localhost:3100` (admin / admin)

---

## Provisioning — Auto-configuration on Startup

Grafana supports **provisioning**: loading datasources and dashboards from config files
at startup, instead of clicking through the UI.

This matters for docker-compose: every `docker-compose up` starts a fresh Grafana
container. Without provisioning, you'd have to manually add the InfluxDB datasource
and re-create dashboards every time. With provisioning, everything auto-loads.

The provisioning directory is mounted into the container:
```yaml
# docker-compose.yml
volumes:
  - ./grafana/provisioning:/etc/grafana/provisioning
```

Grafana scans `/etc/grafana/provisioning` on startup and applies everything it finds.

---

## File Structure

```
grafana/provisioning/
├── datasources/
│   └── influxdb.yaml      ← "connect to InfluxDB at this URL with this token"
└── dashboards/
    ├── dashboards.yaml    ← "look for dashboard JSON files in this directory"
    └── vm-metrics.json    ← the actual dashboard definition (panels, queries, layout)
```

### `datasources/influxdb.yaml`

Declares the InfluxDB connection. Key fields:

```yaml
- name: InfluxDB
  uid: influxdb-vm-metrics      # stable ID referenced by dashboard JSON files
  type: influxdb
  url: http://influxdb:8086     # container-to-container DNS name
  jsonData:
    version: Flux               # use Flux query language (not InfluxQL)
    organization: vm-metrics
    defaultBucket: metrics
  secureJsonData:
    token: my-super-secret-token
```

**Why `uid`?** Dashboard JSON files reference datasources by UID. Without a fixed UID,
Grafana generates a random one each startup — dashboards can't find their datasource.
Always set a stable `uid` in the datasource YAML.

### `dashboards/dashboards.yaml`

Tells Grafana *where to scan* for dashboard JSON files:

```yaml
providers:
  - name: default
    type: file
    options:
      path: /etc/grafana/provisioning/dashboards
```

This is a loader config, not a dashboard itself. One YAML, many JSON files in the directory.

### `dashboards/vm-metrics.json`

The actual dashboard. This is the JSON export of what you'd build in the UI.
Key top-level fields:

```json
{
  "title": "VM Metrics",
  "uid": "vm-metrics-dashboard",   // stable ID for linking/bookmarking
  "refresh": "15s",                // auto-refresh interval
  "time": { "from": "now-1h", "to": "now" },  // default time range
  "panels": [ ... ]                // list of panels (charts, tables, stats)
}
```

---

## Panels

Each panel in `panels: []` is one chart. Key fields:

```json
{
  "id": 1,
  "title": "CPU % (raw, 15s)",
  "type": "timeseries",           // chart type: timeseries, table, stat, gauge, bar, etc.
  "gridPos": { "x": 0, "y": 0, "w": 12, "h": 8 },  // position on 24-column grid
  "datasource": { "uid": "influxdb-vm-metrics" },
  "targets": [ { "query": "..." } ]  // one target = one Flux query = one data series
}
```

### Grid layout

Grafana uses a **24-column grid**:
```
|←————————— 24 columns ——————————→|
| panel (w=12) | panel (w=12)      |  row 1 (y=0)
| panel (w=24, full width)         |  row 2 (y=8)
```

`h` is in "grid units" (roughly 30px each). `h=8` is a typical chart height.

### Chart types

| type | Use for |
|---|---|
| `timeseries` | Line/area charts over time — most common for metrics |
| `table` | Tabular data with columns |
| `stat` | Single big number (current value) |
| `gauge` | Speedometer-style for current value |
| `barchart` | Bar chart |

---

## Flux Queries in Grafana

Grafana injects special variables into Flux queries:

```flux
from(bucket: "metrics")
  |> range(start: v.timeRangeStart, stop: v.timeRangeStop)  // ← Grafana time picker
  |> filter(fn: (r) => r._measurement == "vm_metrics")
  |> filter(fn: (r) => r._field == "cpu_pct")
  |> aggregateWindow(every: v.windowPeriod, fn: mean, createEmpty: false)
  //                       ↑ Grafana auto-adjusts bucket size based on zoom level
```

| Variable | What it is |
|---|---|
| `v.timeRangeStart` | Left edge of the time picker (e.g. "now-1h") |
| `v.timeRangeStop` | Right edge of the time picker (e.g. "now") |
| `v.windowPeriod` | Auto-calculated bucket size based on panel width and time range |

`aggregateWindow` downsamples to avoid rendering thousands of raw points — Grafana
calculates `v.windowPeriod` automatically so the chart never has more points than pixels.

---

## This Project's Dashboard

Four panels — 2 rows of 2:

```
| CPU % (raw, 15s)          | Memory % (raw, 15s)          |  row 1
| Hourly CPU avg/min/max/p95| Hourly Mem avg/min/max/p95   |  row 2
```

| Panel | Measurement | Fields |
|---|---|---|
| CPU raw | `vm_metrics` | `cpu_pct` |
| Memory raw | `vm_metrics` | `mem_pct` |
| Hourly CPU | `vm_metrics_hourly` | `cpu_avg`, `cpu_min`, `cpu_max`, `cpu_p95` |
| Hourly Memory | `vm_metrics_hourly` | `mem_avg`, `mem_min`, `mem_max`, `mem_p95` |

**The hourly panels will be empty until the first hour boundary is hit** — the consumer
only writes to `vm_metrics_hourly` when a full hour of data has been accumulated and flushed.

---

## Access

```
Grafana UI:  http://localhost:3100   (admin / admin)
             ↑ port 3100 because Rancher Desktop occupies 3000
```

Dashboard location in UI: **Dashboards → VM Metrics**

---

## Key Differences from InfluxDB UI

| | InfluxDB UI (localhost:8087) | Grafana (localhost:3100) |
|---|---|---|
| Purpose | Explore raw data, debug queries | Production dashboards |
| Query editor | Flux, interactive | Flux, embedded in panel |
| Sharing | Not designed for sharing | Designed for team dashboards |
| Alerting | No | Yes (Grafana Alerting) |
| Use in this project | Verify writes, debug | Demo dashboard |
