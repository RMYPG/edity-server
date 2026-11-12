package progress

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/githubtest"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/rollout"
	"github.com/RMYPG/edity-server/internal/rollouttest"
)

// The rollout status service's reasons, as the contract describes them,
// for what nothing recorded.
const (
	scanFailing     = "ImageRepository flux-system/edity-sandbox: Ready=False (ReconciliationFailed): auth failed for ghcr.io/rmypg/edity-sandbox"
	noResolvedTag   = "ImagePolicy flux-system/edity-sandbox-production has no resolved tag"
	helmReleaseGone = "HelmRelease edity-sandbox/edity-sandbox not found"
)

// recordedReason is the reason of the service's answer recorded in name.
func recordedReason(t testing.TB, name string) string {
	t.Helper()
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rollouttest.Recorded(t, name).Body, &body); err != nil || body.Reason == "" {
		t.Fatalf("%s: %v, reason %q", name, err, body.Reason)
	}
	return body.Reason
}

// withoutTag edits the tag list to drop a tag, as though the Release on
// Publish run that was to push it had given way to a newer one.
func withoutTag(t testing.TB, a map[string]githubtest.Response, dropped string) {
	t.Helper()
	r := a["git/matching-refs/tags/v"]
	var refs []map[string]any
	if err := json.Unmarshal(r.Body, &refs); err != nil {
		t.Fatal(err)
	}
	refs = slices.DeleteFunc(refs, func(ref map[string]any) bool { return ref["ref"] == "refs/tags/"+dropped })
	b, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	a["git/matching-refs/tags/v"] = githubtest.Response{Status: r.Status, Header: r.Header, Body: b}
}

// lightweight edits the tag list so that the tag is a lightweight one on
// the commit sha, which has no date of its own.
func lightweight(t testing.TB, a map[string]githubtest.Response, name, sha string) {
	t.Helper()
	r := a["git/matching-refs/tags/v"]
	var refs []map[string]any
	if err := json.Unmarshal(r.Body, &refs); err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref["ref"] == "refs/tags/"+name {
			ref["object"] = map[string]any{"sha": sha, "type": "commit", "url": "https://api.github.com/repos/RMYPG/edity-sandbox/git/commits/" + sha}
		}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	a["git/matching-refs/tags/v"] = githubtest.Response{Status: r.Status, Header: r.Header, Body: b}
}

