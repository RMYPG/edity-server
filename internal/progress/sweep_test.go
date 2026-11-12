package progress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/githubtest"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/rollout"
	"github.com/RMYPG/edity-server/internal/rollouttest"
	"github.com/RMYPG/edity-server/internal/site"
)

// liveAt is production running the Release tag, which the platform picked
// up last.
func liveAt(t testing.TB, tag string) func(*fakeGitHub) {
	return func(f *fakeGitHub) { f.status.Set(sandbox.ID, productionAt(t, tag, tag, rollout.Live, "")) }
}

// mainAt is a's answers to what a sweep asks, as GitHub answered them while
// main was the first Publish (first) or the second (second): the commits on
// main since the last Release, or all of main's had there been none.
func mainAt(t testing.TB, a map[string]githubtest.Response, sha string) {
	t.Helper()
	switch sha {
	case first:
		a["compare/v1.0.0...main"] = githubtest.Recorded(t, "compare-v1.0.0-4bc9310.http")
		// Once the first Publish is released, nothing is since.
		a["compare/v1.0.1...main"] = githubtest.Edited(t, githubtest.Recorded(t, "compare-v1.0.0-4bc9310.http"), func(body map[string]any) {
			body["status"], body["ahead_by"], body["total_commits"], body["commits"], body["files"] = "identical", 0, 0, []any{}, []any{}
		})
		a["commits?sha=main&per_page=100"] = githubtest.Recorded(t, "commits-4bc9310.http")
	case second:
		a["compare/v1.0.2...main"] = githubtest.Recorded(t, "compare-v1.0.2-f50d2c4.http")
	}
}

func TestASweepReportsAFailedPublishNoOneAsksAbout(t *testing.T) {
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	a := ciFailed(t)
	mainAt(t, a, first)
	f.set(at("18:52:20"), a)

	sw, err := tracker.Sweep(t.Context(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	failed := Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Run: "https://github.com/RMYPG/edity-sandbox/actions/runs/40934506192", Reason: "CI ended failure"}
	if sw.Release != "v1.0.0" || !maps.Equal(sw.Progress, map[string]Progress{first: failed}) || sw.Left != 0 {
		t.Errorf("Sweep = %+v, want the first Publish failed since v1.0.0", sw)
	}
	want := []Failure{{SHA: first, Message: firstMessage, Version: failed.Version, Run: failed.Run, Reason: failed.Reason, ID: "run:40934506192/1"}}
	if told := reporter.failures(); !slices.Equal(told, want) {
		t.Errorf("the Developer was told of %+v, want %+v", told, want)
	}
}

func TestASweepAsksOnlyAboutPublishesSinceTheLastRelease(t *testing.T) {
	// All released, and production runs v1.0.2: nothing since.
	f, tracker := newFake(t)
	liveAt(t, "v1.0.2")(f)
	a := answers(t)
	mainAt(t, a, second)
	f.set(at("19:05:00"), a)
	sw, err := tracker.Sweep(t.Context(), sandbox)
	if err != nil || sw.Release != "v1.0.2" || len(sw.Progress) != 0 {
		t.Errorf("Sweep = %+v, %v, want nothing since v1.0.2", sw, err)
	}
	if calls, _ := f.counts(); calls != 2 {
		t.Errorf("a sweep with nothing since the last Release made %d calls, want the tags and the comparison", calls)
	}

	// No Release yet: every Publish on main, and none of the Developer's
	// commits.
	f, tracker = newFake(t)
	a = answers(t)
	delete(a, "git/matching-refs/tags/v")
	mainAt(t, a, first)
	f.set(at("18:53:00"), a)
	sw, err = tracker.Sweep(t.Context(), sandbox)
	if err != nil || sw.Release != "" || !maps.Equal(sw.Progress, map[string]Progress{first: {Stage: Releasing, Version: release.Version{1, 0, 1}}}) {
		t.Errorf("Sweep = %+v, %v, want the first Publish releasing, with no Release", sw, err)
	}
	if f.asked["git/commits/"+developer] != 0 {
		t.Error("a sweep asked about a Developer's commit")
	}
}

func TestASweepReportsARolloutThatFailsAfterTheRelease(t *testing.T) {
	// The first Publish is released as v1.0.1, so nothing is since the
	// last Release, but production still runs v1.0.0 and the rollout of
	// v1.0.1 failed.
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	reason := recordedReason(t, "failed-170046.json")
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "v1.0.0", rollout.Failed, reason))
	a := answers(t)
	tagsUpTo(t, a, "v1.0.1")
	mainAt(t, a, first)
	f.set(at("18:58:00"), a)

	sw, err := tracker.Sweep(t.Context(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	failed := Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Reason: "its rollout failed"}
	if sw.Release != "v1.0.1" || sw.Running != "v1.0.0" || !maps.Equal(sw.Progress, map[string]Progress{first: failed}) {
		t.Errorf("Sweep = %+v, want the first Publish failed, since v1.0.0", sw)
	}
	want := []Failure{{SHA: first, Message: firstMessage, Version: failed.Version, Released: true, Reason: failed.Reason, Rollout: reason, ID: "rollout:" + first}}
	if told := reporter.failures(); !slices.Equal(told, want) {
		t.Errorf("the Developer was told of %+v, want %+v", told, want)
	}

	// Once production runs it, there is nothing to sweep.
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "v1.0.1", rollout.Live, ""))
	f.set(at("19:30:00"), a)
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || sw.Running != "v1.0.1" || len(sw.Progress) != 0 {
		t.Errorf("Sweep once it runs = %+v, %v, want nothing since v1.0.1", sw, err)
	}
}

