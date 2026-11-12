package rollout_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/rollout"
	"github.com/RMYPG/edity-server/internal/rollouttest"
)

func TestTheStatusAsTheServiceAnswersIt(t *testing.T) {
	for name, c := range map[string]struct {
		answer rollouttest.Answer
		want   rollout.Status
	}{
		// Recorded.
		"deploying, at 15:06": {
			rollouttest.Recorded(t, "deploying-1506.json"),
			rollout.Status{Target: "v1.0.8", Running: "v1.0.7", Phase: rollout.Deploying},
		},
		"live, at 16:17": {
			rollouttest.Recorded(t, "live-1617.json"),
			rollout.Status{Target: "v1.0.8", Running: "v1.0.8", Phase: rollout.Live},
		},
		"failed, at 17:00:46": {
			rollouttest.Recorded(t, "failed-170046.json"),
			rollout.Status{
				Target: "v1.0.9", Running: "v1.0.8", Phase: rollout.Failed,
				Reason: `Deployment edity-sandbox/edity-sandbox-sws: Progressing=False (ProgressDeadlineExceeded): ReplicaSet "edity-sandbox-sws-594f98bf45" has timed out progressing.; ` +
					`HelmRelease edity-sandbox/edity-sandbox: Released=False (UpgradeFailed): Helm upgrade failed for release edity-sandbox/edity-sandbox with chart sws@0.1.0+d4be20fd838f: failed early due to stalled resources: [Deployment/edity-sandbox/edity-sandbox-sws status: 'Failed']; ` +
					`Kustomization flux-system/edity-sandbox: Ready=False (HealthCheckFailed): health check failed after 5m0.025786058s: timeout waiting for: [HelmRelease/edity-sandbox/edity-sandbox status: 'InProgress']`,
			},
		},
		"failed, at 17:17:11, its reason read truncated": {
			rollouttest.Recorded(t, "failed-171711-truncated.json"),
			rollout.Status{Target: "v1.0.9", Running: "v1.0.8", Phase: rollout.Failed, Reason: "HelmRelease edity-sandbox/edity-sandbox: Released=False (Upg"},
		},
		// Synthetic, as the contract describes them.
		"live with a failing image scan": {
			rollouttest.Synthetic(t, "v1.0.8", "v1.0.8", "live", "ImageRepository flux-system/edity-sandbox: Ready=False (ReconciliationFailed): scan failed"),
			rollout.Status{Target: "v1.0.8", Running: "v1.0.8", Phase: rollout.Live, Reason: "ImageRepository flux-system/edity-sandbox: Ready=False (ReconciliationFailed): scan failed"},
		},
		"the contract's ImagePolicy with no resolved tag": {
			rollouttest.Answer{Status: http.StatusOK, Body: []byte(`{ "target": null, "running": "v1.0.2", "phase": "deploying", "reason": "ImagePolicy flux-system/fisioindoor-production has no resolved tag" }`)},
			rollout.Status{Running: "v1.0.2", Phase: rollout.Deploying, Reason: "ImagePolicy flux-system/fisioindoor-production has no resolved tag"},
		},
		"no ReplicaSet available, as while the pod restarts": {
			rollouttest.Synthetic(t, "v1.0.8", "", "deploying", ""),
			rollout.Status{Target: "v1.0.8", Phase: rollout.Deploying},
		},
		"a field the contract may add later": {
			rollouttest.Answer{Status: http.StatusOK, Body: []byte(`{"target":"v1.0.8","running":"v1.0.8","phase":"live","reason":null,"since":"2026-11-10T16:17:00Z"}`)},
			rollout.Status{Target: "v1.0.8", Running: "v1.0.8", Phase: rollout.Live},
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc := rollouttest.NewService(t)
			svc.Set("edity-sandbox", c.answer)
			got, err := rollout.New(svc.URL, &http.Client{}).Status(t.Context(), "edity-sandbox", rollout.Production)
			if err != nil || got != c.want {
				t.Errorf("Status = %+v, %v, want %+v", got, err, c.want)
			}
		})
	}
}

