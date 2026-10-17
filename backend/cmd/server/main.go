package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/config"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/events"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/migrate"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/notify"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	// Cap per-instance pool so two backends + stress load stay under Postgres max_connections
	// (one connection per instance is held by the outbox LISTEN loop), and fail fast on lock waits.
	poolOpts := store.DefaultPoolOptions()
	poolOpts.MaxConns, poolOpts.LockTimeout, poolOpts.StatementTimeout = cfg.DBMaxConns, cfg.DBLockTimeout, cfg.DBStatementTimeout
	store.ConfigurePool(poolCfg, poolOpts)
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return err
	}
	// pool.Close blocks until every acquired connection is released, so shutdown skips it when
	// workers or requests are still running past the deadline (the process exits anyway).
	closePool := true
	defer func() {
		if closePool {
			pool.Close()
		}
	}()
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	if err := migrate.Up(ctx, pool); err != nil {
		return err
	}

	redisClient, err := cache.New(cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()
	if err := checkRedis(ctx, redisClient, cfg.RedisRequired, log); err != nil {
		return err
	}

	st := store.New(pool)
	defer st.Close() // readiness's own probe connection (outside the pool, +1 per instance)
	if cfg.Seed {
		// The demo users have fixed, published passwords (one of them a group-wide admin).
		log.Warn("SEED is on: demo accounts with published passwords are created; never enable it outside local demos")
		if err := seed.Run(ctx, st, log); err != nil {
			return err
		}
	}

	notifyCfg := notify.DefaultConfig()
	var sender notify.Sender = notify.LogSender{Log: log}
	if cfg.NotifyWebhookURL != "" {
		sender = notify.NewWebhookSender(cfg.NotifyWebhookURL, notifyCfg.BatchSize) // a batch sends at once
	}
	notifier := notify.New(st, sender, log, notifyCfg)

	hub := realtime.NewHub(log)
	hub.SetAllowedOrigins(cfg.AllowedOrigins)
	// Create the topic before any consumer joins (see EnsureTopic). If Kafka is not ready yet, keep
	// retrying in the background: the outbox holds events until it exists, and consumers rebalance
	// once its partitions appear.
	topicCtx, cancelTopic := context.WithTimeout(ctx, 10*time.Second)
	topicErr := events.EnsureTopic(topicCtx, cfg.KafkaBrokers, cfg.KafkaTopic, cfg.KafkaPartitions, cfg.KafkaReplication)
	cancelTopic()
	bus := events.NewBus(cfg.KafkaBrokers, cfg.KafkaTopic, log)
	defer func() { _ = bus.Close() }()
	svc := booking.New(st, cache.NewIdempotency(redisClient))
	api := httpapi.New(
		svc, st, auth.New(cfg.JWTSecret, auth.WithAccessTTL(cfg.AccessTokenTTL)), hub, log, cfg.InstanceID,
		httpapi.WithRateLimiter(cache.NewRateLimiter(redisClient)),
		httpapi.WithTicketRedeemer(cache.NewTickets(redisClient)),
		httpapi.WithTrustedProxies(cfg.TrustedProxies),
		httpapi.WithAllowedOrigins(cfg.AllowedOrigins),
		httpapi.WithSessions(httpapi.SessionConfig{
			IdleTTL: cfg.SessionIdleTTL, MaxTTL: cfg.SessionMaxTTL,
			CookieSecure: cfg.AuthCookieSecure, RefreshLimitPerIP: cfg.RefreshLimitPerIP,
		}),
		httpapi.WithUserCache(cfg.UserCacheTTL),
		httpapi.WithCatalogCache(cfg.CatalogCacheTTL),
		httpapi.WithReadLimits(cfg.ReadLimitPerUser, cfg.ReadLimitPerIP),
		httpapi.WithCustomerLimits(cfg.AvailabilityLimitPerUser, cfg.AvailabilityLimitPerIP, cfg.WriteLimitPerUser, cfg.WriteLimitPerIP),
		httpapi.WithRequestTimeout(cfg.RequestTimeout),
		httpapi.WithEventsSkipped(events.SkippedEvents),
	)
	// Per-instance reactions to events go into this chain. Session caches must be invalidated
	// before Broadcaster closes the sockets, or a client reconnecting at once could get through on
	// a stale cached user. Likewise the catalog cache is dropped on CONFIG_UPDATED (dealership,
	// service type, technician) before clients hear of the change and refetch.
	fanout := events.Chain(
		func(_ context.Context, ev realtime.Event) error {
			switch ev.Kind {
			case realtime.KindSession:
				api.InvalidateUser(ev.UserID)
			case realtime.KindConfig:
				api.InvalidateCatalog()
			}
			return nil
		},
		events.Broadcaster(hub),
	)
	// Per-boot fan-out group: every instance fans every event out to its own sockets. Live only (a
	// socket that connects later resyncs over REST): a fresh group per boot, starting at the end of
	// the topic, so a restart does not replay what happened while it was down (a replayed
	// SESSION_REVOKED would close sockets opened since). It commits like any group, so a rebalance
	// within the boot resumes where it was instead of skipping to the end; Close deletes it.
	consumer := events.NewConsumer(cfg.KafkaBrokers, cfg.KafkaTopic, events.FanoutGroupID(cfg.InstanceID), fanout, log,
		events.StartFrom(events.FromLatest), events.DeleteGroupOnClose(), events.SkipPayload()) // hub never reads the PII payload
	// Shared group: each event is turned into a customer notification once across instances.
	notifyConsumer := events.NewConsumer(cfg.KafkaBrokers, cfg.KafkaTopic, "scheduler-notify", notifier.HandleEvent, log,
		events.StartFrom(events.FromEarliest)) // replay is safe (dedupe_key); nothing before the first commit is lost
	// Each Close commits, leaves its group (and the fan-out one deletes it), retrying while Kafka is
	// unreachable: close both at once so a Kafka outage at shutdown costs one wait, not two.
	defer func() {
		var closing sync.WaitGroup
		closing.Go(func() { _ = consumer.Close() })
		closing.Go(func() { _ = notifyConsumer.Close() })
		closing.Wait()
	}()

	maintenanceCfg := worker.DefaultMaintenanceConfig()
	maintenanceCfg.NoShowSweep = cfg.NoShowSweep
	maintenanceCfg.OverrunStep = cfg.OverrunStep
	outboxCfg := worker.DefaultOutboxConfig()
	// The publish deadline, the Kafka writer's retry budget and the bookkeeping must all fit in
	// the lease, or another instance could republish rows this one may still write.
	if err := outboxCfg.Validate(maintenanceCfg.Lease); err != nil {
		return err
	}
	// Same for a notification batch: sends and their bookkeeping end before the lease does.
	if err := notifyCfg.Validate(maintenanceCfg.Lease); err != nil {
		return err
	}
	outbox := worker.NewOutbox(st, bus, log, cfg.InstanceID, outboxCfg)
	maintenance := worker.NewMaintenance(st, svc, notifier, log, maintenanceCfg)

	var background sync.WaitGroup
	if topicErr != nil {
		log.Warn("kafka topic not ready; retrying in the background", "err", topicErr, "topic", cfg.KafkaTopic)
		background.Go(func() {
			events.KeepEnsuringTopic(ctx, cfg.KafkaBrokers, cfg.KafkaTopic, cfg.KafkaPartitions, cfg.KafkaReplication, log)
		})
	}
	background.Go(func() { outbox.Run(ctx) })
	background.Go(func() {
		st.RunOutboxNotifier(ctx, func(err error) { log.Warn("outbox notify", "err", err, "instance", cfg.InstanceID) })
	})
	background.Go(func() { maintenance.Run(ctx) })
	background.Go(func() { consumer.Run(ctx) })
	background.Go(func() { notifyConsumer.Run(ctx) })
	background.Go(func() { notifier.Run(ctx) })

	// Session sweep (validator installed by httpapi.New): one batched user query per period.
	background.Go(func() { hub.Run(ctx) })
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      config.HTTPWriteTimeout, // > REQUEST_TIMEOUT (config); gorilla clears it after the WS upgrade
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "instance", cfg.InstanceID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	// A serve failure (port taken, listener died) still drains like a signal, then fails the
	// process: returned below, so main exits non-zero and the orchestrator restarts it.
	fatal := awaitStop(ctx, serveErr)
	if fatal != nil {
		log.Error("http server failed; draining before exit", "err", fatal)
	}
	stop()

	// Shutdown order: close sockets with a close frame at once (stop() already ended the fan-out
	// consumer, so a socket left open would look healthy while receiving nothing; clients reconnect
	// to the other instance and resync) while HTTP stops accepting and drains its requests, then
	// let background loops (cancelled with ctx, so already draining) finish; the deferred closes
	// (Kafka, Redis, then the pool) run only after that. The hub's close has its own short budget,
	// independent of a slow HTTP drain. The background wait has its own budget, measured from the
	// signal: an outbox flush or a notification send under way keeps running past cancellation
	// (see drainBudget).
	hubClosed := make(chan struct{})
	go func() {
		defer close(hubClosed)
		hubCtx, hubCancel := context.WithTimeout(context.Background(), hubCloseBudget)
		defer hubCancel()
		hub.Close(hubCtx)
	}()
	drain := drainBudget(outboxCfg, notifyCfg)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drain)
	defer cancelDrain()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownBudget(cfg.RequestTimeout, drain))
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
		closePool = false
	}
	<-hubClosed
	// Bookings that committed while the requests drained asked for a wake-up the stopped notifier
	// never sent: tell the other instances now, so they publish those events without a poll.
	if err := st.FlushOutboxNotify(drainCtx); err != nil {
		log.Warn("outbox notify at shutdown", "err", err)
	}
	done := make(chan struct{})
	go func() {
		background.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-drainCtx.Done():
		log.Warn("background workers did not stop in time; exiting without closing the pool")
		closePool = false
	}
	return fatal
}

