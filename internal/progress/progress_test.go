package progress

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/githubtest"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/rollout"
	"github.com/RMYPG/edity-server/internal/rollouttest"
	"github.com/RMYPG/edity-server/internal/site"
)

// The sandbox's two Publishes of 2026-10-29, which testdata/record.sh
// records GitHub's answers about, and the Developer commit before them.
//
//   - first, v1.0.1, committed at 18:50:01. CI ran from 18:50:04 to
//     18:52:13, and Release on Publish from 18:52:16 to 18:52:53.
//   - second, v1.0.2, committed at 18:52:55. CI ran from 18:52:58 to
//     18:54:58, and Release on Publish from 18:55:01 to 18:55:36.
//
// Their Release on Publish runs are titled "Release on Publish", from before
// the template named each run after its commit. v1.0.1 was tagged at
// 18:52:44, and v1.0.2 at 18:55:28: annotated tags, whose objects are
// tagV101 and tagV102.
const (
	first     = "4bc931076ac8583bd8905a4c508c987877afc09d"
	second    = "f50d2c45e28706d25da874225f0f64770cd1ac12"
	developer = "31077c358f1b793aefc1c495c938055d99d3c227"
	tagV101   = "1ca0822932a9b62ffbd304358cb2c266eab2b6c4"
	tagV102   = "fd6377a10c7e49e869ba86c231298f36c2ec5c5e"
)

var sandbox = site.Site{ID: "edity-sandbox", Repository: site.Repository{Owner: "RMYPG", Name: "edity-sandbox"}}

func at(clock string) time.Time {
	t, err := time.Parse(time.RFC3339, "2026-10-29T"+clock+"Z")
	if err != nil {
		panic(err)
	}
	return t
}

func ciRuns(sha string) string {
	return "actions/runs?head_sha=" + sha + "&event=push&exclude_pull_requests=true"
}

// eachRun edits every run of a list of workflow runs, and drops those edit
// returns false for.
func eachRun(edit func(run map[string]any) bool) func(map[string]any) {
	return func(body map[string]any) {
		var kept []any
		for _, r := range body["workflow_runs"].([]any) {
			if edit(r.(map[string]any)) {
				kept = append(kept, r)
			}
		}
		body["workflow_runs"] = kept
		body["total_count"] = len(kept)
	}
}

// inProgress is a run as GitHub lists it while it runs.
func inProgress(run map[string]any, updated string) {
	run["status"], run["conclusion"], run["updated_at"] = "in_progress", nil, updated
}

// answers is GitHub as it answered about the two Publishes once both were
// released, recorded, by each call's path and query under the repository.
func answers(t testing.TB) map[string]githubtest.Response {
	t.Helper()
	return map[string]githubtest.Response{
		"git/commits/" + first:                       githubtest.Recorded(t, "commit-4bc9310.http"),
		"git/commits/" + second:                      githubtest.Recorded(t, "commit-f50d2c4.http"),
		"git/commits/" + developer:                   githubtest.Recorded(t, "commit-31077c3.http"),
		"contents/package.json?ref=" + first:         githubtest.Recorded(t, "package-4bc9310.http"),
		"contents/package.json?ref=" + second:        githubtest.Recorded(t, "package-f50d2c4.http"),
		"contents/package.json?ref=" + developer:     githubtest.Recorded(t, "package-31077c3.http"),
		ciRuns(first):                                githubtest.Recorded(t, "runs-4bc9310.http"),
		ciRuns(second):                               githubtest.Recorded(t, "runs-f50d2c4.http"),
		"git/matching-refs/tags/v":                   githubtest.Recorded(t, "tags.http"),
		"compare/" + first + "...v1.0.1?per_page=1":  githubtest.Recorded(t, "compare-4bc9310-v1.0.1.http"),
		"compare/" + first + "...v1.0.2?per_page=1":  githubtest.Recorded(t, "compare-4bc9310-v1.0.2.http"),
		"compare/" + second + "...v1.0.2?per_page=1": githubtest.Recorded(t, "compare-f50d2c4-v1.0.2.http"),
		releaseRuns:           githubtest.Recorded(t, "release-runs.http"),
		"git/tags/" + tagV101: githubtest.Recorded(t, "tag-v1.0.1.http"),
		"git/tags/" + tagV102: githubtest.Recorded(t, "tag-v1.0.2.http"),
	}
}

