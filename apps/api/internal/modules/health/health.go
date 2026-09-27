// Package health serves GET /healthz — liveness + datastore probes.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Handler reports API + datastore health.
type Handler struct {
	pool    *pgxpool.Pool
	rdb     *redis.Client
	started time.Time
}

func New(pool *pgxpool.Pool, rdb *redis.Client) *Handler {
	return &Handler{pool: pool, rdb: rdb, started: time.Now()}
}

type check struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

type payload struct {
	Status    string `json:"status"`
	UptimeSec int64  `json:"uptime_s"`
	Postgres  check  `json:"postgres"`
	Redis     check  `json:"redis"`
}

// ServeHTTP implements http.Handler for GET /healthz.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	p := payload{Status: "ok", UptimeSec: int64(time.Since(h.started).Seconds())}

	t := time.Now()
	if err := h.pool.Ping(ctx); err != nil {
		p.Postgres = check{OK: false, Error: err.Error(), LatencyMS: time.Since(t).Milliseconds()}
		p.Status = "degraded"
	} else {
		p.Postgres = check{OK: true, LatencyMS: time.Since(t).Milliseconds()}
	}

	t = time.Now()
	if err := h.rdb.Ping(ctx).Err(); err != nil {
		p.Redis = check{OK: false, Error: err.Error(), LatencyMS: time.Since(t).Milliseconds()}
		p.Status = "degraded"
	} else {
		p.Redis = check{OK: true, LatencyMS: time.Since(t).Milliseconds()}
	}

	code := http.StatusOK
	if p.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(p)
}
