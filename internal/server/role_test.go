package server_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/oidctest"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/server"
	"github.com/RMYPG/edity-server/internal/site"
)

// anasRole is Ana's role assignment on edity-sandbox.
var anasRole = oidctest.Authorization{UserID: ana.Subject, ProjectID: oidctest.ProjectID, Roles: []string{"site:edity-sandbox"}}

// withRole is a browser where Ana logged in, while Zitadel says she holds the
// Site's role.
func (lt *loginTest) withRole() *browser {
	lt.t.Helper()
	lt.zitadel.SetAuthorizations(anasRole)
	b := lt.browser("sandbox.example.com")
	if rec := b.logIn(ana, "/_edity/login"); rec.Code != http.StatusSeeOther {
		lt.t.Fatalf("logging in: status = %d, want %d: %s", rec.Code, http.StatusSeeOther, rec.Body)
	}
	return b
}

// sessionEnded fails t unless rec cleared both of the session's cookies and
// the browser is logged out.
func sessionEnded(t *testing.T, b *browser, rec *httptest.ResponseRecorder) {
	t.Helper()
	for name, path := range map[string]string{"edity_session": "/_edity/api", "edity": "/"} {
		if c := responseCookie(t, rec, name); c == nil || c.MaxAge >= 0 || c.Path != path {
			t.Errorf("cookie %s = %+v, want it cleared on %s", name, c, path)
		}
	}
	if rec := b.session(); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session: status = %d, want 401: %s", rec.Code, rec.Body)
	}
}

func TestAPublishGoesThroughWhileTheClientHoldsTheRole(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()

	rec := b.publish()

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
	if lt.publisher.calls() != 1 {
		t.Errorf("committed %d times, want once", lt.publisher.calls())
	}
	if rec := b.session(); rec.Code != http.StatusOK {
		t.Errorf("GET session: status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// The request's exact JSON is what Zitadel's proto and Connect's JSON mapping
// say: proto/zitadel/authorization/v2 at v4.19.2.
func TestAPublishAsksZitadelForTheUsersRoleInTheProject(t *testing.T) {
	lt := newLoginTest(t)

	lt.withRole().publish()

	requests := lt.zitadel.ListRequests()
	if len(requests) != 1 {
		t.Fatalf("%d ListAuthorizations calls, want 1", len(requests))
	}
	var got any
	if err := json.Unmarshal([]byte(requests[0]), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"filters": []any{
		map[string]any{"inUserIds": map[string]any{"ids": []any{ana.Subject}}},
		map[string]any{"projectId": map[string]any{"id": oidctest.ProjectID}},
		map[string]any{"roleKey": map[string]any{"key": "site:edity-sandbox", "method": "TEXT_FILTER_METHOD_EQUALS"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the request = %s, want %v", requests[0], want)
	}
}

func TestAPublishAfterTheRoleWasRemovedEndsTheSession(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	lt.zitadel.SetAuthorizations()

	rec := b.publish()

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusUnauthorized, rec.Body)
	}
	if lt.publisher.calls() != 0 {
		t.Errorf("committed %d times, want none", lt.publisher.calls())
	}
	sessionEnded(t, b, rec)
}

func TestAPublishNeverRemembersZitadelsAnswer(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Fatalf("first Publish: status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
	lt.zitadel.SetAuthorizations()

	if rec := b.publish(); rec.Code != http.StatusUnauthorized {
		t.Errorf("second Publish: status = %d, want %d: %s", rec.Code, http.StatusUnauthorized, rec.Body)
	}
	if got := len(lt.zitadel.ListRequests()); got != 2 {
		t.Errorf("%d ListAuthorizations calls, want one per Publish", got)
	}
}

func TestAPublishEndsTheSessionUnlessAnActiveAuthorizationHoldsTheRole(t *testing.T) {
	deactivated := anasRole
	deactivated.Inactive = true
	inAnotherProject := anasRole
	inAnotherProject.ProjectID = "340000000000000999"
	someoneElses := anasRole
	someoneElses.UserID = "9999"
	for name, authorization := range map[string]oidctest.Authorization{
		"deactivated":         deactivated,
		"another Site's role": {UserID: ana.Subject, ProjectID: oidctest.ProjectID, Roles: []string{"site:fisioindoor"}},
		"a longer role":       {UserID: ana.Subject, ProjectID: oidctest.ProjectID, Roles: []string{"site:edity-sandbox-2"}},
		"no roles":            {UserID: ana.Subject, ProjectID: oidctest.ProjectID},
		"in another project":  inAnotherProject,
		"someone else's":      someoneElses,
	} {
		for _, ignoreFilters := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, ignoring filters %v", name, ignoreFilters), func(t *testing.T) {
				lt := newLoginTest(t)
				b := lt.withRole()
				lt.zitadel.IgnoreFilters = ignoreFilters
				lt.zitadel.SetAuthorizations(authorization)

				rec := b.publish()

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusUnauthorized, rec.Body)
				}
				if lt.publisher.calls() != 0 {
					t.Errorf("committed %d times, want none", lt.publisher.calls())
				}
				sessionEnded(t, b, rec)
			})
		}
	}
}