// productionAt is the rollout status service's answer about the sandbox's
// production, in the contract's shape, at the versions GitHub's answers
// were recorded for: none was recorded then. An empty target, running or
// reason is null.
func productionAt(t testing.TB, target, running string, phase rollout.Phase, reason string) rollouttest.Answer {
	t.Helper()
	return rollouttest.Synthetic(t, target, running, string(phase), reason)
}

// stillAtV100 is production before the platform picked up either Publish.
func stillAtV100(t testing.TB) rollouttest.Answer {
	return productionAt(t, "v1.0.0", "v1.0.0", rollout.Live, "")
}

// tagsUpTo edits the tag list down to the tags up to last, as it was before
// the later ones were pushed.
func tagsUpTo(t testing.TB, a map[string]githubtest.Response, last string) {
	t.Helper()
	var refs []map[string]any
	if err := json.Unmarshal(a["git/matching-refs/tags/v"].Body, &refs); err != nil {
		t.Fatal(err)
	}
	for i, ref := range refs {
		if ref["ref"] == "refs/tags/"+last {
			refs = refs[:i+1]
			break
		}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	r := a["git/matching-refs/tags/v"]
	a["git/matching-refs/tags/v"] = githubtest.Response{Status: r.Status, Header: r.Header, Body: b}
}

// editRuns edits a list of workflow runs in a.
func editRuns(t testing.TB, a map[string]githubtest.Response, path string, edit func(run map[string]any) bool) {
	t.Helper()
	a[path] = githubtest.Edited(t, a[path], eachRun(edit))
}

// releaseRunsUntil edits the Release on Publish runs down to those before
// the second Publish's, as they were until the second's CI passed, and edits
// the first Publish's run with edit.
func releaseRunsUntil(t testing.TB, a map[string]githubtest.Response, edit func(run map[string]any)) {
	t.Helper()
	editRuns(t, a, releaseRuns, func(run map[string]any) bool {
		switch run["head_sha"] {
		case second:
			return false
		case first:
			edit(run)
		}
		return true
	})
}

// fakeGitHub answers the App's calls on RMYPG/edity-sandbox from answers,
// as GitHub does: each answer has an ETag, which depends on the token too,
// and a call whose If-None-Match is still its answer's gets 304. It counts
// the calls made with an installation token, which count against the
// installation's rate limit unless they get 304.
type fakeGitHub struct {
	t *testing.T
	// status is the rollout status service, which answers stillAtV100
	// unless a test sets another answer.
	status *rollouttest.Service

	mu      sync.Mutex
	now     time.Time
	answers map[string]githubtest.Response
	// tokens are the tokens issued, and when each stops working: 55 minutes
	// after, when the App would replace it.
	tokens map[string]time.Time
	// calls counts the calls made with a token, and notModified those of
	// them answered 304.
	calls, notModified int
	// asked counts the calls made with a token by path and query.
	asked map[string]int
}

func newFake(t *testing.T) (*fakeGitHub, *Tracker) {
	t.Helper()
	f := &fakeGitHub{t: t, now: at("18:50:01"), answers: answers(t), tokens: map[string]time.Time{}, asked: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.status = rollouttest.NewService(t)
	f.status.Set(sandbox.ID, stillAtV100(t))
	app := github.NewApp(5466528, githubtest.Key(), srv.URL, srv.Client())
	tracker := New(func(r site.Repository) Repository { return app.Repo(r.Owner, r.Name) }, app.BotLogin,
		rollout.New(f.status.URL, &http.Client{}), nil)
	tracker.now = f.clock
	return f, tracker
}

// fakeReporter records the Failures it is told of, and answers err.
type fakeReporter struct {
	mu   sync.Mutex
	told []Failure
	err  error
}

func (r *fakeReporter) ReportFailure(ctx context.Context, s site.Site, f Failure) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.ID != sandbox.ID {
		return fmt.Errorf("told of a Failure on %s", s.ID)
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("no deadline")
	}
	r.told = append(r.told, f)
	return r.err
}

func (r *fakeReporter) failures() []Failure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.told)
}

