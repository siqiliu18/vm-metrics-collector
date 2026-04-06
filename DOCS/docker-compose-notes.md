# Docker Compose — Study Notes

## Core Concept

Each entry under `services:` in `docker-compose.yml` becomes one running container.
`docker-compose up` starts all of them; `docker-compose down` stops them.

---

## Key Fields Explained

### `image:` vs `build:`

```yaml
# Pull a pre-built image from Docker Hub
image: confluentinc/cp-kafka:7.6.0

# Build from a local Dockerfile
build:
  context: .                        # repo root is the build context
  dockerfile: cmd/agent/Dockerfile  # which Dockerfile to use
```

- `image:` — download and run, like installing a pre-built binary
- `build:` — compile from source first, then run
- `context: .` matters because it determines what files the Dockerfile can `COPY` from

Run `docker-compose up --build` to force a fresh image build.

---

### `environment:` — Config passed into the container

Equivalent to `docker run -e KEY=VALUE`. The container reads these on startup.

```yaml
environment:
  KAFKA_BROKERS: kafka:9092   # Go code reads this via os.Getenv("KAFKA_BROKERS")
```

There is no universal standard — each image defines which env vars it accepts.
You learn them from the image's documentation or Docker Hub page. Examples:
- `DOCKER_INFLUXDB_INIT_*` → defined by the InfluxDB Docker image
- `KAFKA_LISTENERS`, `KAFKA_NODE_ID` → defined by the Confluent Kafka image
- `GF_SECURITY_ADMIN_PASSWORD` → defined by the Grafana image

---

### `volumes:` — Persistent storage

Without volumes, all data inside a container is lost when it stops.

There are two syntaxes:

```yaml
# 1. Named volume (Docker-managed, no slash at start)
volumes:
  - kafka-data:/var/lib/kafka/data
#   ^^^^^^^^^^ just a label; Docker stores it under /var/lib/docker/volumes/

# 2. Bind mount (real folder on your host, starts with ./ or /)
volumes:
  - ./grafana/provisioning:/etc/grafana/provisioning
#   ^^^^^^^^^^^^^^^^^^^^^ actual folder on your laptop
```

Named volumes must be declared at the bottom of the file:
```yaml
volumes:      # tells Docker to create and manage these
  kafka-data:
  influxdb-data:
  grafana-data:
```

The `:` separator means: `left (host)` → `right (inside container)`.

Delete named volumes with `docker-compose down -v` (the `-v` flag wipes data).

---

### `ports:` — Accessing containers from your laptop

```yaml
ports:
  - "9092:9092"   # host_port:container_port
```

This is **only for host → container** access (e.g., your Mac connecting to `localhost:9092`).

**Other containers do NOT need this** — they communicate over Docker's internal network using service names.

---

### `depends_on:` — Startup ordering

```yaml
depends_on:
  kafka:
    condition: service_healthy   # wait until kafka passes its healthcheck
```

Prevents a service from starting before its dependencies are ready.
Combined with `healthcheck:` on the dependency, this ensures Kafka is actually accepting connections before the agent tries to connect.

---

## Container-to-Container Networking

Docker Compose creates a **private network** for all services automatically.
Every service is reachable by its **service name** as a hostname — no IPs needed.

```
docker network (private)
  ├── kafka      → hostname "kafka",    port 9092
  ├── influxdb   → hostname "influxdb", port 8086
  ├── agent
  ├── consumer
  └── grafana
```

| Who connects | Address | Why |
|---|---|---|
| Containers talking to each other | `kafka:9092` | Docker internal DNS resolves service name → container IP |
| Your Mac | `localhost:9092` | `ports:` forwarding |

This is why `KAFKA_BROKERS: kafka:9092` appears in `agent` and `consumer` — not `localhost`.

---

## Grafana Provisioning

**Problem:** Grafana stores its config (datasources, dashboards) in an internal database.
Without provisioning, you'd have to manually click through the UI every time you recreate the container.

**Solution:** Grafana's provisioning feature — drop YAML files into specific folders and Grafana reads them on startup automatically.

```
/etc/grafana/provisioning/      ← Grafana watches this directory (hardcoded convention)
  ├── datasources/              ← all .yaml files here become datasources
  ├── dashboards/               ← dashboard definitions
  ├── alerting/
  └── plugins/
```

`influxdb.yaml` is not a Kubernetes file — it's Grafana's own config format:

```yaml
apiVersion: 1
datasources:
  - name: InfluxDB
    type: influxdb
    access: proxy              # Grafana backend makes the HTTP call (not your browser)
    url: http://influxdb:8086  # service name, not localhost
    jsonData:
      version: Flux            # query language (Flux, not the older InfluxQL)
      organization: vm-metrics
      defaultBucket: metrics
    secureJsonData:
      token: my-super-secret-token
```

The three values (`organization`, `defaultBucket`, `token`) must exactly match what InfluxDB was initialized with via its `DOCKER_INFLUXDB_INIT_*` env vars.

**How docker-compose delivers the file to Grafana:**
```yaml
# docker-compose.yml
volumes:
  - ./grafana/provisioning:/etc/grafana/provisioning
# Mounts the whole folder — Grafana finds datasources/influxdb.yaml naturally
```

