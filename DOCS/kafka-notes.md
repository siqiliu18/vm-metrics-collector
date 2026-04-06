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