// firstMessage is the first Publish's commit message.
const firstMessage = "content: publish home\n\nChanged:\n- hero.cta\n\nEdity-Client: Edity end-to-end check"

func (f *fakeGitHub) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// set moves the test's clock to now, and GitHub's answers to answers unless
// it is nil.
func (f *fakeGitHub) set(now time.Time, answers map[string]githubtest.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
	if answers != nil {
		f.answers = answers
	}
}

// counts returns the calls made with a token so far, and how many of them
// GitHub counted: those not answered 304.
func (f *fakeGitHub) counts() (calls, counted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.calls - f.notModified
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		writeJSON(http.StatusOK, `{"id": 5466528, "slug": "edity-publisher"}`)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/repos/RMYPG/edity-sandbox/installation":
		writeJSON(http.StatusOK, `{"id": 77}`)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/77/access_tokens":
		issued := fmt.Sprintf("ghs_%d", len(f.tokens)+1)
		f.tokens[issued] = f.now.Add(55 * time.Minute)
		// The App keeps it by the real clock, and the fake ends it by the
		// test's.
		writeJSON(http.StatusCreated, fmt.Sprintf(`{"token": %q, "expires_at": %q}`, issued, time.Now().Add(time.Hour).Format(time.RFC3339)))
		return
	}

	f.calls++
	f.asked[strings.TrimPrefix(r.URL.RequestURI(), "/repos/RMYPG/edity-sandbox/")]++
	if until, ok := f.tokens[token]; !ok || !f.now.Before(until) {
		writeJSON(http.StatusUnauthorized, `{"message": "Bad credentials"}`)
		return
	}
	path, ok := strings.CutPrefix(r.URL.RequestURI(), "/repos/RMYPG/edity-sandbox/")
	answer, found := f.answers[path]
	if !ok || !found || r.Method != http.MethodGet {
		writeJSON(http.StatusNotFound, `{"message": "Not Found"}`)
		return
	}
	if raw := strings.HasPrefix(path, "contents/"); raw != (r.Header.Get("Accept") == "application/vnd.github.raw+json") {
		f.t.Errorf("%s asked for as %s", path, r.Header.Get("Accept"))
	}
	etag := fmt.Sprintf(`W/"%x"`, sha256.Sum256(append([]byte(answer.Header.Get("ETag")+token), answer.Body...)))
	if r.Header.Get("If-None-Match") == etag {
		f.notModified++
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", answer.Header.Get("Content-Type"))
	w.Header().Set("ETag", etag)
	w.WriteHeader(answer.Status)
	_, _ = w.Write(answer.Body)
}

// A moment is GitHub's answers about the first Publish from a moment on,
// until the next: the recorded ones, edited back to how they were then, and
// the rollout status service's once it is released.
type moment struct {
	from    time.Time
	stage   Stage
	answers map[string]githubtest.Response
	status  rollouttest.Answer
}

