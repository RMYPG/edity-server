package server_test

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/apitest"
	"github.com/RMYPG/edity-server/internal/login"
	"github.com/RMYPG/edity-server/internal/oidctest"
	"github.com/RMYPG/edity-server/internal/server"
	"github.com/RMYPG/edity-server/internal/site"
)

const clientID = "337@edity"

var sessionKey = []byte("0123456789abcdef0123456789abcdef")

// ana is a Client of edity-sandbox.
var ana = oidctest.Person{Subject: "2200", Name: "Ana García", Locale: "es", Roles: []string{"site:edity-sandbox"}}

// loginTest is a server of the two Sites, whose Clients log in at a fake
// Zitadel.
type loginTest struct {
	t       *testing.T
	zitadel *oidctest.Provider
	now     time.Time
	key     []byte
	handler http.Handler
}

func newLoginTest(t *testing.T) *loginTest {
	t.Helper()
	lt := &loginTest{t: t, zitadel: oidctest.New(t, clientID), now: time.Now(), key: sessionKey}
	lt.start()
	return lt
}

// start builds the server from nothing but its configuration, as a new
// process does.
func (lt *loginTest) start() {
	lt.t.Helper()
	sites, err := site.Parse(strings.NewReader(twoSites))
	if err != nil {
		lt.t.Fatal(err)
	}
	zitadel, err := login.New(login.Options{
		Issuer: lt.zitadel.Issuer(), ClientID: clientID, Key: lt.key,
		Now: func() time.Time { return lt.now },
	})
	if err != nil {
		lt.t.Fatal(err)
	}
	lt.handler = apitest.Handler(lt.t, server.New(sites, slog.New(slog.NewJSONHandler(io.Discard, nil)), server.Services{
		Sessions: zitadel, Login: zitadel, Publisher: &fakePublisher{}, Tracker: &fakeTracker{},
	}))
}

// A browser keeps cookies as a real one does, by host, path and expiry.
type browser struct {
	lt   *loginTest
	host string
	jar  *cookiejar.Jar
}

func (lt *loginTest) browser(host string) *browser {
	lt.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		lt.t.Fatal(err)
	}
	return &browser{lt: lt, host: host, jar: jar}
}

// send makes a request to the Site, with the browser's cookies for it.
func (b *browser) send(method, target string, edit func(*http.Request)) *httptest.ResponseRecorder {
	b.lt.t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		b.lt.t.Fatal(err)
	}
	u.Scheme, u.Host = "https", b.host
	req := httptest.NewRequestWithContext(b.lt.t.Context(), method, u.RequestURI(), nil)
	req.Host = b.host
	for _, c := range b.jar.Cookies(u) {
		req.AddCookie(c)
	}
	if edit != nil {
		edit(req)
	}
	rec := httptest.NewRecorder()
	b.lt.handler.ServeHTTP(rec, req)
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	b.jar.SetCookies(u, res.Cookies())
	return rec
}

func (b *browser) get(target string) *httptest.ResponseRecorder {
	b.lt.t.Helper()
	return b.send(http.MethodGet, target, nil)
}

func TestLoginRedirectsToZitadelAndBackToTheSite(t *testing.T) {
	lt := newLoginTest(t)

	rec := lt.browser("sandbox.example.com").get("/_edity/login")

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusFound, rec.Body)
	}
	callback := lt.zitadel.LogIn(t, rec.Header().Get("Location"), ana)
	if want := "https://sandbox.example.com/_edity/api/auth/callback?"; !strings.HasPrefix(callback, want) {
		t.Errorf("Zitadel sends the browser back to %s, want %s…", callback, want)
	}
}

// logIn goes through a whole login, and returns the callback's answer.
func (b *browser) logIn(person oidctest.Person, target string) *httptest.ResponseRecorder {
	b.lt.t.Helper()
	rec := b.get(target)
	if rec.Code != http.StatusFound {
		b.lt.t.Fatalf("GET %s: status = %d, want %d: %s", target, rec.Code, http.StatusFound, rec.Body)
	}
	return b.get(b.lt.zitadel.LogIn(b.lt.t, rec.Header().Get("Location"), person))
}

// session asks who is logged in, as the Editor does from the page.
func (b *browser) session() *httptest.ResponseRecorder {
	b.lt.t.Helper()
	return b.get("/_edity/api/session")
}

func responseCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestALoginWithTheSitesRoleSetsTheSessionAndTheMarker(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")

	rec := b.logIn(ana, "/_edity/login?return=/precios")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusSeeOther, rec.Body)
	}
	if got := rec.Header().Get("Location"); got != "/precios" {
		t.Errorf("Location = %q, want /precios", got)
	}
	const thirtyDays = 30 * 24 * 60 * 60
	session := responseCookie(t, rec, "edity_session")
	if session == nil {
		t.Fatal("no session cookie")
	}
	if session.Path != "/_edity/api" || !session.HttpOnly || !session.Secure || session.MaxAge != thirtyDays || session.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie = %+v, want HttpOnly, Secure and SameSite=Strict on /_edity/api for 30 days", session)
	}
	marker := responseCookie(t, rec, "edity")
	if marker == nil {
		t.Fatal("no marker cookie")
	}
	if marker.Value != "1" || marker.Path != "/" || marker.HttpOnly || !marker.Secure || marker.MaxAge != thirtyDays {
		t.Errorf("marker cookie = %+v, want edity=1 on /, readable by the page, for 30 days", marker)
	}

	rec = b.session()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session: status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if want := `{"name":"Ana García","locale":"es","site":"edity-sandbox"}` + "\n"; rec.Body.String() != want {
		t.Errorf("session = %s, want %s", rec.Body, want)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// noSession fails t unless rec sets no session and no marker.
func noSession(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, name := range []string{"edity_session", "edity"} {
		if c := responseCookie(t, rec, name); c != nil && c.MaxAge >= 0 {
			t.Errorf("the response sets the cookie %s", name)
		}
	}
}

func TestALoginWithoutTheSitesRoleGetsNoSession(t *testing.T) {
	allRoles := func(claims map[string]any) {
		claims["urn:zitadel:iam:org:project:roles"] = map[string]map[string]string{
			"site:fisioindoor": {"190000000000000001": "edity.zitadel.example.com"},
		}
	}
	for name, c := range map[string]struct {
		roles []string
		edit  func(map[string]any)
	}{
		"no role":             {roles: nil},
		"another Site's role": {roles: []string{"site:fisioindoor"}},
		// A project can assert every role whatever the scope asked for.
		"another Site's role asserted": {roles: []string{"site:fisioindoor"}, edit: allRoles},
	} {
		t.Run(name, func(t *testing.T) {
			lt := newLoginTest(t)
			lt.zitadel.EditUserInfo = c.edit
			b := lt.browser("sandbox.example.com")
			person := ana
			person.Roles = c.roles

			rec := b.logIn(person, "/_edity/login")

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusForbidden, rec.Body)
			}
			noSession(t, rec)
			if rec := b.session(); rec.Code != http.StatusUnauthorized {
				t.Errorf("GET session: status = %d, want 401: %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestALoginWithoutTheRoleEndsTheSessionTheBrowserHad(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	b.logIn(ana, "/_edity/login")
	person := ana
	person.Roles = nil

	if rec := b.logIn(person, "/_edity/login"); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	if rec := b.session(); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session: status = %d, want 401: %s", rec.Code, rec.Body)
	}
}

func TestASessionSurvivesARestartOfTheServer(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	b.logIn(ana, "/_edity/login")

	// The new server shares nothing with the old but its key, and has no
	// need of Zitadel to know the session.
	lt.zitadel.Stop()
	lt.start()

	rec := b.session()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session after a restart: status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Ana García") {
		t.Errorf("session = %s, want Ana's", rec.Body)
	}
}

func TestASessionIsGoneIfTheKeyChanges(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	b.logIn(ana, "/_edity/login")

	lt.key = []byte("fedcba9876543210fedcba9876543210")
	lt.start()

	if rec := b.session(); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session: status = %d, want 401: %s", rec.Code, rec.Body)
	}
}

func TestASessionLastsThirtyDays(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	b.logIn(ana, "/_edity/login")

	lt.now = lt.now.Add(30*24*time.Hour - time.Minute)
	if rec := b.session(); rec.Code != http.StatusOK {
		t.Errorf("GET session a minute before it expires: status = %d, want 200", rec.Code)
	}

	lt.now = lt.now.Add(time.Minute)
	if rec := b.session(); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session once it expired: status = %d, want 401", rec.Code)
	}
}

