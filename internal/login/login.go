// Package login logs Clients in through Zitadel, with OIDC's authorization
// code flow and PKCE (A1), and keeps their sessions (A2).
//
// It stores nothing. A login under way and a session are each a cookie
// sealed with the server's key, so a session survives a restart of the
// server.
package login

import (
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/site"
)

const (
	callbackPath = "/_edity/api/auth/callback"
	// attemptLifetime is how long a Client has to log in at Zitadel.
	attemptLifetime = 10 * time.Minute
	// sessionLifetime is how long a session lasts (A2).
	sessionLifetime = 30 * 24 * time.Hour
	// roleScope, followed by a role's key, asks Zitadel to assert that role
	// (urn:zitadel:iam:org:project:roles), however the project is set up.
	roleScope = "urn:zitadel:iam:org:project:role:"
	// maxReturn bounds the path a login comes back to, which the attempt's
	// cookie carries and a browser drops over 4 kilobytes.
	maxReturn = 1024
)

// A cookie is one of the server's cookies, as every response that sets or
// clears it writes it.
type cookie struct {
	name     string
	path     string
	httpOnly bool
	sameSite http.SameSite
	lifetime time.Duration
}

var (
	// attemptCookie holds a login under way, from the redirect to Zitadel
	// until Zitadel sends the browser back: a navigation from another site,
	// so not SameSite=Strict.
	attemptCookie = cookie{"edity_login", callbackPath, true, http.SameSiteLaxMode, attemptLifetime}
	// sessionCookie is the session, HttpOnly and for the API alone (A2). Only
	// the page's own calls to the API need it.
	sessionCookie = cookie{"edity_session", "/_edity/api", true, http.SameSiteStrictMode, sessionLifetime}
	// markerCookie tells the page's inline check to load the Editor (A4), so
	// the page has to be able to read it.
	markerCookie = cookie{"edity", "/", false, http.SameSiteLaxMode, sessionLifetime}
)

// set writes the cookie to the browser, over https unless on loopback.
func (c cookie) set(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, c.with(r, value, int(c.lifetime.Seconds())))
}

// clear removes the cookie from the browser.
func (c cookie) clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, c.with(r, "", -1))
}

func (c cookie) with(r *http.Request, value string, maxAge int) *http.Cookie {
	//nolint:gosec // HttpOnly and SameSite are each cookie's above, and Secure is off only on loopback.
	return &http.Cookie{
		Name: c.name, Value: value, Path: c.path, MaxAge: maxAge,
		HttpOnly: c.httpOnly, Secure: isSecure(r), SameSite: c.sameSite,
	}
}

// ErrNotALogin is a callback that is not the end of a login this browser
// started.
var ErrNotALogin = errors.New("not the end of a login this browser started")

// ErrNoRole is a login by someone who does not have the Site's role, so is
// not one of its Clients (A1).
var ErrNoRole = errors.New("the person who logged in does not have the Site's role")

// Options are what a Zitadel needs.
type Options struct {
	// Issuer is the Zitadel instance's URL.
	Issuer string
	// ClientID is the server's OIDC client: one with PKCE and no secret.
	ClientID string
	// Key seals the cookies, 32 bytes.
	Key []byte
	// HTTP is how the server calls Zitadel. A client with a 10-second
	// timeout when it is nil.
	HTTP *http.Client
	// Now is time.Now when it is nil.
	Now func() time.Time
}

// Zitadel logs Clients in through a Zitadel instance. It reads the instance's
// endpoints the first time it needs them, so the server starts, and serves
// everything else, while Zitadel can't be reached.
type Zitadel struct {
	issuer   string
	clientID string
	sealer   sealer
	client   *http.Client
	now      func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
}

// New returns a Zitadel. Its error says only that the key is wrong: the key
// is never in it.
func New(o Options) (*Zitadel, error) {
	block, err := aes.NewCipher(o.Key)
	if err != nil {
		return nil, fmt.Errorf("the session key is not an AES key: %d bytes", len(o.Key))
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	z := &Zitadel{issuer: o.Issuer, clientID: o.ClientID, sealer: sealer{aead}, client: o.HTTP, now: o.Now}
	if z.client == nil {
		z.client = &http.Client{Timeout: 10 * time.Second}
	}
	if z.now == nil {
		z.now = time.Now
	}
	return z, nil
}

// discover returns the instance's endpoints, read once.
func (z *Zitadel) discover(ctx context.Context) (*oidc.Provider, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.provider != nil {
		return z.provider, nil
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, z.client), z.issuer)
	if err != nil {
		return nil, fmt.Errorf("reading Zitadel's endpoints: %w", err)
	}
	z.provider = provider
	return provider, nil
}