// firstPublish is the first Publish's moments, from its push on.
func firstPublish(t *testing.T) []moment {
	t.Helper()
	pushed := answers(t)
	tagsUpTo(t, pushed, "v1.0.0")
	editRuns(t, pushed, ciRuns(first), func(map[string]any) bool { return false })

	running := func(updated string) map[string]githubtest.Response {
		a := answers(t)
		tagsUpTo(t, a, "v1.0.0")
		editRuns(t, a, ciRuns(first), func(run map[string]any) bool { inProgress(run, updated); return true })
		return a
	}

	passed := answers(t)
	tagsUpTo(t, passed, "v1.0.0")
	editRuns(t, passed, releaseRuns, func(run map[string]any) bool { return run["head_sha"] != first && run["head_sha"] != second })

	releasing := answers(t)
	tagsUpTo(t, releasing, "v1.0.0")
	releaseRunsUntil(t, releasing, func(run map[string]any) {
		run["display_title"] = "Release " + first
		inProgress(run, "2026-10-29T18:52:16Z")
	})

	released := answers(t)
	tagsUpTo(t, released, "v1.0.1")

	// Before it is released, the service is not asked.
	none := rollouttest.Answer{}
	return []moment{
		{at("18:50:01"), Validating, pushed, none},
		{at("18:50:04"), Validating, running("2026-10-29T18:50:04Z"), none},
		// Its first job done, the next started.
		{at("18:51:30"), Validating, running("2026-10-29T18:51:30Z"), none},
		{at("18:52:13"), Releasing, passed, none},
		{at("18:52:16"), Releasing, releasing, none},
		{at("18:52:53"), Released, released, stillAtV100(t)},
		// The platform picked v1.0.1 up.
		{at("18:54:50"), Deploying, released, productionAt(t, "v1.0.1", "v1.0.0", rollout.Deploying, "")},
		// The second Publish's v1.0.2, as recorded, picked up before
		// v1.0.1 ran.
		{at("18:55:36"), Deploying, answers(t), productionAt(t, "v1.0.2", "v1.0.0", rollout.Deploying, "")},
		// The newer v1.0.2 runs, so the first is live too (P8).
		{at("18:58:10"), Live, answers(t), productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, "")},
	}
}

