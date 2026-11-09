package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/RMYPG/edity-server/internal/login"
	"github.com/RMYPG/edity-server/internal/site"
)

// Login logs Clients in on a Site. *login.Zitadel is one.
type Login interface {
	// Start begins a login on s and returns where to send the browser to log
	// in.
	Start(w http.ResponseWriter, r *http.Request, s site.Site) (string, error)
	// Finish ends a login on s, sets the session's cookies and returns where
	// on the Site to send the browser back to.
	Finish(w http.ResponseWriter, r *http.Request, s site.Site) (string, error)
	// Logout clears the session's cookies.
	Logout(w http.ResponseWriter, r *http.Request)
}

type loginHandler struct {
	login    Login
	sessions Sessions
	logger   *slog.Logger
}

// start answers GET /_edity/login.
func (h *loginHandler) start(w http.ResponseWriter, r *http.Request) {
	s, _ := site.FromContext(r.Context())
	to, err := h.login.Start(w, r, s)
	if err != nil {
		h.unavailable(w, r, s, err)
		return
	}
	http.Redirect(w, r, to, http.StatusFound) //nolint:gosec // Zitadel's own login, from its discovery document.
}

// finish answers GET /_edity/api/auth/callback.
func (h *loginHandler) finish(w http.ResponseWriter, r *http.Request) {
	s, _ := site.FromContext(r.Context())
	to, err := h.login.Finish(w, r, s)
	switch {
	case errors.Is(err, login.ErrNoRole):
		h.refused(r, s, err)
		http.Error(w, "The person who logged in is not one of the Site's Clients.", http.StatusForbidden)
	case errors.Is(err, login.ErrNoName):
		h.refused(r, s, err)
		http.Error(w, "The person who logged in has no name in Zitadel, which every Publish records: the Developer has to fix it.", http.StatusForbidden)
	case errors.Is(err, login.ErrNotALogin):
		h.refused(r, s, err)
		http.Error(w, "Not the end of a login this browser started: log in again.", http.StatusBadRequest)
	case err != nil:
		h.unavailable(w, r, s, err)
	default:
		http.Redirect(w, r, to, http.StatusSeeOther) //nolint:gosec // login.Zitadel only returns a path on the Site.
	}
}

// logout answers POST /_edity/api/auth/logout.
func (h *loginHandler) logout(w http.ResponseWriter, r *http.Request) {
	s, _ := site.FromContext(r.Context())
	if !fromSite(r, s) {
		http.Error(w, "The request does not come from the Site's own origin.", http.StatusForbidden)
		return
	}
	h.login.Logout(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// session answers GET /_edity/api/session.
func (h *loginHandler) session(w http.ResponseWriter, r *http.Request) {
	s, _ := site.FromContext(r.Context())
	if !getFromSite(r, s) {
		http.Error(w, "The request does not come from the Site's own origin.", http.StatusForbidden)
		return
	}
	client, ok := h.sessions.Client(r, s)
	if !ok {
		http.Error(w, "No session: log in.", http.StatusUnauthorized)
		return
	}
	// It tells who this browser is logged in as, so no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		Name   string `json:"name"`
		Locale string `json:"locale"`
		Site   string `json:"site"`
	}{client.Name, client.Locale, s.ID})
}

// refused logs why a login got no session.
func (h *loginHandler) refused(r *http.Request, s site.Site, err error) {
	h.logger.LogAttrs(r.Context(), slog.LevelInfo, "login refused", slog.String("site", s.ID), slog.String("reason", err.Error()))
}

// unavailable answers a login that Zitadel could not take further for now.
func (h *loginHandler) unavailable(w http.ResponseWriter, r *http.Request, s site.Site, err error) {
	h.logger.LogAttrs(r.Context(), slog.LevelError, "login failed", slog.String("site", s.ID), slog.String("error", err.Error()))
	http.Error(w, "Zitadel could not be reached: log in again later.", http.StatusServiceUnavailable)
}
