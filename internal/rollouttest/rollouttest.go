// Package rollouttest holds what the tests of the packages that ask the
// platform's rollout status service share: its answers, as recorded live,
// answers in the contract's shape for the cases nothing recorded, and a fake
// service that serves them.
//
// The recorded answers are in testdata, each the body of
// GET /v1/sites/edity-sandbox/environments/production as the service gave it
// on 2026-11-10, during milestone 3 of the platform's rollout status plan
// (RMYPG/platform, docs/exec-plans/active/site-rollout-status.md, "Completed
// milestone evidence"). They are copied as that record has them. All times
// are UTC:
//
//   - deploying-1506.json, at 15:06: v1.0.8 picked up while v1.0.7 runs.
//   - live-1617.json, at 16:17: v1.0.8 runs.
//   - failed-170046.json, at 17:00:46: v1.0.9 picked up while v1.0.8 runs,
//     its rollout failing on an earlier rollout's deadline, its HelmRelease and
//     its Kustomization's health check.
//   - failed-171711-truncated.json, at 17:17:11: v1.0.9's own blocked
//     rollout. Its reason was read truncated to 60 characters, so it is not
//     the whole reason the service gave.
//
// Nothing recorded a live Site with a reason, a version the service does not
// know, or the service's error answers: Synthetic and Error build those as
// the contract describes them.
package rollouttest

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

//go:embed testdata/*.json
var recorded embed.FS

// An Answer is one of the service's answers.
type Answer struct {
	Status int
	Body   []byte
}

// Recorded returns the answer recorded in testdata/<name>.
func Recorded(t testing.TB, name string) Answer {
	t.Helper()
	body, err := recorded.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return Answer{Status: http.StatusOK, Body: body}
}

// Synthetic is a status the service would answer, built as the contract
// describes it: an empty target, running or reason is null.
func Synthetic(t testing.TB, target, running, phase, reason string) Answer {
	t.Helper()
	orNull := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	body, err := json.Marshal(map[string]any{
		"target": orNull(target), "running": orNull(running), "phase": phase, "reason": orNull(reason),
	})
	if err != nil {
		t.Fatal(err)
	}
	return Answer{Status: http.StatusOK, Body: body}
}

// Error is one of the service's error answers, {"error": message}, as the
// contract has them.
func Error(t testing.TB, status int, message string) Answer {
	t.Helper()
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		t.Fatal(err)
	}
	return Answer{Status: status, Body: body}
}

// A Service is a fake rollout status service, which answers GET
// /v1/sites/{site}/environments/{environment} with the Answer set for that
// Site and environment, and 404 for any other path, as the service does.
type Service struct {
	// URL is where it listens.
	URL string

	mu      sync.Mutex
	answers map[string]Answer
	down    bool
	asked   map[string]int
}

// NewService starts a Service that is stopped when the test ends.
func NewService(t testing.TB) *Service {
	t.Helper()
	s := &Service{answers: map[string]Answer{}, asked: map[string]int{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Set makes the Service answer a for the Site whose id is site, in
// production, or 404 when a is the zero Answer, as for a Site it is not
// configured for.
func (s *Service) Set(site string, a Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers["/v1/sites/"+site+"/environments/production"] = a
}

// Down makes the Service drop every connection without an answer, as one
// that can't be reached, until it is called with false.
func (s *Service) Down(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// Asked counts the requests made for the Site whose id is site, in
// production, including those it dropped.
func (s *Service) Asked(site string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked["/v1/sites/"+site+"/environments/production"]
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.asked[r.URL.Path]++
	down := s.down
	answer, ok := s.answers[r.URL.Path]
	s.mu.Unlock()
	if down {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		answer = Answer{Status: http.StatusMethodNotAllowed, Body: []byte(`{"error":"method not allowed"}`)}
	case !ok || answer.Status == 0:
		message := "unknown site or environment"
		if !strings.HasPrefix(r.URL.Path, "/v1/sites/") {
			message = "not found"
		}
		answer = Answer{Status: http.StatusNotFound, Body: []byte(`{"error":"` + message + `"}`)}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(answer.Status)
	_, _ = w.Write(answer.Body)
}
