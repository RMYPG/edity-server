package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/apitest"
	"github.com/RMYPG/edity-server/internal/fields"
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/server"
	"github.com/RMYPG/edity-server/internal/site"
)

const twoSites = `{
  "sites": [
    {
      "id": "edity-sandbox",
      "hostnames": ["sandbox.example.com"],
      "githubRepository": "RMYPG/edity-sandbox",
      "locale": "en",
      "zitadelRole": "site:edity-sandbox",
      "kubernetes": {"namespace": "edity-sandbox", "deployment": "edity-sandbox-sws"}
    },
    {
      "id": "fisioindoor",
      "hostnames": ["fisioindoor.example.com"],
      "githubRepository": "RMYPG/fisioindoor",
      "locale": "es",
      "zitadelRole": "site:fisioindoor",
      "kubernetes": {"namespace": "fisioindoor", "deployment": "fisioindoor-sws"}
    }
  ]
}`

// cookieSessions is a session for every request that carries the session
// cookie, until login (ticket 25) makes real ones.
type cookieSessions struct{}

func (cookieSessions) Client(r *http.Request, _ site.Site) (publish.Client, bool) {
	if _, err := r.Cookie("edity_session"); err != nil {
		return publish.Client{}, false
	}
	return publish.Client{Name: "Ana García"}, true
}

// fakePublisher records what it is asked to publish, and answers as told.
type fakePublisher struct {
	mu        sync.Mutex
	drafts    []publish.Draft
	sites     []string
	clients   []publish.Client
	published publish.Published
	err       error
	ctxOK     bool
}

func (p *fakePublisher) Publish(ctx context.Context, s site.Site, d publish.Draft, c publish.Client) (publish.Published, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drafts = append(p.drafts, d)
	p.sites = append(p.sites, s.ID)
	p.clients = append(p.clients, c)
	_, hasDeadline := ctx.Deadline()
	p.ctxOK = ctx.Err() == nil && hasDeadline
	return p.published, p.err
}

func (p *fakePublisher) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.drafts)
}

func publishServer(t *testing.T, sessions server.Sessions, p server.Publisher, limit server.RateLimit) http.Handler {
	t.Helper()
	sites, err := site.Parse(strings.NewReader(twoSites))
	if err != nil {
		t.Fatal(err)
	}
	return apitest.Handler(t, server.New(sites, slog.New(slog.NewJSONHandler(io.Discard, nil)), server.Services{
		Sessions: sessions, Publisher: p, RateLimit: limit,
	}))
}

const draft = `{
  "page": "home",
  "changes": [
    {"path": "hero.title", "before": "Fisioterapia a domicilio", "after": "Fisioterapia en casa"},
    {"path": "plans.1.price", "before": 45, "after": 49.50},
    {"path": "services", "before": ["Masaje", "Punción seca"], "after": ["Punción seca", "Masaje", "Pilates"]}
  ]
}`

// post sends a Publish from the Site's own page, with a session cookie.
func post(t *testing.T, h http.Handler, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/_edity/api/publish", strings.NewReader(body))
	req.Host = "sandbox.example.com"
	req.Header.Set("Origin", "https://sandbox.example.com")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "edity_session", Value: "a-session"})
	if edit != nil {
		edit(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPublishAnswersItsCommit(t *testing.T) {
	p := &fakePublisher{published: publish.Published{
		SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 4, 3}, Conflicts: []string{"hero.title"},
	}}
	rec := post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), draft, nil)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"sha":"4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2","version":"1.4.3","conflicts":[{"path":"hero.title"}]}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if p.calls() != 1 || p.sites[0] != "edity-sandbox" || p.clients[0].Name != "Ana García" {
		t.Fatalf("published %d times, for %v by %v", p.calls(), p.sites, p.clients)
	}
	d := p.drafts[0]
	var paths []string
	for _, c := range d.Changes {
		paths = append(paths, c.Path+": "+c.Before.String()+" → "+c.After.String())
	}
	want := []string{
		`hero.title: Fisioterapia a domicilio → Fisioterapia en casa`,
		`plans.1.price: 45 → 49.5`,
		`services: ["Masaje", "Punción seca"] → ["Punción seca", "Masaje", "Pilates"]`,
	}
	if d.Page != "home" || !slices.Equal(paths, want) {
		t.Errorf("published %s %q, want home %q", d.Page, paths, want)
	}
	if !p.ctxOK {
		t.Error("the Publish ran without a deadline, or with the request's cancelled context")
	}

	// With every Change merged, the answer has no conflicts.
	p.published.Conflicts = nil
	rec = post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), draft, nil)
	if strings.Contains(rec.Body.String(), "conflicts") {
		t.Errorf("body = %s, want no conflicts", rec.Body)
	}
}

