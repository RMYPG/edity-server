package site_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/RMYPG/edity-server/internal/site"
)

const sandbox = `{
  "sites": [
    {
      "id": "edity-sandbox",
      "hostnames": ["sandbox.example.com", "www.sandbox.example.com"],
      "githubRepository": "RMYPG/edity-sandbox",
      "locale": "en",
      "zitadelRole": "site:edity-sandbox",
      "kubernetes": {"namespace": "edity-sandbox", "deployment": "edity-sandbox-sws"}
    }
  ]
}`

func TestLookupFindsTheSiteForEachOfItsHostnames(t *testing.T) {
	sites, err := site.Parse(strings.NewReader(sandbox))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := site.Site{
		ID:          "edity-sandbox",
		Hostnames:   []string{"sandbox.example.com", "www.sandbox.example.com"},
		Repository:  site.Repository{Owner: "RMYPG", Name: "edity-sandbox"},
		Locale:      "en",
		ZitadelRole: "site:edity-sandbox",
		Kubernetes:  site.Kubernetes{Namespace: "edity-sandbox", Deployment: "edity-sandbox-sws"},
	}
	for _, host := range []string{"sandbox.example.com", "www.sandbox.example.com"} {
		got, ok := sites.Lookup(host)
		if !ok {
			t.Fatalf("Lookup(%q) found no Site", host)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q) = %+v, want %+v", host, got, want)
		}
	}
}

func TestParseAcceptsLanguageTagsWithAScriptOrRegion(t *testing.T) {
	for _, locale := range []string{"es", "es-ES", "zh-Hant-TW", "es-419"} {
		file := withSandbox(t, func(s map[string]any) { s["locale"] = locale })
		if _, err := site.Parse(strings.NewReader(file)); err != nil {
			t.Errorf("locale %q: Parse: %v", locale, err)
		}
	}
}

func TestParseAcceptsWhitespaceAfterTheDocument(t *testing.T) {
	if _, err := site.Parse(strings.NewReader(sandbox + "\n\n")); err != nil {
		t.Errorf("Parse: %v", err)
	}
}

func TestLookupMatchesTheHostHeaderWhateverItsCaseOrPort(t *testing.T) {
	sites, err := site.Parse(strings.NewReader(sandbox))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, host := range []string{"Sandbox.Example.COM", "sandbox.example.com:8443", "sandbox.example.com."} {
		got, ok := sites.Lookup(host)
		if !ok || got.ID != "edity-sandbox" {
			t.Errorf("Lookup(%q) = %q, %v, want edity-sandbox", host, got.ID, ok)
		}
	}
}

func TestLookupFindsNoSiteForAnUnknownHost(t *testing.T) {
	sites, err := site.Parse(strings.NewReader(sandbox))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, host := range []string{"", "example.com", "evil.sandbox.example.com", "sandbox.example.com.evil.test"} {
		if got, ok := sites.Lookup(host); ok {
			t.Errorf("Lookup(%q) found %q, want no Site", host, got.ID)
		}
	}
}

func TestRequireGivesTheHandlerTheSiteOfTheRequest(t *testing.T) {
	sites, err := site.Parse(strings.NewReader(sandbox))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var got site.Site
	handler := sites.Require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = site.FromContext(r.Context())
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_edity/api/session", nil)
	req.Host = "sandbox.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got.ID != "edity-sandbox" {
		t.Errorf("FromContext gave Site %q, want edity-sandbox", got.ID)
	}
}

