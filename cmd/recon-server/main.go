package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mehmetkr/recon-go/internal/http/handler"
	"github.com/mehmetkr/recon-go/internal/http/middleware"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/recon"
	"github.com/mehmetkr/recon-go/internal/store"
	"github.com/mehmetkr/recon-go/internal/store/postgres"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg := loadConfig()

	st, cleanup, err := buildStore(cfg, log)
	if err != nil {
		return err
	}
	defer cleanup()

	svc := &recon.Service{Matcher: match.Run}
	h := &handler.Handler{Store: st, Service: svc, Log: log}

	mux := middleware.Chain(
		h.Routes(),
		middleware.RequestID,
		middleware.Logger(log),
		middleware.Recovery(log),
		middleware.Timeout(cfg.requestTimeout),
	)

	srv := &http.Server{
		Addr:         ":" + cfg.port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server starting", "addr", srv.Addr, "store", cfg.storeType)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Info("shutting down", "signal", sig)
	case err := <-errCh:
		return fmt.Errorf("server failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("server stopped")
	return nil
}

type config struct {
	storeType      string
	port           string
	databaseURL    string
	statePath      string
	requestTimeout time.Duration
}

func loadConfig() config {
	return config{
		storeType:      envDefault("STORE_TYPE", "postgres"),
		port:           envDefault("PORT", "8080"),
		databaseURL:    os.Getenv("DATABASE_URL"),
		statePath:      envDefault("STATE_PATH", "state.json"),
		requestTimeout: 30 * time.Second,
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func buildStore(cfg config, log *slog.Logger) (store.Store, func(), error) {
	switch cfg.storeType {
	case "postgres":
		if cfg.databaseURL == "" {
			return nil, nil, fmt.Errorf("DATABASE_URL is required when STORE_TYPE=postgres")
		}
		if err := postgres.Migrate(cfg.databaseURL); err != nil {
			return nil, nil, fmt.Errorf("migrations: %w", err)
		}
		log.Info("migrations applied")

		pool, err := pgxpool.New(context.Background(), cfg.databaseURL)
		if err != nil {
			return nil, nil, fmt.Errorf("database connection: %w", err)
		}
		return postgres.New(pool), pool.Close, nil

	case "file":
		return store.FileStore{Path: cfg.statePath}, func() {}, nil

	default:
		return nil, nil, fmt.Errorf("unknown STORE_TYPE %q (use postgres or file)", cfg.storeType)
	}
}
