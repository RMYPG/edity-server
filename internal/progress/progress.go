// Package progress works out how far a Publish has got: where the Release of
// its commit is (P4). It asks the Site repository on GitHub each time, and
// stores nothing (V8), so that no missed event can leave a Publish stuck.
//
// A Publish's commit is
//
//   - validating while the Site's CI runs on it: the workflow runs that its
//     push started;
//   - releasing once they all passed, until a Release contains it. Release
//     on Publish, a workflow of the Site template, cuts the Release, and its
//     release.yml pushes the vX.Y.Z tag only once the image of that version
//     exists;
//   - released once the highest vX.Y.Z tag contains it, whether that is the
//     commit's own Release or a newer one (P8);
//   - failed when one of its CI runs or its Release on Publish run failed,
//     or when nothing came of it in time.
//
// The Editor tells the Client of a failed Publish that the Developer has
// been notified (P5), so a Tracker answers failed only once its Reporter
// has told the Developer (V6).
//
// Deploying and live are ticket 33's.
package progress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/site"
)

// A Stage is where the Release of a Publish's commit is.
type Stage string

// The stages GitHub tells.
const (
	Validating Stage = "validating"
	Releasing  Stage = "releasing"
	Released   Stage = "released"
	Failed     Stage = "failed"
)

// A Progress is how far a Publish has got.
type Progress struct {
	Stage Stage
	// Version is the version of the Release the commit becomes: the one in
	// its package.json.
	Version release.Version
	// Run is the address of the run that made the Publish fail, when one
	// did, and Reason says what failed, for the Developer.
	Run    string
	Reason string
}

// ErrNotPublish is a sha that is not the commit of a Publish on the Site: no
// commit of its repository, or one that the publisher App did not author.
var ErrNotPublish = errors.New("not the commit of a Publish")

// A Failure is a Publish that failed, as the Developer is told of it.
type Failure struct {
	// SHA is the Publish's commit, Message its commit message, and Version
	// the version of the Release it was to become.
	SHA, Message string
	Version      release.Version
	// Run is the address of the run that failed, when one did, and Reason
	// says what failed.
	Run, Reason string
	// ID tells this failure from every other on the Site, so that it is
	// told once: the run that failed and its attempt, or the deadline the
	// commit missed. It has no spaces.
	ID string
}

// A Reporter tells the Developer that a Publish failed (V6).
// *issues.Reporter is one.
type Reporter interface {
	// ReportFailure returns nil once the Developer has been told of f,
	// whether now or earlier.
	ReportFailure(ctx context.Context, s site.Site, f Failure) error
}

// A Repository is a Site repository, as the publisher App reads it.
// *github.Repo is one.
type Repository interface {
	// GitCommit returns github.ErrNotFound when there is no such commit.
	GitCommit(ctx context.Context, sha string) (github.GitCommit, error)
	// File returns github.ErrNotFound when there is no file at path.
	File(ctx context.Context, path, ref string) ([]byte, error)
	Get(ctx context.Context, path string, out any) error
}

const (
	// reuse is how long a Progress answers again for the same commit, so
	// that every browser tab polling it costs GitHub one look in that time.
	reuse = 5 * time.Second
	// ciDeadline is how long after its commit a Publish waits for its first
	// CI run. Runs appear seconds after the push, unless Actions or the
	// workflow is disabled.
	ciDeadline = 10 * time.Minute
	// releaseDeadline is how long after CI passed a Publish waits for a
	// Release to contain it, while no Release on Publish run of it is under
	// way: one that was skipped, cancelled or never named after the commit.
	releaseDeadline = 30 * time.Minute
	// lookTimeout bounds one look at GitHub.
	lookTimeout = 20 * time.Second
)

// releaseRuns lists the newest runs of the Site template's Release on
// Publish workflow. Each run is titled "Release <sha>", after the commit CI
// passed on; a run's head_sha is main's HEAD when it started, which may be a
// later Publish's commit, so it says nothing about which Publish it
// releases.
const releaseRuns = "actions/workflows/release-on-publish.yml/runs?event=workflow_run&per_page=20"

// A Tracker works out the progress of Publishes. It is safe for concurrent
// use.
type Tracker struct {
	repository func(site.Repository) Repository
	bot        func(context.Context) (string, error)
	reporter   Reporter
	now        func() time.Time

	mu     sync.Mutex
	recent map[key]*look
}

type key struct{ site, sha string }

// A look is one look at GitHub for a commit's progress, under way until
// done is closed.
type look struct {
	done     chan struct{}
	at       time.Time
	progress Progress
	err      error
}

// New returns a Tracker that reaches a Site's repository through
// repository, and tells a Publish's commit by its author, the login bot
// returns: the publisher App's. It answers that a Publish failed only once
// reporter has told the Developer (P5). A nil reporter tells no one, for
// what only reads, such as mise run check:progress.
func New[R Repository](repository func(site.Repository) R, bot func(context.Context) (string, error), reporter Reporter) *Tracker {
	return &Tracker{
		repository: func(r site.Repository) Repository { return repository(r) },
		bot:        bot,
		reporter:   reporter,
		now:        time.Now,
		recent:     map[key]*look{},
	}
}

