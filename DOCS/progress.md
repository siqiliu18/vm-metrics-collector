# Build Progress

## World 1 Checkpoints

### Checkpoint 1 — Agent scrapes and logs (no Kafka) [DONE]

**Goal:** Prove the agent can connect to Rancher Desktop, call the Kubernetes Metrics API,
and print node metrics to stdout. No Kafka, no InfluxDB needed yet.

**Files to create:**
- [x] `go.mod` — module definition + dependencies
- [x] `cmd/agent/main.go` — scrape loop, stdout logging
- [x] `cmd/agent/Dockerfile` — build the Go binary

**What it scrapes** (via `GET /apis/metrics.k8s.io/v1beta1/nodes`):

| Field | Type | Source |
|---|---|---|
| `cpu_percent` | float | node CPU usage / allocatable |
| `mem_percent` | float | node memory usage / allocatable |
| `disk_percent` | float | node disk usage / allocatable |
| `net_in_bytes` | int | network receive bytes |
| `net_out_bytes` | int | network transmit bytes |

Tagged with: `vm_id`, `hostname`, `region`, `timestamp`

**Env vars used:**
- `KUBECONFIG` — path(s) to kubeconfig file(s), colon-separated for multiple clusters
- `KUBE_CONTEXTS` — comma-separated context names to scrape; empty = scrape all
- `SCRAPE_INTERVAL_SECONDS` — how often to scrape (default: 15)

**Test A — local (no Docker, fastest feedback):**
```bash
# prerequisite: Rancher Desktop is running
# NOTE: use absolute path — Go does not expand ~, so ~/.kube/config will not work
export KUBECONFIG=/Users/siqiliu/.kube/config
export KUBE_CONTEXTS=rancher-desktop
export SCRAPE_INTERVAL_SECONDS=5   # shorter interval for testing

go run ./cmd/agent
```
Expected output every 5s:
```
starting agent: contexts=[rancher-desktop] interval=5s
[rancher-desktop] node=rancher-desktop                 cpu= 12.3%  mem= 45.6%
```

**Test B — via docker run (single container, no compose):**

Running the agent container requires a patched kubeconfig because `127.0.0.1` inside
a container means the container itself, not your Mac. Steps:

```bash
# 1. patch kubeconfig: replace 127.0.0.1 with host.docker.internal
sed 's/127.0.0.1/host.docker.internal/g' ~/.kube/config > ~/docker-kube/config

# 2. fix tls-server-name indentation in ~/docker-kube/config (under the cluster block):
#      server: https://host.docker.internal:6443
#      tls-server-name: localhost       ← must be at same indent level as server:
#    Without this, TLS cert verification fails because the cert is valid for
#    "localhost" not "host.docker.internal"

# 3. run
docker rm agent-test 2>/dev/null
docker run --name agent-test \
  -e KUBECONFIG=/root/.kube/config \
  -e KUBE_CONTEXTS=rancher-desktop \
  -e SCRAPE_INTERVAL_SECONDS=5 \
  -v ~/docker-kube:/root/.kube:ro \
  agent
```

Expected output:
```
starting agent: contexts=[rancher-desktop] interval=5s timeout=10s
[rancher-desktop] node=lima-rancher-desktop           cpu=  7.1%  mem= 73.6%
```

**Test C — via Docker Compose:**
```bash
docker-compose up --build agent
docker-compose logs -f agent
```

**What to check:**
- [x] Agent starts without error
- [x] At least one node line printed per scrape interval
- [x] CPU and memory percentages are non-zero and plausible (0–100%)
- [x] Re-run with `KUBE_CONTEXTS=""` to verify all contexts are scraped
  - result: all contexts found and attempted; unreachable ones log errors and are skipped (correct behavior)
  - NOTE: empty KUBE_CONTEXTS is for debugging only — always set it explicitly in docker-compose to avoid scraping 40+ contexts and hitting network timeouts on each cycle
- [x] Test B (docker run) passes — confirmed output:
  ```
  [rancher-desktop] node=lima-rancher-desktop  cpu=7.1%  mem=73.6%
  ```
- [ ] Test C (docker-compose) — pending Checkpoint 2

---

### Checkpoint 2 — Agent produces to Kafka [DONE]

**Goal:** Replace stdout logging with real Kafka messages.
Topic: `vm-metrics-ts`, partition key: `vm_id`.

**Files created/updated:**
- [x] `internal/kafka/producer.go` — Kafka producer helper (sarama SyncProducer)
- [x] `cmd/agent/main.go` — producer created once in main(), passed to scrape()

