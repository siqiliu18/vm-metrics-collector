package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/siqiliu/vm-metrics-collector/internal/influx"
)

func main() {
	// 1. Read env vars
	influxURL := os.Getenv("INFLUXDB_URL")       // "http://influxdb:8086"
	influxToken := os.Getenv("INFLUXDB_TOKEN")   // "my-super-secret-token"
	influxOrg := os.Getenv("INFLUXDB_ORG")       // "vm-metrics"
	influxBucket := os.Getenv("INFLUXDB_BUCKET") // "metrics"
	port := os.Getenv("API_PORT")                // "8080"
	if port == "" {
		port = "8080"
	}

	// 2. Create InfluxDB querier
	querier := influx.NewQuerier(influxURL, influxToken, influxOrg, influxBucket)
	defer querier.Close()

	// 3. Set up chi router
	// middleware.Logger   — logs every request: method, path, status, duration
	// middleware.Recoverer — catches panics and returns 500 instead of crashing
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// 4. Register routes
	// hint: r.Get("/path", handlerFunc)
	// handler funcs receive (w http.ResponseWriter, r *http.Request)
	// use chi.URLParam(r, "vm_id") to extract URL parameters
	// use writeJSON(w, data) helper below to send JSON responses

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		// hint: just return {"status": "ok"}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Get("/vms", func(w http.ResponseWriter, r *http.Request) {
		vms, err := querier.ListVMs(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, vms)
	})

	r.Get("/metrics/{vm_id}", func(w http.ResponseWriter, r *http.Request) {
		vmID := chi.URLParam(r, "vm_id")
		startStr := r.URL.Query().Get("start")
		endStr := r.URL.Query().Get("end")
		start, _ := strconv.ParseInt(startStr, 10, 64)
		end, _ := strconv.ParseInt(endStr, 10, 64)
		metrics, err := querier.GetMetrics(r.Context(), vmID, start, end)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, metrics)
	})

	r.Get("/metrics/{vm_id}/summary", func(w http.ResponseWriter, r *http.Request) {
		vmID := chi.URLParam(r, "vm_id")
		summaries, err := querier.GetHourlySummary(r.Context(), vmID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, summaries)
	})

	// 5. Start server
	log.Printf("api listening on :%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// writeJSON encodes v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
