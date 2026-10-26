package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RMYPG/edity-server/internal/server"
	"github.com/RMYPG/edity-server/internal/site"
)

const sandbox = `{
  "sites": [
    {
      "id": "edity-sandbox",
      "hostnames": ["sandbox.example.com"],
      "githubRepository": "RMYPG/edity-sandbox",
      "locale": "en",
      "zitadelRole": "site:edity-sandbox",
      "kubernetes": {"namespace": "edity-sandbox", "deployment": "edity-sandbox-sws"}
    }
  ]
}`

func TestHealthAnswersOKWhateverTheHost(t *testing.T) {
	handler := newServer(t, io.Discard)

	for _, host := range []string{"10.0.0.7:8080", "sandbox.example.com"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("GET /healthz on %s: status = %d, want %d", host, rec.Code, http.StatusOK)
		}
	}
}

// apiRoutes is the API table of the spec.
var apiRoutes = []struct{ method, path string }{
	{http.MethodGet, "/_edity/login"},
	{http.MethodGet, "/_edity/api/auth/callback"},
	{http.MethodPost, "/_edity/api/auth/logout"},
	{http.MethodGet, "/_edity/api/session"},
	{http.MethodPost, "/_edity/api/publish"},
	{http.MethodGet, "/_edity/api/publish/0123abc"},
}

func TestEveryRouteAnswersNotFoundForAnUnknownHost(t *testing.T) {
	handler := newServer(t, io.Discard)

	for _, route := range apiRoutes {
		for _, host := range []string{"unknown.example.com", "10.0.0.7:8080", ""} {
			req := httptest.NewRequestWithContext(t.Context(), route.method, route.path, nil)
			req.Host = host
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s on %q: status = %d, want %d", route.method, route.path, host, rec.Code, http.StatusNotFound)
			}
		}
	}
}

func TestAnUnknownHostGetsNotFoundWhateverThePathSpelling(t *testing.T) {
	var logs bytes.Buffer
	handler := newServer(t, &logs)

	// A ServeMux redirects a path it would clean, so the Site lookup has to
	// come before any ServeMux.
	paths := []string{"/_edity/api//session", "/_edity/api/./session", "/_edity/x/../api/session", "//_edity/api/session", "//healthz"}
	for _, path := range paths {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Host = "unknown.example.com"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s on an unknown host: status = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
	if lines := strings.Split(strings.TrimSpace(logs.String()), "\n"); len(lines) != len(paths) {
		t.Errorf("got %d log lines for %d requests:\n%s", len(lines), len(paths), logs.String())
	}
}

func TestEveryRouteReachesItsHandlerForAKnownSite(t *testing.T) {
	handler := newServer(t, io.Discard)

	for _, route := range apiRoutes {
		req := httptest.NewRequestWithContext(t.Context(), route.method, route.path, nil)
		req.Host = "sandbox.example.com"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// Every route is answered 501 until it is implemented.
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: status = %d, want %d", route.method, route.path, rec.Code, http.StatusNotImplemented)
		}
	}
}

func TestEachRequestIsLoggedAsOneJSONLine(t *testing.T) {
	var logs bytes.Buffer
	handler := newServer(t, &logs)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_edity/api/auth/callback?code=secret-code&state=xyz", nil)
	req.Host = "sandbox.example.com"
	handler.ServeHTTP(httptest.NewRecorder(), req)
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/_edity/api/publish", nil)
	req.Host = "unknown.example.com"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2:\n%s", len(lines), logs.String())
	}
	if strings.Contains(logs.String(), "secret-code") {
		t.Errorf("the log leaks the query string:\n%s", logs.String())
	}
	want := []map[string]any{
		{"msg": "request", "method": "GET", "path": "/_edity/api/auth/callback", "host": "sandbox.example.com", "site": "edity-sandbox", "status": 501.0},
		{"msg": "request", "method": "POST", "path": "/_edity/api/publish", "host": "unknown.example.com", "status": 404.0},
	}
	for i, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("log line %d is not JSON: %v\n%s", i+1, err, line)
		}
		for key, value := range want[i] {
			if got[key] != value {
				t.Errorf("log line %d: %s = %v, want %v", i+1, key, got[key], value)
			}
		}
		if _, ok := want[i]["site"]; !ok && got["site"] != nil {
			t.Errorf("log line %d: site = %v, want none", i+1, got["site"])
		}
	}
}

func TestHealthChecksAreNotLogged(t *testing.T) {
	var logs bytes.Buffer
	handler := newServer(t, &logs)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	if logs.Len() != 0 {
		t.Errorf("GET /healthz was logged:\n%s", logs.String())
	}
}

func newServer(t *testing.T, logs io.Writer) http.Handler {
	t.Helper()
	sites, err := site.Parse(strings.NewReader(sandbox))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return server.New(sites, slog.New(slog.NewJSONHandler(logs, nil)))
}
