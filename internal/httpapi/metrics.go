package httpapi

import (
	"context"
	"time"

	"example.com/bca/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

func metrics(db *pgxpool.Pool) (*prometheus.Registry, *prometheus.HistogramVec) {
	registry := prometheus.NewRegistry()
	gauge := func(name, help, query string, labels prometheus.Labels) {
		registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help, ConstLabels: labels}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var value float64
			if db.QueryRow(ctx, query).Scan(&value) != nil {
				return 0
			}
			return value
		}))
	}
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "bca_jobs_pending", Help: "Jobs waiting for completion."}, func() float64 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n, err := database.New(db).CountPendingJobs(ctx)
		if err != nil {
			return 0
		}
		return float64(n)
	}))
	gauge("bca_jobs_oldest_age_seconds", "Age of the oldest incomplete job.", "SELECT coalesce(extract(epoch FROM now()-min(created_at)),0)::float8 FROM bca_email_jobs WHERE state IN ('queued','processing','retry_wait')", nil)
	gauge("bca_sync_recent_success", "Integrations with discovery success in the last five minutes.", "SELECT count(*)::float8 FROM gmail_sync_state WHERE last_success>now()-interval '5 minutes'", nil)
	gauge("bca_integrations_reconnect_required", "Integrations requiring Google reconnection.", "SELECT count(*)::float8 FROM gmail_integrations WHERE status='reconnect_required'", nil)
	for _, state := range []string{"completed", "ignored", "unsupported", "needs_review", "failed"} {
		// The state names are fixed constants, never user input.
		gauge("bca_jobs_state", "Jobs by current outcome.", "SELECT count(*)::float8 FROM bca_email_jobs WHERE state='"+state+"'", prometheus.Labels{"state": state})
	}
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bca_http_request_duration_seconds", Help: "HTTP request duration.", Buckets: prometheus.DefBuckets}, []string{"code", "method"})
	registry.MustRegister(latency)
	return registry, latency
}