func TestASweepReportsAFailureOfTheLastReleaseWhenNoVersionRuns(t *testing.T) {
	// The first Publish is released as v1.0.1, and the Site has no Pod
	// available, as when its first install fails: the service knows no
	// version that runs.
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	reason := recordedReason(t, "failed-170046.json")
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "", rollout.Failed, reason))
	a := answers(t)
	tagsUpTo(t, a, "v1.0.1")
	mainAt(t, a, first)
	f.set(at("18:58:00"), a)

	sw, err := tracker.Sweep(t.Context(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	failed := Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Reason: "its rollout failed"}
	if sw.Release != "v1.0.1" || sw.Running != "" || !maps.Equal(sw.Progress, map[string]Progress{first: failed}) {
		t.Errorf("Sweep = %+v, want the first Publish failed, since v1.0.0", sw)
	}
	want := []Failure{{SHA: first, Message: firstMessage, Version: failed.Version, Released: true, Reason: failed.Reason, Rollout: reason, ID: "rollout:" + first}}
	if told := reporter.failures(); !slices.Equal(told, want) {
		t.Errorf("the Developer was told of %+v, want %+v", told, want)
	}

	// A Pod restarting, long after the tag, is swept the same way, and
	// tells the Developer of nothing, even while the image scan fails.
	f, tracker = newFake(t)
	reporter = &fakeReporter{}
	tracker.reporter = reporter
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "", rollout.Deploying, scanFailing))
	f.set(at("21:00:00"), a)
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || sw.Progress[first].Stage != Deploying || len(reporter.failures()) != 0 {
		t.Errorf("Sweep = %+v, %v, told %+v, want the first Publish deploying, and nothing told", sw, err, reporter.failures())
	}

	// With a single Release, the last is all of main's.
	f, tracker = newFake(t)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "", rollout.Deploying, ""))
	b := answers(t)
	tagsUpTo(t, b, "v1.0.1")
	withoutTag(t, b, "v1.0.0")
	mainAt(t, b, first)
	f.set(at("18:58:00"), b)
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || sw.Progress[first].Stage != Deploying || f.asked["commits?sha=main&per_page=100"] != 1 {
		t.Errorf("Sweep = %+v, %v, want the first Publish deploying, from main's commits", sw, err)
	}
}

func TestASweepWithoutTheRolloutStatusSweepsSinceTheLastRelease(t *testing.T) {
	// The service can't be reached: what GitHub tells is still reported.
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	f.status.Down(true)
	a := ciFailed(t)
	mainAt(t, a, first)
	f.set(at("18:52:20"), a)
	sw, err := tracker.Sweep(t.Context(), sandbox)
	var e *rollout.Error
	if !errors.As(err, &e) || sw.Release != "v1.0.0" || sw.Running != "" || sw.Progress[first].Stage != Failed || len(reporter.failures()) != 1 {
		t.Errorf("Sweep = %+v, %v, want the first Publish failed, and the service's error", sw, err)
	}

	// Production runs a tag GitHub has no Release of: since the last
	// Release.
	f, tracker = newFake(t)
	f.status.Set(sandbox.ID, productionAt(t, "v0.9.0", "v0.9.0", rollout.Live, ""))
	a = answers(t)
	mainAt(t, a, second)
	f.set(at("19:05:00"), a)
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || sw.Running != "v0.9.0" || len(sw.Progress) != 0 || f.asked["compare/v0.9.0...main"] != 1 {
		t.Errorf("Sweep = %+v, %v, want nothing since v1.0.2, once v0.9.0 was not found", sw, err)
	}
}