func TestASessionIsOnlyForItsOwnSite(t *testing.T) {
	lt := newLoginTest(t)
	sealed := responseCookie(t, lt.browser("sandbox.example.com").logIn(ana, "/_edity/login"), "edity_session").Value

	rec := lt.browser("fisioindoor.example.com").send(http.MethodGet, "/_edity/api/session", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "edity_session", Value: sealed})
	})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session on another Site: status = %d, want 401: %s", rec.Code, rec.Body)
	}
}

func TestWhatIsNotASessionGets401(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	started := b.get("/_edity/login")
	attempt := responseCookie(t, started, "edity_login").Value
	sealed := responseCookie(t, b.logIn(ana, "/_edity/login"), "edity_session").Value

	for name, value := range map[string]string{
		"garbage":     "not-a-session",
		"empty":       "",
		"tampered":    sealed[:len(sealed)-2] + "AA",
		"a login's":   attempt,
		"unencrypted": base64.RawURLEncoding.EncodeToString([]byte(`{"site":"edity-sandbox","name":"Eve","locale":"es","expires":"2999-01-01T00:00:00Z"}`)),
	} {
		rec := lt.browser("sandbox.example.com").send(http.MethodGet, "/_edity/api/session", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "edity_session", Value: value})
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401: %s", name, rec.Code, rec.Body)
		}
	}
}

// started begins a login, and returns the URL of Zitadel's login.
func (b *browser) started() string {
	b.lt.t.Helper()
	rec := b.get("/_edity/login")
	if rec.Code != http.StatusFound {
		b.lt.t.Fatalf("GET login: status = %d, want %d: %s", rec.Code, http.StatusFound, rec.Body)
	}
	return rec.Header().Get("Location")
}

// withQuery is target with its query's name set to value, or removed when
// value is empty.
func withQuery(t *testing.T, target, name, value string) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Del(name)
	if value != "" {
		q.Set(name, value)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func TestWhatIsNotTheEndOfThisBrowsersLoginGetsNoSession(t *testing.T) {
	const callback = "/_edity/api/auth/callback"
	forged := func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
		return b.get(lt.zitadel.LogIn(t, b.started(), ana))
	}
	for name, finish := range map[string]func(lt *loginTest, b *browser) *httptest.ResponseRecorder{
		"no login started": func(_ *loginTest, b *browser) *httptest.ResponseRecorder {
			return b.get(callback + "?code=a-code&state=a-state")
		},
		"another state": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			return b.get(withQuery(t, lt.zitadel.LogIn(t, b.started(), ana), "state", "another"))
		},
		"no state": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			return b.get(withQuery(t, lt.zitadel.LogIn(t, b.started(), ana), "state", ""))
		},
		"no code": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			return b.get(withQuery(t, lt.zitadel.LogIn(t, b.started(), ana), "code", ""))
		},
		"cancelled at Zitadel": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			return b.get(lt.zitadel.Cancel(t, b.started()))
		},
		"a code Zitadel refuses": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			return b.get(withQuery(t, lt.zitadel.LogIn(t, b.started(), ana), "code", "not-the-code"))
		},
		"another browser's callback": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			b.started()
			return b.get(lt.zitadel.LogIn(t, lt.browser("sandbox.example.com").started(), ana))
		},
		"started too long ago": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			authorize := b.started()
			lt.now = lt.now.Add(10 * time.Minute)
			return b.get(lt.zitadel.LogIn(t, authorize, ana))
		},
		"started on another Site": func(lt *loginTest, _ *browser) *httptest.ResponseRecorder {
			sandbox := lt.browser("sandbox.example.com")
			authorize := sandbox.started()
			attempt := jarCookie(t, sandbox, callback, "edity_login")
			return lt.browser("fisioindoor.example.com").send(http.MethodGet, lt.zitadel.LogIn(t, authorize, ana), func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: "edity_login", Value: attempt})
			})
		},
		"an ID token signed with another key": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			lt.zitadel.Forge = true
			return forged(lt, b)
		},
		"another login's nonce": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			lt.zitadel.EditIDToken = func(c map[string]any) { c["nonce"] = "another" }
			return forged(lt, b)
		},
		"an ID token for another client": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			lt.zitadel.EditIDToken = func(c map[string]any) { c["aud"] = []string{"another@edity"} }
			return forged(lt, b)
		},
		"an ID token from another issuer": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			lt.zitadel.EditIDToken = func(c map[string]any) { c["iss"] = "https://evil.example.com" }
			return forged(lt, b)
		},
		"an expired ID token": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			lt.zitadel.EditIDToken = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }
			return forged(lt, b)
		},
		"userinfo about someone else": func(lt *loginTest, b *browser) *httptest.ResponseRecorder {
			lt.zitadel.EditUserInfo = func(c map[string]any) { c["sub"] = "9999" }
			return forged(lt, b)
		},
	} {
		t.Run(name, func(t *testing.T) {
			lt := newLoginTest(t)
			b := lt.browser("sandbox.example.com")

			rec := finish(lt, b)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body)
			}
			noSession(t, rec)
			if rec := b.session(); rec.Code != http.StatusUnauthorized {
				t.Errorf("GET session: status = %d, want 401: %s", rec.Code, rec.Body)
			}
		})
	}
}

