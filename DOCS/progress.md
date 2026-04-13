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

### Checkpoint 3 — Consumer reads Kafka and writes InfluxDB [DONE]

**Goal:** Consumer reads raw messages from Kafka and writes them to InfluxDB immediately
(real-time path, no windowing yet).

**Files created:**
- [x] `internal/influx/writer.go` — InfluxDB write client (non-blocking WriteAPI)
- [x] `cmd/consumer/main.go` — Kafka consumer group loop + InfluxDB write
- [x] `cmd/consumer/Dockerfile` — same two-stage pattern as agent

**Test — docker-compose Kafka + InfluxDB + consumer, local agent:**
```bash
# terminal 1 — start infrastructure + consumer
docker-compose up --build kafka influxdb consumer

# terminal 2 — run agent locally to produce messages
export KUBECONFIG=/Users/siqiliu/.kube/config
export KUBE_CONTEXTS=rancher-desktop
export KAFKA_BROKERS=localhost:9092
export KAFKA_TOPIC=vm-metrics-ts
export SCRAPE_INTERVAL_SECONDS=5
go run ./cmd/agent

# verify data in InfluxDB UI
# http://localhost:8086 (admin / adminpassword)
# Data Explorer → bucket: metrics → measurement: vm_metrics
# Flux query:
# from(bucket: "metrics")
#   |> range(start: -1h)
#   |> filter(fn: (r) => r._measurement == "vm_metrics")
```

**To stop:**
```bash
docker-compose stop kafka influxdb consumer
# named volumes (InfluxDB data) are preserved — use docker-compose down -v to wipe
```

**What to check:**
- [x] Consumer starts and joins consumer group
- [x] Consumer logs metric lines as messages arrive
- [x] Data visible in InfluxDB Data Explorer
- [x] `vm_id` and `hostname` appear as tags, `cpu_pct` and `mem_pct` as fields

---

### Checkpoint 4 — 1-hour tumbling window aggregation [DONE]

**Goal:** Consumer accumulates metrics per `vm_id` and on each hour boundary flushes
avg/min/max/p95 to InfluxDB. Kafka offset committed only after flush.

**Files updated:**
- [x] `cmd/consumer/main.go` — window flusher goroutine (ticker every minute, flushes on hour boundary)
- [x] `internal/influx/writer.go` — `WriteHourlySummary()` writes to `vm_metrics_hourly`

**Design:**
- `cpuWindows`/`memWindows` — `map[string][]float64` accumulates readings per `vm_id`
- `sync.Mutex` protects concurrent access between message loop and flusher goroutine
- Flusher snapshots and resets the maps atomically, then writes summaries
- p95 computed via sorted slice at flush time (cannot be recomputed after raw data expires)

---

### Checkpoint 5 — REST Query API [DONE]

**Goal:** HTTP server that queries InfluxDB via Flux and returns JSON.

**Files created:**
- [x] `internal/influx/query.go` — Flux queries: ListVMs, GetMetrics, GetHourlySummary
- [x] `cmd/api/main.go` — chi router + 4 handlers
- [x] `cmd/api/Dockerfile` — same two-stage build pattern

**Endpoints:**
```
GET /health                               → {"status":"ok"}
GET /vms                                  → ["rancher-desktop", ...]
GET /metrics/{vm_id}?start=<unix>&end=<unix>  → raw metric points (default: last 1h)
GET /metrics/{vm_id}/summary              → hourly avg/min/max/p95
```

**Port remapping (Rancher Desktop occupies defaults):**
```
API:      localhost:8081  (was 8080)
InfluxDB: localhost:8087  (was 8086)
Kafka:    localhost:9093  (was 9092)
Grafana:  localhost:3100  (was 3000)
```

