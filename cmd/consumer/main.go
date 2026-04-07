package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	// hint: github.com/IBM/sarama — same library as the producer
	// hint: sarama.NewConsumerGroup(brokers, groupID, config)

	"github.com/IBM/sarama"
	"github.com/siqiliu/vm-metrics-collector/internal/influx"
	"github.com/siqiliu/vm-metrics-collector/internal/kafka"
)

func main() {
	// 1. Read env vars
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")   // "kafka:9092"
	kafkaTopic := os.Getenv("KAFKA_TOPIC")       // "vm-metrics-ts"
	groupID := os.Getenv("KAFKA_GROUP_ID")       // "vm-metrics-consumer-group"
	influxURL := os.Getenv("INFLUXDB_URL")       // "http://influxdb:8086"
	influxToken := os.Getenv("INFLUXDB_TOKEN")   // "my-super-secret-token"
	influxOrg := os.Getenv("INFLUXDB_ORG")       // "vm-metrics"
	influxBucket := os.Getenv("INFLUXDB_BUCKET") // "metrics"

	// 2. Create InfluxDB writer
	writer := influx.NewWriter(influxURL, influxToken, influxOrg, influxBucket)
	defer writer.Close()

	// 3. Create sarama consumer group config
	// hint: config := sarama.NewConfig()
	// hint: config.Consumer.Offsets.Initial = sarama.OffsetNewest
	//       OffsetNewest = only consume messages produced after this consumer starts
	//       OffsetOldest = replay all messages from the beginning (useful for testing)
	config := sarama.NewConfig()
	config.Consumer.Offsets.Initial = sarama.OffsetNewest

	// 4. Create consumer group
	// hint: sarama.NewConsumerGroup(strings.Split(kafkaBrokers, ","), groupID, config)
	consumerGroup, _ := sarama.NewConsumerGroup(strings.Split(kafkaBrokers, ","), groupID, config)

	// 5. Set up OS signal handler for graceful shutdown
	// When you press Ctrl+C, the OS sends SIGINT. Without catching it, the process
	// exits immediately — buffered InfluxDB writes would be lost.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("starting consumer: brokers=%s topic=%s group=%s", kafkaBrokers, kafkaTopic, groupID)

	// 6. Consume loop runs in a goroutine so main() can also wait on the signal.
	// Consume() returns after each rebalance (e.g. new consumer joins the group),
	// so it must be called in a loop — each call handles one "session".
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if err := consumerGroup.Consume(ctx, []string{kafkaTopic}, &handler{writer: writer}); err != nil {
				log.Printf("consume error: %v", err)
				return
			}
			if ctx.Err() != nil {
				return // context cancelled — shutting down
			}
		}
	}()

	// 7. Block until signal received, then shut down cleanly
	<-sigChan
	log.Println("shutting down consumer")
	cancel()
	consumerGroup.Close()
}

// handler implements sarama.ConsumerGroupHandler
// sarama requires three methods:
//   - Setup()       called once when the consumer joins the group
//   - Cleanup()     called once when the consumer leaves the group
//   - ConsumeClaim() called once per partition — your main message loop lives here
type handler struct {
	writer *influx.Writer
}

// Setup is called before ConsumeClaim — nothing needed here for now
func (h *handler) Setup(_ sarama.ConsumerGroupSession) error { return nil }

// Cleanup is called after ConsumeClaim — nothing needed here for now
func (h *handler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim is called once per partition this consumer is assigned.
// It must loop over claim.Messages() until the channel is closed.
func (h *handler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		// 1. Decode JSON message into NodeMetric
		// hint: json.Unmarshal(msg.Value, &metric)
		var metric kafka.NodeMetric
		if err := json.Unmarshal(msg.Value, &metric); err != nil {
			log.Printf("failed to decode message: %v", err)
			// hint: still mark message as processed so we don't get stuck
			session.MarkMessage(msg, "")
			continue
		}

		// 2. Write to InfluxDB
		h.writer.Write(metric)

		// 3. Mark message as processed — tells Kafka to advance the offset
		// This is the "commit" — if the consumer crashes before this line,
		// Kafka will re-deliver the message on restart (at-least-once delivery)
		session.MarkMessage(msg, "")

		log.Printf("wrote metric: vm_id=%s hostname=%s cpu=%.1f%% mem=%.1f%%",
			metric.VmID, metric.Hostname, metric.CPUPct, metric.MemPct)
	}
	return nil
}
