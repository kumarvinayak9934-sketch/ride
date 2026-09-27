// Package middleware provides request logging, problem+json errors,
// IP rate limiting, JWT auth and Idempotency-Key replay (§5.4, §5.9).
package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
)

// ------------------------------------------------------------ problem -----

// Problem is RFC 7807 application/problem+json.
type Problem struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// WriteProblem writes an RFC7807 body.
func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

// ------------------------------------------------------------ logging -----

// statusRecorder captures the response status/bytes for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Logging emits one structured line per request with latency.
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic",
						slog.Any("panic", rec),
						slog.String("stack", string(debug.Stack())),
						slog.String("path", r.URL.Path))
					WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "unexpected error")
				}
				st := rec.status
				if st == 0 {
					st = http.StatusOK
				}
				log.Info("http",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", st),
					slog.Int("bytes", rec.bytes),
					slog.Duration("dur", time.Since(start)),
					slog.String("req_id", RequestID(r.Context())))
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// ------------------------------------------------------- request id -------

type ctxKey int

const reqIDKey ctxKey = iota

// RequestID returns the inbound X-Request-ID or a generated one.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(reqIDKey).(string); ok {
		return v
	}
	return ""
}

// RequestIDMiddleware propagates X-Request-ID.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqIDKey, id)))
	})
}

// -------------------------------------------------------- rate limit ------

// RateLimit applies a Redis fixed window per client IP (§5.9: 600/min default).
// Fails open when Redis errors so an outage doesn't take the API down, but
// logs the failure.
func RateLimit(c *cache.Cache, perMin int, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r)
			count, allowed, err := c.FixedWindow(r.Context(), "ip:"+ip, time.Minute, perMin)
			if err != nil {
				log.Warn("rate limit unavailable, failing open", slog.String("err", err.Error()))
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", perMin))
			w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", max(0, perMin-count)))
			if !allowed {
				WriteProblem(w, http.StatusTooManyRequests, "Too Many Requests",
					"rate limit exceeded, retry later")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP extracts the caller address, honouring X-Forwarded-For first hop.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

// ------------------------------------------------------ idempotency -------

// clientIP-scope helper: the user id when authenticated, "-" otherwise.
func middlewareUserID(r *http.Request) string {
	if id := UserID(r.Context()); id != "" {
		return id
	}
	return "-"
}

// bodyRecorder caches the response body so successful writes can be replayed.
type bodyRecorder struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (b *bodyRecorder) WriteHeader(code int) { b.status = code; b.ResponseWriter.WriteHeader(code) }
func (b *bodyRecorder) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.buf.Write(p)
	return b.ResponseWriter.Write(p)
}

// Idempotency enforces the §5.4 contract for mutating endpoints:
//   - requires Idempotency-Key (400 when missing)
//   - replays "status\nbody" for 24 h on identical keys (scoped per user)
//   - stores only successful (2xx) responses
//
// scope namespaces the key (e.g. "booking").
func Idempotency(c *cache.Cache, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				WriteProblem(w, http.StatusBadRequest, "Missing Idempotency-Key",
					"mutating requests must carry an Idempotency-Key header")
				return
			}
			full := scope + ":" + middlewareUserID(r) + ":" + key
			if stored, ok, err := c.GetReplay(r.Context(), full); err == nil && ok {
				if i := strings.IndexByte(stored, '\n'); i > 0 {
					code := 0
					fmt.Sscanf(stored[:i], "%d", &code)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Idempotent-Replay", "true")
					w.WriteHeader(code)
					_, _ = w.Write([]byte(stored[i+1:]))
					return
				}
			}
			rec := &bodyRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status >= 200 && rec.status < 300 && rec.buf.Len() > 0 {
				stored := fmt.Sprintf("%d\n%s", rec.status, rec.buf.String())
				if err := c.PutReplay(r.Context(), full, stored); err != nil {
					// non-fatal: the DB unique constraint still dedupes
					_ = err
				}
			}
		})
	}
}