func TestEachStageFromGitHubsAnswers(t *testing.T) {
	for name, c := range map[string]struct {
		sha  string
		now  time.Time
		edit func(t *testing.T, a map[string]githubtest.Response)
		want Progress
		// failure is the ID of the Failure the Developer is told of, when
		// the Publish failed.
		failure string
	}{
		"validating: pushed, before GitHub lists its CI run": {
			sha: first, now: at("18:50:02"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, ciRuns(first), func(map[string]any) bool { return false })
			},
			want: Progress{Stage: Validating, Version: release.Version{1, 0, 1}},
		},
		"validating: CI running": {
			sha: first, now: at("18:51:00"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, ciRuns(first), func(run map[string]any) bool { inProgress(run, "2026-10-29T18:51:00Z"); return true })
			},
			want: Progress{Stage: Validating, Version: release.Version{1, 0, 1}},
		},
		"releasing: its Release on Publish run under way": {
			sha: first, now: at("18:52:30"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				releaseRunsUntil(t, a, func(run map[string]any) {
					run["display_title"] = "Release " + first
					inProgress(run, "2026-10-29T18:52:30Z")
				})
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 1}},
		},
		"releasing: a Release on Publish run that does not name its commit": {
			sha: first, now: at("18:52:30"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				releaseRunsUntil(t, a, func(run map[string]any) { inProgress(run, "2026-10-29T18:52:30Z") })
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 1}},
		},
		"releasing: its Release on Publish run cancelled, as one a newer run takes the place of": {
			sha: first, now: at("18:53:00"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				releaseRunsUntil(t, a, func(run map[string]any) {
					run["display_title"], run["conclusion"] = "Release "+first, "cancelled"
				})
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 1}},
		},
		"releasing: CI ran again since its Release on Publish run failed": {
			sha: first, now: at("18:55:05"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, ciRuns(first), func(run map[string]any) bool {
					run["run_attempt"], run["run_started_at"], run["updated_at"] = 2, "2026-10-29T18:53:00Z", "2026-10-29T18:55:00Z"
					return true
				})
				releaseRunsUntil(t, a, func(run map[string]any) {
					run["display_title"], run["conclusion"] = "Release "+first, "failure"
				})
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 1}},
		},
		"releasing: another Publish's Release failed on main while this commit was its HEAD": {
			sha: second, now: at("18:55:20"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.1")
				editRuns(t, a, releaseRuns, func(run map[string]any) bool {
					switch run["head_sha"] {
					case second:
						run["display_title"] = "Release " + second
						inProgress(run, "2026-10-29T18:55:20Z")
					case first:
						// Had it started after the second's push, as it
						// might have.
						run["display_title"], run["conclusion"], run["head_sha"] = "Release "+first, "failure", second
					}
					return true
				})
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 2}},
		},
		"releasing: another Publish's untitled Release failed on this commit": {
			sha: second, now: at("18:55:20"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.1")
				editRuns(t, a, releaseRuns, func(run map[string]any) bool {
					switch run["head_sha"] {
					case second:
						// Not started yet.
						return false
					case first:
						run["conclusion"], run["head_sha"] = "failure", second
					}
					return true
				})
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 2}},
		},
		"released: its own Release": {
			sha: first, now: at("18:53:00"),
			edit: func(t *testing.T, a map[string]githubtest.Response) { tagsUpTo(t, a, "v1.0.1") },
			want: Progress{Stage: Released, Version: release.Version{1, 0, 1}},
		},
		"released: in a newer Release": {
			sha: first, now: at("19:05:00"),
			want: Progress{Stage: Released, Version: release.Version{1, 0, 1}},
		},
		"released: the second": {
			sha: second, now: at("19:05:00"),
			want: Progress{Stage: Released, Version: release.Version{1, 0, 2}},
		},
		"released: overtaken by a newer Release although its CI failed (P8)": {
			sha: first, now: at("19:05:00"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				editRuns(t, a, ciRuns(first), func(run map[string]any) bool { run["conclusion"] = "failure"; return true })
			},
			want: Progress{Stage: Released, Version: release.Version{1, 0, 1}},
		},
		"failed: CI": {
			sha: first, now: at("18:52:20"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, ciRuns(first), func(run map[string]any) bool { run["conclusion"] = "failure"; return true })
			},
			want:    Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Run: "https://github.com/RMYPG/edity-sandbox/actions/runs/40934506192", Reason: "CI ended failure"},
			failure: "run:40934506192/1",
		},
		"failed: CI, run again and failed again": {
			sha: first, now: at("18:56:00"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, ciRuns(first), func(run map[string]any) bool {
					run["conclusion"], run["run_attempt"], run["run_started_at"] = "failure", 2, "2026-10-29T18:53:00Z"
					return true
				})
			},
			want:    Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Run: "https://github.com/RMYPG/edity-sandbox/actions/runs/40934506192", Reason: "CI ended failure"},
			failure: "run:40934506192/2",
		},
		"failed: its Release on Publish run": {
			sha: first, now: at("18:53:00"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				releaseRunsUntil(t, a, func(run map[string]any) {
					run["display_title"], run["conclusion"] = "Release "+first, "failure"
				})
			},
			want:    Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Run: "https://github.com/RMYPG/edity-sandbox/actions/runs/40934846962", Reason: "Release on Publish ended failure"},
			failure: "run:40934846962/1",
		},
		"failed: no CI run ten minutes after the commit": {
			sha: first, now: at("19:00:02"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, ciRuns(first), func(map[string]any) bool { return false })
			},
			want:    Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Reason: "no CI run 10 minutes after the commit"},
			failure: "no-ci:" + first,
		},
		"failed: no Release half an hour after CI passed, its Release on Publish run skipped": {
			sha: first, now: at("19:22:14"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				// As when EDITY_PUBLISHER_LOGINS does not name the App.
				releaseRunsUntil(t, a, func(run map[string]any) {
					run["display_title"], run["conclusion"] = "Release "+first, "skipped"
				})
			},
			want:    Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Reason: "no Release 30 minutes after CI passed"},
			failure: "no-release:" + first + "@2026-10-29T18:50:04Z",
		},
		"failed: no Release half an hour after CI passed, and no Release on Publish run of it": {
			sha: first, now: at("19:22:14"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				// The second Publish's run named, and none of the first's,
				// as when the workflow was disabled in between.
				editRuns(t, a, releaseRuns, func(run map[string]any) bool {
					if run["head_sha"] == second {
						run["display_title"] = "Release " + second
					}
					return run["head_sha"] != first
				})
			},
			want:    Progress{Stage: Failed, Version: release.Version{1, 0, 1}, Reason: "no Release 30 minutes after CI passed"},
			failure: "no-release:" + first + "@2026-10-29T18:50:04Z",
		},
		"releasing: half an hour after CI passed, on a Site whose Release on Publish runs are not named after their commits": {
			sha: first, now: at("19:22:14"),
			edit: func(t *testing.T, a map[string]githubtest.Response) { tagsUpTo(t, a, "v1.0.0") },
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 1}},
		},
		"releasing: half an hour after CI passed, its run cancelled while a newer Release on Publish run is under way": {
			sha: first, now: at("19:22:14"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, releaseRuns, func(run map[string]any) bool {
					switch run["head_sha"] {
					case first:
						run["display_title"], run["conclusion"] = "Release "+first, "cancelled"
					case second:
						run["display_title"] = "Release " + second
						inProgress(run, "2026-10-29T19:22:00Z")
					}
					return true
				})
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 1}},
		},
		"validating: its CI run queued half an hour after the commit": {
			sha: first, now: at("19:22:14"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				editRuns(t, a, ciRuns(first), func(run map[string]any) bool {
					inProgress(run, "2026-10-29T18:50:04Z")
					run["status"] = "queued"
					return true
				})
			},
			want: Progress{Stage: Validating, Version: release.Version{1, 0, 1}},
		},
		"releasing: half an hour after CI passed, its Release on Publish run waiting its turn": {
			sha: first, now: at("19:22:14"),
			edit: func(t *testing.T, a map[string]githubtest.Response) {
				tagsUpTo(t, a, "v1.0.0")
				releaseRunsUntil(t, a, func(run map[string]any) {
					run["display_title"] = "Release " + first
					inProgress(run, "2026-10-29T18:52:16Z")
					run["status"] = "queued"
				})
			},
			want: Progress{Stage: Releasing, Version: release.Version{1, 0, 1}},
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
			got, err := tracker.Progress(t.Context(), sandbox, c.sha)
			if err != nil || got != c.want {
				t.Errorf("Progress = %+v, %v, want %+v", got, err, c.want)
			}
			var want []Failure
			if c.failure != "" {
				want = []Failure{{SHA: c.sha, Message: firstMessage, Version: c.want.Version, Run: c.want.Run, Reason: c.want.Reason, ID: c.failure}}
			}
			if told := reporter.failures(); !slices.Equal(told, want) {
				t.Errorf("the Developer was told of %+v, want %+v", told, want)
			}
		})
	}
}

