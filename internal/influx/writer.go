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
