package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
)

func testCache(t *testing.T) *cache.Cache {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return cache.NewFromClient(rdb)
}

func TestRateLimitAllowsUpToLimit(t *testing.T) {
	c := testCache(t)
	h := RateLimit(c, 3, discardLog())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d: status %d, want 200", i+1, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request: status %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "problem") &&
		rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("429 body must be problem+json, got %q (%s)",
			rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

func TestRateLimitIsolatesClients(t *testing.T) {
	c := testCache(t)
	h := RateLimit(c, 1, discardLog())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	one := httptest.NewRequest(http.MethodGet, "/x", nil)
	one.RemoteAddr = "10.0.0.1:1"
	h.ServeHTTP(httptest.NewRecorder(), one)
	two := httptest.NewRequest(http.MethodGet, "/x", nil)
	two.RemoteAddr = "10.0.0.2:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, two)
	if rec.Code != http.StatusOK {
		t.Fatalf("different IP blocked: %d", rec.Code)
	}
}

func TestIdempotencyRequiresKey(t *testing.T) {
	c := testCache(t)
	h := Idempotency(c, "booking")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing key: %d, want 400", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("want problem+json, got %s", rec.Header().Get("Content-Type"))
	}
}

// The §5.4 replay contract: same key => byte-identical replay within 24 h.
func TestIdempotencyReplaysSuccess(t *testing.T) {
	c := testCache(t)
	calls := 0
	h := Idempotency(c, "booking")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"bk-1"}`))
	}))
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/bookings", nil)
		req.Header.Set("Idempotency-Key", "key-abc")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	r1 := do()
	if r1.Code != http.StatusCreated || calls != 1 {
		t.Fatalf("first: %d calls=%d", r1.Code, calls)
	}
	r2 := do()
	if calls != 1 {
		t.Fatalf("handler ran twice for same key (calls=%d)", calls)
	}
	if r2.Code != http.StatusCreated || r2.Body.String() != `{"id":"bk-1"}` {
		t.Fatalf("replay mismatch: %d %q", r2.Code, r2.Body.String())
	}
	if r2.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("replay must be flagged")
	}
	// different key hits the handler again
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", nil)
	req.Header.Set("Idempotency-Key", "key-xyz")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if calls != 2 {
		t.Fatalf("new key must run handler (calls=%d)", calls)
	}
}

// Failed responses are NOT cached (client may retry the same key).
func TestIdempotencyDoesNotReplay5xx(t *testing.T) {
	c := testCache(t)
	calls := 0
	h := Idempotency(c, "booking")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"bk-2"}`))
	}))
	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("Idempotency-Key", "k1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do(); got != http.StatusServiceUnavailable {
		t.Fatalf("first = %d", got)
	}
	if got := do(); got != http.StatusCreated {
		t.Fatalf("retry after 5xx must re-run handler, got %d", got)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	if ClientIP(req) != "1.2.3.4" {
		t.Fatalf("xff = %q", ClientIP(req))
	}
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "192.168.1.9:5555"
	if ClientIP(req2) != "192.168.1.9" {
		t.Fatalf("remote = %q", ClientIP(req2))
	}
}

func TestWriteProblemShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, http.StatusConflict, "Not Enough Seats", "2 of 4 booked")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type %s", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":409`, `"title":"Not Enough Seats"`, `"detail":"2 of 4 booked"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestRequestIDPropagation(t *testing.T) {
	var seen string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-42")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "req-42" {
		t.Fatalf("ctx id = %q", seen)
	}
	if rec.Header().Get("X-Request-ID") != "req-42" {
		t.Fatalf("response header = %q", rec.Header().Get("X-Request-ID"))
	}
}

func TestCORS(t *testing.T) {
	h := CORS([]string{"http://localhost:3000"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// allowed origin
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("allow-origin = %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	// unknown origin gets nothing
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Origin", "http://evil.example")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("evil origin must not be allowed")
	}
	// preflight short-circuits
	req3 := httptest.NewRequest(http.MethodOptions, "/", nil)
	req3.Header.Set("Origin", "http://localhost:3000")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d", rec3.Code)
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1,"zz":2}`))
	rec := httptest.NewRecorder()
	var dst struct {
		A int `json:"a"`
	}
	if DecodeJSON(rec, req, &dst) {
		t.Fatal("unknown field must fail")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

// context import guard
var _ = context.Background
var _ = time.Second
