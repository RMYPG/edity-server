package apitest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// publish is a Publish as the Editor sends it, with a Field, a List of texts
// and a List of Items with a List inside.
const publish = `{
  "page": "home",
  "changes": [
    {"path": "hero.title", "before": "Fisioterapia a domicilio", "after": "Fisioterapia en casa"},
    {"path": "plans.1.price", "before": 45, "after": 49.5},
    {"path": "services", "before": ["Masaje", "Punción seca"], "after": ["Punción seca", "Masaje"]},
    {"path": "plans", "before": [{"name": "Básico", "features": ["Una sesión"]}], "after": []}
  ]
}`

const sha = "4f9d2c1a7b3e8f60d5a9c2b1e7f40386a5d1c9e2"

// request is a request for a Site, carrying a session cookie unless
// signedOut, and a JSON body unless body is empty.
type request struct {
	method, target, body string
	signedOut            bool
}

func (r request) build(t *testing.T) *http.Request {
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	req := httptest.NewRequestWithContext(t.Context(), r.method, r.target, body)
	req.Host = "sandbox.example.com"
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if !r.signedOut {
		req.AddCookie(&http.Cookie{Name: "edity_session", Value: "a-session"})
	}
	return req
}

// response is what a stub handler answers.
type response struct {
	status int
	header map[string]string
	body   string
}

func (res response) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for key, value := range res.header {
			w.Header().Set(key, value)
		}
		w.WriteHeader(res.status)
		_, _ = io.WriteString(w, res.body)
	})
}

var (
	session   = request{method: http.MethodGet, target: "/_edity/api/session"}
	published = request{method: http.MethodPost, target: "/_edity/api/publish", body: publish}

	text = map[string]string{"Content-Type": "text/plain; charset=utf-8"}
	json = map[string]string{"Content-Type": "application/json"}
)

func TestTheResponsesTheFileDescribesPass(t *testing.T) {
	for name, tc := range map[string]struct {
		request  request
		response response
	}{
		"a session": {
			session, response{http.StatusOK, json, `{"name": "Ana", "locale": "es", "site": "edity-sandbox"}`},
		},
		"no session": {
			request{method: http.MethodGet, target: "/_edity/api/session", signedOut: true},
			response{http.StatusUnauthorized, text, "Unauthorized\n"},
		},
		"an unknown host": {
			published, response{http.StatusNotFound, text, "404 page not found\n"},
		},
		"a Publish with Conflicts": {
			published, response{http.StatusAccepted, json, `{"sha": "` + sha + `", "version": "1.4.3", "conflicts": [{"path": "hero.title"}]}`},
		},
		"a Publish in which nothing merged": {
			published, response{http.StatusConflict, json, `{"conflicts": [{"path": "hero.title"}, {"path": "services"}]}`},
		},
		"a Publish that breaks a Limit": {
			published, response{http.StatusUnprocessableEntity, json, `{"violations": [{"path": "plans.0.features", "limit": "minItems"}]}`},
		},
		"a Publish too soon": {
			published, response{http.StatusTooManyRequests, map[string]string{"Content-Type": "text/plain", "Retry-After": "30"}, "Too Many Requests\n"},
		},
		"a malformed Publish refused": {
			request{method: http.MethodPost, target: "/_edity/api/publish", body: `{"page": "home"}`},
			response{http.StatusBadRequest, text, "Bad Request\n"},
		},
		"a login": {
			request{method: http.MethodGet, target: "/_edity/login?return=/precios", signedOut: true},
			response{http.StatusFound, map[string]string{"Location": "https://zitadel.example.com/oauth/v2/authorize"}, ""},
		},
		"a finished login": {
			request{method: http.MethodGet, target: "/_edity/api/auth/callback?code=c&state=s", signedOut: true},
			response{http.StatusSeeOther, map[string]string{"Location": "/precios", "Set-Cookie": "edity=1; Path=/"}, ""},
		},
		"the progress of a Publish": {
			request{method: http.MethodGet, target: "/_edity/api/publish/" + sha},
			response{http.StatusOK, json, `{"stage": "releasing", "version": "1.4.3"}`},
		},
		"another path": {
			request{method: http.MethodGet, target: "/_edity/api/sessions"},
			response{http.StatusNotFound, text, "404 page not found\n"},
		},
		"another method": {
			request{method: http.MethodDelete, target: "/_edity/api/session"},
			response{http.StatusMethodNotAllowed, text, "Method Not Allowed\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			problems := load(t).serve(httptest.NewRecorder(), tc.request.build(t), tc.response.handler())
			for _, problem := range problems {
				t.Error(problem)
			}
		})
	}
}