func TestAPublishGoesOnWhenTheBrowserGoesAway(t *testing.T) {
	p := &fakePublisher{published: publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 0, 1}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), draft, func(r *http.Request) { *r = *r.WithContext(ctx) })
	if p.calls() != 1 || !p.ctxOK {
		t.Error("the Publish's context ended with the request's")
	}
}

func TestWhatAPublishThatCommittedNothingAnswers(t *testing.T) {
	for name, c := range map[string]struct {
		err        error
		status     int
		body       string
		retryAfter string
	}{
		"every Change a Conflict": {
			err:    &publish.ConflictsError{Paths: []string{"hero.title", "services"}},
			status: http.StatusConflict, body: `{"conflicts":[{"path":"hero.title"},{"path":"services"}]}`,
		},
		"violations": {
			err:    &publish.ViolationsError{Violations: []fields.Violation{{Path: "hero.title", Limit: fields.MaxChars}, {Path: "nope", Limit: fields.Unknown}}},
			status: http.StatusUnprocessableEntity, body: `{"violations":[{"path":"hero.title","limit":"maxChars"},{"path":"nope","limit":"unknown"}]}`,
		},
		"a broken repository": {err: &publish.BrokenError{Reason: errors.New("no version")}, status: http.StatusInternalServerError},
		"no installation":     {err: github.ErrNotInstalled, status: http.StatusInternalServerError},
		"no main":             {err: fmt.Errorf("GET /repos/RMYPG/edity-sandbox/git/ref/heads/main: %w", github.ErrNotFound), status: http.StatusInternalServerError},
		"a refused App":       {err: &github.Error{Call: "createCommitOnBranch", Status: 200, Message: "Resource not accessible by integration"}, status: http.StatusInternalServerError},
		"main kept moving":    {err: publish.ErrBusy, status: http.StatusServiceUnavailable, retryAfter: "10"},
		"a rate limit":        {err: &github.Error{Call: "GET /x", Status: 403, RetryAfter: 1500 * time.Millisecond}, status: http.StatusServiceUnavailable, retryAfter: "2"},
		"GitHub down":         {err: &github.Error{Call: "GET /x", Status: 502}, status: http.StatusServiceUnavailable},
		"no answer":           {err: github.ErrUncertain, status: http.StatusServiceUnavailable},
		"no network":          {err: errors.New("dial tcp: i/o timeout"), status: http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, publishServer(t, cookieSessions{}, &fakePublisher{err: c.err}, server.RateLimit{}), draft, nil)
			if rec.Code != c.status {
				t.Errorf("status = %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			if c.body != "" && strings.TrimSpace(rec.Body.String()) != c.body {
				t.Errorf("body = %s, want %s", rec.Body, c.body)
			}
			if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, c.retryAfter)
			}
		})
	}
}

func TestAPublishWithoutASessionGets401(t *testing.T) {
	for name, sessions := range map[string]server.Sessions{"before login exists": server.NoSessions{}, "with no cookie": cookieSessions{}} {
		p := &fakePublisher{}
		rec := post(t, publishServer(t, sessions, p, server.RateLimit{}), draft, func(r *http.Request) {
			if name == "with no cookie" {
				r.Header.Del("Cookie")
			}
		})
		if rec.Code != http.StatusUnauthorized || p.calls() != 0 {
			t.Errorf("%s: status = %d, published %d times, want 401 and none", name, rec.Code, p.calls())
		}
	}
}

