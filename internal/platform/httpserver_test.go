package platform

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"oms/internal/auth"
	"oms/internal/orders"
)

func TestHTTPServerAppliesConfiguredTimeouts(t *testing.T) {
	cfg := Config{
		AuthMode:     auth.ModeLocked,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	server := NewHTTPServer(cfg, discardLogger(), orders.NewHandler(nil), auth.IdentityMiddleware(auth.ModeLocked))

	if server.ReadTimeout != cfg.ReadTimeout {
		t.Fatalf("expected read timeout %s, got %s", cfg.ReadTimeout, server.ReadTimeout)
	}

	if server.WriteTimeout != cfg.WriteTimeout {
		t.Fatalf("expected write timeout %s, got %s", cfg.WriteTimeout, server.WriteTimeout)
	}

	if server.IdleTimeout != cfg.IdleTimeout {
		t.Fatalf("expected idle timeout %s, got %s", cfg.IdleTimeout, server.IdleTimeout)
	}

	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("expected read header timeout 5s, got %s", server.ReadHeaderTimeout)
	}
}

func TestHTTPServerHealthEndpointRemainsPublic(t *testing.T) {
	server := NewHTTPServer(Config{
		AuthMode:     auth.ModeLocked,
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
		IdleTimeout:  defaultIdleTimeout,
	}, discardLogger(), orders.NewHandler(nil), auth.IdentityMiddleware(auth.ModeLocked))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHTTPServerLockedModeRejectsForgedDevHeaders(t *testing.T) {
	server := NewHTTPServer(Config{
		AuthMode:     auth.ModeLocked,
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
		IdleTimeout:  defaultIdleTimeout,
	}, discardLogger(), orders.NewHandler(nil), auth.IdentityMiddleware(auth.ModeLocked))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
	req.Header.Set("X-OMS-Role", "ADMIN")
	req.Header.Set("X-OMS-Customer-ID", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	rec := httptest.NewRecorder()

	server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