// ciFailed is GitHub's answers once the first Publish's CI failed.
func ciFailed(t *testing.T) map[string]githubtest.Response {
	t.Helper()
	a := answers(t)
	tagsUpTo(t, a, "v1.0.0")
	editRuns(t, a, ciRuns(first), func(run map[string]any) bool { run["conclusion"] = "failure"; return true })
	return a
}

func TestAFailureTheDeveloperWasNotToldOfIsNotAnswered(t *testing.T) {
	f, tracker := newFake(t)
	reporter := &fakeReporter{err: &github.Error{Call: "POST /repos/RMYPG/edity-sandbox/issues", Status: http.StatusBadGateway, Message: "Server Error"}}
	tracker.reporter = reporter
	f.set(at("18:52:20"), ciFailed(t))
	var gerr *github.Error
	if p, err := tracker.Progress(t.Context(), sandbox, first); !errors.As(err, &gerr) || gerr.Status != http.StatusBadGateway {
		t.Fatalf("Progress = %+v, %v, want GitHub's 502 from telling the Developer", p, err)
	}

	// The next request tells the Developer again, however soon it comes.
	reporter.mu.Lock()
	reporter.err = nil
	reporter.mu.Unlock()
	if p, err := tracker.Progress(t.Context(), sandbox, first); err != nil || p.Stage != Failed {
		t.Fatalf("Progress once the Developer is told = %+v, %v, want failed", p, err)
	}
	if told := reporter.failures(); len(told) != 2 || told[0] != told[1] {
		t.Errorf("the Developer was told %+v, want the same Failure twice", told)
	}
}