func TestAPublishGoesThroughOnAnActiveAuthorizationAmongOthers(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	deactivated := anasRole
	deactivated.Inactive = true
	bothRoles := anasRole
	bothRoles.Roles = []string{"site:fisioindoor", "site:edity-sandbox"}
	lt.zitadel.SetAuthorizations(deactivated, bothRoles)

	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
}

// sessionKept fails t unless rec left the session as it was, and the browser
// is still logged in.
func sessionKept(t *testing.T, b *browser, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, name := range []string{"edity_session", "edity"} {
		if c := responseCookie(t, rec, name); c != nil {
			t.Errorf("the response changes the cookie %s: %+v", name, c)
		}
	}
	if rec := b.session(); rec.Code != http.StatusOK {
		t.Errorf("GET session: status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestAPublishGets503AndCommitsNothingWhenZitadelCanNotAnswer(t *testing.T) {
	for name, fail := range map[string]func(z *oidctest.Provider){
		"Zitadel is down":                              func(z *oidctest.Provider) { z.Stop() },
		"the token endpoint fails":                     func(z *oidctest.Provider) { z.TokenStatus = http.StatusInternalServerError },
		"the token endpoint refuses":                   func(z *oidctest.Provider) { z.TokenStatus = http.StatusBadRequest },
		"the list fails":                               func(z *oidctest.Provider) { z.ListStatus = http.StatusInternalServerError },
		"the list is refused":                          func(z *oidctest.Provider) { z.ListStatus = http.StatusForbidden },
		"the list is not there":                        func(z *oidctest.Provider) { z.ListStatus = http.StatusNotFound },
		"an answer that is not JSON":                   func(z *oidctest.Provider) { z.ListBody = "<html>Bad gateway</html>" },
		"an answer that is no list":                    func(z *oidctest.Provider) { z.ListBody = "{}" },
		"a list without the service user's permission": func(z *oidctest.Provider) { z.Unreadable = true },
	} {
		t.Run(name, func(t *testing.T) {
			lt := newLoginTest(t)
			b := lt.withRole()
			fail(lt.zitadel)

			rec := b.publish()

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
			}
			if lt.publisher.calls() != 0 {
				t.Errorf("committed %d times, want none", lt.publisher.calls())
			}
			sessionKept(t, b, rec)
		})
	}
}

func TestTheServiceUsersTokenFollowsNoRedirect(t *testing.T) {
	var hits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(elsewhere.Close)
	lt := newLoginTest(t)
	b := lt.withRole()
	lt.zitadel.ListRedirect = elsewhere.URL + "/stolen"

	rec := b.publish()

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
	if hits.Load() != 0 {
		t.Error("the request went to where Zitadel's answer redirected it")
	}
}

func TestAPublishGets503WhenZitadelGoesDownAfterItsFirstAnswer(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Fatalf("first Publish: status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
	lt.zitadel.Stop()

	rec := b.publish()

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("second Publish: status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
	if lt.publisher.calls() != 1 {
		t.Errorf("committed %d times, want only the first Publish", lt.publisher.calls())
	}
}

func TestAPublishGoesThroughAgainOnceZitadelAnswers(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	lt.zitadel.ListStatus = http.StatusInternalServerError
	if rec := b.publish(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("while Zitadel fails: status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
	lt.zitadel.ListStatus = 0

	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Errorf("once it answers: status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
}

func TestPublishesShareAnAccessToken(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()

	for i := range 3 {
		if rec := b.publish(); rec.Code != http.StatusAccepted {
			t.Fatalf("Publish %d: status = %d, want %d: %s", i+1, rec.Code, http.StatusAccepted, rec.Body)
		}
	}

	if got := lt.zitadel.AccessTokensIssued(); got != 1 {
		t.Errorf("%d access tokens issued, want 1", got)
	}
	if got := len(lt.zitadel.ListRequests()); got != 3 {
		t.Errorf("%d ListAuthorizations calls, want 3", got)
	}
}

func TestAPublishGetsANewAccessTokenShortlyBeforeTheOldExpires(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	b.publish()

	lt.now = lt.now.Add(oidctest.AccessTokenLifetime - 2*time.Minute)
	b.publish()
	if got := lt.zitadel.AccessTokensIssued(); got != 1 {
		t.Errorf("two minutes before it expires: %d access tokens issued, want 1", got)
	}

	lt.now = lt.now.Add(90 * time.Second)
	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Errorf("half a minute before it expires: status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
	if got := lt.zitadel.AccessTokensIssued(); got != 2 {
		t.Errorf("half a minute before it expires: %d access tokens issued, want 2", got)
	}
}

func TestAPublishGetsANewAccessTokenOnceTheOldExpired(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	b.publish()

	lt.now = lt.now.Add(oidctest.AccessTokenLifetime + time.Minute)
	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}

	if got := lt.zitadel.AccessTokensIssued(); got != 2 {
		t.Errorf("%d access tokens issued, want 2", got)
	}
}

// Zitadel may stop honouring a token sooner than it said, as when a token's
// signing key is rotated or its service user is reset.
func TestAPublishGetsANewAccessTokenWhenZitadelRefusesTheOld(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	b.publish()
	lt.zitadel.RevokeAccessTokens()

	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}

	if got := lt.zitadel.AccessTokensIssued(); got != 2 {
		t.Errorf("%d access tokens issued, want 2", got)
	}
}

func TestAPublishGets503WhenZitadelKeepsRefusingAccessTokens(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	lt.zitadel.ListStatus = http.StatusUnauthorized

	rec := b.publish()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
	if lt.publisher.calls() != 0 {
		t.Errorf("committed %d times, want none", lt.publisher.calls())
	}
	if got := len(lt.zitadel.ListRequests()); got != 2 {
		t.Errorf("%d ListAuthorizations calls, want 2: one, and one with a new token", got)
	}
	sessionKept(t, b, rec)
}

func TestAPublishWithAKeyZitadelDoesNotKnowGets503(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	lt := newLoginTest(t)
	lt.serviceKey = other
	lt.start()
	b := lt.withRole()

	if rec := b.publish(); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
	if got := lt.zitadel.AccessTokensIssued(); got != 0 {
		t.Errorf("%d access tokens issued, want none", got)
	}
}

// roleSessions is cookieSessions, whose Client holds the Site's role as told.
type roleSessions struct {
	cookieSessions

	mu       sync.Mutex
	held     bool
	err      error
	calls    int
	ended    int
	deadline time.Time
	alive    bool
}

func (s *roleSessions) HoldsRole(ctx context.Context, _ site.Site, _ publish.Client) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.deadline, _ = ctx.Deadline()
	s.alive = ctx.Err() == nil
	return s.held, s.err
}

func (s *roleSessions) Logout(http.ResponseWriter, *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended++
}

func (s *roleSessions) hold(held bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held, s.err = held, err
}

func TestAPublishIsCheckedOnlyOnceItIsValidated(t *testing.T) {
	for name, send := range map[string]struct {
		body string
		edit func(*http.Request)
	}{
		"not a Draft":     {body: `{"page":"home"}`},
		"not JSON":        {body: draft, edit: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		"from elsewhere":  {body: draft, edit: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.com") }},
		"without session": {body: draft, edit: func(r *http.Request) { r.Header.Del("Cookie") }},
		"too large":       {body: `{"page":"` + strings.Repeat("a", 1<<20) + `","changes":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			sessions := &roleSessions{held: true}

			rec := post(t, publishServer(t, sessions, &fakePublisher{}, server.RateLimit{}), send.body, send.edit)

			if rec.Code < 400 || rec.Code >= 500 {
				t.Errorf("status = %d, want a 4xx: %s", rec.Code, rec.Body)
			}
			if sessions.calls != 0 {
				t.Errorf("asked about the role %d times, want none", sessions.calls)
			}
		})
	}
}

func TestAPublishTheRoleCheckRefusesTakesNothingFromTheRateLimit(t *testing.T) {
	sessions := &roleSessions{}
	p := &fakePublisher{published: publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 4, 3}}}
	h := publishServer(t, sessions, p, server.RateLimit{Burst: 1, Every: time.Hour})

	if rec := post(t, h, draft, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("role gone: status = %d, want 401: %s", rec.Code, rec.Body)
	}
	sessions.hold(false, errors.New("Zitadel is down"))
	if rec := post(t, h, draft, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Zitadel down: status = %d, want 503: %s", rec.Code, rec.Body)
	}
	if p.calls() != 0 {
		t.Fatalf("committed %d times, want none", p.calls())
	}

	sessions.hold(true, nil)
	if rec := post(t, h, draft, nil); rec.Code != http.StatusAccepted {
		t.Errorf("role held: status = %d, want 202: %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, draft, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the next Publish: status = %d, want 429: %s", rec.Code, rec.Body)
	}
}

func TestAPublishWithoutTheRoleGets401EvenPastTheRateLimit(t *testing.T) {
	sessions := &roleSessions{held: true}
	p := &fakePublisher{published: publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 4, 3}}}
	h := publishServer(t, sessions, p, server.RateLimit{Burst: 1, Every: time.Hour})
	post(t, h, draft, nil)
	sessions.hold(false, nil)

	rec := post(t, h, draft, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: %s", rec.Code, rec.Body)
	}
	if sessions.ended != 1 {
		t.Errorf("ended the session %d times, want once", sessions.ended)
	}
}

func TestTheRoleCheckIsBoundedShortlyWithinThePublish(t *testing.T) {
	sessions := &roleSessions{held: true}
	p := &fakePublisher{published: publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 4, 3}}}

	post(t, publishServer(t, sessions, p, server.RateLimit{}), draft, nil)

	left := time.Until(sessions.deadline)
	if left <= 0 || left > 5*time.Second {
		t.Errorf("the check had %v left, want at most 5 seconds", left)
	}
	if !sessions.deadline.Before(p.deadline) {
		t.Errorf("the check's deadline %v is not before the Publish's %v", sessions.deadline, p.deadline)
	}
	if time.Until(p.deadline) > time.Minute {
		t.Errorf("the Publish had %v left, want it inside the server's write timeout", time.Until(p.deadline))
	}
}

func TestTheRoleCheckGoesOnWhenTheBrowserGoesAway(t *testing.T) {
	sessions := &roleSessions{held: true}
	p := &fakePublisher{published: publish.Published{SHA: "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2", Version: release.Version{1, 4, 3}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	post(t, publishServer(t, sessions, p, server.RateLimit{}), draft, func(r *http.Request) { *r = *r.WithContext(ctx) })

	if !sessions.alive || !p.ctxOK {
		t.Errorf("the check's context alive = %v, the Publish's = %v, want both", sessions.alive, p.ctxOK)
	}
}
