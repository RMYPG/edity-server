package server_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/apitest"
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/server"
	"github.com/RMYPG/edity-server/internal/site"
)

const commit = "4bc931076ac8583bd8905a4c508c987877afc09d"

// fakeTracker records what it is asked about, and answers as told.
type fakeTracker struct {
	mu       sync.Mutex
	asked    []string
	progress progress.Progress
	err      error
}

func (f *fakeTracker) Progress(ctx context.Context, s site.Site, sha string) (progress.Progress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, s.ID+" "+sha)
	if _, ok := ctx.Deadline(); !ok {
		return progress.Progress{}, errors.New("no deadline")
	}
	return f.progress, f.err
}

func (f *fakeTracker) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

func progressServer(t *testing.T, sessions server.Sessions, tracker server.Tracker) http.Handler {
	t.Helper()
	sites, err := site.Parse(strings.NewReader(twoSites))
	if err != nil {
		t.Fatal(err)
	}
	return apitest.Handler(t, server.New(sites, slog.New(slog.NewJSONHandler(io.Discard, nil)), server.Services{
		Sessions: sessions, Tracker: tracker,
	}))
}

// get asks for a Publish's progress from the Site's own page, with a
// session cookie and, as browsers do for a GET on the page's own origin, no
// Origin.
func get(t *testing.T, h http.Handler, sha string, edit func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_edity/api/publish/"+sha, nil)
	req.Host = "sandbox.example.com"
	req.AddCookie(&http.Cookie{Name: "edity_session", Value: "a-session"})
	if edit != nil {
		edit(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPublishsProgressIsItsStageAndVersion(t *testing.T) {
	for _, stage := range []progress.Stage{progress.Validating, progress.Releasing, progress.Released, progress.Failed} {
		tracker := &fakeTracker{progress: progress.Progress{Stage: stage, Version: "1.0.1", Run: "https://github.com/x", Reason: "CI ended failure"}}
		rec := get(t, progressServer(t, cookieSessions{}, tracker), commit, nil)
		if want := fmt.Sprintf(`{"stage":%q,"version":"1.0.1"}`, stage); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: status = %d, body = %s, want 200 %s", stage, rec.Code, rec.Body, want)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", stage, rec.Header().Get("Cache-Control"))
		}
	}

	// Each Site's Publishes are its own repository's.
	tracker := &fakeTracker{progress: progress.Progress{Stage: progress.Validating, Version: "2.3.4"}}
	get(t, progressServer(t, cookieSessions{}, tracker), commit, func(r *http.Request) { r.Host = "fisioindoor.example.com" })
	if len(tracker.asked) != 1 || tracker.asked[0] != "fisioindoor "+commit {
		t.Errorf("asked %v, want fisioindoor's %s", tracker.asked, commit)
	}
}

func TestTheProgressOfWhatIsNotAPublishIs404(t *testing.T) {
	tracker := &fakeTracker{err: fmt.Errorf("a Developer's commit: %w", progress.ErrNotPublish)}
	if rec := get(t, progressServer(t, cookieSessions{}, tracker), commit, nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	// Not a sha at all: GitHub is not asked.
	tracker = &fakeTracker{}
	for _, sha := range []string{"4BC931076AC8583BD8905A4C508C987877AFC09D", "4bc9310", commit + "0", "main", "v1.0.1"} {
		if rec := get(t, progressServer(t, cookieSessions{}, tracker), sha, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", sha, rec.Code)
		}
	}
	if tracker.calls() != 0 {
		t.Errorf("GitHub was asked %d times about what is not a sha", tracker.calls())
	}
}

func TestTheProgressWithoutASessionGets401(t *testing.T) {
	tracker := &fakeTracker{}
	for name, sessions := range map[string]server.Sessions{"before login exists": server.NoSessions{}, "with no cookie": cookieSessions{}} {
		rec := get(t, progressServer(t, sessions, tracker), commit, func(r *http.Request) { r.Header.Del("Cookie") })
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	if tracker.calls() != 0 {
		t.Errorf("GitHub was asked %d times without a session", tracker.calls())
	}
}

func TestTheProgressAskedFromAnotherOriginGets403(t *testing.T) {
	tracker := &fakeTracker{progress: progress.Progress{Stage: progress.Validating, Version: "1.0.1"}}
	h := progressServer(t, cookieSessions{}, tracker)
	for _, origin := range []string{"null", "https://evil.example.com", "https://fisioindoor.example.com", "https://sandbox.example.com:8443"} {
		if rec := get(t, h, commit, func(r *http.Request) { r.Header.Set("Origin", origin) }); rec.Code != http.StatusForbidden {
			t.Errorf("Origin %q: status = %d, want 403", origin, rec.Code)
		}
	}
	if tracker.calls() != 0 {
		t.Errorf("GitHub was asked %d times from another origin", tracker.calls())
	}
	if rec := get(t, h, commit, func(r *http.Request) { r.Header.Set("Origin", "https://sandbox.example.com") }); rec.Code != http.StatusOK {
		t.Errorf("from the Site's own origin: status = %d, want 200", rec.Code)
	}
}

func TestWhatTheProgressAnswersWhenGitHubDoesNotTell(t *testing.T) {
	for name, c := range map[string]struct {
		err        error
		status     int
		retryAfter string
	}{
		"no installation": {err: github.ErrNotInstalled, status: http.StatusInternalServerError},
		"a refused App":   {err: &github.Error{Call: "GET /x", Status: 403, Message: "Resource not accessible by integration"}, status: http.StatusInternalServerError},
		"a rate limit":    {err: &github.Error{Call: "GET /x", Status: 429, RetryAfter: 30 * time.Second}, status: http.StatusServiceUnavailable, retryAfter: "30"},
		"GitHub down":     {err: &github.Error{Call: "GET /x", Status: 502}, status: http.StatusServiceUnavailable},
		"no network":      {err: errors.New("dial tcp: i/o timeout"), status: http.StatusServiceUnavailable},
		"too slow":        {err: context.DeadlineExceeded, status: http.StatusServiceUnavailable},
	} {
		rec := get(t, progressServer(t, cookieSessions{}, &fakeTracker{err: c.err}), commit, nil)
		if rec.Code != c.status || rec.Header().Get("Retry-After") != c.retryAfter {
			t.Errorf("%s: status = %d, Retry-After = %q, want %d and %q", name, rec.Code, rec.Header().Get("Retry-After"), c.status, c.retryAfter)
		}
	}
}