func TestRequestsForOneFailedCommitTellTheDeveloperOnce(t *testing.T) {
	f, tracker := newFake(t)
	reporter := &fakeReporter{}
	tracker.reporter = reporter
	f.set(at("18:52:20"), ciFailed(t))
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if p, err := tracker.Progress(t.Context(), sandbox, first); err != nil || p.Stage != Failed {
				t.Errorf("Progress = %+v, %v", p, err)
			}
		})
	}
	wg.Wait()
	if told := reporter.failures(); len(told) != 1 {
		t.Errorf("three requests at once told the Developer %d times, want once", len(told))
	}
}

func TestOnlyAPublishsCommitHasProgress(t *testing.T) {
	for name, sha := range map[string]string{
		"a Developer's commit": developer,
		"no commit":            "0000000000000000000000000000000000000000",
		"not a sha":            "../../../app",
	} {
		_, tracker := newFake(t)
		if p, err := tracker.Progress(t.Context(), sandbox, sha); !errors.Is(err, ErrNotPublish) {
			t.Errorf("%s: Progress = %+v, %v, want ErrNotPublish", name, p, err)
		}
	}
}

func TestRequestsForOneCommitShareALook(t *testing.T) {
	f, tracker := newFake(t)
	f.set(at("19:05:00"), nil)
	f.status.Set(sandbox.ID, productionAt(t, "v1.0.2", "v1.0.2", rollout.Live, ""))
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if p, err := tracker.Progress(t.Context(), sandbox, first); err != nil || p.Stage != Live {
				t.Errorf("Progress = %+v, %v", p, err)
			}
		})
	}
	wg.Wait()
	if calls, _ := f.counts(); calls != 4 || f.status.Asked(sandbox.ID) != 1 {
		t.Errorf("three requests at once made %d calls, and asked the rollout status %d times, want one look's 4, and once", calls, f.status.Asked(sandbox.ID))
	}

	f.set(at("19:05:04"), nil)
	if _, err := tracker.Progress(t.Context(), sandbox, first); err != nil {
		t.Fatal(err)
	}
	if calls, _ := f.counts(); calls != 4 {
		t.Errorf("a request 4s later made %d calls, want none", calls-4)
	}
	f.set(at("19:05:05"), nil)
	if _, err := tracker.Progress(t.Context(), sandbox, first); err != nil {
		t.Fatal(err)
	}
	if calls, _ := f.counts(); calls != 8 {
		t.Errorf("a request 5s later made %d calls, want a new look's 4", calls-4)
	}
}

