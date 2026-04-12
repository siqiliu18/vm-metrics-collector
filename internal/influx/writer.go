package influx

import (
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"

	"github.com/siqiliu/vm-metrics-collector/internal/kafka"
)

// Writer wraps an InfluxDB write client.
type Writer struct {
	// hint: two fields needed:
	//   - the top-level influxdb2 client (for closing)
	//   - a write API handle (for writing points)
	// hint: influxdb2.Client and api.WriteAPI
	client   influxdb2.Client
	writeApi api.WriteAPI
}

// NewWriter creates a connected InfluxDB writer.
// url    e.g. "http://influxdb:8086"
// token  e.g. "my-super-secret-token"
// org    e.g. "vm-metrics"
// bucket e.g. "metrics"
//
// hint: influxdb2.NewClient(url, token) → gives you influxdb2.Client
// hint: client.WriteAPI(org, bucket)    → gives you api.WriteAPI (non-blocking writes)
func NewWriter(url, token, org, bucket string) *Writer {
	c := influxdb2.NewClient(url, token)
	writer := Writer{
		client:   c,
		writeApi: c.WriteAPI(org, bucket),
	}
	return &writer
}

// Close flushes pending writes and shuts down the client.
// hint: w.writeAPI.Flush() then w.client.Close()
func (w *Writer) Close() {
	w.writeApi.Flush()
	w.client.Close()
}

// Stats holds the computed aggregates for one vm_id over a 1-hour window.
type Stats struct {
	Avg float64
	Min float64
	Max float64
	P95 float64
}

// WriteHourlySummary writes avg/min/max/p95 for one vm_id to a separate
// measurement so hourly summaries are queryable independently of raw points.
func (w *Writer) WriteHourlySummary(vmID string, cpu, mem Stats) {
	point := influxdb2.NewPointWithMeasurement("vm_metrics_hourly").
		AddTag("vm_id", vmID).
		AddField("cpu_avg", cpu.Avg).
		AddField("cpu_min", cpu.Min).
		AddField("cpu_max", cpu.Max).
		AddField("cpu_p95", cpu.P95).
		AddField("mem_avg", mem.Avg).
		AddField("mem_min", mem.Min).
		AddField("mem_max", mem.Max).
		AddField("mem_p95", mem.P95).
		SetTime(time.Now())
	w.writeApi.WritePoint(point)
}

// Write sends one NodeMetric to InfluxDB as a data point.
//
// InfluxDB data model:
//   - Measurement: the table name → "vm_metrics"
//   - Tags:   indexed string labels used for filtering → vm_id, hostname
//   - Fields: the actual numeric values → cpu_pct, mem_pct
//   - Time:   the timestamp (nanoseconds)
//
// hint: influxdb2.NewPointWithMeasurement("vm_metrics")
//
//	.AddTag("vm_id", metric.VmID)
//	.AddTag("hostname", metric.Hostname)
//	.AddField("cpu_pct", metric.CPUPct)
//	.AddField("mem_pct", metric.MemPct)
//	.SetTime(time.Unix(0, metric.Timestamp))   ← convert nanoseconds back to time.Time
//
// hint: w.writeAPI.WritePoint(point)
func (w *Writer) Write(metric kafka.NodeMetric) {
	data := influxdb2.NewPointWithMeasurement("vm_metrics").
		AddTag("vm_id", metric.VmID).
		AddTag("hostname", metric.Hostname).
		AddField("cpu_pct", metric.CPUPct).
		AddField("mem_pct", metric.MemPct).
		SetTime(time.Unix(0, metric.Timestamp))
	w.writeApi.WritePoint(data)
}