func TestAPublishFromAnotherOriginGets403(t *testing.T) {
	for _, origin := range []string{
		"", "null", "https://evil.example.com", "https://sandbox.example.com.evil.example.com",
		"https://sandbox.example.com:8443", "https://fisioindoor.example.com", "ftp://sandbox.example.com",
		"https://sandbox.example.com/page", "https://user@sandbox.example.com", "sandbox.example.com",
	} {
		p := &fakePublisher{}
		rec := post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), draft, func(r *http.Request) {
			r.Header.Set("Origin", origin)
			if origin == "" {
				r.Header.Del("Origin")
			}
		})
		if rec.Code != http.StatusForbidden || p.calls() != 0 {
			t.Errorf("Origin %q: status = %d, published %d times, want 403 and none", origin, rec.Code, p.calls())
		}
	}
	// The same origin, whatever the case of its host or its scheme.
	for _, origin := range []string{"https://Sandbox.Example.com", "http://sandbox.example.com"} {
		p := &fakePublisher{published: publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 0, 1}}}
		rec := post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), draft, func(r *http.Request) { r.Header.Set("Origin", origin) })
		if rec.Code != http.StatusAccepted {
			t.Errorf("Origin %q: status = %d, want 202", origin, rec.Code)
		}
	}
}

func TestWhatIsNotAPublishGets400(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":             `{"page": "home",`,
		"two JSON values":      `{"page": "home", "changes": []} {}`,
		"no page":              `{"changes": [{"path": "a", "before": "x", "after": "y"}]}`,
		"no changes":           `{"page": "home"}`,
		"empty changes":        `{"page": "home", "changes": []}`,
		"an unknown key":       `{"page": "home", "changes": [{"path": "a", "before": "x", "after": "y"}], "draft": 1}`,
		"a key twice":          `{"page": "home", "page": "about", "changes": [{"path": "a", "before": "x", "after": "y"}]}`,
		"a key in other case":  `{"Page": "home", "changes": [{"path": "a", "before": "x", "after": "y"}]}`,
		"a page as a number":   `{"page": 1, "changes": [{"path": "a", "before": "x", "after": "y"}]}`,
		"a page out of place":  `{"page": "../package", "changes": [{"path": "a", "before": "x", "after": "y"}]}`,
		"no path":              `{"page": "home", "changes": [{"before": "x", "after": "y"}]}`,
		"an empty path":        `{"page": "home", "changes": [{"path": "", "before": "x", "after": "y"}]}`,
		"a path with an end .": `{"page": "home", "changes": [{"path": "hero.", "before": "x", "after": "y"}]}`,
		"no after":             `{"page": "home", "changes": [{"path": "a", "before": "x"}]}`,
		"a null before":        `{"page": "home", "changes": [{"path": "a", "before": null, "after": "y"}]}`,
		"a true after":         `{"page": "home", "changes": [{"path": "a", "before": "x", "after": true}]}`,
		"a null in a List":     `{"page": "home", "changes": [{"path": "a", "before": [], "after": ["x", null]}]}`,
		"a Change's extra key": `{"page": "home", "changes": [{"path": "a", "before": "x", "after": "y", "origins": [0]}]}`,
		"one path twice":       `{"page": "home", "changes": [{"path": "a", "before": "x", "after": "y"}, {"path": "a", "before": "x", "after": "z"}]}`,
		"one inside another":   `{"page": "home", "changes": [{"path": "plans", "before": [], "after": []}, {"path": "plans.0.price", "before": 1, "after": 2}]}`,
	} {
		p := &fakePublisher{}
		rec := post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), body, nil)
		if rec.Code != http.StatusBadRequest || p.calls() != 0 {
			t.Errorf("%s: status = %d, published %d times, want 400 and none: %s", name, rec.Code, p.calls(), rec.Body)
		}
	}

	p := &fakePublisher{}
	rec := post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), draft, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	if rec.Code != http.StatusBadRequest || p.calls() != 0 {
		t.Errorf("as text/plain: status = %d, want 400", rec.Code)
	}
}

