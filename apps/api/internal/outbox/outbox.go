// Package outbox implements §5.8: a transactional outbox with
// FOR UPDATE SKIP LOCKED claiming, exponential backoff + jitter, and a DLQ
// after 5 failed attempts.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// MaxAttempts before a row is parked in the DLQ (spec: 5).
	MaxAttempts = 5
	// BatchSize claimed per poll.
	BatchSize = 20
)

// Event is an outbox row.
type Event struct {
	ID       uuid.UUID       `json:"id"`
	Topic    string          `json:"topic"`
	Payload  json.RawMessage `json:"payload"`
	Attempts int             `json:"attempts"`
}

// Publisher delivers a claimed event; failure triggers backoff.
// The MVP logs + optionally POSTs to a webhook sink (configurable later).
type Publisher func(ctx context.Context, ev Event) error

// Store persists outbox rows.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Enqueue MUST be called inside the business transaction so events and
// writes commit atomically (the whole point of the pattern).
func (s *Store) Enqueue(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox marshal: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO outbox (topic, payload) VALUES ($1, $2)`, topic, b)
	return err
}

// Poller runs the background loop.
type Poller struct {
	store *Store
	pub   Publisher
	log   *slog.Logger
	every time.Duration
	stop  chan struct{}
	done  chan struct{}
}

func NewPoller(s *Store, pub Publisher, log *slog.Logger) *Poller {
	return &Poller{store: s, pub: pub, log: log, every: 2 * time.Second,
		stop: make(chan struct{}), done: make(chan struct{})}
}

// Run starts the loop (call in a goroutine).
func (p *Poller) Run() {
	defer close(p.done)
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if n, err := p.PollOnce(ctx); err != nil {
				p.log.Warn("outbox poll error", slog.String("err", err.Error()))
			} else if n > 0 {
				p.log.Info("outbox processed", slog.Int("n", n))
			}
			cancel()
		}
	}
}

// Stop signals the loop to exit and waits.
func (p *Poller) Stop() {
	close(p.stop)
	<-p.done
}

// backoff: 2^attempts seconds, capped at 30 s, ±20% jitter (spec §5.8).
func backoff(attempts int) time.Duration {
	d := time.Duration(1<<uint(min(attempts, 5))) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(d)/5+1)) - d/10 // ±10..20%
	return d + jitter
}

// PollOnce claims and dispatches one batch. Returns processed count.
func (p *Poller) PollOnce(ctx context.Context) (int, error) {
	tx, err := p.store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, topic, payload, attempts
		FROM outbox
		WHERE status = 'PENDING' AND next_attempt_at <= now()
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, BatchSize)
	if err != nil {
		return 0, err
	}
	var claimed []Event
	for rows.Next() {
		var ev Event
		var payload []byte
		if err := rows.Scan(&ev.ID, &ev.Topic, &payload, &ev.Attempts); err != nil {
			rows.Close()
			return 0, err
		}
		ev.Payload = payload
		claimed = append(claimed, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(claimed) == 0 {
		return 0, tx.Commit(ctx)
	}

	processed := 0
	for _, ev := range claimed {
		ev.Attempts++
		pubErr := p.pub(ctx, ev)
		if pubErr == nil {
			_, err = tx.Exec(ctx,
				`UPDATE outbox SET status='SENT', attempts=$2, last_error='' WHERE id=$1`,
				ev.ID, ev.Attempts)
			if err != nil {
				return processed, err
			}
			processed++
			continue
		}
		if ev.Attempts >= MaxAttempts {
			_, err = tx.Exec(ctx,
				`UPDATE outbox SET status='DLQ', attempts=$2, last_error=$3 WHERE id=$1`,
				ev.ID, ev.Attempts, pubErr.Error())
			p.log.Error("outbox DLQ",
				slog.String("id", ev.ID.String()),
				slog.String("topic", ev.Topic),
				slog.Int("attempts", ev.Attempts),
				slog.String("err", pubErr.Error()))
		} else {
			_, err = tx.Exec(ctx,
				`UPDATE outbox SET attempts=$2, next_attempt_at=now() + $3::interval, last_error=$4 WHERE id=$1`,
				ev.ID, ev.Attempts, backoff(ev.Attempts).String(), pubErr.Error())
		}
		if err != nil {
			return processed, err
		}
	}
	return processed, tx.Commit(ctx)
}

// DLQCount exposes DLQ depth for /healthz metrics.
func (s *Store) DLQCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE status='DLQ'`).Scan(&n)
	return n, err
}
