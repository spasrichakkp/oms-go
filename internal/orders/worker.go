package orders

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	db "oms/internal/db"
)

const workerInterval = 5 * time.Minute

type workerService interface {
	ProcessPendingOrdersBatch(ctx context.Context, input ProcessPendingOrdersBatchInput) ([]db.Order, error)
}

type workerTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type timeTicker struct {
	*time.Ticker
}

func (t timeTicker) Chan() <-chan time.Time {
	return t.C
}

type Worker struct {
	service    workerService
	logger     *slog.Logger
	batchSize  int32
	interval   time.Duration
	newTicker  func(time.Duration) workerTicker
	isRunning  atomic.Bool
	activeRuns sync.WaitGroup
}

func NewWorker(service workerService, logger *slog.Logger, batchSize int32) *Worker {
	return &Worker{
		service:   service,
		logger:    logger,
		batchSize: batchSize,
		interval:  workerInterval,
		newTicker: func(d time.Duration) workerTicker {
			return timeTicker{Ticker: time.NewTicker(d)}
		},
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if w.service == nil {
		return errors.New("worker service is required")
	}

	if w.batchSize < 1 {
		return errors.New("worker batch size must be positive")
	}

	ticker := w.newTicker(w.interval)
	defer ticker.Stop()

	w.logger.Info("orders worker started", slog.Int("batch_size", int(w.batchSize)), slog.Duration("interval", w.interval))
	defer w.logger.Info("orders worker stopped")

	for {
		select {
		case <-ctx.Done():
			w.activeRuns.Wait()
			return nil
		case <-ticker.Chan():
			if !w.isRunning.CompareAndSwap(false, true) {
				w.logger.Warn("orders worker tick skipped because previous run is still active", slog.Int("batch_size", int(w.batchSize)))
				continue
			}

			w.activeRuns.Add(1)
			go func() {
				defer w.activeRuns.Done()
				defer w.isRunning.Store(false)
				w.runOnce(ctx)
			}()
		}
	}
}

func (w *Worker) runOnce(ctx context.Context) {
	startedAt := time.Now()

	orders, err := w.service.ProcessPendingOrdersBatch(ctx, ProcessPendingOrdersBatchInput{
		Limit: w.batchSize,
	})
	duration := time.Since(startedAt)

	if err != nil {
		if ctx.Err() != nil {
			w.logger.Info("orders worker run canceled", slog.Int("batch_size", int(w.batchSize)), slog.Duration("duration", duration))
			return
		}

		w.logger.Error("orders worker run failed", slog.Int("batch_size", int(w.batchSize)), slog.Duration("duration", duration), slog.Any("error", err))
		return
	}

	w.logger.Info("orders worker run completed", slog.Int("batch_size", int(w.batchSize)), slog.Int("processed_count", len(orders)), slog.Duration("duration", duration))
}