// jarCookie is the value of the browser's cookie name at path.
func jarCookie(t *testing.T, b *browser, path, name string) string {
	t.Helper()
	for _, c := range b.jar.Cookies(&url.URL{Scheme: "https", Host: b.host, Path: path}) {
		if c.Name == name {
			return c.Value
		}
	}
	t.Fatalf("the browser has no cookie %s at %s", name, path)
	return ""
}

func TestALoginCanBeFinishedOnlyOnce(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	callback := lt.zitadel.LogIn(t, b.started(), ana)

	rec := b.get(callback)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusSeeOther, rec.Body)
	}
	if c := responseCookie(t, rec, "edity_login"); c == nil || c.MaxAge >= 0 {
		t.Errorf("the login's cookie is not cleared: %+v", c)
	}
	if rec := b.get(callback); rec.Code != http.StatusBadRequest {
		t.Errorf("the same callback again: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestALoginGets503WhenZitadelCanNotBeReached(t *testing.T) {
	t.Run("starting", func(t *testing.T) {
		lt := newLoginTest(t)
		lt.zitadel.Stop()

		if rec := lt.browser("sandbox.example.com").get("/_edity/login"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
		}
	})
	t.Run("finishing", func(t *testing.T) {
		lt := newLoginTest(t)
		b := lt.browser("sandbox.example.com")
		callback := lt.zitadel.LogIn(t, b.started(), ana)
		lt.zitadel.Stop()

		rec := b.get(callback)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
		}
		noSession(t, rec)
	})
	t.Run("failing to exchange the code", func(t *testing.T) {
		lt := newLoginTest(t)
		b := lt.browser("sandbox.example.com")
		lt.zitadel.TokenStatus = http.StatusInternalServerError

		rec := b.get(lt.zitadel.LogIn(t, b.started(), ana))

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
		}
		noSession(t, rec)
	})
}

// fromTheSite makes a request come from one of the Site's own pages.
func fromTheSite(r *http.Request) {
	r.Header.Set("Origin", "https://sandbox.example.com")
}

func TestLogoutEndsTheSessionAndClearsBothCookies(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	b.logIn(ana, "/_edity/login")

	rec := b.send(http.MethodPost, "/_edity/api/auth/logout", fromTheSite)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNoContent, rec.Body)
	}
	if c := responseCookie(t, rec, "edity_session"); c == nil || c.MaxAge >= 0 || c.Path != "/_edity/api" {
		t.Errorf("session cookie = %+v, want it cleared on /_edity/api", c)
	}
	if c := responseCookie(t, rec, "edity"); c == nil || c.MaxAge >= 0 || c.Path != "/" {
		t.Errorf("marker cookie = %+v, want it cleared on /", c)
	}
	if rec := b.session(); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session: status = %d, want 401: %s", rec.Code, rec.Body)
	}
}

