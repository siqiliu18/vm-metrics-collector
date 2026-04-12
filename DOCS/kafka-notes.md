# Kafka — Study Notes

## Core Concept

Kafka is a **durable message queue**. Producers write messages; consumers read them.
Messages are stored on the broker (server) in a topic, independent of whether any
consumer has read them yet.

```
Producer → [Kafka Broker] → Consumer
               ↑
          stores messages durably
          (consumers read at their own pace)
```

The broker and the consumer are completely separate. A producer's job ends when the
broker confirms receipt. What the consumer does with the message — and when — is
unrelated to the producer.

---

## KAFKA_ADVERTISED_LISTENERS — The Two-Step Connection

When a producer connects to Kafka, there are **two steps**:

1. **Bootstrap connection** — client connects to the address you give it (e.g. `localhost:9092`)
   to fetch cluster metadata
2. **Actual connection** — Kafka responds with its `KAFKA_ADVERTISED_LISTENERS` address
   and tells the client "reconnect to me here"

This means even if the bootstrap succeeds, the real connection uses the advertised address.
In docker-compose:
```yaml
KAFKA_ADVERTISED_LISTENERS: PLAINTEXT://kafka:9092
```

So Kafka tells every client "reconnect to `kafka:9092`". Inside the Docker network, all
containers resolve `kafka` via Docker DNS — works fine. But on your Mac, `kafka` is not
a known hostname → `dial tcp: lookup kafka: no such host`.

**Fix for local development** — add `kafka` to your Mac's hosts file:
```bash
echo "127.0.0.1 kafka" | sudo tee -a /etc/hosts
```

Now your Mac resolves `kafka` → `127.0.0.1` → reaches the Kafka container via the
`ports: "9092:9092"` mapping. This is the standard workaround when connecting to
docker-compose Kafka from the host machine.

---

## Topics and Partitions

```
Topic: vm-metrics-ts
├── Partition 0: [msg1, msg4, msg7, ...]
├── Partition 1: [msg2, msg5, msg8, ...]
└── Partition 2: [msg3, msg6, msg9, ...]
```

- A **topic** is a named stream of messages (like a table name)
- A topic is split into **partitions** for parallelism
- Each partition is an ordered, append-only log
- Messages within a partition are ordered; across partitions, order is not guaranteed

**Partition key:** When producing, you specify a key. Kafka hashes the key to pick a
partition. Same key → always same partition → ordered delivery for that key.

In this project: `vm_id` is the key → all metrics for one VM land on the same partition
→ the consumer sees them in order, which is required for correct windowing.

---

## SyncProducer vs AsyncProducer

### SyncProducer

`SendMessage` **blocks** until the Kafka broker acknowledges the message was written
to the partition log.

```go
// blocks here until broker confirms
partition, offset, err := producer.SendMessage(msg)
if err != nil {
    // definitely failed — broker did not receive it
}
// if no error → message is safely on the broker
```

- **Confirmation from:** the Kafka broker (not the consumer)
- **Throughput:** lower — one message at a time
- **When to use:** when you need guaranteed delivery and simplicity matters more than
  throughput (e.g. this project — scraping every 15s, low volume)

### AsyncProducer

`Input() <- msg` returns immediately. The producer sends in the background.
Errors and successes come back on separate channels you must drain.

```go
producer.Input() <- msg   // returns immediately, no confirmation yet

// you MUST read these channels or they block and deadlock
go func() {
    for err := range producer.Errors() {
        log.Printf("failed to send: %v", err)  // broker did not receive it
    }
}()
go func() {
    for range producer.Successes() {
        // broker confirmed — optional to handle
    }
}()
```

- **Confirmation from:** the Kafka broker (via the Errors/Successes channels)
- **Throughput:** much higher — batches many messages in flight simultaneously
- **When to use:** high-throughput pipelines where you can tolerate some complexity
  (e.g. 100k+ messages/sec)

### When to use which

| Scenario | Choice | Reason |
|---|---|---|
| Low volume, simplicity important | SyncProducer | Straightforward error handling |
| High throughput (100k+ msg/sec) | AsyncProducer | Batching and pipelining |
| Must not lose messages | SyncProducer | Explicit error per message |
| Can tolerate occasional loss | AsyncProducer | Fire-and-forget with background drain |

**This project uses SyncProducer** — scraping every 15s per node is low volume,
and knowing immediately if a message failed is simpler to reason about.

---

## Acknowledgment Levels (`acks`)

When the broker "confirms", how many brokers must acknowledge?
Configured via `config.Producer.RequiredAcks`:

| Setting | Meaning | Risk |
|---|---|---|
| `NoResponse` | Don't wait for any ack | Can lose messages |
| `WaitForLocal` (default) | Leader partition confirms | Safe for single-broker setups |
| `WaitForAll` | All replicas confirm | Safest, slower (multi-broker clusters) |

For this project (single Kafka broker in docker-compose), `WaitForLocal` is fine.

---

## sarama Config — Required Settings for SyncProducer

```go
config := sarama.NewConfig()

// REQUIRED for SyncProducer — without these, SendMessage returns an error
config.Producer.Return.Successes = true
config.Producer.Return.Errors = true  // true by default, but explicit is clear
```

---

## Message Structure in sarama

