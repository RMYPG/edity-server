// Package oidctest is a fake Zitadel for the tests of logging in: an OIDC
// provider with discovery, the authorization code flow with PKCE, signed ID
// tokens and a userinfo endpoint. It holds a login to what Zitadel documents:
// the ID token carries no profile or roles, and userinfo asserts a role only
// when its scope was asked for. Only tests import it.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	rolesClaim = "urn:zitadel:iam:org:project:roles"
	roleScope  = "urn:zitadel:iam:org:project:role:"
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

	clientID string
	key      *rsa.PrivateKey
	server   *httptest.Server

	mu     sync.Mutex
	codes  map[string]grant
	tokens map[string]grant
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
)

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
	p := &Provider{clientID: clientID, key: signingKey(), codes: map[string]grant{}, tokens: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /oauth/v2/keys", p.keys)
	mux.HandleFunc("POST /oauth/v2/token", p.token)
	mux.HandleFunc("GET /oidc/v1/userinfo", p.userInfo)
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// Issuer is the provider's URL.
func (p *Provider) Issuer() string { return p.server.URL }

// Stop shuts the provider down, as when Zitadel can't be reached.
func (p *Provider) Stop() { p.server.Close() }

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

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
