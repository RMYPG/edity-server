package progress

import (
	"bytes"
	"context"
	"encoding/json"
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

	// With Releases before the last, from the one before it, not from all
	// of main's: the second Publish, and not the first, whose v1.0.1 the
	// Site ran before. main is the second Publish, whose comparison with
	// v1.0.1 is the first's with v1.0.0 for its commit's sha, and whose
	// commits are the first's with it on top.
	f, tracker = newFake(t)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.2", "", rollout.Failed, reason))
	c := answers(t)
	c["compare/v1.0.1...main"] = githubtest.Edited(t, githubtest.Recorded(t, "compare-v1.0.0-4bc9310.http"), func(body map[string]any) {
		body["commits"].([]any)[0].(map[string]any)["sha"] = second
	})
	listed := githubtest.Recorded(t, "commits-4bc9310.http")
	var commits []map[string]any
	if err := json.Unmarshal(listed.Body, &commits); err != nil {
		t.Fatal(err)
	}
	top := maps.Clone(commits[0])
	top["sha"] = second
	body, err := json.Marshal(append([]map[string]any{top}, commits...))
	if err != nil {
		t.Fatal(err)
	}
	c["commits?sha=main&per_page=100"] = githubtest.Response{Status: listed.Status, Header: listed.Header, Body: body}
	f.set(at("18:58:00"), c)
	sw, err = tracker.Sweep(t.Context(), sandbox)
	if err != nil || !slices.Equal(slices.Collect(maps.Keys(sw.Progress)), []string{second}) || sw.Progress[second].Stage != Failed || f.asked["commits?sha=main&per_page=100"] != 0 {
		t.Errorf("Sweep = %+v, %v, want the second Publish alone, failed, since v1.0.1", sw, err)
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

func TestASweepFromBeforeAnOlderTargetWhenNoVersionRuns(t *testing.T) {
	// v1.0.1's rollout failed with no version running, and v1.0.2 followed,
	// which the platform has yet to pick up. The sweep starts from the
	// Release before the target, v1.0.0, not from v1.0.1, so that the
	// first Publish, whose rollout is the one that failed, is swept. main
	// is the second Publish: since v1.0.0 are both Publishes, the recorded
	// comparison with the second added, and since v1.0.1 the second alone.
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	reason := recordedReason(t, "failed-170046.json")
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "", rollout.Failed, reason))
	a := answers(t)
	compared := githubtest.Recorded(t, "compare-v1.0.0-4bc9310.http")
	a["compare/v1.0.0...main"] = githubtest.Edited(t, compared, func(body map[string]any) {
		commits := body["commits"].([]any)
		next := maps.Clone(commits[0].(map[string]any))
		next["sha"] = second
		body["commits"] = append(commits, next)
	})
	a["compare/v1.0.1...main"] = githubtest.Edited(t, compared, func(body map[string]any) {
		body["commits"].([]any)[0].(map[string]any)["sha"] = second
	})
	f.set(at("18:58:00"), a)

	sw, err := tracker.Sweep(t.Context(), sandbox)
	failed := Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Reason: "its rollout failed"}
	if err != nil || !maps.Equal(sw.Progress, map[string]Progress{first: failed, second: {Stage: Released, Version: release.Version{1, 0, 2}}}) {
		t.Errorf("Sweep = %+v, %v, want the first Publish failed and the second released, since v1.0.0", sw, err)
	}
	want := []Failure{{SHA: first, Message: firstMessage, Version: failed.Version, Released: true, Reason: failed.Reason, Rollout: reason, ID: "rollout:" + first}}
	if told := reporter.failures(); !slices.Equal(told, want) {
		t.Errorf("the Developer was told of %+v, want %+v", told, want)
	}

	// The target the first Release, with no Release before it: all of
	// main's.
	f, tracker = newFake(t)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.0", "", rollout.Deploying, ""))
	b := answers(t)
	mainAt(t, b, first)
	f.set(at("18:58:00"), b)
	if _, err := tracker.Sweep(t.Context(), sandbox); err != nil || f.asked["commits?sha=main&per_page=100"] != 1 {
		t.Errorf("Sweep: %v, and main's commits asked %d times, want once", err, f.asked["commits?sha=main&per_page=100"])
	}
}

