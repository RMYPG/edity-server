// Command edity-server is the one server behind every Site's /_edity/ routes.
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

	"github.com/RMYPG/edity-server/internal/config"
	"github.com/RMYPG/edity-server/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger); err != nil {
		logger.Error("edity-server stopped", "error", err.Error())
		stop()
		os.Exit(1)
	}
}

// run serves until ctx is done, then lets requests in flight finish.
func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(cfg.Sites, logger),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	logger.Info("edity-server listening", "addr", cfg.Addr, "github_app_id", cfg.GitHubApp.ID)

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	logger.Info("edity-server shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
