// Command uptrack checks HTTP endpoints on a schedule, records uptime and alerts on outages.
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

	"github.com/oldsoska/uptrack/internal/checker"
	"github.com/oldsoska/uptrack/internal/config"
	"github.com/oldsoska/uptrack/internal/httpapi"
	"github.com/oldsoska/uptrack/internal/metrics"
	"github.com/oldsoska/uptrack/internal/notify"
	"github.com/oldsoska/uptrack/internal/scheduler"
	"github.com/oldsoska/uptrack/internal/store"
)

func main() {
	// The runtime image has no shell or curl, so the container health check calls the binary itself
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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

	// SIGTERM is what Docker, Kubernetes and Render send before stopping a container
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	m := metrics.New()
	sched := scheduler.New(st, checker.New(cfg.BlockPrivateTargets), notifier(cfg, log), m, log,
		scheduler.Options{MaxConcurrent: cfg.MaxConcurrentChecks, MaxStartDelay: 10 * time.Second})
	if err := sched.Start(ctx); err != nil {
		return err
	}
	go purgeOldResults(ctx, st, cfg.ResultRetention, log)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.New(st, sched, cfg.AdminToken, m.Registry, log),
		ReadHeaderTimeout: 5 * time.Second, // slowloris protection
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "block_private_targets", cfg.BlockPrivateTargets)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Graceful shutdown: stop accepting requests, let in-flight ones finish, then wait for checks
	// and alert deliveries. A second signal or the timeout ends it regardless.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	done := make(chan struct{})
	go func() { sched.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		log.Warn("scheduler did not stop in time")
	}
	return nil
}

func notifier(cfg config.Config, log *slog.Logger) notify.Notifier {
	var channels notify.Multi
	if cfg.TelegramBotToken != "" {
		channels = append(channels, notify.NewTelegram(cfg.TelegramBotToken, cfg.TelegramChatID))
	}
	if cfg.WebhookURL != "" {
		channels = append(channels, notify.NewWebhook(cfg.WebhookURL))
	}
	if len(channels) == 0 {
		log.Warn("no alert channel configured; alerts are only logged")
		return notify.Log{Logger: log}
	}
	retrying := make(notify.Multi, len(channels))
	for i, c := range channels {
		retrying[i] = notify.Retry{Next: c, Attempts: 4, Base: 2 * time.Second}
	}
	return notify.Multi{notify.Log{Logger: log}, retrying}
}

func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func purgeOldResults(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		deleted, err := st.DeleteResultsBefore(ctx, time.Now().Add(-retention))
		if err != nil && ctx.Err() == nil {
			log.Error("purge old results", "err", err)
		} else if deleted > 0 {
			log.Info("purged old results", "rows", deleted)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}