// TestEachDeploymentStageFromTheRolloutStatus follows each stage past
// released, with GitHub's answers as recorded, or edited to how they were
// earlier, and the rollout status service's at the versions GitHub's
// answers were recorded for: the first Publish is v1.0.1, tagged at
// 18:52:44, and the second v1.0.2, tagged at 18:55:28. Nothing recorded the
// service at those versions, so its answers are built as its contract
// describes them, with the reason it gave live at 17:00:46 on 2026-11-10
// where one is needed. TestEachStageFromTheRecordedRolloutStatus checks the
// same rules against its recorded answers.
func TestEachDeploymentStageFromTheRolloutStatus(t *testing.T) {
	recorded := recordedReason(t, "failed-170046.json")
	onlyFirst := func(t *testing.T, a map[string]githubtest.Response) { tagsUpTo(t, a, "v1.0.1") }
	v101, v102 := release.Version{1, 0, 1}, release.Version{1, 0, 2}
	for name, c := range map[string]struct {
		sha    string
		now    time.Time
		edit   func(t *testing.T, a map[string]githubtest.Response)
		status rollouttest.Answer
		want   Progress
		// failure is the ID of the Failure the Developer is told of, when
		// the Publish failed, and rollout what the Failure quotes of the
		// service.
		failure, rollout string
	}{
		"released: the platform has yet to pick it up": {
			sha: first, now: at("18:53:00"), edit: onlyFirst,
			status: stillAtV100(t),
			want:   Progress{Stage: Released, Version: v101},
		},
		"released: neither version known, and nothing failed": {
			sha: first, now: at("18:53:00"), edit: onlyFirst,
			status: productionAt(t, "", "", rollout.Deploying, ""),
			want:   Progress{Stage: Released, Version: v101},
		},
		"deploying: its version picked up": {
			sha: first, now: at("18:55:00"), edit: onlyFirst,
			status: productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, ""),
			want:   Progress{Stage: Deploying, Version: v101},
		},
		"deploying: a newer version picked up": {
			sha: first, now: at("18:56:00"),
			status: productionAt(t, "v1.0.2", "v1.0.0", rollout.Deploying, ""),
			want:   Progress{Stage: Deploying, Version: v101},
		},
		"deploying: no ReplicaSet available, as while the pod restarts": {
			sha: first, now: at("18:56:00"), edit: onlyFirst,
			status: productionAt(t, "v1.0.1", "", rollout.Deploying, ""),
			want:   Progress{Stage: Deploying, Version: v101},
		},
		// With nothing failing, a Pod restarting is no Release that did
		// not go live, however long after its tag.
		"deploying: the pod restarting, an hour after its tag": {
			sha: first, now: at("21:00:00"), edit: onlyFirst,
			status: productionAt(t, "v1.0.1", "", rollout.Deploying, ""),
			want:   Progress{Stage: Deploying, Version: v101},
		},
		"released: the pod restarting, an hour after its tag, the platform yet to pick it up": {
			sha: first, now: at("21:00:00"), edit: onlyFirst,
			status: productionAt(t, "v1.0.0", "", rollout.Deploying, ""),
			want:   Progress{Stage: Released, Version: v101},
		},
		"failed: no version running an hour after its tag, with what the service says": {
			sha: first, now: at("21:00:00"), edit: onlyFirst,
			status:  productionAt(t, "v1.0.1", "", rollout.Deploying, "Deployment edity-sandbox/edity-sandbox-sws not found"),
			want:    Progress{Stage: Failed, Version: v101, Reason: "not live 60 minutes after its Release"},
			failure: "not-live:" + first, rollout: "Deployment edity-sandbox/edity-sandbox-sws not found",
		},
		"deploying: picked up while the image scan fails": {
			sha: first, now: at("18:56:00"), edit: onlyFirst,
			status: productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, scanFailing),
			want:   Progress{Stage: Deploying, Version: v101},
		},
		"live: its version runs": {
			sha: first, now: at("18:57:00"), edit: onlyFirst,
			status: productionAt(t, "v1.0.1", "v1.0.1", rollout.Live, ""),
			want:   Progress{Stage: Live, Version: v101},
		},
		"live: the first, overtaken, once the second's runs (P8)": {
			sha: first, now: at("18:58:10"),
			status: productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, ""),
			want:   Progress{Stage: Live, Version: v101},
		},
		"live: the second, which overtook the first": {
			sha: second, now: at("18:58:10"),
			status: productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, ""),
			want:   Progress{Stage: Live, Version: v102},
		},
		"live: it runs while the rollout of a newer version fails": {
			sha: first, now: at("18:58:10"),
			status: productionAt(t, "v1.0.2", "v1.0.1", rollout.Failed, recorded),
			want:   Progress{Stage: Live, Version: v101},
		},
		"live: it runs while the image scan fails": {
			sha: second, now: at("18:58:10"),
			status: productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, scanFailing),
			want:   Progress{Stage: Live, Version: v102},
		},
		"failed: its rollout, without waiting for the hour": {
			sha: second, now: at("18:58:10"),
			status:  productionAt(t, "v1.0.2", "v1.0.1", rollout.Failed, recorded),
			want:    Progress{Stage: Failed, Version: v102, Reason: "its rollout failed"},
			failure: "rollout:" + second, rollout: recorded,
		},
		"failed: the rollout of the newer version that contains it": {
			sha: first, now: at("18:58:10"),
			status:  productionAt(t, "v1.0.2", "v1.0.0", rollout.Failed, recorded),
			want:    Progress{Stage: Failed, Version: v101, Reason: "its rollout failed"},
			failure: "rollout:" + first, rollout: recorded,
		},
		"failed: a failing image scan, while live at a target older than the Publish": {
			sha: second, now: at("18:58:10"),
			status:  productionAt(t, "v1.0.1", "v1.0.1", rollout.Live, scanFailing),
			want:    Progress{Stage: Failed, Version: v102, Reason: "the platform did not pick up its Release"},
			failure: "no-target:" + second, rollout: scanFailing,
		},
		"failed: a missing HelmRelease, while live at a target older than the Publish": {
			sha: second, now: at("18:58:10"),
			status:  productionAt(t, "v1.0.1", "v1.0.1", rollout.Live, helmReleaseGone),
			want:    Progress{Stage: Failed, Version: v102, Reason: "the platform did not pick up its Release"},
			failure: "no-target:" + second, rollout: helmReleaseGone,
		},
		"failed: an ImagePolicy with no resolved tag": {
			sha: first, now: at("18:53:00"), edit: onlyFirst,
			status:  productionAt(t, "", "v1.0.0", rollout.Deploying, noResolvedTag),
			want:    Progress{Stage: Failed, Version: v101, Reason: "the platform did not pick up its Release"},
			failure: "no-target:" + first, rollout: noResolvedTag,
		},
		"deploying: an hour after its tag, to the second": {
			sha: first, now: at("19:52:44"), edit: onlyFirst,
			status: productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, ""),
			want:   Progress{Stage: Deploying, Version: v101},
		},
		"failed: not live an hour after its tag": {
			sha: first, now: at("19:52:45"), edit: onlyFirst,
			status:  productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, ""),
			want:    Progress{Stage: Failed, Version: v101, Reason: "not live 60 minutes after its Release"},
			failure: "not-live:" + first,
		},
		"failed: not picked up an hour after its tag": {
			sha: first, now: at("19:52:45"), edit: onlyFirst,
			status:  stillAtV100(t),
			want:    Progress{Stage: Failed, Version: v101, Reason: "not live 60 minutes after its Release"},
			failure: "not-live:" + first,
		},
		"failed: not live an hour after its tag, with what the service says": {
			sha: first, now: at("19:52:45"), edit: onlyFirst,
			status:  productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, scanFailing),
			want:    Progress{Stage: Failed, Version: v101, Reason: "not live 60 minutes after its Release"},
			failure: "not-live:" + first, rollout: scanFailing,
		},
		"failed: its rollout, still, an hour after its tag": {
			sha: first, now: at("20:00:00"), edit: onlyFirst,
			status:  productionAt(t, "v1.0.1", "v1.0.0", rollout.Failed, recorded),
			want:    Progress{Stage: Failed, Version: v101, Reason: "its rollout failed"},
			failure: "rollout:" + first, rollout: recorded,
		},
		"live: rolled out after the hour": {
			sha: first, now: at("21:00:00"), edit: onlyFirst,
			status: productionAt(t, "v1.0.1", "v1.0.1", rollout.Live, ""),
			want:   Progress{Stage: Live, Version: v101},
		},
		"live: a newer version rolled out after the hour": {
			sha: first, now: at("21:00:00"),
			status: productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, ""),
			want:   Progress{Stage: Live, Version: v101},
		},
		"failed: an hour after its own tag, though a newer Release contains it too": {
			sha: first, now: at("19:52:45"),
			status:  productionAt(t, "v1.0.2", "v1.0.0", rollout.Deploying, ""),
			want:    Progress{Stage: Failed, Version: v101, Reason: "not live 60 minutes after its Release"},
			failure: "not-live:" + first,
		},
		"deploying: the hour counts from the Release that first contained it, when a newer one overtook its own": {
			sha: first, now: at("19:55:28"),
			edit:   func(t *testing.T, a map[string]githubtest.Response) { withoutTag(t, a, "v1.0.1") },
			status: productionAt(t, "v1.0.2", "v1.0.0", rollout.Deploying, ""),
			want:   Progress{Stage: Deploying, Version: v101},
		},
		"failed: an hour after the Release that first contained it": {
			sha: first, now: at("19:55:29"),
			edit:    func(t *testing.T, a map[string]githubtest.Response) { withoutTag(t, a, "v1.0.1") },
			status:  productionAt(t, "v1.0.2", "v1.0.0", rollout.Deploying, ""),
			want:    Progress{Stage: Failed, Version: v101, Reason: "not live 60 minutes after its Release"},
			failure: "not-live:" + first,
		},
		"failed: an hour after the commit of a lightweight tag": {
			sha: first, now: at("19:50:02"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.1")
				lightweight(t, a, "v1.0.1", first)
			},
			status:  productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, ""),
			want:    Progress{Stage: Failed, Version: v101, Reason: "not live 60 minutes after its Release"},
			failure: "not-live:" + first,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, tracker := newFake(t)
			reporter := &fakeReporter{}
			tracker.reporter = reporter
			a := answers(t)
			if c.edit != nil {
				c.edit(t, a)
			}
			f.set(c.now, a)
			f.status.Set(sandbox.ID, c.status)
			got, err := tracker.Progress(t.Context(), sandbox, c.sha)
			if err != nil || got != c.want {
				t.Errorf("Progress = %+v, %v, want %+v", got, err, c.want)
			}
			var want []Failure
			if c.failure != "" {
				want = []Failure{{SHA: c.sha, Message: firstMessage, Version: c.want.Version, Released: true, Reason: c.want.Reason, Rollout: c.rollout, ID: c.failure}}
			}
			if told := reporter.failures(); !slices.Equal(told, want) {
				t.Errorf("the Developer was told of %+v, want %+v", told, want)
			}
			if f.status.Asked(sandbox.ID) != 1 {
				t.Errorf("the rollout status service was asked %d times, want once", f.status.Asked(sandbox.ID))
			}
		})
	}
}