**How to verify:**
```bash
curl http://localhost:8081/health
curl http://localhost:8081/vms
curl http://localhost:8081/metrics/rancher-desktop
curl "http://localhost:8081/metrics/rancher-desktop?start=1"   # all data ever
curl http://localhost:8081/metrics/rancher-desktop/summary
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

---

## World 2 — Kubernetes Deployment [DONE]

### Why World 2?

World 1 (docker-compose) ran the agent on your Mac. To monitor remote IBM Fyre clusters
it needed a VPN connection inside the container — impractical. World 2 deploys one agent
**pod inside each cluster**, using in-cluster auth. No VPN, no kubeconfig file distribution.

---

### The Image Registry Story

Kubernetes pulls images from a registry at deploy time. Three choices exist:
- **Docker Hub** — default, but anonymous pulls are rate-limited to ~10/6 hours per IP.
  On a shared IBM Fyre cluster this limit hits fast and causes `ImagePullBackOff`.
- **Private registry** — requires `kubectl create secret` on every cluster. Cumbersome.
- **GitHub Container Registry (ghcr.io)** — free, no rate limits for public packages,
  and packages can be made public so no pull secret is needed anywhere.

**Decision: push all images to `ghcr.io/siqiliu18/` and make them public.**

There are two categories of images:

**Custom images** (built from this repo's Dockerfiles):
- `ghcr.io/siqiliu18/vm-metrics-agent:latest`
- `ghcr.io/siqiliu18/vm-metrics-consumer:latest`
- `ghcr.io/siqiliu18/vm-metrics-api:latest`

These are built automatically by GitHub Actions (`.github/workflows/build.yml`) on every
push to `main` that touches `cmd/**` or `internal/**`. The Actions runner is `ubuntu-latest`
(linux/amd64), so the images are always built for the right architecture.

**Third-party images** (mirrored to avoid Docker Hub rate limits):
- `ghcr.io/siqiliu18/cp-kafka:7.6.0` (mirrored from `confluentinc/cp-kafka:7.6.0`)
- `ghcr.io/siqiliu18/influxdb:2.7` (mirrored from `influxdb:2.7`)
- `ghcr.io/siqiliu18/grafana:10.4.0` (mirrored from `grafana/grafana:10.4.0`)

Mirroring is a manual one-time step per version:
```bash
docker pull --platform linux/amd64 influxdb:2.7
docker tag influxdb:2.7 ghcr.io/siqiliu18/influxdb:2.7
docker push ghcr.io/siqiliu18/influxdb:2.7
```
The `--platform linux/amd64` flag is critical — without it, Docker on an Apple Silicon Mac
pulls the arm64 variant, which causes `exec format error` on amd64 cluster nodes.

**How to make a ghcr.io package public:**
GitHub → your profile → Packages → select package → Package settings →
Change visibility → Public. Must be done once per package after first push.

**GitHub Actions write permissions:**
For GitHub Actions to push to an existing package, the repo must have write access:
Package settings → Manage Actions access → Add repository → Write role.
(New packages created by Actions get this automatically; packages first pushed manually need it set explicitly.)

---

### Architecture on the Hub Cluster

The hub cluster (sgn3) runs the full stack:

```
[agent pod] ──→ Kafka (PLAINTEXT:9092) ──→ [consumer pod] ──→ InfluxDB
                Kafka (EXTERNAL:9094)  ←── agent pods on other clusters
                     ↑ NodePort 30094
[api pod] ──→ InfluxDB     (NodePort 30080)
[grafana pod] ──→ InfluxDB (NodePort 30300)
```

**Why Kafka has two listeners:**
Kafka tells connecting clients "reconnect to my advertised address". Internal clients
(consumer pod, hub agent) use `kafka-0.kafka-headless.monitoring.svc.cluster.local:9092`
(the headless service DNS, only resolvable inside the cluster). External clients (agent pods
on other clusters) can't resolve that DNS, so they use the NodePort address
(`9.60.158.240:30094`) via the EXTERNAL listener. A single listener would advertise one
address that only works for one side.

**Why the hub agent uses a different address than external agents:**
The hub agent pod is inside the same cluster as Kafka. Connecting via NodePort
(pod → node IP → NodePort) requires hairpin NAT, which bare-metal clusters often don't
support. So the hub agent connects via the internal PLAINTEXT listener (port 9092)
while external cluster agents connect via NodePort 30094.

---

### NFS and the initContainer

IBM Fyre clusters use NFS for persistent storage. NFS is often configured with `root_squash`,
which remaps root (uid=0) inside the container to `nobody` (uid=65534) on the NFS server.

Confluent Kafka's entrypoint internally switches to `appuser` (uid=1000) regardless of the
container's `runAsUser` setting. If the NFS directory is not owned by uid=1000, Kafka
cannot write its data and crashes.

The fix: an `initContainer` runs first as root, does `chown -R 1000:1000 /var/lib/kafka/data`,
then exits. The main Kafka container starts after this succeeds and can write freely.

The initContainer uses `ghcr.io/siqiliu18/vm-metrics-agent:latest` (an alpine-based image
already on ghcr.io) to avoid pulling yet another Docker Hub image.

On non-NFS clusters (EKS, GKE, AKS), `securityContext.fsGroup: 1000` on the pod spec
is the cleaner solution — Kubernetes handles the ownership change automatically.

---

### Kafka's ADVERTISED_LISTENERS Chicken-and-Egg

The StatefulSet env var `KAFKA_ADVERTISED_LISTENERS` must contain the node's actual IP
so external agents can reach it. But the IP is only known after the cluster exists.

Solution in `deploy.sh`:
1. Apply `k8s/hub/kafka/service.yaml` first (creates the NodePort service)
2. Query the node IP: `kubectl get nodes -o jsonpath=...`
3. Use `sed` to substitute `REPLACE_KAFKA_LB_IP` with the real IP before applying the StatefulSet

**Important:** always apply the Kafka StatefulSet through `deploy.sh`, never with
`kubectl apply -f k8s/hub/kafka/statefulset.yaml` directly — that applies the file with
the literal placeholder, breaking ADVERTISED_LISTENERS.

---

### deploy.sh Design

```
./deploy.sh hub [kubeconfig]              # deploy full hub stack
./deploy.sh agent <hub-kc> <agent-kc>... # deploy agent on one or more clusters
./deploy.sh undeploy hub [kubeconfig]    # tear down hub stack
./deploy.sh undeploy agent <kc>...       # remove agent from clusters
```

- Kubeconfig argument is optional if `$KUBECONFIG` is already exported in the shell
- Hub deploy/undeploy shows a confirmation prompt (kubeconfig path + node IP) before acting
- `derive_cluster_name`: uses the kubeconfig **filename** if it's descriptive (e.g. `sgn3.yaml` → `sgn3`),
  falls back to parent directory name (e.g. `sgn3/kubeconfig.yaml` → `sgn3`)
- `deploy agents`: takes hub kubeconfig as first arg, queries Kafka IP directly from the
  hub cluster — no local state files

---

### Issues Hit During Deployment and Their Fixes

| Issue | Root Cause | Fix |
|---|---|---|
| `ImagePullBackOff` on all pods | Docker Hub anonymous rate limit | Mirror all images to ghcr.io |
| `exec format error` on cluster | Images built on Mac (arm64), cluster is amd64 | `docker pull --platform linux/amd64` for third-party; GitHub Actions builds custom images |
| `exec format error` after re-push | Pinned tag (`:10.4.0`) → `IfNotPresent` pull policy — node used cached arm64 | Add `imagePullPolicy: Always` to grafana deployment |
| Kafka `/var/lib/kafka/data` not writable | NFS root_squash + Confluent forces uid=1000 | initContainer does `chown -R 1000:1000` before Kafka starts |
| `REPLACE_KAFKA_LB_IP` literal in Kafka | Applied StatefulSet directly without `sed` substitution | Always deploy Kafka via `deploy.sh` which runs `sed` |
| Kafka readiness probe timing out | Default timeout was 1s, too short for Kafka startup | Added `timeoutSeconds: 10` to readiness probe |
| Consumer: `no such host kafka-0.kafka-headless...` | Kafka pod not Ready → headless DNS has no endpoint | Fixed Kafka first; DNS entry appears only when pod is Ready |
| Hub agent: `connection refused` on port 9094 | Pod→nodeIP→NodePort (hairpin NAT) not supported on bare-metal | Hub agent uses internal PLAINTEXT listener (port 9092) instead |
| External agents: `connection refused` on port 9094 even with correct KAFKA_BROKERS=ip:30094 | Kafka metadata redirect — client bootstraps on 30094 but Kafka advertises `EXTERNAL://ip:9094` in metadata; client then dials 9094 which is refused | `KAFKA_ADVERTISED_LISTENERS` must use the NodePort (`EXTERNAL://ip:30094`), not the pod port (9094) |
| Consumer: `no such host kafka-0.kafka-headless...` after kafka-0 bounce | Consumer's Kafka client cached the DNS failure from when the pod was down; doesn't self-heal | `kubectl rollout restart deployment/consumer` after kafka-0 is Ready again |
| GitHub Actions `permission_denied` pushing image | Package existed but repo didn't have write access | Package settings → Manage Actions access → Add repo with Write role |
| `kubectl get nodes -o jsonpath` returns empty | Fish shell or kubectl version issue with complex jsonpath filters | Hardcode the known IP or use `kubectl get nodes -o wide` |

---

## Definition of Done (from design.md)

- [x] Go agent collects CPU/mem from k8s nodes via kubeconfig
- [x] Agent produces to Kafka with `vm_id` as partition key
- [x] Consumer reads from Kafka and writes raw metrics to InfluxDB
- [x] 1-hour tumbling window aggregation working and flushing to InfluxDB
- [x] REST API: query metrics by VM + time range
- [x] `docker-compose up` brings up full stack
- [x] Grafana dashboard shows per-VM time-series
