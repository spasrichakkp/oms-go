package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubDatabasePinger struct {
	err      error
	deadline time.Time
}

func (s *stubDatabasePinger) Ping(ctx context.Context) error {
	s.deadline, _ = ctx.Deadline()
	return s.err
}

func TestPingDatabaseUsesBoundedContext(t *testing.T) {
	startedAt := time.Now()
	pinger := &stubDatabasePinger{}

	if err := pingDatabase(context.Background(), pinger); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	if pinger.deadline.IsZero() {
		t.Fatal("expected ping context deadline")
	}

	timeout := pinger.deadline.Sub(startedAt)
	if timeout <= 0 || timeout > databasePingTimeout+time.Second {
		t.Fatalf("expected timeout near %s, got %s", databasePingTimeout, timeout)
	}
}

func TestPingDatabaseReturnsConnectionError(t *testing.T) {
	wantErr := errors.New("database unavailable")

	err := pingDatabase(context.Background(), &stubDatabasePinger{err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}
