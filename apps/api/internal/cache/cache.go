// Package cache is the thin Redis layer: seat locks, OTP storage, rate-limit
// windows, idempotency replay and JWT denylist. All operations take timeouts
// and return typed errors so handlers can map them to problem+json.
package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrLocked = errors.New("cache: seat locked")

type Cache struct {
	rdb *redis.Client
}

// New builds a client; Ping verifies connectivity.
func New(addr, password string) (*Cache, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: password})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("cache: ping %s: %w", addr, err)
	}
	return &Cache{rdb: rdb}, nil
}

// NewFromClient is used by tests (miniredis).
func NewFromClient(rdb *redis.Client) *Cache { return &Cache{rdb: rdb} }

func (c *Cache) Close() error { return c.rdb.Close() }

// Ping /healthz probe.
func (c *Cache) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// ---------------------------------------------------------------- OTP ----

// PutOTP stores the code with the §5.1 TTL (600 s).
func (c *Cache) PutOTP(ctx context.Context, phoneHash, code string) error {
	return c.rdb.Set(ctx, "otp:"+phoneHash, code, 600*time.Second).Err()
}

// TakeOTP returns and deletes the code (single-use). ok=false when expired.
func (c *Cache) TakeOTP(ctx context.Context, phoneHash string) (string, bool, error) {
	key := "otp:" + phoneHash
	v, err := c.rdb.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ---------------------------------------------------------- rate limits ---

// FixedWindow INCRs key for the current window; returns the count after
// increment and the window TTL. Windows are aligned to `every` durations.
func (c *Cache) FixedWindow(ctx context.Context, key string, every time.Duration, limit int) (count int, allowed bool, err error) {
	now := time.Now().UnixNano()
	window := now / int64(every)
	k := fmt.Sprintf("rl:%s:%d", key, window)
	pipe := c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, every*2)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, false, err
	}
	count = int(incr.Val())
	return count, count <= limit, nil
}

// --------------------------------------------------------- seat locks -----

// AcquireSeats tries to SET NX each seat key for the ride. It releases any
// keys it did acquire when a later one fails (all-or-nothing).
// TTL = 30 s per spec (booking think-time + payment redirect).
func (c *Cache) AcquireSeats(ctx context.Context, rideID string, seatIdx []int) error {
	acquired := make([]string, 0, len(seatIdx))
	for _, i := range seatIdx {
		key := fmt.Sprintf("seatlock:%s:%d", rideID, i)
		ok, err := c.rdb.SetNX(ctx, key, "1", 30*time.Second).Result()
		if err != nil {
			c.release(ctx, acquired)
			return err
		}
		if !ok {
			c.release(ctx, acquired)
			return fmt.Errorf("%w: seat %d", ErrLocked, i)
		}
		acquired = append(acquired, key)
	}
	return nil
}

// ReleaseSeats drops locks after the tx commits (best-effort; TTL is the backstop).
func (c *Cache) ReleaseSeats(ctx context.Context, rideID string, seatIdx []int) {
	keys := make([]string, 0, len(seatIdx))
	for _, i := range seatIdx {
		keys = append(keys, fmt.Sprintf("seatlock:%s:%d", rideID, i))
	}
	c.release(ctx, keys)
}

func (c *Cache) release(ctx context.Context, keys []string) {
	if len(keys) == 0 {
		return
	}
	_ = c.rdb.Del(ctx, keys...).Err()
}

// --------------------------------------------------- idempotency replay ---

// GetReplay returns "status\nbody" stored under the key within 24 h.
func (c *Cache) GetReplay(ctx context.Context, key string) (string, bool, error) {
	v, err := c.rdb.Get(ctx, "idem:"+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return v, err == nil, err
}

// PutReplay stores a replayable response for 24 h.
func (c *Cache) PutReplay(ctx context.Context, key, statusBody string) error {
	return c.rdb.Set(ctx, "idem:"+key, statusBody, 24*time.Hour).Err()
}

// ---------------------------------------------------------- JWT block ----

// BlockJTI denylists a token id until it would have expired anyway.
func (c *Cache) BlockJTI(ctx context.Context, jti string, ttl time.Duration) error {
	return c.rdb.Set(ctx, "jti:"+jti, "1", ttl).Err()
}

// IsBlocked reports whether a jti was revoked (logout).
func (c *Cache) IsBlocked(ctx context.Context, jti string) (bool, error) {
	n, err := c.rdb.Exists(ctx, "jti:"+jti).Result()
	return n > 0, err
}

// SetNX generic helper used by outbox single-flight etc.
func (c *Cache) SetNX(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, key, val, ttl).Result()
}

// Store saves an opaque payload (PKCE state etc.) with a TTL.
func (c *Cache) Store(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, val, ttl).Err()
}

// TakeState returns and deletes a stored payload (single-use state).
func (c *Cache) TakeState(ctx context.Context, key string) ([]byte, error) {
	v, err := c.rdb.GetDel(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return v, err
}

// IncrWithTTL implements "INCR + expire on first hit" for custom counters.
func (c *Cache) IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	n, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		if err := c.rdb.Expire(ctx, key, ttl).Err(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Atoi is a tiny parse helper for cached counters.
func Atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
