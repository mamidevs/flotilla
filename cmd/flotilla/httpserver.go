package main

import (
	"context"
	"net/http"
	"time"
)

// httpServerCompat is a thin wrapper around http.Server that shuts down
// cleanly when its context is canceled.
type httpServerCompat struct {
	Addr    string
	Handler http.Handler
}

func (s *httpServerCompat) ListenAndServeWithCtx(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}
