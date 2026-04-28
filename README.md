# vm-metrics-collector

A distributed VM metrics monitoring pipeline built in Go. Collects CPU and memory metrics from Kubernetes nodes across multiple clusters, streams them through Kafka, stores them in InfluxDB, and exposes them via a REST API and Grafana dashboard.

Built as a portfolio project to demonstrate: Kafka windowed aggregation, time-series databases, push vs pull metrics, and multi-cluster Kubernetes deployment.

---

## Architecture

```
[agent pod]  ──→  Kafka  ──→  [consumer pod]  ──→  InfluxDB
  (per cluster)                                       ↑
                                              [api pod] + Grafana
```

- **agent** — scrapes node CPU/memory from the Kubernetes Metrics API every 15s, publishes to Kafka
- **consumer** — reads from Kafka, writes raw metrics + 1-hour tumbling window summaries to InfluxDB
- **api** — REST API to query metrics by cluster/VM and time range
- **grafana** — dashboard showing per-cluster time-series and hourly summaries

---

## Two deployment modes

### World 1 — docker-compose (local dev)

Runs everything on your Mac. Agent scrapes Rancher Desktop (and any remote clusters reachable via kubeconfig).

```bash
docker-compose up --build
```

| Service   | URL                                 |
|-----------|-------------------------------------|
| API       | http://localhost:8081               |
| InfluxDB  | http://localhost:8087               |
| Grafana   | http://localhost:3100 (admin/admin) |
| Kafka     | localhost:9093                      |

> Ports are remapped from defaults (8080→8081, 8086→8087, 3000→3100, 9092→9093) because
> Rancher Desktop SSH tunnels occupy those ports. Adjust `docker-compose.yml` if your setup differs.

See [DOCS/docker-compose-notes.md](DOCS/docker-compose-notes.md) for Rancher Desktop quirks.

### World 2 — Kubernetes (multi-cluster)

One agent pod runs **inside** each monitored cluster using in-cluster auth — no VPN, no kubeconfig distribution. A single hub cluster runs the central stack (Kafka, InfluxDB, consumer, API, Grafana).

```bash
# Deploy hub stack
./deploy.sh hub ~/Downloads/sgn3/kubeconfig.yaml

# Deploy agents on monitored clusters
./deploy.sh agent ~/Downloads/sql108/kubeconfig.yaml \
                  ~/Downloads/sql120/kubeconfig.yaml

# Tear down
./deploy.sh undeploy hub
./deploy.sh undeploy agent ~/Downloads/sql108/kubeconfig.yaml
```

| Service  | URL (hub node)               |
|----------|------------------------------|
| API      | http://\<node-ip\>:30080     |
| Grafana  | http://\<node-ip\>:30300     |

See [DOCS/design.md](DOCS/design.md) for full architecture and [DOCS/progress.md](DOCS/progress.md) for build progress and lessons learned.

---

## Image registry

All images are on [ghcr.io/siqiliu18](https://github.com/siqiliu18?tab=packages) (public, no pull secret needed):

| Image | Built by |
|---|---|
| `ghcr.io/siqiliu18/vm-metrics-agent:latest` | GitHub Actions |
| `ghcr.io/siqiliu18/vm-metrics-consumer:latest` | GitHub Actions |
| `ghcr.io/siqiliu18/vm-metrics-api:latest` | GitHub Actions |
| `ghcr.io/siqiliu18/cp-kafka:7.6.0` | Mirrored from Docker Hub |
| `ghcr.io/siqiliu18/influxdb:2.7` | Mirrored from Docker Hub |
| `ghcr.io/siqiliu18/grafana:10.4.0` | Mirrored from Docker Hub |

---

## API endpoints

```
GET /health                          → {"status":"ok"}
GET /vms                             → ["sgn3", "sql108", ...]
GET /metrics/{vm_id}                 → raw metric points (default: last 1h)
GET /metrics/{vm_id}/summary         → hourly avg/min/max/p95
```

---

## Tech stack

| Layer | Choice |
|---|---|
| Language | Go |
| Message queue | Kafka (KRaft, no Zookeeper) |
| Time-series DB | InfluxDB 2.7 |
| HTTP router | chi |
| Deployment (World 1) | docker-compose |
| Deployment (World 2) | Kubernetes + deploy.sh |
