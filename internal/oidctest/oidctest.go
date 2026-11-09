// Package oidctest is a fake Zitadel for the tests of logging in: an OIDC
// provider with discovery, the authorization code flow with PKCE, signed ID
// tokens and a userinfo endpoint. It holds a login to what Zitadel documents:
// the ID token carries no profile or roles, and userinfo asserts a role only
// when its scope was asked for.
//
// It also has a service user that signs in with a JWT-profile key, and the
// role assignments that user can list through the authorization service
// (zitadel.authorization.v2), as Zitadel v4.19.2 answers them over Connect.
// Only tests import it.
package oidctest

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	rolesClaim      = "urn:zitadel:iam:org:project:roles"
	roleScope       = "urn:zitadel:iam:org:project:role:"
	jwtProfileGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	apiScope        = "urn:zitadel:iam:org:project:id:zitadel:aud"
)

// The Edity project and the service user that every fake Zitadel has.
const (
	ProjectID     = "340000000000000001"
	ServiceUserID = "350000000000000002"
	ServiceKeyID  = "360000000000000003"
	// AccessTokenLifetime is how long the service user's access tokens last,
	// which is Zitadel's default.
	AccessTokenLifetime = 12 * time.Hour
)

// A Person is someone with a Zitadel account.
type Person struct {
	Subject string
	Name    string
	// Locale is left out of userinfo when it is empty, as Zitadel does for a
	// user with no preferred language.
	Locale string
	// Roles are the keys of the project's roles the Person has.
	Roles []string
}

// An Authorization is a role assignment: the roles a user has on a project.
type Authorization struct {
	UserID    string
	ProjectID string
	Roles     []string
	// Inactive is an authorization that was deactivated. It stays, and
	// Zitadel lists it, but it grants nothing.
	Inactive bool
}

// Provider is a running fake Zitadel, with one OIDC client.
type Provider struct {
	// EditIDToken, when it is set, changes an ID token's claims before they
	// are signed.
	EditIDToken func(claims map[string]any)
	// EditUserInfo, when it is set, changes what userinfo answers.
	EditUserInfo func(claims map[string]any)
	// TokenStatus, when it is set, is what the token endpoint answers,
	// with no tokens.
	TokenStatus int
	// Forge signs ID tokens with a key the provider does not publish.
	Forge bool
	// Now is the clock of the service user's assertions and access tokens,
	// time.Now when it is nil.
	Now func() time.Time
	// ListStatus, when it is set, is what ListAuthorizations answers, with no
	// list, even to a token it would refuse.
	ListStatus int
	// ListBody, when it is set, is the body ListAuthorizations answers 200
	// with.
	ListBody string
	// ListRedirect, when it is set, is where ListAuthorizations redirects to.
	ListRedirect string
	// Hang makes ListAuthorizations answer nothing until the caller gives up.
	Hang bool
	// HangDiscovery makes the discovery document answer nothing until the
	// caller gives up.
	HangDiscovery bool
	// HangServiceToken makes the token endpoint answer the service user
	// nothing until the caller gives up.
	HangServiceToken bool
	// IgnoreFilters makes ListAuthorizations answer every authorization,
	// whatever it was asked.
	IgnoreFilters bool
	// Unreadable makes ListAuthorizations answer as Zitadel answers a caller
	// without user.grant.read: it drops the authorizations it finds from the
	// list, and still counts them.
	Unreadable bool

	clientID string
	key      *rsa.PrivateKey
	server   *httptest.Server

	mu             sync.Mutex
	codes          map[string]grant
	tokens         map[string]grant
	authorizations []Authorization
	access         map[string]time.Time
	issued         int
	lists          []string
	hanging        int
}

// A grant is a login that went through: first its code, then its access
// token.
type grant struct {
	person      Person
	nonce       string
	challenge   string
	redirectURI string
	scopes      []string
}

// The keys are slow to make, and no test needs its own.
var (
	signingKey = sync.OnceValue(newKey)
	forgedKey  = sync.OnceValue(newKey)
	serviceKey = sync.OnceValue(newKey)
)

// ServiceKey is the private key of every fake Zitadel's service user.
func ServiceKey() *rsa.PrivateKey { return serviceKey() }

func newKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

// New starts a fake Zitadel with the OIDC client clientID, until the end of
// the test.
func New(t testing.TB, clientID string) *Provider {
	t.Helper()
	p := &Provider{
		clientID: clientID, key: signingKey(),
		codes: map[string]grant{}, tokens: map[string]grant{}, access: map[string]time.Time{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /oauth/v2/keys", p.keys)
	mux.HandleFunc("POST /oauth/v2/token", p.token)
	mux.HandleFunc("GET /oidc/v1/userinfo", p.userInfo)
	mux.HandleFunc("POST /zitadel.authorization.v2.AuthorizationService/ListAuthorizations", p.listAuthorizations)
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// Issuer is the provider's URL.
func (p *Provider) Issuer() string { return p.server.URL }

// Stop shuts the provider down, as when Zitadel can't be reached.
func (p *Provider) Stop() { p.server.Close() }

// SetAuthorizations makes list every role assignment Zitadel has.
func (p *Provider) SetAuthorizations(list ...Authorization) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorizations = list
}

// AccessTokensIssued counts the access tokens the service user was given.
func (p *Provider) AccessTokensIssued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.issued
}

// Hanging counts the calls the provider has left without an answer.
func (p *Provider) Hanging() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hanging
}

// hang leaves r without an answer until the caller gives up.
func (p *Provider) hang(r *http.Request) {
	p.mu.Lock()
	p.hanging++
	p.mu.Unlock()
	<-r.Context().Done()
}

// RevokeAccessTokens makes Zitadel refuse every access token it gave so far,
// whenever they would have expired.
func (p *Provider) RevokeAccessTokens() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.access)
}

// ListRequests returns the bodies of the ListAuthorizations calls so far.
func (p *Provider) ListRequests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.lists)
}

// LogIn is person logging in at authorize, the URL a login redirected to. It
// fails t unless authorize asks for the authorization code flow with PKCE,
// and returns the URL Zitadel sends the browser back to.
func (p *Provider) LogIn(t testing.TB, authorize string, person Person) string {
	t.Helper()
	q := p.authorization(t, authorize)
	code := rand.Text()
	p.mu.Lock()
	p.codes[code] = grant{
		person: person, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirectURI: q.Get("redirect_uri"), scopes: strings.Fields(q.Get("scope")),
	}
	p.mu.Unlock()
	return back(t, q, url.Values{"code": {code}})
}

// Cancel is someone cancelling the login at authorize. It returns the URL
// Zitadel sends the browser back to, with the OAuth 2.0 error.
func (p *Provider) Cancel(t testing.TB, authorize string) string {
	t.Helper()
	return back(t, p.authorization(t, authorize), url.Values{"error": {"access_denied"}, "error_description": {"cancelled"}})
}

// authorization checks an authorization URL and returns its parameters.
func (p *Provider) authorization(t testing.TB, authorize string) url.Values {
	t.Helper()
	u, err := url.Parse(authorize)
	if err != nil {
		t.Fatalf("authorization URL %q: %v", authorize, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != p.Issuer()+"/oauth/v2/authorize" {
		t.Errorf("redirected to %s, not to the authorization endpoint", got)
	}
	q := u.Query()
	for name, want := range map[string]string{"response_type": "code", "client_id": p.clientID, "code_challenge_method": "S256"} {
		if q.Get(name) != want {
			t.Errorf("authorization parameter %s = %q, want %q", name, q.Get(name), want)
		}
	}
	for _, name := range []string{"redirect_uri", "state", "nonce", "code_challenge"} {
		if q.Get(name) == "" {
			t.Errorf("the authorization URL has no %s", name)
		}
	}
	if !slices.Contains(strings.Fields(q.Get("scope")), "openid") {
		t.Errorf("scope = %q, want openid in it", q.Get("scope"))
	}
	return q
}

// back is the URL Zitadel sends the browser back to, with answer and the
// login's state.
func back(t testing.TB, q, answer url.Values) string {
	t.Helper()
	callback, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatalf("redirect_uri %q: %v", q.Get("redirect_uri"), err)
	}
	answer.Set("state", q.Get("state"))
	callback.RawQuery = answer.Encode()
	return callback.String()
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	if p.HangDiscovery {
		p.hang(r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.Issuer(),
		"authorization_endpoint":                p.Issuer() + "/oauth/v2/authorize",
		"token_endpoint":                        p.Issuer() + "/oauth/v2/token",
		"userinfo_endpoint":                     p.Issuer() + "/oidc/v1/userinfo",
		"jwks_uri":                              p.Issuer() + "/oauth/v2/keys",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "fake",
		"n": base64.RawURLEncoding.EncodeToString(p.key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.E)).Bytes()),
	}}})
}