// config is the login's OAuth 2.0 flow on s, for the hostname r came to.
func (z *Zitadel) config(provider *oidc.Provider, r *http.Request, s site.Site) *oauth2.Config {
	endpoint := provider.Endpoint()
	// The client has no secret. The default would try again another way
	// after a refusal, with a code that works only once.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &oauth2.Config{
		ClientID:    z.clientID,
		Endpoint:    endpoint,
		RedirectURL: callbackURL(r),
		Scopes:      []string{oidc.ScopeOpenID, oidc.ScopeProfile, roleScope + s.ZitadelRole},
	}
}

// An attempt is a login under way, which ties the callback to the browser
// that started it.
type attempt struct {
	Site     string    `json:"site"`
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Return   string    `json:"return"`
	Expires  time.Time `json:"expires"`
}

// Start begins a login on s. It sets the attempt's cookie and returns the URL
// of Zitadel's login to send the browser to.
func (z *Zitadel) Start(w http.ResponseWriter, r *http.Request, s site.Site) (string, error) {
	provider, err := z.discover(r.Context())
	if err != nil {
		return "", err
	}
	a := attempt{
		Site:     s.ID,
		State:    rand.Text(),
		Nonce:    rand.Text(),
		Verifier: oauth2.GenerateVerifier(),
		Return:   returnPath(r.URL.Query().Get("return")),
		Expires:  z.now().Add(attemptLifetime),
	}
	sealed, err := z.sealer.seal(attemptCookie.name, a)
	if err != nil {
		return "", err
	}
	attemptCookie.set(w, r, sealed)
	return z.config(provider, r, s).AuthCodeURL(a.State, oidc.Nonce(a.Nonce), oauth2.S256ChallengeOption(a.Verifier)), nil
}

// A session is a logged-in Client of one Site, which its cookie holds.
type session struct {
	Site    string    `json:"site"`
	Name    string    `json:"name"`
	Locale  string    `json:"locale"`
	Expires time.Time `json:"expires"`
}

// Finish ends a login on s: it sets the session's cookies, and returns the
// path to send the browser back to. Its errors are ErrNotALogin, ErrNoRole,
// or any other when Zitadel could not be reached.
func (z *Zitadel) Finish(w http.ResponseWriter, r *http.Request, s site.Site) (string, error) {
	// A login can be finished once, whatever comes of it.
	attemptCookie.clear(w, r)

	a, ok := z.readAttempt(r, s)
	if !ok {
		return "", fmt.Errorf("%w: no login of this browser is under way", ErrNotALogin)
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(a.State)) != 1 {
		return "", fmt.Errorf("%w: the state is not this browser's", ErrNotALogin)
	}
	if answer := q.Get("error"); answer != "" {
		return "", fmt.Errorf("%w: Zitadel answered %s", ErrNotALogin, answer)
	}
	if q.Get("code") == "" {
		return "", fmt.Errorf("%w: no code", ErrNotALogin)
	}
	who, err := z.identify(r, s, a, q.Get("code"))
	if err != nil {
		return "", err
	}
	if len(who.Roles[s.ZitadelRole]) == 0 {
		// Whoever logged in here before them, or they themselves before the
		// role was taken away, is no longer the one at this browser.
		clearSession(w, r)
		return "", ErrNoRole
	}

	sealed, err := z.sealer.seal(sessionCookie.name, session{
		// A Client with no preferred language in Zitadel has no locale.
		Site: s.ID, Name: who.Name, Locale: cmp.Or(who.Locale, s.Locale), Expires: z.now().Add(sessionLifetime),
	})
	if err != nil {
		return "", err
	}
	sessionCookie.set(w, r, sealed)
	markerCookie.set(w, r, "1")
	return a.Return, nil
}

// readAttempt reads the login r's browser started on s, while it lasts.
func (z *Zitadel) readAttempt(r *http.Request, s site.Site) (attempt, bool) {
	var a attempt
	c, err := r.Cookie(attemptCookie.name)
	if err != nil || !z.sealer.open(attemptCookie.name, c.Value, &a) || a.Site != s.ID || !z.now().Before(a.Expires) {
		return attempt{}, false
	}
	return a, true
}