// TestEachStageFromTheRecordedRolloutStatus works out each stage the
// rollout status service's answers recorded live on edity-sandbox tell
// (rollouttest), read as the server reads them, for a Publish of each
// version around them. Those versions were the Developer's, not Publishes,
// so no GitHub answers go with them: TestEachDeploymentStageFromTheRolloutStatus
// follows the same rules through the Tracker.
func TestEachStageFromTheRecordedRolloutStatus(t *testing.T) {
	for _, c := range []struct {
		recording string
		version   release.Version
		stage     Stage
		kind      string
	}{
		// v1.0.8 picked up while v1.0.7 runs.
		{"deploying-1506.json", release.Version{1, 0, 8}, Deploying, ""},
		{"deploying-1506.json", release.Version{1, 0, 7}, Live, ""},
		{"deploying-1506.json", release.Version{1, 0, 6}, Live, ""},
		{"deploying-1506.json", release.Version{1, 0, 9}, Released, ""},
		// v1.0.8 runs.
		{"live-1617.json", release.Version{1, 0, 8}, Live, ""},
		{"live-1617.json", release.Version{1, 0, 7}, Live, ""},
		{"live-1617.json", release.Version{1, 0, 9}, Released, ""},
		// v1.0.9's rollout failing while v1.0.8 runs.
		{"failed-170046.json", release.Version{1, 0, 9}, Failed, "rollout"},
		{"failed-170046.json", release.Version{1, 0, 8}, Live, ""},
		// A Publish newer than the target while a failure holds: the
		// platform's next target would fail at once too.
		{"failed-170046.json", release.Version{1, 0, 10}, Failed, "no-target"},
		// Its reason was read truncated: the phase tells all the same.
		{"failed-171711-truncated.json", release.Version{1, 0, 9}, Failed, "rollout"},
		{"failed-171711-truncated.json", release.Version{1, 0, 8}, Live, ""},
	} {
		svc := rollouttest.NewService(t)
		svc.Set(sandbox.ID, rollouttest.Recorded(t, c.recording))
		status, err := rollout.New(svc.URL, &http.Client{}).Status(t.Context(), sandbox.ID, rollout.Production)
		if err != nil {
			t.Fatal(err)
		}
		if stage, kind, _ := byStatus(c.version, status); stage != c.stage || kind != c.kind {
			t.Errorf("%s, a Publish of v%s: %s %q, want %s %q", c.recording, c.version, stage, kind, c.stage, c.kind)
		}
	}
}

