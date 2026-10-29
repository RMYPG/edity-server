package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/site"
)

// A Tracker tells how far a Publish has got. *progress.Tracker is one.
type Tracker interface {
	// Progress returns progress.ErrNotPublish for a sha that is not the
	// commit of a Publish on s.
	Progress(ctx context.Context, s site.Site, sha string) (progress.Progress, error)
}

// progressTimeout bounds how long a request waits for GitHub to tell a
// Publish's progress.
const progressTimeout = 25 * time.Second

// shaPattern is the OpenAPI file's pattern for a commit's sha.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type progressHandler struct {
	sessions Sessions
	tracker  Tracker
	logger   *slog.Logger
}

// ServeHTTP answers GET /_edity/api/publish/{sha}.
func (h *progressHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, _ := site.FromContext(r.Context())
	// A browser sends no Origin with a GET from the page's own origin, and
	// one with a GET from any other.
	if r.Header.Get("Origin") != "" && !sameOrigin(r) {
		http.Error(w, "The request does not come from the Site's own origin.", http.StatusForbidden)
		return
	}
	if _, ok := h.sessions.Client(r, s); !ok {
		http.Error(w, "No session: log in to follow a Publish.", http.StatusUnauthorized)
		return
	}
	commit := r.PathValue("sha")
	if !shaPattern.MatchString(commit) {
		http.Error(w, "No Publish of the Site has that commit.", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), progressTimeout)
	defer cancel()
	p, err := h.tracker.Progress(ctx, s, commit)
	if errors.Is(err, progress.ErrNotPublish) {
		http.Error(w, "No Publish of the Site has that commit.", http.StatusNotFound)
		return
	}
	if err != nil {
		h.fail(w, r, s, err)
		return
	}
	// The progress moves on, so no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		Stage   progress.Stage `json:"stage"`
		Version string         `json:"version"`
	}{p.Stage, p.Version})
}

// fail answers a request whose progress GitHub did not tell.
func (h *progressHandler) fail(w http.ResponseWriter, r *http.Request, s site.Site, err error) {
	h.logger.LogAttrs(r.Context(), slog.LevelError, "progress failed", slog.String("site", s.ID), slog.String("error", err.Error()))
	var limited *github.Error
	switch {
	case errors.As(err, &limited) && limited.RetryAfter > 0:
		w.Header().Set("Retry-After", seconds(limited.RetryAfter))
		http.Error(w, "GitHub is limiting the server's requests: ask again later.", http.StatusServiceUnavailable)
	case errors.Is(err, github.ErrNotInstalled), errors.As(err, &limited) && limited.Status < 500:
		// The App is not installed, or lacks a permission.
		http.Error(w, "GitHub refused the publisher App: the Developer has to fix it.", http.StatusInternalServerError)
	default:
		http.Error(w, "GitHub could not be reached: ask again later.", http.StatusServiceUnavailable)
	}
}
