package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/siqiliu/vm-metrics-collector/internal/influx"
	"github.com/siqiliu/vm-metrics-collector/internal/kafka"
)

// windows accumulates readings per vm_id during the current hour.
// separate maps for CPU and memory so we can compute stats independently.
// mu protects both maps — the message loop (ConsumeClaim) and the flush
// goroutine (startWindowFlusher) both access them concurrently.
var (
	cpuWindows = map[string][]float64{}
	memWindows = map[string][]float64{}
	mu         sync.Mutex
)

func main() {
	// 1. Read env vars
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")   // "kafka:9092"
	kafkaTopic   := os.Getenv("KAFKA_TOPIC")     // "vm-metrics-ts"
	groupID      := os.Getenv("KAFKA_GROUP_ID")  // "vm-metrics-consumer-group"
	influxURL    := os.Getenv("INFLUXDB_URL")    // "http://influxdb:8086"
	influxToken  := os.Getenv("INFLUXDB_TOKEN")  // "my-super-secret-token"
	influxOrg    := os.Getenv("INFLUXDB_ORG")    // "vm-metrics"
	influxBucket := os.Getenv("INFLUXDB_BUCKET") // "metrics"

	// 2. Create InfluxDB writer
	writer := influx.NewWriter(influxURL, influxToken, influxOrg, influxBucket)
	defer writer.Close()

	// 3. Create sarama consumer group config
	config := sarama.NewConfig()
	config.Consumer.Offsets.Initial = sarama.OffsetNewest

	// 4. Create consumer group
	consumerGroup, err := sarama.NewConsumerGroup(strings.Split(kafkaBrokers, ","), groupID, config)
	if err != nil {
		log.Fatalf("failed to create consumer group: %v", err)
	}

	// 5. Set up OS signal handler for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("starting consumer: brokers=%s topic=%s group=%s", kafkaBrokers, kafkaTopic, groupID)

	ctx, cancel := context.WithCancel(context.Background())

	// 6. Window flusher — runs every minute, flushes on hour boundary
	go startWindowFlusher(ctx, writer)

	// 7. Consume loop — runs in goroutine so main() can wait on signal
	go func() {
		for {
			if err := consumerGroup.Consume(ctx, []string{kafkaTopic}, &handler{writer: writer}); err != nil {
				log.Printf("consume error: %v", err)
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()

	// 8. Block until signal, then shut down cleanly
	<-sigChan
	log.Println("shutting down consumer")
	cancel()
	consumerGroup.Close()
}

// startWindowFlusher ticks every minute and flushes window data on each hour boundary.
func startWindowFlusher(ctx context.Context, writer *influx.Writer) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	currentHour := time.Now().Hour()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Now().Hour() != currentHour {
				flushWindows(writer)
				currentHour = time.Now().Hour()
			}
		}
	}
}

// flushWindows computes avg/min/max/p95 for each vm_id and writes hourly
// summaries to InfluxDB, then resets the accumulators for the next hour.
func flushWindows(writer *influx.Writer) {
	mu.Lock()
	// snapshot and reset atomically so the message loop can keep appending
	cpu := cpuWindows
	mem := memWindows
	cpuWindows = map[string][]float64{}
	memWindows = map[string][]float64{}
	mu.Unlock()

	for vmID, cpuVals := range cpu {
		memVals := mem[vmID]
		if len(cpuVals) == 0 {
			continue
		}
		writer.WriteHourlySummary(vmID, stats(cpuVals), stats(memVals))
		log.Printf("[hourly flush] vm_id=%s points=%d", vmID, len(cpuVals))
	}
}

// stats computes avg, min, max, p95 from a slice of float64 values.
func stats(vals []float64) influx.Stats {
	if len(vals) == 0 {
		return influx.Stats{}
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)

	sum := 0.0
	for _, v := range sorted {
		sum += v
	}

	p95idx := int(math.Ceil(float64(len(sorted))*0.95)) - 1
	if p95idx < 0 {
		p95idx = 0
	}

	return influx.Stats{
		Avg: sum / float64(len(sorted)),
		Min: sorted[0],
		Max: sorted[len(sorted)-1],
		P95: sorted[p95idx],
	}
}

// handler implements sarama.ConsumerGroupHandler
type handler struct {
	writer *influx.Writer
}

func (h *handler) Setup(_ sarama.ConsumerGroupSession) error   { return nil }
func (h *handler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h *handler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		var metric kafka.NodeMetric
		if err := json.Unmarshal(msg.Value, &metric); err != nil {
			log.Printf("failed to decode message: %v", err)
			session.MarkMessage(msg, "")
			continue
		}

		// write raw point to InfluxDB immediately (real-time path)
		h.writer.Write(metric)

		// accumulate in window for hourly summary
		mu.Lock()
		cpuWindows[metric.VmID] = append(cpuWindows[metric.VmID], metric.CPUPct)
		memWindows[metric.VmID] = append(memWindows[metric.VmID], metric.MemPct)
		mu.Unlock()

		session.MarkMessage(msg, "")

		log.Printf("wrote metric: vm_id=%s hostname=%s cpu=%.1f%% mem=%.1f%%",
			metric.VmID, metric.Hostname, metric.CPUPct, metric.MemPct)
	}
	return nil
}
