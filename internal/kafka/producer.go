package kafka

import (
	"encoding/json"
	"strings"

	"github.com/IBM/sarama"
)

// Producer wraps a sarama sync producer.
// We use a sync producer so we know the message was accepted by Kafka
// before moving on (vs async where failures are silent).
type Producer struct {
	syncProducer sarama.SyncProducer
}

// NewProducer creates a connected Kafka producer.
// brokers is a comma-separated list e.g. "kafka:9092"
func NewProducer(brokers string) (*Producer, error) {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true

	sp, err := sarama.NewSyncProducer(strings.Split(brokers, ","), config)
	if err != nil {
		return nil, err
	}

	return &Producer{syncProducer: sp}, nil
}

// Close shuts down the producer cleanly.
func (p *Producer) Close() error {
	return p.syncProducer.Close()
}

// NodeMetric is the message schema sent to Kafka.
// Keep it flat JSON — easy for the consumer to decode.
type NodeMetric struct {
	VmID      string  `json:"vm_id"`     // context name — identifies the cluster
	Hostname  string  `json:"hostname"`  // node name
	Timestamp int64   `json:"timestamp"` // Unix nanoseconds
	CPUPct    float64 `json:"cpu_pct"`
	MemPct    float64 `json:"mem_pct"`
}

// Publish sends one NodeMetric to the Kafka topic.
// The partition key is vm_id — ensures all metrics for one VM go to the same partition.
func (p *Producer) Publish(topic string, metric NodeMetric) error {
	bytes, err := json.Marshal(metric)
	if err != nil {
		return err
	}

	msg := &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder(metric.VmID),
		Value: sarama.ByteEncoder(bytes),
	}

	_, _, err = p.syncProducer.SendMessage(msg)
	return err
}