func TestTheStatusIsAskedByTheSitesID(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.EscapedPath()
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept: %q", r.Header.Get("Accept"))
		}
		_, _ = w.Write(rollouttest.Recorded(t, "live-1617.json").Body)
	}))
	t.Cleanup(srv.Close)
	// A trailing slash on the address is not doubled.
	if _, err := rollout.New(srv.URL+"/", srv.Client()).Status(t.Context(), "fisioindoor", rollout.Production); err != nil {
		t.Fatal(err)
	}
	if asked != "GET /v1/sites/fisioindoor/environments/production" {
		t.Errorf("asked %q", asked)
	}
	// An id goes into the path escaped.
	if _, err := rollout.New(srv.URL, srv.Client()).Status(t.Context(), "a/../b", rollout.Production); err != nil {
		t.Fatal(err)
	}
	if asked != "GET /v1/sites/a%2F..%2Fb/environments/production" {
		t.Errorf("asked %q", asked)
	}
}

func TestEveryAnswerThatIsNotAStatusIsAnError(t *testing.T) {
	for name, c := range map[string]struct {
		answer  rollouttest.Answer
		status  int
		refused bool
	}{
		// The contract's error answers. None was recorded.
		"a Site the service is not configured for": {rollouttest.Error(t, http.StatusNotFound, "unknown site or environment"), http.StatusNotFound, true},
		"a method other than GET":                  {rollouttest.Error(t, http.StatusMethodNotAllowed, "method not allowed"), http.StatusMethodNotAllowed, true},
		"watches not synced, or failing":           {rollouttest.Error(t, http.StatusServiceUnavailable, "watches not synced"), http.StatusServiceUnavailable, false},
		"a proxy's 502 with no JSON":               {rollouttest.Answer{Status: http.StatusBadGateway, Body: []byte("<html>Bad Gateway</html>")}, http.StatusBadGateway, false},
		"not JSON":                                 {rollouttest.Answer{Status: http.StatusOK, Body: []byte("ok")}, http.StatusOK, false},
		"no phase":                                 {rollouttest.Answer{Status: http.StatusOK, Body: []byte(`{"target":"v1.0.8","running":"v1.0.8"}`)}, http.StatusOK, false},
		"a phase this server does not know":        {rollouttest.Synthetic(t, "v1.0.8", "v1.0.7", "paused", ""), http.StatusOK, false},
		"too big":                                  {rollouttest.Answer{Status: http.StatusOK, Body: []byte(`{"reason":"` + strings.Repeat("x", 1<<20) + `"}`)}, http.StatusOK, false},
	} {
		t.Run(name, func(t *testing.T) {
			svc := rollouttest.NewService(t)
			svc.Set("edity-sandbox", c.answer)
			got, err := rollout.New(svc.URL, &http.Client{}).Status(t.Context(), "edity-sandbox", rollout.Production)
			var e *rollout.Error
			if !errors.As(err, &e) || e.Status != c.status || e.Refused() != c.refused {
				t.Fatalf("Status = %+v, %v, want an Error with %d, refused %t", got, err, c.status, c.refused)
			}
		})
	}
}

func TestAServiceThatCantBeReachedIsAnError(t *testing.T) {
	svc := rollouttest.NewService(t)
	svc.Set("edity-sandbox", rollouttest.Recorded(t, "live-1617.json"))
	svc.Down(true)
	client := rollout.New(svc.URL, &http.Client{})
	var e *rollout.Error
	if got, err := client.Status(t.Context(), "edity-sandbox", rollout.Production); !errors.As(err, &e) || e.Status != 0 || e.Refused() {
		t.Errorf("a dropped connection: Status = %+v, %v, want an Error with no status", got, err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if got, err := rollout.New(closed.URL, &http.Client{}).Status(t.Context(), "edity-sandbox", rollout.Production); !errors.As(err, &e) || e.Status != 0 {
		t.Errorf("nothing listening: Status = %+v, %v, want an Error with no status", got, err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(slow.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	got, err := rollout.New(slow.URL, &http.Client{}).Status(ctx, "edity-sandbox", rollout.Production)
	if !errors.As(err, &e) || e.Status != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("too slow: Status = %+v, %v, want an Error with no status that the deadline caused", got, err)
	}
}
