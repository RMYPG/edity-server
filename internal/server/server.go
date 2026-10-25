// Package server routes the HTTP requests of every Site.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/RMYPG/edity-server/internal/site"
)

// New returns the server's handler. Apart from the health check, it answers
// only requests for one of sites, and 404 to any other host.
func New(sites *site.Registry, logger *slog.Logger) http.Handler {
	api := http.NewServeMux()
	for _, route := range []string{
		"GET /_edity/login",
		"GET /_edity/api/auth/callback",
		"POST /_edity/api/auth/logout",
		"GET /_edity/api/session",
		"POST /_edity/api/publish",
		"GET /_edity/api/publish/{sha}",
	} {
		api.HandleFunc(route, notImplemented)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health)
	mux.Handle("/", logRequests(logger, sites, sites.Require(api)))
	return mux
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
