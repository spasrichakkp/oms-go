package orders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	db "oms/internal/db"
)

type stubWorkerService struct {
	mu        sync.Mutex
	callCount int
	limits    []int32
	result    []db.Order
	err       error
	blockCh   chan struct{}
}

func (s *stubWorkerService) ProcessPendingOrdersBatch(_ context.Context, input ProcessPendingOrdersBatchInput) ([]db.Order, error) {
	s.mu.Lock()
	s.callCount++
	s.limits = append(s.limits, input.Limit)
	s.mu.Unlock()

	if s.blockCh != nil {
		<-s.blockCh
	}

	return s.result, s.err
}

func (s *stubWorkerService) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCount
}

type fakeTicker struct {
	ch chan time.Time
}

func (t *fakeTicker) Chan() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()                  {}

func TestWorkerUsesConfiguredBatchSize(t *testing.T) {
	t.Parallel()

	service := &stubWorkerService{result: []db.Order{{}}}
	worker := NewWorker(service, testLogger(), 42)
	worker.runOnce(context.Background())

	if service.callCount != 1 {
		t.Fatalf("expected one worker run, got %d", service.callCount)
	}

	if len(service.limits) != 1 || service.limits[0] != 42 {
		t.Fatalf("expected batch size 42, got %v", service.limits)
	}
}

func TestWorkerSkipsOverlappingRuns(t *testing.T) {
	t.Parallel()

	blockCh := make(chan struct{})
	service := &stubWorkerService{blockCh: blockCh}
	ticker := &fakeTicker{ch: make(chan time.Time, 2)}
	worker := NewWorker(service, testLogger(), 10)
	worker.interval = time.Millisecond
	worker.newTicker = func(time.Duration) workerTicker { return ticker }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx)
	}()

	ticker.ch <- time.Now()
	waitFor(t, func() bool { return service.CallCount() == 1 })
	ticker.ch <- time.Now()
	time.Sleep(20 * time.Millisecond)

	callCount := service.CallCount()
	if callCount != 1 {
		t.Fatalf("expected overlapping tick to be skipped, got %d calls", callCount)
	}

	close(blockCh)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil worker shutdown error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestWorkerStopsCleanlyOnContextCancellation(t *testing.T) {
	t.Parallel()

	service := &stubWorkerService{}
	ticker := &fakeTicker{ch: make(chan time.Time)}
	worker := NewWorker(service, testLogger(), 5)
	worker.newTicker = func(time.Duration) workerTicker { return ticker }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil worker shutdown error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop on context cancellation")
	}
}

func TestWorkerSurfacesRunErrorsWithoutStoppingLoop(t *testing.T) {
	t.Parallel()

	service := &stubWorkerService{err: errors.New("boom")}
	ticker := &fakeTicker{ch: make(chan time.Time, 1)}
	worker := NewWorker(service, testLogger(), 7)
	worker.newTicker = func(time.Duration) workerTicker { return ticker }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx)
	}()

	ticker.ch <- time.Now()
	waitFor(t, func() bool { return service.CallCount() == 1 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil worker shutdown error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("condition not met before deadline")
}