func TestAStatusServiceWithoutAnAnswerNeverFailsAPublish(t *testing.T) {
	for name, noAnswer := range map[string]func(t *testing.T, f *fakeGitHub){
		"it can't be reached": func(_ *testing.T, f *fakeGitHub) { f.status.Down(true) },
		"its watches are not synced": func(t *testing.T, f *fakeGitHub) {
			f.status.Set(sandbox.ID, rollouttest.Error(t, http.StatusServiceUnavailable, "watches not synced"))
		},
		"it is not configured for the Site": func(t *testing.T, f *fakeGitHub) {
			f.status.Set(sandbox.ID, rollouttest.Error(t, http.StatusNotFound, "unknown site or environment"))
		},
		"a proxy in front of it fails": func(_ *testing.T, f *fakeGitHub) {
			f.status.Set(sandbox.ID, rollouttest.Answer{Status: http.StatusBadGateway, Body: []byte("Bad Gateway")})
		},
		// Read as nulls, the missing versions would be older than the
		// Publish, and the hour would fail it.
		"it answers a status with no versions": func(_ *testing.T, f *fakeGitHub) {
			f.status.Set(sandbox.ID, rollouttest.Answer{Status: http.StatusOK, Body: []byte(`{"phase":"deploying"}`)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, tracker := newFake(t)
			reporter := &fakeReporter{}
			tracker.reporter = reporter
			a := answers(t)
			tagsUpTo(t, a, "v1.0.1")
			noAnswer(t, f)

			// Long past the hour after its tag, and with no answer, the
			// Publish is not failed: the request is made again.
			for _, now := range []time.Time{at("18:53:00"), at("19:52:45"), at("23:00:00")} {
				f.set(now, a)
				var e *rollout.Error
				if p, err := tracker.Progress(t.Context(), sandbox, first); !errors.As(err, &e) {
					t.Fatalf("at %s: Progress = %+v, %v, want the service's Error", now.Format(time.TimeOnly), p, err)
				}
			}
			if told := reporter.failures(); len(told) != 0 {
				t.Errorf("the Developer was told of %+v", told)
			}

			// A Publish not released yet does not ask the service: the
			// second, its CI passed, waits for v1.0.2.
			asked := f.status.Asked(sandbox.ID)
			f.set(at("23:00:05"), a)
			if p, err := tracker.Progress(t.Context(), sandbox, second); err != nil || p.Stage != Releasing || f.status.Asked(sandbox.ID) != asked {
				t.Errorf("the second Publish = %+v, %v, want releasing without asking the service", p, err)
			}

			// Once it answers, the hour counts, and a later rollout still
			// makes the Publish live.
			f.status.Down(false)
			f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, ""))
			f.set(at("23:00:10"), a)
			if p, err := tracker.Progress(t.Context(), sandbox, first); err != nil || p.Stage != Failed {
				t.Errorf("answered an hour after its tag: Progress = %+v, %v, want failed", p, err)
			}
			if told := reporter.failures(); len(told) != 1 || told[0].ID != "not-live:"+first {
				t.Errorf("the Developer was told of %+v, want the missed hour", told)
			}
			f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "v1.0.1", rollout.Live, ""))
			f.set(at("23:00:20"), a)
			if p, err := tracker.Progress(t.Context(), sandbox, first); err != nil || p.Stage != Live {
				t.Errorf("rolled out: Progress = %+v, %v, want live", p, err)
			}
		})
	}
}