func TestASweepAsksAboutTheNewestPublishesOnly(t *testing.T) {
	f, tracker := newFake(t)
	a := answers(t)
	tagsUpTo(t, a, "v1.0.0")
	// Twelve Publishes since v1.0.0, oldest first, which GitHub has no
	// answers about.
	var shas []string
	a["compare/v1.0.0...main"] = githubtest.Edited(t, githubtest.Recorded(t, "compare-v1.0.0-4bc9310.http"), func(body map[string]any) {
		publish := body["commits"].([]any)[0].(map[string]any)
		var commits []any
		for n := range 12 {
			sha := fmt.Sprintf("%040x", n+1)
			shas = append(shas, sha)
			c := maps.Clone(publish)
			c["sha"] = sha
			commits = append(commits, c)
		}
		body["commits"] = commits
	})
	f.set(at("19:05:00"), a)

	sw, err := tracker.Sweep(t.Context(), sandbox)
	if !errors.Is(err, ErrNotPublish) || len(sw.Progress) != 0 || sw.Left != 2 {
		t.Errorf("Sweep = %+v, %v, want 2 left and no Publish found", sw, err)
	}
	for i, sha := range shas {
		if asked := f.asked["git/commits/"+sha] > 0; asked != (i >= 2) {
			t.Errorf("Publish %d of 12 asked about: %t", i+1, asked)
		}
	}
}

// syncBuffer is a log the test reads while the sweeps write it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestSweepsLogEachSweepAndStopWithTheServer(t *testing.T) {
	f, tracker := newFake(t)
	liveAt(t, "v1.0.2")(f)
	a := answers(t)
	mainAt(t, a, second)
	f.set(at("19:05:00"), a)
	logs := &syncBuffer{}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tracker.Sweeps(ctx, []site.Site{sandbox}, 10*time.Millisecond, slog.New(slog.NewJSONHandler(logs, nil)))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(logs.String(), `"msg":"swept"`) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), `"msg":"swept","site":"edity-sandbox","release":"v1.0.2","running":"v1.0.2","publishes":0,"failed":0,"left":0`) {
		t.Errorf("the sweeps logged:\n%s", logs.String())
	}
	stop()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeps went on after the server stopped")
	}
}

// TestSweepingStaysWithinTheRateLimit counts what the sweeps cost the
// installation's rate limit, the 5,000 calls an hour that every Site
// shares, in the hour of a Site with no Publish since the Release
// production runs, of one whose Publish stays under way all along, and of
// one whose Release production does not run yet all along. GitHub does not
// count a call it answers 304.
//
// A sweep of a Site with nothing since its last Release, which production
// runs, makes 2 calls: the tags and the comparison of the last one with
// main, 24 an hour. A Publish since then, or since the Release production
// runs, adds the 5 calls of a look at its progress. The rollout status
// service, asked once a sweep and once a look at a released Publish, is not
// GitHub's, and has no rate limit. Each answer is counted the first time,
// again when the token is replaced, and when it changes, so an hour of
// sweeps costs a few calls for each Site, and a few more for each Publish
// under way: next to the Editor's polling
// (TestPollingStaysWithinTheRateLimit), nothing.
func TestSweepingStaysWithinTheRateLimit(t *testing.T) {
	releasing := firstPublish(t)[4]
	mainAt(t, releasing.answers, first)
	deploying := firstPublish(t)[6]
	mainAt(t, deploying.answers, first)
	idle := answers(t)
	mainAt(t, idle, second)

	for _, c := range []struct {
		name           string
		from           time.Time
		a              map[string]githubtest.Response
		status         rollouttest.Answer
		stage          Stage
		calls, counted int
	}{
		{"no Publish since the last Release", at("19:05:00"), idle, productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, ""), "", 25, 5},
		{"a Publish releasing all along", releasing.from, releasing.answers, stillAtV100(t), Releasing, 85, 13},
		{"a Release deploying all along", deploying.from, deploying.answers, deploying.status, Deploying, 85, 13},
	} {
		f, tracker := newFake(t)
		f.status.Set(sandbox.ID, c.status)
		for now := c.from; now.Before(c.from.Add(time.Hour)); now = now.Add(SweepPeriod) {
			f.set(now, c.a)
			sw, err := tracker.Sweep(t.Context(), sandbox)
			if err != nil || (c.stage == "") != (len(sw.Progress) == 0) || (c.stage != "" && sw.Progress[first].Stage != c.stage) {
				t.Fatalf("%s, at %s: Sweep = %+v, %v", c.name, now.Format(time.TimeOnly), sw, err)
			}
		}
		calls, counted := f.counts()
		t.Logf("%s: an hour of sweeps makes %d calls, %d counted against the installation's 5,000", c.name, calls, counted)
		if calls > c.calls || counted > c.counted {
			t.Errorf("%s: %d calls, %d counted, want at most %d and %d", c.name, calls, counted, c.calls, c.counted)
		}
	}
}
