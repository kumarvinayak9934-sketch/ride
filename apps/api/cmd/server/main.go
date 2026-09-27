// Command server is the Safar Share modular-monolith API entrypoint (§3).
//
//	apps/api/cmd/server/main.go — wires config, datastores, modules, outbox.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/db"
	mw "github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/auth"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/booking"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/health"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/matching"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/payment"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/pricing"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/routes"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/verification"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/outbox"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config load failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	if err := pricing.ValidateConfig(cfg.Pricing); err != nil {
		log.Error("pricing config invalid", slog.String("err", err.Error()))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database unavailable", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()

	cch, err := cache.New(cfg.RedisAddr, cfg.RedisPass)
	if err != nil {
		log.Error("redis unavailable", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer cch.Close()

	// raw redis client for /healthz (shares config)
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPass})
	defer rdb.Close()

	engine := pricing.New(cfg.Pricing, log)
	obStore := outbox.NewStore(pool)
	pub := func(ctx context.Context, ev outbox.Event) error {
		// MVP publisher: structured log sink. Swap for a webhook/SQS worker
		// without touching the business modules (topic-driven).
		log.Info("outbox publish",
			slog.String("topic", ev.Topic),
			slog.String("id", ev.ID.String()),
			slog.Int("attempt", ev.Attempts),
			slog.String("payload", string(ev.Payload)))
		return nil
	}
	poller := outbox.NewPoller(obStore, pub, log)
	go poller.Run()
	defer poller.Stop()

	authSvc := auth.NewService(cfg, cch, auth.NewPGStore(pool), log)
	routeH := routes.NewHandler(pool, engine, log)
	searchH := matching.NewHandler(pool, engine, cfg.Match, log)
	bookH := booking.NewHandler(pool, cch, obStore, engine, log, cfg.WebhookSecret, cfg.UPILocation)
	payH := payment.NewHandler(pool, obStore, cfg.WebhookSecret, log)
	verifH := verification.NewHandler(pool, cch, cfg.DigiLocker, log)
	healthH := health.New(pool, rdb)

	r := chi.NewRouter()

	// ---- global middleware (§5.9) ----
	r.Use(mw.RequestIDMiddleware)
	r.Use(middleware.RealIP)
	r.Use(mw.Logging(log))
	r.Use(mw.CORS(cfg.AllowedOrigins))
	r.Use(mw.RateLimit(cch, cfg.RateLimitPerMin, log))

	// ---- public ----
	r.Get("/healthz", healthH.ServeHTTP)

	r.Route("/v1/auth", func(sr chi.Router) {
		sr.Post("/otp/request", authSvc.RequestOTP)
		sr.Post("/otp/verify", authSvc.VerifyOTP)
		sr.Post("/logout", authSvc.Logout) // logout needs no fresh auth: it
		//                                revokes whatever token is presented
	})

	// payment webhook: HMAC-authenticated, explicitly outside JWT middleware
	r.Route("/v1/payments", func(sr chi.Router) {
		sr.Post("/webhook", payH.Webhook)
	})

	// ---- authenticated ----
	r.Group(func(sr chi.Router) {
		sr.Use(func(next http.Handler) http.Handler {
			return mw.UserAuth(cfg.JWTSecret, cch)(next)
		})

		sr.Route("/v1/routes", routeH.Routes)
		sr.Post("/v1/rides/search", searchH.Handle)

		sr.Route("/v1/bookings", func(br chi.Router) {
			// §5.4: Idempotency-Key enforced at the edge + DB unique constraint
			br.Use(mw.Idempotency(cch, "booking"))
			br.Post("/", bookH.Create)
			br.Get("/", bookH.ListMine)
			br.Get("/{id}", bookH.Get)
			br.Post("/{id}/cancel", bookH.Cancel)
		})

		sr.Route("/v1/verify", verifH.Routes)
		sr.Get("/v1/payments/{id}", payH.Status)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("listening", slog.String("addr", cfg.HTTPAddr), slog.String("env", cfg.Env))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", slog.String("err", err.Error()))
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