`docker-compose.yml` only knows how to start containers and mount files.
The application (Grafana) reads those files and acts on them — that's outside docker-compose's responsibility.

---

## Volume Mount Options — Three-Part Syntax

```yaml
- ~/.kube:/root/.kube:ro
#  ^^^^^^  ^^^^^^^^^^^  ^^
#  host    container    options
```

A third colon adds **mount options**. `ro` = read-only. The container can read the folder but not modify it. Default (no third part) is `rw` (read-write). Using `ro` is a security best practice when the container has no reason to write.

---

## `environment:` vs Application Logic — A Critical Distinction

Docker's only job with env vars is to **pass them into the container**. What happens with them is entirely up to the application code.

Example — the agent's three env vars work together:

```yaml
KUBECONFIG: /root/.kube/config    # tells Go where the kubeconfig file is
KUBE_CONTEXTS: "rancher-desktop"  # tells Go which contexts to scrape
```

```yaml
volumes:
  - ~/.kube:/root/.kube:ro        # makes the actual file available at that path
```

The Go agent code reads and acts on these:

```go
kubeconfig  := os.Getenv("KUBECONFIG")     // "/root/.kube/config"
contextsEnv := os.Getenv("KUBE_CONTEXTS")  // "rancher-desktop"

var contexts []string
if contextsEnv == "" {
    contexts = getAllContextsFromKubeconfig(kubeconfig)  // scrape ALL clusters
} else {
    contexts = strings.Split(contextsEnv, ",")           // scrape specific ones
}

for _, ctx := range contexts {
    client := buildK8sClient(kubeconfig, ctx)
    go scrapeMetrics(client, ctx)
}
```

So the three behaviors are **Go logic, not Docker features**:

| `KUBE_CONTEXTS` value | Result |
|---|---|
| `"rancher-desktop"` | scrape only rancher-desktop |
| `"rancher-desktop,cluster-2"` | scrape both |
| `""` (empty) | scrape all contexts in the kubeconfig |

Docker is just the delivery mechanism. The same pattern applies to every env var in this project — `KAFKA_BROKERS`, `INFLUXDB_URL`, `WINDOW_DURATION_MINUTES`, etc. docker-compose passes them in; the application reads and acts on them.

---

## `host.docker.internal` — Reaching the Host from Inside a Container

Inside a container, `127.0.0.1` means **the container itself** — not your Mac.

This matters for the agent because Rancher Desktop's k8s API listens on your Mac at
`127.0.0.1:6443`, and `~/.kube/config` has `server: https://127.0.0.1:6443`.
When the agent runs inside Docker and tries that address, it hits the container's own
loopback — nothing is there → `connection refused`.

```
Your Mac
├── Rancher Desktop → k8s API at 127.0.0.1:6443
├── ~/.kube/config  → server: https://127.0.0.1:6443
│
└── Docker container
    ├── /root/.kube/config (mounted from ~/.kube/config)
    │   still says server: https://127.0.0.1:6443
    │
    └── app → 127.0.0.1:6443 = container's own loopback → connection refused
```

**Fix:** Docker injects `host.docker.internal` into every container — it resolves to your
Mac's IP. Create a patched kubeconfig before running:

```bash
sed 's/127.0.0.1/host.docker.internal/g' ~/.kube/config > /tmp/docker-kube-config

docker run --name agent-test \
  -e KUBECONFIG=/root/.kube/config \
  -e KUBE_CONTEXTS=rancher-desktop \
  -e SCRAPE_INTERVAL_SECONDS=5 \
  -v /tmp/docker-kube-config:/root/.kube/config:ro \
  agent
```

```
Your Mac
├── Rancher Desktop → k8s API at 127.0.0.1:6443
├── /tmp/docker-kube-config → server: https://host.docker.internal:6443
│
└── Docker container
    ├── /root/.kube/config (mounted from /tmp/docker-kube-config)
    │   says server: https://host.docker.internal:6443
    │
    └── app → host.docker.internal:6443
                    ↑ Docker resolves to Mac's IP → reaches Rancher Desktop → works
```

**Note on startup logs:** The agent prints `starting agent: contexts=[...] interval=5s`
immediately on boot — before the first scrape. The first actual API call happens after
the first ticker tick (e.g. 5 seconds later). A startup log line does NOT mean the
scrape succeeded.

---

## Where to Learn Image-Specific Config

There is no way to guess image-specific env vars or folder conventions — always look them up:

| Image | Where to look |
|---|---|
| `influxdb:2.7` | hub.docker.com/_/influxdb |
| `confluentinc/cp-kafka` | docs.confluent.io |
| `grafana/grafana` | grafana.com/docs → Administration → Provisioning |

---

## Quick Reference — Mental Models

| Concept | Analogy |
|---|---|
| `image:` | Download and run a pre-built binary |
| `build:` | Compile from source, then run |
| `environment:` | Command-line flags / startup config |
| `volumes:` | External hard drive plugged into the container |
| `ports:` | Port forwarding from your laptop into the container |
| `depends_on:` | "Don't start me until X is healthy" |
| Named volume | Docker manages the storage location for you |
| Bind mount | You provide a real folder path from your host |
| Service name DNS | `kafka` resolves to the kafka container's IP inside the network |