// comparedWith is GitHub's comparison of a tag with main, holding the
// commits shas, oldest first, made from the recorded comparison of v1.0.0
// with the first Publish. A sha that is not a Publish's is a Developer's
// commit, not by the App.
func comparedWith(t testing.TB, shas ...string) githubtest.Response {
	t.Helper()
	return githubtest.Edited(t, githubtest.Recorded(t, "compare-v1.0.0-4bc9310.http"), func(body map[string]any) {
		publish := body["commits"].([]any)[0].(map[string]any)
		var commits []any
		for _, sha := range shas {
			c := maps.Clone(publish)
			c["sha"] = sha
			if sha != first && sha != second {
				c["commit"] = map[string]any{"author": map[string]any{"name": "Mario Rojas Padrón"}}
			}
			commits = append(commits, c)
		}
		body["commits"] = commits
	})
}

// developerBump is a Developer's commit on main, which bumps the version
// and is no Publish.
const developerBump = "dddddddddddddddddddddddddddddddddddddddd"

func TestASweepStartsWhereProductionWasLastSeenWhenNoVersionRuns(t *testing.T) {
	// v1.0.0 is live, then v1.0.1 holds the first Publish, and v1.0.2
	// only a Developer's commit, both cut before either rolled out. The
	// service then knows no version that runs, and the rollout of v1.0.2
	// failed. A sweep that saw v1.0.0 running starts from it, and reports
	// the first Publish, which the rule for a server that remembers
	// nothing, from the Release before the last, leaves out.
	reason := recordedReason(t, "failed-170046.json")
	a := answers(t)
	a["compare/v1.0.0...main"] = comparedWith(t, first, developerBump)
	a["compare/v1.0.1...main"] = comparedWith(t, developerBump)
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	f.set(at("18:58:00"), a)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.0", "v1.0.0", rollout.Live, ""))
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || sw.Progress[first].Stage != Released {
		t.Fatalf("Sweep at v1.0.0 = %+v, %v, want the first Publish released", sw, err)
	}

	f.status.Set(sandbox.ID, productionAt(t, "v1.0.2", "", rollout.Failed, reason))
	f.set(at("19:10:00"), a)
	sw, err := tracker.Sweep(t.Context(), sandbox)
	failed := Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Reason: "its rollout failed"}
	if err != nil || !maps.Equal(sw.Progress, map[string]Progress{first: failed}) {
		t.Errorf("Sweep with no version running = %+v, %v, want the first Publish failed, since v1.0.0", sw, err)
	}
	want := []Failure{{SHA: first, Message: firstMessage, Version: failed.Version, Released: true, Reason: failed.Reason, Rollout: reason, ID: "rollout:" + first}}
	if told := reporter.failures(); !slices.Equal(told, want) {
		t.Errorf("the Developer was told of %+v, want %+v", told, want)
	}

	// A new Tracker, as after the server starts, remembers nothing, and
	// starts from the Release before the last.
	f, tracker = newFake(t)
	f.set(at("19:10:00"), a)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.2", "", rollout.Failed, reason))
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || len(sw.Progress) != 0 || f.asked["compare/v1.0.1...main"] != 1 || f.asked["compare/v1.0.0...main"] != 0 {
		t.Errorf("Sweep remembering nothing = %+v, %v, want nothing swept, since v1.0.1", sw, err)
	}
}

func TestASweepThatSawTheLastReleaseRunningReportsNothingWhileAPodRestarts(t *testing.T) {
	// Live at v1.0.2, with a rollout signal of something else holding: the
	// Pod restarts, and the service knows no version that runs. A sweep
	// that saw v1.0.2 running starts from it, and reports none of v1.0.2's
	// Publishes, which were live; the Release before the last would have
	// had the second Publish reported as its rollout failing.
	a := answers(t)
	mainAt(t, a, second)
	a["compare/v1.0.1...main"] = comparedWith(t, second)
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	f.set(at("19:05:00"), a)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, ""))
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || len(sw.Progress) != 0 {
		t.Fatalf("Sweep at v1.0.2 = %+v, %v, want nothing since it", sw, err)
	}

	f.status.Set(sandbox.ID, productionAt(t, "v1.0.2", "", rollout.Failed, recordedReason(t, "failed-171711-truncated.json")))
	f.set(at("19:10:00"), a)
	if sw, err := tracker.Sweep(t.Context(), sandbox); err != nil || len(sw.Progress) != 0 || f.asked["compare/v1.0.1...main"] != 0 {
		t.Errorf("Sweep while the Pod restarts = %+v, %v, want nothing, since v1.0.2", sw, err)
	}
	if told := reporter.failures(); len(told) != 0 {
		t.Errorf("the Developer was told of %+v", told)
	}
}