func TestLogoutClearsTheCookiesWithoutASessionToo(t *testing.T) {
	lt := newLoginTest(t)

	rec := lt.browser("sandbox.example.com").send(http.MethodPost, "/_edity/api/auth/logout", fromTheSite)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNoContent, rec.Body)
	}
	for _, name := range []string{"edity_session", "edity"} {
		if c := responseCookie(t, rec, name); c == nil || c.MaxAge >= 0 {
			t.Errorf("cookie %s = %+v, want it cleared", name, c)
		}
	}
}

func TestEveryAPICallFromAnotherOriginIsRejected(t *testing.T) {
	foreign := []string{
		"https://evil.example.com", "null", "https://fisioindoor.example.com", "http://sandbox.example.com",
	}
	for _, op := range apitest.Operations(t) {
		if !strings.HasPrefix(op.Path, "/_edity/api/") || op.ID == "finishLogin" {
			continue
		}
		t.Run(op.ID, func(t *testing.T) {
			lt := newLoginTest(t)
			b := lt.browser("sandbox.example.com")
			b.logIn(ana, "/_edity/login")

			for _, origin := range foreign {
				rec := b.send(op.Method, op.Example, func(r *http.Request) { r.Header.Set("Origin", origin) })
				if rec.Code != http.StatusForbidden {
					t.Errorf("Origin %s: status = %d, want %d: %s", origin, rec.Code, http.StatusForbidden, rec.Body)
				}
			}

			// Nothing happened: not even a logout.
			if rec := b.session(); rec.Code != http.StatusOK {
				t.Errorf("GET session afterwards: status = %d, want 200: %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestAPostWithoutAnOriginIsRejected(t *testing.T) {
	for _, op := range apitest.Operations(t) {
		if op.Method != http.MethodPost {
			continue
		}
		t.Run(op.ID, func(t *testing.T) {
			lt := newLoginTest(t)
			b := lt.browser("sandbox.example.com")
			b.logIn(ana, "/_edity/login")

			if rec := b.send(op.Method, op.Example, nil); rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d: %s", rec.Code, http.StatusForbidden, rec.Body)
			}
		})
	}
}

func TestTheSessionRouteAnswersTheSitesOwnPage(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	b.logIn(ana, "/_edity/login")

	if rec := b.send(http.MethodGet, "/_edity/api/session", fromTheSite); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestALoginReturnsOnlyToAPathOnTheSite(t *testing.T) {
	for value, want := range map[string]string{
		"":                       "/",
		"/":                      "/",
		"/precios":               "/precios",
		"/precios/?plan=2":       "/precios/?plan=2",
		"//other.example/":       "/",
		"///other.example/":      "/",
		"https://other.example/": "/",
		"/\\other.example":       "/",
		"\\\\other.example":      "/",
		"/\t/other.example":      "/",
		"javascript:alert(1)":    "/",
		"precios":                "/",
		// Longer than the login's cookie can carry.
		"/" + strings.Repeat("a", 4096): "/",
	} {
		lt := newLoginTest(t)
		rec := lt.browser("sandbox.example.com").logIn(ana, "/_edity/login?return="+url.QueryEscape(value))

		if got := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || got != want {
			t.Errorf("return %q: status = %d, Location = %q, want %d and %q", value, rec.Code, got, http.StatusSeeOther, want)
		}
	}
}

func TestAClientWithNoLocaleInZitadelGetsTheSites(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("sandbox.example.com")
	person := ana
	person.Locale = ""
	b.logIn(person, "/_edity/login")

	rec := b.session()

	if want := `{"name":"Ana García","locale":"en","site":"edity-sandbox"}` + "\n"; rec.Body.String() != want {
		t.Errorf("session = %s, want %s", rec.Body, want)
	}
}

func TestOnLocalhostTheLoginRunsOverHTTP(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.browser("localhost:8080")

	callback := lt.zitadel.LogIn(t, b.started(), ana)

	if want := "http://localhost:8080/_edity/api/auth/callback?"; !strings.HasPrefix(callback, want) {
		t.Errorf("Zitadel sends the browser back to %s, want %s…", callback, want)
	}
	rec := b.get(callback)
	if c := responseCookie(t, rec, "edity_session"); c == nil || c.Secure {
		t.Errorf("session cookie = %+v, want it without Secure over http", c)
	}
	if c := responseCookie(t, rec, "edity"); c == nil || c.Secure {
		t.Errorf("marker cookie = %+v, want it without Secure over http", c)
	}
}