func TestATooLargePublishGets413(t *testing.T) {
	p := &fakePublisher{}
	huge := `{"page": "home", "changes": [{"path": "a", "before": "x", "after": "` + strings.Repeat("a", 1<<20) + `"}]}`
	rec := post(t, publishServer(t, cookieSessions{}, p, server.RateLimit{}), huge, nil)
	if rec.Code != http.StatusRequestEntityTooLarge || p.calls() != 0 {
		t.Errorf("status = %d, published %d times, want 413 and none", rec.Code, p.calls())
	}
}

func TestEachSitePublishesWithinItsRateLimit(t *testing.T) {
	p := &fakePublisher{published: publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 0, 1}}}
	h := publishServer(t, cookieSessions{}, p, server.RateLimit{Burst: 2, Every: time.Hour})

	// A request that is not a Publish spends nothing.
	post(t, h, `{}`, nil)
	for i := range 2 {
		if rec := post(t, h, draft, nil); rec.Code != http.StatusAccepted {
			t.Fatalf("Publish %d: status = %d, want 202", i+1, rec.Code)
		}
	}
	rec := post(t, h, draft, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "3600" {
		t.Errorf("third Publish: status = %d, Retry-After = %q, want 429 after 3600", rec.Code, rec.Header().Get("Retry-After"))
	}
	if p.calls() != 2 {
		t.Errorf("published %d times, want 2", p.calls())
	}

	// Another Site has its own limit.
	rec = post(t, h, draft, func(r *http.Request) {
		r.Host = "fisioindoor.example.com"
		r.Header.Set("Origin", "https://fisioindoor.example.com")
	})
	if rec.Code != http.StatusAccepted {
		t.Errorf("another Site's Publish: status = %d, want 202", rec.Code)
	}
}

// The 422 body is the one the Editor reads, with core's rule names.
func TestViolationsAreWrittenAsTheEditorReadsThem(t *testing.T) {
	b, err := json.Marshal(fields.Violation{Path: "plans.1.features", Limit: fields.MaxItems})
	if err != nil || string(b) != `{"path":"plans.1.features","limit":"maxItems"}` {
		t.Errorf("a Violation is %s, %v", b, err)
	}
}

// fakeNotifier sends each Publish it is asked to note on noted, and answers
// err once release is closed.
type fakeNotifier struct {
	noted   chan publish.Published
	release chan struct{}
	err     error
}

func (n *fakeNotifier) NoteDeveloperCommits(ctx context.Context, s site.Site, p publish.Published) error {
	if _, ok := ctx.Deadline(); !ok || s.ID != "edity-sandbox" {
		return errors.New("no deadline, or another Site")
	}
	n.noted <- p
	<-n.release
	if ctx.Err() != nil {
		return errors.New("the note's context ended with the request's")
	}
	return n.err
}

func TestAPublishIsAnsweredBeforeTheDeveloperCommitsItShipsAreNoted(t *testing.T) {
	sites, err := site.Parse(strings.NewReader(twoSites))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	p := &fakePublisher{err: &publish.ConflictsError{Paths: []string{"hero.title"}}}
	n := &fakeNotifier{noted: make(chan publish.Published, 2), release: make(chan struct{}), err: errors.New("GitHub could not be reached")}
	h := apitest.Handler(t, server.New(sites, slog.New(slog.NewJSONHandler(logs, nil)), server.Services{
		Sessions: cookieSessions{}, Publisher: p, Notifier: n,
	}))

	// A Publish that commits nothing ships nothing.
	if rec := post(t, h, draft, nil); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}

	// A note that has not ended, and then fails, does not hold up or fail
	// the Publish, even once the browser has gone away.
	p.mu.Lock()
	p.err, p.published = nil, publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 0, 1}}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	rec := post(t, h, draft, func(r *http.Request) { *r = *r.WithContext(ctx) })
	cancel()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}
	select {
	case noted := <-n.noted:
		if noted.SHA != p.published.SHA || noted.Version != p.published.Version {
			t.Errorf("noted %+v, want the Publish %+v", noted, p.published)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Publish's commits were not noted")
	}
	close(n.release)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "Developer commits not noted") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), `"error":"GitHub could not be reached"`) {
		t.Errorf("the failed note was not logged with its error: %s", logs.String())
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