```go
msg := &sarama.ProducerMessage{
    Topic: "vm-metrics-ts",
    Key:   sarama.StringEncoder("rancher-desktop"),  // partition key = vm_id
    Value: sarama.ByteEncoder(jsonBytes),             // message payload
}
partition, offset, err := producer.SendMessage(msg)
```

- `Key` determines which partition the message lands on
- `Value` is the raw bytes of your message (JSON in this project)
- `partition` and `offset` identify exactly where the message was stored — useful for debugging

---

## Consumer Groups

A **consumer group** is a set of consumers that coordinate to read a topic together.
Kafka divides partitions among group members — each partition is read by exactly one
consumer in the group at a time.

```
Topic: vm-metrics-ts (3 partitions)
Consumer group: vm-metrics-consumer-group

consumer-1 reads: partition 0
consumer-2 reads: partition 1
consumer-3 reads: partition 2
```

If you scale to 2 consumer instances, Kafka **rebalances** automatically — no config change.
This is why `docker-compose up --scale consumer=3` works out of the box.

The group ID (`KAFKA_GROUP_ID: vm-metrics-consumer-group` in docker-compose) is what
ties consumers together into a group. Each group tracks its own read position (offset)
independently — multiple groups can read the same topic without interfering.

---

## Offset — Where Am I in the Log?

An **offset** is just a sequential number for each message within a partition.

```
Partition 0: [msg(offset=0), msg(offset=1), msg(offset=2), ...]
```

The consumer group tracks which offset it has processed. "Committing an offset" means
telling Kafka "I've processed up to here — if I restart, start from the next one."

In this project the consumer commits the offset **after** flushing the 1-hour window —
so if it crashes mid-window, it re-reads and recomputes rather than losing data.

---

## 1-Hour Tumbling Window — Interview Question

**Question asked:** "What is a good way to capture 1 hour of data from the topic?"

### What the question is really asking

Kafka already **captures** the data — it retains messages for 7 days (configured via
`KAFKA_LOG_RETENTION_HOURS`). The real question is: **how do you aggregate 1 hour of
messages into a single summary record?**

### Tumbling window — the core answer

A tumbling window is a fixed, non-overlapping time bucket:

```
│── 00:00 ──────────────── 01:00 ──── flush ──── 02:00 ──── flush ──│
│   accumulate all points            write 1 row             write 1 row
│   in memory per vm_id              avg/min/max/p95
```

- "Tumbling" = windows don't overlap — when 01:00 hits, the 00:00–01:00 bucket closes
  and a fresh 01:00–02:00 bucket opens
- Contrast with "sliding window" where the window moves continuously

In-memory implementation in the consumer:
```go
// per vm_id, accumulate readings during the window
windows map[string][]float64  // vm_id → []cpu_pct readings

// on each Kafka message:
windows[metric.VmID] = append(windows[metric.VmID], metric.CPUPct)

// at the hour boundary:
avg, min, max, p95 := compute(windows[metric.VmID])
writer.WriteHourlySummary(vmID, avg, min, max, p95)
windows[metric.VmID] = nil   // reset for next window
```

### Where Redis fits

Redis is a valid answer to the **follow-up** question: "what if the consumer crashes
mid-window?"

```
Without Redis:  crash at 00:45 → lose 45 min of accumulated data → no summary written
With Redis:     crash at 00:45 → restart → reload window state from Redis → continue
```

Redis is about **fault tolerance of the window state**, not about capturing the data.
Introducing Redis in the original question jumps ahead to a production concern before
answering the core algorithm.

### Kafka Streams — the industry-standard tool

If the interviewer wants a framework answer: **Kafka Streams** has built-in windowing
(`TimeWindows.ofSizeWithNoGrace(Duration.ofHours(1))`). It handles state, fault
tolerance, and rebalancing automatically. We use plain `sarama` in this project and
implement windowing manually — which is fine for a demo and shows deeper understanding.

### Strongest interview answer

1. Lead with tumbling window concept — accumulate in memory, flush at hour boundary
2. Mention Kafka Streams as the production-grade tool
3. Add Redis as the fault tolerance layer for window state if crashes are a concern

### Why not just let InfluxDB or Grafana compute the average?

Both InfluxDB (via Flux queries) and Grafana (via panel aggregation) can compute
averages on the fly — so why pre-compute in the consumer?

**Three reasons:**

**1. Scale** — raw data grows fast:
```
100 VMs × 4 points/min × 60min × 24h × 30 days = 17M points
Query-time avg must scan all 17M points every dashboard load.
Pre-computed: 100 VMs × 24h × 30 days = 72K summary rows — always fast.
```

**2. p95 cannot be recomputed after raw data is deleted**
p95 requires the full list of raw values at computation time. Once raw points
expire (7-day retention), the p95 for that hour is gone unless you saved it.
Avg and sum can be approximated later; percentiles cannot.

**3. Retention strategy — downsampling**
```
vm_metrics        (raw, every 15s)  → keep 7 days  → then delete
vm_metrics_hourly (summary, 1/hour) → keep forever

Day 1–7:   both raw and hourly exist
Day 8+:    only hourly survives

Query "last 6 hours"  → use raw points     (granular, real-time)
Query "last 6 months" → use hourly summary (raw is gone)
```

This is the standard **downsampling** pattern used by Datadog, Prometheus (recording
rules), and every production metrics pipeline at scale. The interviewer was testing
whether you know this pattern — not whether you know how to compute an average.