// TestPollingStaysWithinTheRateLimit counts what polling a Publish costs the
// installation's rate limit: 5,000 calls an hour, which every Site shares,
// since one installation covers every RMYPG repository. The Editor polls
// every 5 seconds, 720 times an hour.
//
// One poll makes 4 calls, or 5 while releasing, and while released or
// deploying, when it reads when its Release was tagged. Even if GitHub
// counted every call, a Publish polled for a whole hour would cost at most
// 5 × 720 = 3,600, within the 5,000 but with room for little else. GitHub
// does not count a call it answers 304, which is every call whose answer did
// not change, so a poll that finds nothing new costs nothing. The hour
// below, from the push through both Releases until the Site runs them and
// on, costs one call for each answer that changed, and each answer once more
// when the token is replaced: under 25 calls, so the 5,000 hold some 200
// Publishes polled for an hour each. A real Publish is polled for 5 to 15
// minutes. The rollout status service, which each poll of a released
// Publish asks too, is the platform's, and has no rate limit.
//
// Every browser tab polling the same Publish shares one look every 5 seconds
// (TestRequestsForOneCommitShareALook), so more tabs cost nothing more.
func TestPollingStaysWithinTheRateLimit(t *testing.T) {
	moments := firstPublish(t)
	failed := ciFailed(t)

	for _, c := range []struct {
		name   string
		now    time.Time
		want   Stage
		a      map[string]githubtest.Response
		status rollouttest.Answer
		calls  int
	}{
		{"validating, no run yet", at("18:50:02"), Validating, moments[0].answers, moments[0].status, 4},
		{"validating, CI running", at("18:51:00"), Validating, moments[1].answers, moments[1].status, 4},
		{"releasing, no run yet", at("18:52:14"), Releasing, moments[3].answers, moments[3].status, 5},
		{"releasing", at("18:52:30"), Releasing, moments[4].answers, moments[4].status, 5},
		{"released", at("18:53:00"), Released, moments[5].answers, moments[5].status, 5},
		{"deploying", at("18:55:00"), Deploying, moments[6].answers, moments[6].status, 5},
		{"live", at("18:58:10"), Live, moments[8].answers, moments[8].status, 4},
		{"failed", at("18:52:20"), Failed, failed, rollouttest.Answer{}, 4},
	} {
		f, tracker := newFake(t)
		f.set(c.now, c.a)
		f.status.Set(sandbox.ID, c.status)
		poll := func() (calls, counted int) {
			calls, counted = f.counts()
			p, err := tracker.Progress(t.Context(), sandbox, first)
			if err != nil || p.Stage != c.want {
				t.Fatalf("%s: Progress = %+v, %v, want %s", c.name, p, err, c.want)
			}
			after, countedAfter := f.counts()
			return after - calls, countedAfter - counted
		}
		calls, counted := poll()
		f.set(c.now.Add(reuse), nil)
		again, countedAgain := poll()
		t.Logf("%-23s a poll makes %d calls, all counted the first time, %d of them 5s later", c.name, calls, countedAgain)
		if calls != c.calls || counted != c.calls || again != c.calls || countedAgain != 0 {
			t.Errorf("%s: %d calls (%d counted), then %d (%d counted), want %d, all counted, then %d, none counted",
				c.name, calls, counted, again, countedAgain, c.calls, c.calls)
		}
	}

	// An hour of polling the first Publish every 5 seconds from its push,
	// with GitHub's answers as they were at each moment.
	f, tracker := newFake(t)
	start := moments[0].from
	var stages []Stage
	for now := start; now.Before(start.Add(time.Hour)); now = now.Add(5 * time.Second) {
		m := moments[0]
		for _, next := range moments {
			if !now.Before(next.from) {
				m = next
			}
		}
		f.set(now, m.answers)
		f.status.Set(sandbox.ID, m.status)
		p, err := tracker.Progress(t.Context(), sandbox, first)
		if err != nil || p.Stage != m.stage {
			t.Fatalf("at %s: Progress = %+v, %v, want %s", now.Format(time.TimeOnly), p, err, m.stage)
		}
		if len(stages) == 0 || stages[len(stages)-1] != p.Stage {
			stages = append(stages, p.Stage)
		}
	}
	calls, counted := f.counts()
	t.Logf("an hour of polling: %d calls, %d counted against the installation's 5,000", calls, counted)
	if fmt.Sprint(stages) != "[validating releasing released deploying live]" {
		t.Errorf("stages %v", stages)
	}
	if calls > 5*720 || counted > 25 {
		t.Errorf("an hour of polling made %d calls and %d counted, want at most 3,600 and 25", calls, counted)
	}
	if len(f.tokens) != 2 {
		t.Errorf("%d tokens in the hour, want the token replaced once", len(f.tokens))
	}
}

func TestLastReleaseIsTheHighestTag(t *testing.T) {
	f, tracker := newFake(t)
	repo := tracker.repository(sandbox.Repository)
	if last, err := LastRelease(t.Context(), repo); err != nil || last != "v1.0.2" {
		t.Errorf("LastRelease = %q, %v, want v1.0.2", last, err)
	}
	a := answers(t)
	delete(a, "git/matching-refs/tags/v")
	f.set(at("18:50:01"), a)
	if last, err := LastRelease(t.Context(), repo); err != nil || last != "" {
		t.Errorf("LastRelease with no tags = %q, %v, want none", last, err)
	}
}