// checkRedis pings Redis once at startup. Unreachable is fatal only with REDIS_REQUIRED: every
// Redis use fails open and go-redis reconnects on its own, so by default the instance starts
// degraded (readiness says so) instead of crash-looping and turning a Redis outage into an API one.
func checkRedis(ctx context.Context, r interface{ Ping(context.Context) error }, required bool, log *slog.Logger) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := r.Ping(pingCtx)
	if err == nil {
		return nil
	}
	if required {
		return fmt.Errorf("redis: %w", err)
	}
	log.Warn("redis unreachable at startup; continuing degraded (idempotency replay, rate limits and WS tickets fail open)", "err", err)
	return nil
}

// awaitStop blocks until a shutdown signal (ctx) or the HTTP server's end, returning the serve
// error that ended it (nil for a signal, or a server that closed cleanly).
func awaitStop(ctx context.Context, serveErr <-chan error) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}
}

// httpShutdownBudget bounds draining HTTP requests and closing sockets: long enough for a request
// that just started to reach its own deadline (REQUEST_TIMEOUT answers 503 busy, which the client
// retries on the other instance, instead of a gateway 502), but never past the background drain,
// which runs alongside it and which the container's stop grace period is sized for.
func httpShutdownBudget(requestTimeout, drain time.Duration) time.Duration {
	return min(requestTimeout+time.Second, drain)
}

const (
	// hubCloseBudget bounds waiting for the WebSocket close frames at shutdown.
	hubCloseBudget = 3 * time.Second
	// drainMargin covers the loops' own exits once their last flush or batch is done (claims and
	// stale checks already count against the outbox and notify deadlines).
	drainMargin = 5 * time.Second
)

// drainBudget is how long shutdown waits for the background loops. Each stops at its next ctx
// check, but an outbox flush or a notification batch already under way deliberately ignores
// cancellation (abandoning it would lose its outcome, and the rows would wait out the lease and go
// out twice) and is bounded by its own deadlines instead. Keep the container's stop grace period
// (docker-compose stop_grace_period) above this plus the deferred closes.
func drainBudget(outbox worker.OutboxConfig, notifier notify.Config) time.Duration {
	return max(outbox.DrainTimeout(), notifier.DrainTimeout()) + drainMargin
}