**Test — local agent + docker-compose Kafka:**
```bash
# prerequisite: add kafka to /etc/hosts (one-time setup)
echo "127.0.0.1 kafka" | sudo tee -a /etc/hosts
# NOTE: needed because KAFKA_ADVERTISED_LISTENERS=kafka:9092 — Kafka tells clients
# to reconnect to "kafka", which must resolve on your Mac too

# start Kafka only
docker-compose up kafka

# in another terminal — run agent
export KUBECONFIG=/Users/siqiliu/.kube/config
export KUBE_CONTEXTS=rancher-desktop
export KAFKA_BROKERS=localhost:9092
export KAFKA_TOPIC=vm-metrics-ts
export SCRAPE_INTERVAL_SECONDS=5
go run ./cmd/agent

# in a third terminal — verify messages arrive
docker-compose exec kafka kafka-console-consumer \
  --bootstrap-server kafka:9092 \
  --topic vm-metrics-ts \
  --from-beginning
```

Expected consumer output:
```json
{"vm_id":"rancher-desktop","hostname":"lima-rancher-desktop","timestamp":1775453494311600000,"cpu_pct":9.8,"mem_pct":76.2}
```

**What to check:**
- [x] Messages appear in kafka-console-consumer
- [x] `vm_id`, `hostname`, `cpu_pct`, `mem_pct` fields present
- [x] Timestamp is nanoseconds (large number ~1.7e18)
- [ ] Test via full docker-compose — pending Checkpoint 3

---

### Checkpoint 3 — Consumer reads Kafka and writes InfluxDB [ ]

**Goal:** Consumer reads raw messages from Kafka and writes them to InfluxDB immediately
(real-time path, no windowing yet).

**Files to create:**
- [ ] `internal/influx/writer.go` — InfluxDB write client
- [ ] `cmd/consumer/main.go` — Kafka consumer loop + InfluxDB write
- [ ] `cmd/consumer/Dockerfile`

**How to verify:**
```bash
docker-compose up --build
# check InfluxDB UI at http://localhost:8086 (admin / adminpassword)
# Data Explorer → bucket: metrics → measurement: vm_metrics
```

---

### Checkpoint 4 — 1-hour tumbling window aggregation [ ]

**Goal:** Consumer accumulates metrics per `vm_id` and on each hour boundary flushes
avg/min/max/p95 to InfluxDB. Kafka offset committed only after flush.

**Files to update:**
- [ ] `cmd/consumer/main.go` — add windowing logic

**InfluxDB schema after this checkpoint:**
```
Measurement: vm_metrics
Tags:        vm_id, hostname, region
Fields:      cpu_percent, mem_percent, disk_percent, net_in_bytes, net_out_bytes
Timestamp:   nanosecond precision
```

---

### Checkpoint 5 — REST Query API [ ]

**Goal:** HTTP server that queries InfluxDB via Flux and returns JSON.

**Files to create:**
- [ ] `internal/influx/query.go` — InfluxDB query client
- [ ] `cmd/api/main.go` — chi router + handlers
- [ ] `cmd/api/Dockerfile`

**Endpoints:**
```
GET /metrics/{vm_id}?start=<unix>&end=<unix>&resolution=1m
GET /metrics/{vm_id}/summary?window=1h
GET /vms
GET /health
```

**How to verify:**
```bash
curl http://localhost:8080/health
curl http://localhost:8080/vms
```

---

### Checkpoint 6 — Full stack + Grafana dashboard [ ]

**Goal:** `docker-compose up` brings everything up. Grafana dashboard shows per-VM
time-series and 1-hour summaries.

**Files to create:**
- [ ] `grafana/provisioning/dashboards/vm-metrics.json` — dashboard definition

**How to verify:**
```bash
docker-compose up --build
# Grafana at http://localhost:3000 (admin / admin)
# InfluxDB at http://localhost:8086
# API at http://localhost:8080
```

---

## Definition of Done (from design.md)

- [ ] Go agent collects CPU/mem/disk from k8s nodes via kubeconfig
- [ ] Agent produces to Kafka with `vm_id` as partition key
- [ ] Consumer reads from Kafka and writes raw metrics to InfluxDB
- [ ] 1-hour tumbling window aggregation working and flushing to InfluxDB
- [ ] REST API: query metrics by VM + time range
- [ ] `docker-compose up` brings up full stack
- [ ] Demo: 3 agents scraping Rancher Desktop, metrics queryable, 1-hour summary visible
- [ ] Grafana dashboard shows per-VM time-series
