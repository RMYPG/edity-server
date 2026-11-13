package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/rollout"
	"github.com/RMYPG/edity-server/internal/site"
)

// A Tracker tells how far a Publish has got. *progress.Tracker is one.
type Tracker interface {
	// Progress returns progress.ErrNotPublish for a sha that is not the
	// commit of a Publish on s, and a *rollout.Error when the platform's
	// rollout status service gives no answer.
	Progress(ctx context.Context, s site.Site, sha string) (progress.Progress, error)
}

// progressTimeout bounds how long a request waits for GitHub and the rollout
// status service to tell a Publish's progress.
const progressTimeout = 25 * time.Second

type progressHandler struct {
	sessions Sessions
	tracker  Tracker
	logger   *slog.Logger
}

// ServeHTTP answers GET /_edity/api/publish/{sha}.
func (h *progressHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, _ := site.FromContext(r.Context())
	if !getFromSite(r, s) {
		http.Error(w, "The request does not come from the Site's own origin.", http.StatusForbidden)
		return
	}
	if _, ok := h.sessions.Client(r, s); !ok {
		http.Error(w, "No session: log in to follow a Publish.", http.StatusUnauthorized)
		return
	}
	commit := r.PathValue("sha")
	if !github.IsSHA(commit) {
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
		Stage   progress.Stage  `json:"stage"`
		Version release.Version `json:"version"`
	}{p.Stage, p.Version})
}

// fail answers a request whose progress GitHub or the rollout status
// service did not tell. Neither ever answers that the Publish failed: the
// Editor asks again.
func (h *progressHandler) fail(w http.ResponseWriter, r *http.Request, s site.Site, err error) {
	h.logger.LogAttrs(r.Context(), slog.LevelError, "progress failed", slog.String("site", s.ID), slog.String("error", err.Error()))
	var unanswered *rollout.Error
	switch {
	case errors.As(err, &unanswered) && unanswered.Refused():
		// The service is not configured for the Site, which the Developer
		// or the platform has to fix.
		http.Error(w, "The platform's rollout status service does not know the Site: the Developer has to fix it.", http.StatusInternalServerError)
	case errors.As(err, &unanswered):
		http.Error(w, "The platform's rollout status could not be read: ask again later.", http.StatusServiceUnavailable)
	default:
		failedOnGitHub(w, err, "ask again")
	}
}
