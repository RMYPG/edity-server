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
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/issues"
	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/server"
	"github.com/RMYPG/edity-server/internal/site"
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

	publisher := github.NewApp(cfg.Publisher.ID, cfg.Publisher.PrivateKey, github.API, &http.Client{Timeout: 20 * time.Second})
	reporter := issues.New(func(r site.Repository) issues.Repository {
		return publisher.Repo(r.Owner, r.Name)
	}, publisher.BotLogin, logger)
	services := server.Services{
		// No Client can log in before ticket 25, so every Publish gets 401.
		Sessions: server.NoSessions{},
		Publisher: publish.New(func(r site.Repository) publish.Repository {
			return publisher.Repo(r.Owner, r.Name)
		}),
		Notifier: reporter,
		Tracker: progress.New(func(r site.Repository) progress.Repository {
			return publisher.Repo(r.Owner, r.Name)
		}, publisher.BotLogin, reporter),
	}
	srv := httpServer(cfg.Addr, server.New(cfg.Sites, logger, services), logger)
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	logger.Info("edity-server listening", "addr", cfg.Addr, "github_app_id", cfg.Publisher.ID)

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

// httpServer bounds how long a client may take to send a request, and how
// long a connection may stay open, so that slow or idle clients can't hold
// the connections every Site shares. A Publish waits on GitHub before it
// answers, hence the longer write timeout.
func httpServer(addr string, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}