// Progress returns how far the Publish whose commit is sha has got on s, or
// ErrNotPublish. Requests for the same commit within a few seconds share one
// look at GitHub.
func (t *Tracker) Progress(ctx context.Context, s site.Site, sha string) (Progress, error) {
	// It goes into the calls' paths and queries as it is.
	if !github.IsSHA(sha) {
		return Progress{}, fmt.Errorf("%q is not a sha: %w", sha, ErrNotPublish)
	}
	k := key{s.ID, sha}
	t.mu.Lock()
	now := t.now()
	for k, l := range t.recent {
		if !l.at.IsZero() && now.Sub(l.at) >= reuse {
			delete(t.recent, k)
		}
	}
	l, ok := t.recent[k]
	if !ok {
		l = &look{done: make(chan struct{})}
		t.recent[k] = l
		// It runs to the end for every request that waits on it, even if
		// this one goes away.
		go t.look(context.WithoutCancel(ctx), s, sha, k, l)
	}
	t.mu.Unlock()
	select {
	case <-l.done:
		return l.progress, l.err
	case <-ctx.Done():
		return Progress{}, ctx.Err()
	}
}

func (t *Tracker) look(ctx context.Context, s site.Site, sha string, k key, l *look) {
	ctx, cancel := context.WithTimeout(ctx, lookTimeout)
	defer cancel()
	p, failure, err := t.workOut(ctx, s, sha)
	if failure != nil && t.reporter != nil {
		// The Editor tells the Client that the Developer has been notified,
		// so that has to be true before the answer is.
		if err = t.reporter.ReportFailure(ctx, s, *failure); err != nil {
			p, err = Progress{}, fmt.Errorf("telling the Developer that %s failed: %w", sha, err)
		}
	}
	t.mu.Lock()
	l.progress, l.err, l.at = p, err, t.now()
	if err != nil && t.recent[k] == l {
		// The next request looks again.
		delete(t.recent, k)
	}
	t.mu.Unlock()
	close(l.done)
}

// workOut asks GitHub where the commit sha is, and, when the Publish
// failed, what the Developer is to be told.
func (t *Tracker) workOut(ctx context.Context, s site.Site, sha string) (Progress, *Failure, error) {
	repo := t.repository(s.Repository)
	bot, err := t.bot(ctx)
	if err != nil {
		return Progress{}, nil, err
	}
	commit, err := repo.GitCommit(ctx, sha)
	if errors.Is(err, github.ErrNotFound) {
		return Progress{}, nil, fmt.Errorf("%s: %w", sha, ErrNotPublish)
	}
	if err != nil {
		return Progress{}, nil, err
	}
	if commit.Author != bot {
		return Progress{}, nil, fmt.Errorf("%s is authored by %s, not %s: %w", sha, commit.Author, bot, ErrNotPublish)
	}
	pkg, err := repo.File(ctx, "package.json", sha)
	if errors.Is(err, github.ErrNotFound) {
		return Progress{}, nil, fmt.Errorf("%s has no package.json: %w", sha, ErrNotPublish)
	}
	if err != nil {
		return Progress{}, nil, err
	}
	v, ok := versionOf(pkg)
	if !ok {
		return Progress{}, nil, fmt.Errorf("%s's package.json has no X.Y.Z version: %w", sha, ErrNotPublish)
	}
	p := Progress{Version: v}
	failed := func(run, reason, id string) (Progress, *Failure, error) {
		p.Stage, p.Run, p.Reason = Failed, run, reason
		return p, &Failure{SHA: sha, Message: commit.Message, Version: p.Version, Run: run, Reason: reason, ID: id}, nil
	}

	// A Release that contains the commit is the end, whatever its own runs
	// did: a newer Publish's Release may have overtaken it.
	released, err := releasedIn(ctx, repo, sha, v)
	if err != nil {
		return Progress{}, nil, err
	}
	if released {
		p.Stage = Released
		return p, nil, nil
	}

	var ci runs
	if err := repo.Get(ctx, "actions/runs?head_sha="+sha+"&event=push&exclude_pull_requests=true", &ci); err != nil {
		return Progress{}, nil, err
	}
	now := t.now()
	if r := ci.failed(); r != nil {
		return failed(r.URL, fmt.Sprintf("%s ended %s", r.Name, r.Conclusion), r.id())
	}
	if len(ci.Runs) == 0 && !commit.Committed.IsZero() && now.Sub(commit.Committed) > ciDeadline {
		return failed("", fmt.Sprintf("no CI run %d minutes after the commit", int(ciDeadline.Minutes())), "no-ci:"+sha)
	}
	if len(ci.Runs) == 0 || ci.underWay() {
		p.Stage = Validating
		return p, nil, nil
	}

	var releases runs
	if err := repo.Get(ctx, releaseRuns, &releases); err != nil && !errors.Is(err, github.ErrNotFound) {
		return Progress{}, nil, err
	}
	// The newest run for the commit since its CI last started: an older one
	// is from a CI run that has been run again since.
	title := "Release " + sha
	for _, r := range releases.Runs {
		if r.DisplayTitle != title || r.CreatedAt.Before(ci.lastStarted()) {
			continue
		}
		if r.Status != "completed" {
			p.Stage = Releasing
			return p, nil, nil
		}
		if !releaseEnds(r.Conclusion) {
			return failed(r.URL, fmt.Sprintf("%s ended %s", r.Name, r.Conclusion), r.id())
		}
		break
	}
	if now.Sub(ci.lastUpdated()) > releaseDeadline {
		// By when CI last started: CI run again, passing again with still no
		// Release after it, is another failure.
		return failed("", fmt.Sprintf("no Release %d minutes after CI passed", int(releaseDeadline.Minutes())),
			"no-release:"+sha+"@"+ci.lastStarted().UTC().Format(time.RFC3339))
	}
	p.Stage = Releasing
	return p, nil, nil
}

