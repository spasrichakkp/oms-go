package platform

import (
	"log/slog"
	"net/http"
	"time"

	"oms/internal/orders"

	"github.com/go-chi/chi/v5"
)

func NewHTTPServer(cfg Config, logger *slog.Logger, orderHandler *orders.Handler, identityMiddleware func(http.Handler) http.Handler) *http.Server {
	_ = logger

	router := chi.NewRouter()
	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	router.Route("/api/v1/orders", func(r chi.Router) {
		r.Use(identityMiddleware)
		orders.RegisterRoutes(r, orderHandler)
	})

	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       cfg.IdleTimeout,
	}
}
