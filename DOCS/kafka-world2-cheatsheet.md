# Kafka World 2 Cheat Sheet (K8s)

This note explains how Kafka listener settings map to Kubernetes services in this repo.

Scope:
- `k8s/hub/kafka/statefulset.yaml`
- `k8s/hub/kafka/service.yaml`

## Port and Listener Map

- `PLAINTEXT` listener
  - Bind: `KAFKA_LISTENERS` -> `PLAINTEXT://0.0.0.0:9092`
  - Container port: `9092`
  - Service: `kafka-headless` port `9092`
  - Used by: in-cluster producer/consumer traffic

- `CONTROLLER` listener (KRaft internal control plane)
  - Bind: `KAFKA_LISTENERS` -> `CONTROLLER://0.0.0.0:9093`
  - Container port: `9093`
  - Service: `kafka-headless` port `9093`
  - Used by: KRaft controller quorum traffic, not app produce/consume

- `EXTERNAL` listener
  - Bind: `KAFKA_LISTENERS` -> `EXTERNAL://0.0.0.0:9094`
  - Container port: `9094`
  - Service: `kafka-external` port `9094`, NodePort `30094`
  - Advertised to external clients as: `<node-ip>:30094`
  - Used by: agents in other clusters

## Must-Match Rules

- `KAFKA_LISTENERS` ports must match container bind ports.
  - If listener says `...:9093`, Kafka must actually bind `9093`.

- Controller settings must be name-consistent:
  - `KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER`
  - `CONTROLLER` must exist in `KAFKA_LISTENERS`
  - `CONTROLLER` must exist in `KAFKA_LISTENER_SECURITY_PROTOCOL_MAP`

- `KAFKA_CONTROLLER_QUORUM_VOTERS` endpoint must point to controller listener port.
  - In this repo: `1@kafka-0.kafka-headless.monitoring.svc.cluster.local:9093`
  - `:9093` must align with `CONTROLLER://...:9093`

- Internal advertised listener should be resolvable internally.
  - `PLAINTEXT://kafka-0.kafka-headless.monitoring.svc.cluster.local:9092`

## Allowed-To-Differ (By Design)

- `EXTERNAL` bind port and advertised port can differ.
  - Bind in pod: `9094`
  - Advertise to other clusters: NodePort `30094`
  - This is expected in K8s NodePort setups.

## What `1@` Means in Quorum Voters

- Format: `<controller-node-id>@<host>:<port>`
- `1@...:9093` means:
  - controller node id = `1`
  - controller endpoint = `...:9093`

This `1` is not the same concept as replication factor settings.

## `1` in Offsets Replication Factor

- `KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1` means offsets topic has one replica.
- It is a data durability/replication setting, not a node id.
- For single-broker dev setup, `1` is required.

## `CLUSTER_ID` in KRaft

- `CLUSTER_ID` identifies the Kafka cluster metadata in KRaft mode.
- Keep it stable for the same persisted data directory.
- Changing it against existing data can cause startup/metadata mismatch failures.

## Quick Validation Checklist

- Listener names are consistent across:
  - `KAFKA_LISTENERS`
  - `KAFKA_LISTENER_SECURITY_PROTOCOL_MAP`
  - `KAFKA_CONTROLLER_LISTENER_NAMES`
  - `KAFKA_INTER_BROKER_LISTENER_NAME`

- Port consistency:
  - `PLAINTEXT` is `9092` across listener/bind/service/advertised internal
  - `CONTROLLER` is `9093` across listener/bind/service/quorum voters
  - `EXTERNAL` bind is `9094`, advertised external is `nodeIP:30094`

- Service routing:
  - `kafka-headless` for in-cluster DNS-based access
  - `kafka-external` NodePort for cross-cluster agent access

## Common Failure Patterns

- Kafka boots, clients cannot connect after bootstrap:
  - Usually wrong `KAFKA_ADVERTISED_LISTENERS` endpoint/port.

- Controller quorum errors in logs:
  - `KAFKA_CONTROLLER_LISTENER_NAMES` mismatch or wrong `...VOTERS` port.

- External agents time out:
  - NodePort not reachable, wrong node IP replacement, or advertised external port not `30094`.