// A person is who logged in, as Zitadel asserts them.
type person struct {
	Name   string                       `json:"name"`
	Locale string                       `json:"locale"`
	Roles  map[string]map[string]string `json:"urn:zitadel:iam:org:project:roles"`
}

// identify exchanges the callback's code and returns who logged in. It reads
// them from userinfo, which asserts what the scopes asked for whatever the
// application's settings, unlike the ID token, which needs them turned on.
func (z *Zitadel) identify(r *http.Request, s site.Site, a attempt, code string) (person, error) {
	provider, err := z.discover(r.Context())
	if err != nil {
		return person{}, err
	}
	ctx := oidc.ClientContext(r.Context(), z.client)
	token, err := z.config(provider, r, s).Exchange(ctx, code, oauth2.VerifierOption(a.Verifier))
	var refused *oauth2.RetrieveError
	if errors.As(err, &refused) && refused.Response.StatusCode < 500 {
		return person{}, fmt.Errorf("%w: Zitadel refused the code: %w", ErrNotALogin, err)
	}
	if err != nil {
		return person{}, fmt.Errorf("exchanging the code: %w", err)
	}
	raw, _ := token.Extra("id_token").(string)
	id, err := provider.Verifier(&oidc.Config{ClientID: z.clientID}).Verify(ctx, raw)
	if err != nil {
		return person{}, fmt.Errorf("%w: its ID token: %w", ErrNotALogin, err)
	}
	if id.Nonce != a.Nonce {
		return person{}, fmt.Errorf("%w: the ID token's nonce is not this login's", ErrNotALogin)
	}
	info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return person{}, fmt.Errorf("reading userinfo: %w", err)
	}
	if info.Subject != id.Subject {
		return person{}, fmt.Errorf("%w: userinfo is about someone else", ErrNotALogin)
	}
	var who person
	if err := info.Claims(&who); err != nil {
		return person{}, fmt.Errorf("%w: its userinfo: %w", ErrNotALogin, err)
	}
	return who, nil
}

// Logout clears the session's cookies. The session itself is only that
// cookie, so there is nothing else to end.
func (z *Zitadel) Logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, r)
}

// clearSession clears the session's cookies from the browser.
func clearSession(w http.ResponseWriter, r *http.Request) {
	sessionCookie.clear(w, r)
	markerCookie.clear(w, r)
}

// Client returns the Client of the session r carries for s, and false when
// r carries none, or an expired one.
func (z *Zitadel) Client(r *http.Request, s site.Site) (publish.Client, bool) {
	c, err := r.Cookie(sessionCookie.name)
	if err != nil {
		return publish.Client{}, false
	}
	var sess session
	if !z.sealer.open(sessionCookie.name, c.Value, &sess) || sess.Site != s.ID || !z.now().Before(sess.Expires) {
		return publish.Client{}, false
	}
	return publish.Client{Name: sess.Name, Locale: sess.Locale}, true
}

// returnPath is the path on the Site that value names, or the Site's home
// when it could lead anywhere else: browsers read a second slash or a
// backslash after the first as the start of another host.
func returnPath(value string) string {
	u, err := url.Parse(value)
	if err != nil || len(value) > maxReturn || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.Contains(value, `\`) || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return value
}

// callbackURL is where Zitadel sends the browser back to, on the hostname r
// came to. The request's own scheme says nothing of the browser's: behind
// Cloudflare Tunnel the server sees every request over http.
func callbackURL(r *http.Request) string {
	scheme := "https"
	if !isSecure(r) {
		scheme = "http"
	}
	return scheme + "://" + r.Host + callbackPath
}

// isSecure tells whether the browser reaches the server over https: anywhere
// but on a loopback hostname, as in development.
func isSecure(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return !site.IsLoopback(host)
}

// A sealer encrypts and authenticates cookies with the server's key. Each
// cookie's name is its purpose: a value sealed for one opens as no other.
type sealer struct {
	aead cipher.AEAD
}

func (s sealer) seal(purpose string, value any) (string, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	//nolint:gosec // The AEAD makes a random nonce for each value and prepends it.
	return base64.RawURLEncoding.EncodeToString(s.aead.Seal(nil, nil, plain, []byte(purpose))), nil
}

// open reads into value what seal sealed for purpose, and tells whether it
// could.
func (s sealer) open(purpose, sealed string, value any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return false
	}
	plain, err := s.aead.Open(nil, nil, raw, []byte(purpose))
	if err != nil {
		return false
	}
	return json.Unmarshal(plain, value) == nil
}
