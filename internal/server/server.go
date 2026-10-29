// Package server routes the HTTP requests of every Site.
package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/site"
)

// Services are what the routes work with.
type Services struct {
	// Sessions is NoSessions when it is nil.
	Sessions  Sessions
	Publisher Publisher
	// Notifier notes what a Publish ships, unless it is nil.
	Notifier Notifier
	Tracker  Tracker
	// RateLimit is DefaultRateLimit when it is zero.
	RateLimit RateLimit
}

// New returns the server's handler. Apart from the health check, it answers
// only requests for one of sites, and 404 to any other host.
func New(sites *site.Registry, logger *slog.Logger, services Services) http.Handler {
	if services.Sessions == nil {
		services.Sessions = NoSessions{}
	}
	limit := services.RateLimit
	if limit.Burst == 0 || limit.Every == 0 {
		limit = DefaultRateLimit
	}
	api := http.NewServeMux()
	api.Handle("POST /_edity/api/publish", &publishHandler{
		sessions:  services.Sessions,
		publisher: services.Publisher,
		notifier:  services.Notifier,
		limit:     newLimiter(limit, time.Now),
		logger:    logger,
	})
	api.Handle("GET /_edity/api/publish/{sha}", &progressHandler{
		sessions: services.Sessions,
		tracker:  services.Tracker,
		logger:   logger,
	})
	for _, route := range []string{
		"GET /_edity/login",
		"GET /_edity/api/auth/callback",
		"POST /_edity/api/auth/logout",
		"GET /_edity/api/session",
	} {
		api.HandleFunc(route, notImplemented)
	}

	// Everything but the health check goes through the Site lookup before a
	// ServeMux sees it: a ServeMux redirects a path it would clean, such as
	// //_edity/api/session, whatever the host.
	sited := logRequests(logger, sites, sites.Require(api))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			health(w, r)
			return
		}
		sited.ServeHTTP(w, r)
	})
}

// health answers Kubernetes probes, which reach the pod directly rather than
// through a Site's hostname.
func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// notImplemented answers the routes of the spec that are not built yet.
func notImplemented(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, http.StatusText(http.StatusNotImplemented), http.StatusNotImplemented)
}

// failedOnGitHub answers a request that GitHub failed: 503 with GitHub's
// wait in Retry-After when it limits the App's requests, 500 when it refuses
// the App, which the Developer has to fix, and 503 when it could not be
// reached. again is what the Client does later, such as "publish again".
func failedOnGitHub(w http.ResponseWriter, err error, again string) {
	var refused *github.Error
	switch {
	case errors.As(err, &refused) && refused.RetryAfter > 0:
		w.Header().Set("Retry-After", seconds(refused.RetryAfter))
		http.Error(w, "GitHub is limiting the server's requests: "+again+" later.", http.StatusServiceUnavailable)
	case errors.Is(err, github.ErrNotInstalled), errors.As(err, &refused) && refused.Status < 500:
		// The App is not installed, or lacks a permission.
		http.Error(w, "GitHub refused the publisher App: the Developer has to fix it.", http.StatusInternalServerError)
	default:
		http.Error(w, "GitHub could not be reached: "+again+" later.", http.StatusServiceUnavailable)
	}
}

// logRequests writes one line per request. It leaves out the query string,
// which carries the login callback's authorization code.
func logRequests(logger *slog.Logger, sites *site.Registry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("host", r.Host),
		}
		if s, ok := sites.Lookup(r.Host); ok {
			attrs = append(attrs, slog.String("site", s.ID))
		}
		attrs = append(attrs,
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
	})
}

// statusRecorder remembers the status a handler answered with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
