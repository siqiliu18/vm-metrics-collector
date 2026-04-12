package influx

import (
	"context"
	"fmt"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
)

// Querier reads data from InfluxDB using Flux queries.
type Querier struct {
	client   influxdb2.Client
	queryAPI api.QueryAPI
	org      string
	bucket   string
}

// NewQuerier creates a connected InfluxDB query client.
func NewQuerier(url, token, org, bucket string) *Querier {
	c := influxdb2.NewClient(url, token)
	return &Querier{
		client:   c,
		queryAPI: c.QueryAPI(org),
		org:      org,
		bucket:   bucket,
	}
}

// Close shuts down the client.
func (q *Querier) Close() {
	q.client.Close()
}

// MetricPoint is one raw data point returned by GetMetrics.
type MetricPoint struct {
	Time     time.Time
	VmID     string
	Hostname string
	CPUPct   float64
	MemPct   float64
}

// HourlySummary is one pre-aggregated row returned by GetHourlySummary.
type HourlySummary struct {
	Time   time.Time
	VmID   string
	CPUAvg float64
	CPUMin float64
	CPUMax float64
	CPUP95 float64
	MemAvg float64
	MemMin float64
	MemMax float64
	MemP95 float64
}

// ListVMs returns all unique vm_id values seen in InfluxDB.
// hint: query vm_metrics, group by vm_id tag, return distinct values
// hint: |> keep(columns: ["vm_id"])
//
//	|> distinct(column: "vm_id")
func (q *Querier) ListVMs(ctx context.Context) ([]string, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: -30d)
  |> filter(fn: (r) => r._measurement == "vm_metrics")
  |> keep(columns: ["vm_id"])
  |> distinct(column: "vm_id")
`, q.bucket)

	result, err := q.queryAPI.Query(ctx, flux)
	if err != nil {
		return nil, err
	}

	var vms []string
	for result.Next() {
		// hint: result.Record().ValueByKey("vm_id")
		if vmID, ok := result.Record().ValueByKey("vm_id").(string); ok {
			vms = append(vms, vmID)
		}
	}
	return vms, result.Err()
}

// GetMetrics returns raw metric points for a VM in a time range.
// start and end are Unix timestamps in seconds (0 = use defaults).
//
// hint: Flux range() accepts time values:
//
//	range(start: time(v: startUnix * 1000000000))
//
// hint: result.Record().Time() → time.Time
// hint: result.Record().ValueByKey("vm_id") → interface{}
// hint: result.Record().Value() → the field value (_value column)
// hint: result.Record().Field() → the field name (_field column)
//
// The query returns one row per field per timestamp — you need to
// pivot or accumulate rows to get cpu_pct and mem_pct in the same struct.
func (q *Querier) GetMetrics(ctx context.Context, vmID string, start, end int64) ([]MetricPoint, error) {
	// build time range — default to last 1 hour if not specified
	startExpr := "-1h"
	endExpr := "now()"
	if start > 0 {
		startExpr = fmt.Sprintf("time(v: %d)", start*int64(time.Second))
	}
	if end > 0 {
		endExpr = fmt.Sprintf("time(v: %d)", end*int64(time.Second))
	}

	// pivot brings cpu_pct and mem_pct into the same row
	// without pivot, each field is a separate row:
	//   row 1: _field=cpu_pct _value=9.8
	//   row 2: _field=mem_pct _value=76.2
	// with pivot, one row per timestamp:
	//   row 1: cpu_pct=9.8 mem_pct=76.2
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "vm_metrics")
  |> filter(fn: (r) => r.vm_id == "%s")
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
`, q.bucket, startExpr, endExpr, vmID)

	result, err := q.queryAPI.Query(ctx, flux)
	if err != nil {
		return nil, err
	}

	var points []MetricPoint
	for result.Next() {
		rec := result.Record()
		point := MetricPoint{
			Time:     rec.Time(),
			VmID:     vmID,
			Hostname: stringVal(rec.ValueByKey("hostname")),
			CPUPct:   floatVal(rec.ValueByKey("cpu_pct")),
			MemPct:   floatVal(rec.ValueByKey("mem_pct")),
		}
		points = append(points, point)
	}
	return points, result.Err()
}

// GetHourlySummary returns pre-aggregated hourly rows for a VM.
func (q *Querier) GetHourlySummary(ctx context.Context, vmID string) ([]HourlySummary, error) {
	flux := fmt.Sprintf(`
from(bucket: "%s")
  |> range(start: -30d)
  |> filter(fn: (r) => r._measurement == "vm_metrics_hourly")
  |> filter(fn: (r) => r.vm_id == "%s")
  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
`, q.bucket, vmID)

	result, err := q.queryAPI.Query(ctx, flux)
	if err != nil {
		return nil, err
	}

	var summaries []HourlySummary
	for result.Next() {
		rec := result.Record()
		summaries = append(summaries, HourlySummary{
			Time:   rec.Time(),
			VmID:   vmID,
			CPUAvg: floatVal(rec.ValueByKey("cpu_avg")),
			CPUMin: floatVal(rec.ValueByKey("cpu_min")),
			CPUMax: floatVal(rec.ValueByKey("cpu_max")),
			CPUP95: floatVal(rec.ValueByKey("cpu_p95")),
			MemAvg: floatVal(rec.ValueByKey("mem_avg")),
			MemMin: floatVal(rec.ValueByKey("mem_min")),
			MemMax: floatVal(rec.ValueByKey("mem_max")),
			MemP95: floatVal(rec.ValueByKey("mem_p95")),
		})
	}
	return summaries, result.Err()
}

// helpers to safely cast interface{} values from Flux result records
func floatVal(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func stringVal(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