func TestWhatTheFileLeavesOutFails(t *testing.T) {
	for name, tc := range map[string]struct {
		request  request
		response response
		want     string
	}{
		"a status it does not list": {
			session, response{http.StatusNotImplemented, text, "Not Implemented\n"},
			"the 501 response is not one the OpenAPI file describes",
		},
		"a body without a required property": {
			session, response{http.StatusOK, json, `{"name": "Ana", "locale": "es"}`},
			"the 200 response is not one the OpenAPI file describes",
		},
		"a body with another property": {
			session, response{http.StatusOK, json, `{"name": "Ana", "locale": "es", "site": "edity-sandbox", "role": "x"}`},
			"the 200 response is not one the OpenAPI file describes",
		},
		"a body of another media type": {
			session, response{http.StatusOK, text, `{"name": "Ana", "locale": "es", "site": "edity-sandbox"}`},
			"the 200 response is not one the OpenAPI file describes",
		},
		"a limit the Editor does not know": {
			published, response{http.StatusUnprocessableEntity, json, `{"violations": [{"path": "hero.title", "limit": "maxLines"}]}`},
			"the 422 response is not one the OpenAPI file describes",
		},
		"empty conflicts": {
			published, response{http.StatusAccepted, json, `{"sha": "` + sha + `", "version": "1.4.3", "conflicts": []}`},
			"the 202 response is not one the OpenAPI file describes",
		},
		"a version with a v": {
			published, response{http.StatusAccepted, json, `{"sha": "` + sha + `", "version": "v1.4.3"}`},
			"the 202 response is not one the OpenAPI file describes",
		},
		"a missing required header": {
			published, response{http.StatusTooManyRequests, text, "Too Many Requests\n"},
			"the 429 response is not one the OpenAPI file describes",
		},
		"a login that sets no cookie": {
			request{method: http.MethodGet, target: "/_edity/api/auth/callback?code=c&state=s", signedOut: true},
			response{http.StatusSeeOther, map[string]string{"Location": "/"}, ""},
			"the 303 response is not one the OpenAPI file describes",
		},
		"another path served": {
			request{method: http.MethodGet, target: "/_edity/api/sessions"},
			response{http.StatusOK, json, `{}`},
			"answered 200 to a route the OpenAPI file does not describe",
		},
		"another method served": {
			request{method: http.MethodPut, target: "/_edity/api/publish", body: publish},
			response{http.StatusAccepted, json, `{"sha": "` + sha + `", "version": "1.4.3"}`},
			"answered 202 to a route the OpenAPI file does not describe",
		},
		"a Change with another property accepted": {
			request{method: http.MethodPost, target: "/_edity/api/publish", body: `{"page": "home", "changes": [{"path": "hero.title", "before": "a", "after": "b", "origins": [0]}]}`},
			response{http.StatusAccepted, json, `{"sha": "` + sha + `", "version": "1.4.3"}`},
			"answered 202 to a request the OpenAPI file does not allow",
		},
		"a page outside src/content accepted": {
			request{method: http.MethodPost, target: "/_edity/api/publish", body: `{"page": "../package", "changes": [{"path": "version", "before": "1.4.2", "after": "9.9.9"}]}`},
			response{http.StatusAccepted, json, `{"sha": "` + sha + `", "version": "1.4.3"}`},
			"answered 202 to a request the OpenAPI file does not allow",
		},
		"a Publish without a session accepted": {
			request{method: http.MethodPost, target: "/_edity/api/publish", body: publish, signedOut: true},
			response{http.StatusAccepted, json, `{"sha": "` + sha + `", "version": "1.4.3"}`},
			"answered 202 to a request the OpenAPI file does not allow",
		},
		"a malformed sha answered": {
			request{method: http.MethodGet, target: "/_edity/api/publish/main"},
			response{http.StatusOK, json, `{"stage": "releasing", "version": "1.4.3"}`},
			"answered 200 to a request the OpenAPI file does not allow",
		},
	} {
		t.Run(name, func(t *testing.T) {
			problems := load(t).serve(httptest.NewRecorder(), tc.request.build(t), tc.response.handler())
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Errorf("problems = %q, want one with %q", problems, tc.want)
			}
		})
	}
}

func TestTheHandlerGetsTheWholeRequestAndTheTestTheWholeResponse(t *testing.T) {
	var got string
	h := Handler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"conflicts": [{"path": "hero.title"}]}`)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, published.build(t))

	if got != publish {
		t.Errorf("the handler read %q, want the whole body", got)
	}
	if rec.Code != http.StatusConflict || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != `{"conflicts": [{"path": "hero.title"}]}` {
		t.Errorf("the test got %d %v %q, not the handler's response", rec.Code, rec.Header(), rec.Body)
	}
}

func TestEveryOperationHasAnExamplePath(t *testing.T) {
	for _, op := range Operations(t) {
		if op.ID == "" || strings.ContainsAny(op.Example, "{}") {
			t.Errorf("%s %s: id %q, example %q", op.Method, op.Path, op.ID, op.Example)
		}
	}
}