// token exchanges a code once, for the client it was given to and the
// verifier of its challenge.
func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if p.TokenStatus != 0 {
		writeJSON(w, p.TokenStatus, map[string]string{"error": "server_error"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if r.PostForm.Get("grant_type") == jwtProfileGrant {
		p.serviceToken(w, r)
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, ok := p.codes[code]
	delete(p.codes, code)
	p.mu.Unlock()
	challenge := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("client_id") != p.clientID ||
		r.PostForm.Get("redirect_uri") != g.redirectURI ||
		base64.RawURLEncoding.EncodeToString(challenge[:]) != g.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}

	access := rand.Text()
	p.mu.Lock()
	p.tokens[access] = g
	p.mu.Unlock()
	now := time.Now()
	claims := map[string]any{
		"iss": p.Issuer(), "aud": []string{p.clientID}, "sub": g.person.Subject, "nonce": g.nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if p.EditIDToken != nil {
		p.EditIDToken(claims)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 3600, "id_token": p.sign(claims),
	})
}

// sign writes claims as a JWS with RS256.
func (p *Provider) sign(claims map[string]any) string {
	key := p.key
	if p.Forge {
		key = forgedKey()
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	signed := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fake","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// userInfo answers the claims of the scopes the login asked for.
func (p *Provider) userInfo(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	g, ok := p.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	p.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	claims := map[string]any{"sub": g.person.Subject}
	if slices.Contains(g.scopes, "profile") {
		claims["name"] = g.person.Name
		if g.person.Locale != "" {
			claims["locale"] = g.person.Locale
		}
	}
	roles := map[string]map[string]string{}
	for _, role := range g.person.Roles {
		if slices.Contains(g.scopes, roleScope+role) {
			roles[role] = map[string]string{"190000000000000001": "edity.zitadel.example.com"}
		}
	}
	if len(roles) > 0 {
		claims[rolesClaim] = roles
	}
	if p.EditUserInfo != nil {
		p.EditUserInfo(claims)
	}
	writeJSON(w, http.StatusOK, claims)
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// serviceToken gives the service user an access token for a valid JWT-profile
// assertion. Without the scope that adds Zitadel's own project to the
// audience, the token would be good for nothing at its API.
func (p *Provider) serviceToken(w http.ResponseWriter, r *http.Request) {
	if p.HangServiceToken {
		p.hang(r)
		return
	}
	if err := p.checkAssertion(r.PostForm.Get("assertion")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": err.Error()})
		return
	}
	scopes := strings.Fields(r.PostForm.Get("scope"))
	if !slices.Contains(scopes, "openid") || !slices.Contains(scopes, apiScope) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_scope"})
		return
	}
	access := rand.Text()
	p.mu.Lock()
	p.access[access] = p.now().Add(AccessTokenLifetime)
	p.issued++
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(AccessTokenLifetime.Seconds()),
	})
}

// checkAssertion holds an assertion to what Zitadel's verifier wants: RS256
// with the key named by kid, signed by the user in iss and sub, for the
// issuer, no more than an hour old and at most a second in the future, and
// not expired.
func (p *Provider) checkAssertion(assertion string) error {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return err
	}
	if header.Alg != "RS256" || header.Kid != ServiceKeyID {
		return errors.New("not the service user's key")
	}
	var claims struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return err
	}
	now := p.now()
	switch {
	case claims.Iss != ServiceUserID || claims.Sub != ServiceUserID:
		return errors.New("not the service user")
	case claims.Aud != p.Issuer():
		return errors.New("not for this issuer")
	case time.Unix(claims.Iat, 0).After(now.Add(time.Second)) || time.Unix(claims.Iat, 0).Before(now.Add(-time.Hour)):
		return errors.New("iat is out of range")
	case !time.Unix(claims.Exp, 0).After(now):
		return errors.New("expired")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("not a signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&serviceKey().PublicKey, crypto.SHA256, digest[:], signature) != nil {
		return errors.New("a bad signature")
	}
	return nil
}

func decodeSegment(segment string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// A listRequest is ListAuthorizationsRequest as protojson writes it.
type listRequest struct {
	Filters []struct {
		InUserIDs *struct {
			IDs []string `json:"ids"`
		} `json:"inUserIds"`
		ProjectID *struct {
			ID string `json:"id"`
		} `json:"projectId"`
		RoleKey *struct {
			Key    string `json:"key"`
			Method string `json:"method"`
		} `json:"roleKey"`
	} `json:"filters"`
}

// matches tells whether a meets every filter of the request, as Zitadel
// combines them. A role key is compared with each role whole, whatever
// method it is sent with.
func (l listRequest) matches(a Authorization) bool {
	for _, f := range l.Filters {
		switch {
		case f.InUserIDs != nil && !slices.Contains(f.InUserIDs.IDs, a.UserID),
			f.ProjectID != nil && f.ProjectID.ID != a.ProjectID,
			f.RoleKey != nil && !slices.Contains(a.Roles, f.RoleKey.Key):
			return false
		}
	}
	return true
}

// listAuthorizations answers the Connect call of the service user, whose
// access token has to be current, with every authorization that meets its
// filters.
func (p *Provider) listAuthorizations(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_argument", "message": "unreadable"})
		return
	}
	p.mu.Lock()
	p.lists = append(p.lists, string(body))
	all := slices.Clone(p.authorizations)
	expires, known := p.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	p.mu.Unlock()
	if p.Hang {
		p.hang(r)
		return
	}
	if p.ListRedirect != "" {
		http.Redirect(w, r, p.ListRedirect, http.StatusTemporaryRedirect)
		return
	}
	if p.ListStatus != 0 {
		writeJSON(w, p.ListStatus, map[string]string{"code": "internal", "message": "failing"})
		return
	}
	if !known || !expires.After(p.now()) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "Errors.Token.Invalid"})
		return
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Connect-Protocol-Version") != "1" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"code": "unknown", "message": "not Connect's JSON"})
		return
	}
	if p.ListBody != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(p.ListBody))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request listRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_argument", "message": err.Error()})
		return
	}
	var found []map[string]any
	for _, a := range all {
		if p.IgnoreFilters || request.matches(a) {
			found = append(found, wireAuthorization(a))
		}
	}
	if p.Unreadable {
		writeJSON(w, http.StatusOK, listAnswer(len(found), nil))
		return
	}
	writeJSON(w, http.StatusOK, listAnswer(len(found), found))
}

// listAnswer is a ListAuthorizationsResponse as protojson writes it: it
// leaves out what is empty and writes a uint64 as a string.
func listAnswer(total int, shown []map[string]any) map[string]any {
	pagination := map[string]any{"appliedLimit": "100"}
	if total > 0 {
		pagination["totalResult"] = strconv.Itoa(total)
	}
	answer := map[string]any{"pagination": pagination}
	if len(shown) > 0 {
		answer["authorizations"] = shown
	}
	return answer
}

func wireAuthorization(a Authorization) map[string]any {
	state := "STATE_ACTIVE"
	if a.Inactive {
		state = "STATE_INACTIVE"
	}
	roles := []map[string]string{}
	for _, role := range a.Roles {
		roles = append(roles, map[string]string{"key": role, "displayName": role})
	}
	return map[string]any{
		"id":           rand.Text(),
		"project":      map[string]string{"id": a.ProjectID, "name": "Edity", "organizationId": "190000000000000001"},
		"organization": map[string]string{"id": "190000000000000001", "name": "RMYPG"},
		"user":         map[string]string{"id": a.UserID, "displayName": "Someone", "organizationId": "190000000000000001"},
		"state":        state,
		"roles":        roles,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