func TestASweepIgnoresARememberedReleaseThatIsNoLongerTagged(t *testing.T) {
	// The sweep saw v1.0.2 running, and then v1.0.2's tag was deleted. With
	// no version running and v1.0.1's rollout failing, it starts from the
	// Release before the last, v1.0.0, and not from v1.0.2.
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	a := answers(t)
	mainAt(t, a, second)
	f.set(at("19:05:00"), a)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, ""))
	if _, err := tracker.Sweep(t.Context(), sandbox); err != nil {
		t.Fatal(err)
	}

	b := answers(t)
	tagsUpTo(t, b, "v1.0.1")
	mainAt(t, b, first)
	f.set(at("19:10:00"), b)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.1", "", rollout.Failed, recordedReason(t, "failed-170046.json")))
	sw, err := tracker.Sweep(t.Context(), sandbox)
	if err != nil || sw.Progress[first].Stage != Failed || f.asked["compare/v1.0.2...main"] != 1 {
		t.Errorf("Sweep = %+v, %v, want the first Publish failed, since v1.0.0", sw, err)
	}
	if told := reporter.failures(); len(told) != 1 || told[0].ID != "rollout:"+first {
		t.Errorf("the Developer was told of %+v, want the first Publish's rollout", told)
	}
}

func TestASweepWhileTheClusterRecoversReportsNothing(t *testing.T) {
	// The Site's Deployment and ImagePolicy are re-created, and the service
	// knows neither version, with a reason. The sweep reads the last
	// Release, v1.0.1, again, whose Publish was live minutes before: it is
	// released, and the Developer is told of nothing.
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	f.status.Set(sandbox.ID, productionAt(t, "", "", rollout.Deploying,
		"ImagePolicy flux-system/edity-sandbox-production not found; Deployment edity-sandbox/edity-sandbox-sws not found"))
	a := answers(t)
	tagsUpTo(t, a, "v1.0.1")
	mainAt(t, a, first)
	f.set(at("19:20:00"), a)
	sw, err := tracker.Sweep(t.Context(), sandbox)
	if err != nil || sw.Running != "" || !maps.Equal(sw.Progress, map[string]Progress{first: {Stage: Released, Version: release.Version{1, 0, 1}}}) {
		t.Errorf("Sweep = %+v, %v, want the first Publish released, since v1.0.0", sw, err)
	}
	if told := reporter.failures(); len(told) != 0 {
		t.Errorf("the Developer was told of %+v", told)
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

func TestTheVersionLastSeenRunningIsSafeForConcurrentUse(t *testing.T) {
	_, tracker := newFake(t)
	tags := []tag{{version: release.Version{1, 0, 1}}, {version: release.Version{1, 0, 2}}}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			tracker.remember(sandbox.ID, fmt.Sprintf("v1.0.%d", 1+i%2))
			if seen, ok := tracker.lastRan(sandbox.ID, tags); !ok || (seen != "v1.0.1" && seen != "v1.0.2") {
				t.Errorf("lastRan = %q, %t", seen, ok)
			}
		})
	}
	wg.Wait()
	// Each answer replaces the last, so a rollback moves it down.
	tracker.remember(sandbox.ID, "v1.0.2")
	tracker.remember(sandbox.ID, "v1.0.1")
	if seen, ok := tracker.lastRan(sandbox.ID, tags); !ok || seen != "v1.0.1" {
		t.Errorf("lastRan after a rollback = %q, %t, want v1.0.1", seen, ok)
	}
}