// releasedIn tells whether the highest vX.Y.Z tag contains the commit sha,
// whose version is v. Tags go on main's commits with rising versions, so a
// Release older than v can't contain it, and the highest tells for every
// other.
func releasedIn(ctx context.Context, repo Repository, sha string, v release.Version) (bool, error) {
	highest, tagged, err := highestRelease(ctx, repo)
	if err != nil {
		return false, err
	}
	if !tagged || highest.Less(v) {
		return false, nil
	}
	var compared struct {
		Status string `json:"status"`
	}
	if err := repo.Get(ctx, "compare/"+sha+"...v"+highest.String()+"?per_page=1", &compared); err != nil {
		return false, err
	}
	// The tag is ahead of the commit, or on it.
	return compared.Status == "ahead" || compared.Status == "identical", nil
}

// LastRelease returns the tag of the Site's highest Release, such as
// v1.0.2, or "" when it has none. Tags only ever go on main with rising
// versions, so it is the last Release cut.
func LastRelease(ctx context.Context, repo Repository) (string, error) {
	highest, tagged, err := highestRelease(ctx, repo)
	if err != nil || !tagged {
		return "", err
	}
	return "v" + highest.String(), nil
}

// highestRelease returns the version of the highest vX.Y.Z tag, and false
// when there is none.
func highestRelease(ctx context.Context, repo Repository) (release.Version, bool, error) {
	var refs []struct {
		Ref string `json:"ref"`
	}
	err := repo.Get(ctx, "git/matching-refs/tags/v", &refs)
	if errors.Is(err, github.ErrNotFound) {
		return release.Version{}, false, nil
	}
	if err != nil {
		return release.Version{}, false, err
	}
	var highest release.Version
	tagged := false
	for _, ref := range refs {
		tag, ok := strings.CutPrefix(ref.Ref, "refs/tags/v")
		if w, isVersion := release.ParseVersion(tag); ok && isVersion && (!tagged || highest.Less(w)) {
			highest, tagged = w, true
		}
	}
	return highest, tagged, nil
}

// runs is GitHub's list of workflow runs.
type runs struct {
	Runs []run `json:"workflow_runs"`
}

type run struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	Status       string `json:"status"`
	// Conclusion is empty until the run is completed.
	Conclusion string    `json:"conclusion"`
	URL        string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	StartedAt  time.Time `json:"run_started_at"`
	// Attempt counts the times the run was run: one run again keeps its
	// ID.
	Attempt int `json:"run_attempt"`
}

// id is the Failure ID of a run that failed: its ID and attempt.
func (r run) id() string {
	return fmt.Sprintf("run:%d/%d", r.ID, r.Attempt)
}

// passes tells whether a completed CI run let its commit through.
func passes(conclusion string) bool {
	return conclusion == "success" || conclusion == "neutral" || conclusion == "skipped"
}

// releaseEnds tells whether a completed Release on Publish run leaves its
// commit to be released by a tag: it did (success), had nothing to do
// (neutral, skipped), or a newer run took its place (cancelled). Any other
// conclusion is a failure.
func releaseEnds(conclusion string) bool {
	return passes(conclusion) || conclusion == "cancelled"
}

// failed returns the first completed CI run that did not pass.
func (rs runs) failed() *run {
	for i, r := range rs.Runs {
		if r.Status == "completed" && !passes(r.Conclusion) {
			return &rs.Runs[i]
		}
	}
	return nil
}

func (rs runs) underWay() bool {
	for _, r := range rs.Runs {
		if r.Status != "completed" {
			return true
		}
	}
	return false
}

// lastStarted is when the latest attempt of a run started.
func (rs runs) lastStarted() time.Time {
	var last time.Time
	for _, r := range rs.Runs {
		if r.StartedAt.After(last) {
			last = r.StartedAt
		}
	}
	return last
}

// lastUpdated is when the last run ended, once all have.
func (rs runs) lastUpdated() time.Time {
	var last time.Time
	for _, r := range rs.Runs {
		if r.UpdatedAt.After(last) {
			last = r.UpdatedAt
		}
	}
	return last
}

// versionOf reads the version of a package.json.
func versionOf(pkg []byte) (release.Version, bool) {
	var p struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(pkg, &p) != nil {
		return release.Version{}, false
	}
	return release.ParseVersion(p.Version)
}
