package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"oms/internal/auth"
	"oms/internal/orders"
	"oms/internal/platform"

	"github.com/jackc/pgx/v5/pgxpool"
)

const databasePingTimeout = 5 * time.Second

type databasePinger interface {
	Ping(context.Context) error
}

func main() {
	logger := platform.NewLogger()
	cfg, err := platform.LoadConfig()
	if err != nil {
		logger.Error("load config failed", slog.Any("error", err))
		return
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	appCtx, cancel := context.WithCancel(rootCtx)
	defer cancel()

	identityMiddleware, err := auth.NewIdentityMiddleware(appCtx, cfg.AuthConfig)
	if err != nil {
		logger.Error("initialize auth middleware failed", slog.Any("error", err))
		return
	}

	dbPool, err := pgxpool.New(appCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("create database pool failed", slog.Any("error", err))
		return
	}
	defer dbPool.Close()

	if err := pingDatabase(appCtx, dbPool); err != nil {
		logger.Error("connect database failed", slog.Any("error", err))
		return
	}

	repo := orders.NewRepository(dbPool)
	service := orders.NewService(repo)
	orderHandler := orders.NewHandler(service)
	worker := orders.NewWorker(service, logger.With(slog.String("component", "orders-worker")), cfg.WorkerBatchSize)
	server := platform.NewHTTPServer(cfg, logger, orderHandler, identityMiddleware)

	runErrCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		logger.Info("starting http server", slog.String("addr", cfg.HTTPAddr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErrCh <- err
		}
	}()

	go func() {
		defer wg.Done()
		if err := worker.Run(appCtx); err != nil {
			runErrCh <- err
		}
	}()

	select {
	case err := <-runErrCh:
		if err != nil {
			logger.Error("application component exited", slog.Any("error", err))
		}
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown failed", slog.Any("error", err))
		return
	}

	wg.Wait()
	logger.Info("http server stopped cleanly")
}

func pingDatabase(ctx context.Context, database databasePinger) error {
	pingCtx, cancel := context.WithTimeout(ctx, databasePingTimeout)
	defer cancel()

	return database.Ping(pingCtx)
}