func TestRequireAnswersNotFoundForAnUnknownHost(t *testing.T) {
	sites, err := site.Parse(strings.NewReader(sandbox))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	handler := sites.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler was called for an unknown host")
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_edity/api/session", nil)
	req.Host = "unknown.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestParseRejectsAnInvalidSitesFile(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{"not JSON", "sites: []", "Sites file"},
		{"a second document after it", sandbox + `{"sites": []}`, "more than one JSON value"},
		{"text after it", sandbox + " garbage", "more than one JSON value"},
		{"a key twice", strings.Replace(sandbox, `"locale": "en",`, `"locale": "en", "locale": "es",`, 1), `"sites[0].locale" appears twice`},
		{"a key twice in another case", strings.Replace(sandbox, `"locale": "en",`, `"locale": "en", "Locale": "es",`, 1), `"sites[0].Locale" appears twice`},
		{"a nested key twice", strings.Replace(sandbox, `"namespace": "edity-sandbox",`, `"namespace": "edity-sandbox", "namespace": "other",`, 1), `"sites[0].kubernetes.namespace" appears twice`},
		{"the sites key twice", strings.Replace(sandbox, `{`, `{"sites": [], `, 1), `"sites" appears twice`},
		{"misspelt field", withSandbox(t, func(s map[string]any) { s["hostname"] = "sandbox.example.com" }), `unknown field "hostname"`},
		{"no Sites", `{"sites": []}`, "no Sites"},
		{"no id", withSandbox(t, func(s map[string]any) { delete(s, "id") }), "site 1: id is required"},
		{"no hostnames", withSandbox(t, func(s map[string]any) { s["hostnames"] = []string{} }), `site "edity-sandbox": hostnames: at least one is required`},
		{"hostname with a port", withSandbox(t, func(s map[string]any) { s["hostnames"] = []string{"sandbox.example.com:443"} }), `"sandbox.example.com:443" is not a bare hostname`},
		{"hostname with a scheme", withSandbox(t, func(s map[string]any) { s["hostnames"] = []string{"https://sandbox.example.com"} }), `"https://sandbox.example.com" is not a bare hostname`},
		{"repository without an owner", withSandbox(t, func(s map[string]any) { s["githubRepository"] = "edity-sandbox" }), `"edity-sandbox" is not owner/name`},
		{"repository with a path", withSandbox(t, func(s map[string]any) { s["githubRepository"] = "RMYPG/edity-sandbox/tree" }), `"RMYPG/edity-sandbox/tree" is not owner/name`},
		{"blank id", withSandbox(t, func(s map[string]any) { s["id"] = " " }), "id is required"},
		{"blank repository", withSandbox(t, func(s map[string]any) { s["githubRepository"] = " / " }), `" / " is not owner/name`},
		{"repository name with a space", withSandbox(t, func(s map[string]any) { s["githubRepository"] = "RMYPG/edity sandbox" }), `"RMYPG/edity sandbox" is not owner/name`},
		{"owner starting with a hyphen", withSandbox(t, func(s map[string]any) { s["githubRepository"] = "-RMYPG/edity-sandbox" }), `"-RMYPG/edity-sandbox" is not owner/name`},
		{"repository name ..", withSandbox(t, func(s map[string]any) { s["githubRepository"] = "RMYPG/.." }), `"RMYPG/.." is not owner/name`},
		{"no locale", withSandbox(t, func(s map[string]any) { delete(s, "locale") }), `site "edity-sandbox": locale is required`},
		{"locale that is not a language tag", withSandbox(t, func(s map[string]any) { s["locale"] = "not_a_locale" }), `locale: "not_a_locale" is not a language tag`},
		{"blank Zitadel role", withSandbox(t, func(s map[string]any) { s["zitadelRole"] = " " }), `site "edity-sandbox": zitadelRole is required`},
		{"Zitadel role with a space", withSandbox(t, func(s map[string]any) { s["zitadelRole"] = "site: edity-sandbox" }), `zitadelRole: "site: edity-sandbox" has spaces`},
		{"namespace that is not a Kubernetes name", withSandbox(t, func(s map[string]any) { s["kubernetes"].(map[string]any)["namespace"] = "UPPER_CASE" }), `kubernetes.namespace: "UPPER_CASE" is not a Kubernetes namespace name`},
		{"Deployment that is not a Kubernetes name", withSandbox(t, func(s map[string]any) { s["kubernetes"].(map[string]any)["deployment"] = "bad/name" }), `kubernetes.deployment: "bad/name" is not a Kubernetes object name`},
		{"no Zitadel role", withSandbox(t, func(s map[string]any) { delete(s, "zitadelRole") }), `site "edity-sandbox": zitadelRole is required`},
		{"no namespace", withSandbox(t, func(s map[string]any) { delete(s["kubernetes"].(map[string]any), "namespace") }), `site "edity-sandbox": kubernetes.namespace is required`},
		{"no Deployment", withSandbox(t, func(s map[string]any) { delete(s["kubernetes"].(map[string]any), "deployment") }), `site "edity-sandbox": kubernetes.deployment is required`},
		{"two Sites with one id", withSandboxTwice(t, func(other map[string]any) { other["hostnames"] = []string{"other.example.com"} }), `two Sites have the id "edity-sandbox"`},
		{"two Sites with one hostname", withSandboxTwice(t, func(other map[string]any) {
			other["id"] = "other"
			other["hostnames"] = []string{"SANDBOX.example.com"}
		}), `two Sites have the hostname "sandbox.example.com"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := site.Parse(strings.NewReader(tt.file))
			if err == nil {
				t.Fatalf("Parse accepted %s", tt.file)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// withSandbox returns the sandbox Sites file after edit changes its Site.
func withSandbox(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	sandboxSite := sandboxSite(t)
	edit(sandboxSite)
	return sitesFile(t, sandboxSite)
}

// withSandboxTwice returns a Sites file listing the sandbox Site and a copy
// of it changed by edit.
func withSandboxTwice(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	other := sandboxSite(t)
	edit(other)
	return sitesFile(t, sandboxSite(t), other)
}

func sandboxSite(t *testing.T) map[string]any {
	t.Helper()
	var file struct{ Sites []map[string]any }
	if err := json.Unmarshal([]byte(sandbox), &file); err != nil {
		t.Fatal(err)
	}
	return file.Sites[0]
}

func sitesFile(t *testing.T, sites ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"sites": sites})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
