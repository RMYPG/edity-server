// Package progress works out how far a Publish has got: where the Release of
// its commit is, and whether the Site runs it (P4). It asks the Site
// repository on GitHub and the platform's rollout status service each time,
// and stores nothing (V8), so that no missed event can leave a Publish stuck.
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
//     commit's own Release or a newer one (P8), while the platform has not
//     picked up its version or a newer one yet;
//   - deploying once the platform has picked up its version or a newer one,
//     its target, until the Site runs it;
//   - live once the Site runs its version or a newer one (P8), whatever
//     else holds, as released wins over what CI and Release on Publish did;
//   - failed when one of its CI runs or its Release on Publish run failed,
//     or when nothing came of it in time: no CI run some minutes after the
//     commit, or, on a Site whose Release on Publish runs are named after
//     their commits, no Release some time after CI passed while none of
//     those runs is under way. A Site whose runs are not named yet releases
//     the commit by its tag alone, with no deadline: no run there tells
//     whether this commit's Release is still to come. A CI run that GitHub
//     keeps queued leaves the commit validating, with no deadline either:
//     it starts once a runner is free, or the Developer cancels it, which
//     fails the Publish. Once released, it fails when the rollout of its
//     target fails, when the platform reports a failure while its target is
//     older than the Publish, as when the image scan fails, or when the Site
//     still runs an older version an hour after the Publish's Release was
//     tagged. The last two count only while the service knows the older
//     version the Site runs: a Site that runs no version it knows is between
//     two Pods, as when one restarts or the cluster recovers, and its rollout
//     failures, first installs included, arrive as a failed phase within
//     minutes.
//
// The rollout status service answers how production's rollout stands, by
// the Site's id (rollout.Client). An answer it does not give, whatever the
// reason, is no answer: the Tracker returns an error, as when GitHub can't
// be reached, so that the request is made again, and no deadline can fail a
// Publish without the service's answer.
//
// The Editor tells the Client of a failed Publish that the Developer has
// been notified (P5), so a Tracker answers failed only once its Reporter
// has reported the failure to the Developer (V6). Its sweeps ask after
// each Site's Publishes that production does not run yet every few
// minutes, so that a failure is reported even when no Client asks about it.
package progress

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/rollout"
	"github.com/RMYPG/edity-server/internal/site"
)

// A Stage is where the Release of a Publish's commit is.
type Stage string

// The stages GitHub and the rollout status service tell.
const (
	Validating Stage = "validating"
	Releasing  Stage = "releasing"
	Released   Stage = "released"
	Deploying  Stage = "deploying"
	Live       Stage = "live"
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

// A Failure is a Publish that failed, as it is reported to the Developer.
type Failure struct {
	// SHA is the Publish's commit, Message its commit message, and Version
	// the version of the Release it was to become.
	SHA, Message string
	Version      release.Version
	// Released tells that the Publish's Release was cut, and that what
	// failed is its going live.
	Released bool
	// Run is the address of the run that failed, when one did, and Reason
	// says what failed.
	Run, Reason string
	// Rollout is what the platform's rollout status service said failed,
	// its reason, when the Release failed to go live and the service said
	// why.
	Rollout string
	// ID tells this failure from every other on the Site, so that it is
	// reported once: the run that failed and its attempt, the deadline the
	// commit missed, or the Publish whose rollout failed. It has no spaces.
	ID string
}

// A Reporter reports to the Developer that a Publish failed (V6).
// *issues.Reporter is one.
type Reporter interface {
	// ReportFailure returns nil once f has been reported to the Developer,
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

// Rollouts is the platform's rollout status service, which tells how a
// Site's rollout stands. *rollout.Client is one.
type Rollouts interface {
	// Status returns how the rollout of the Site whose id is site stands in
	// environment, or an error when the service gives no answer.
	Status(ctx context.Context, site, environment string) (rollout.Status, error)
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
	// Release to contain it, on a Site whose Release on Publish runs are
	// named after their commits, while none of them is under way: its own
	// was skipped or cancelled, or never started. Any run under way may
	// still release it, as a newer Release.
	releaseDeadline = 30 * time.Minute
	// liveDeadline is how long after its Release was tagged a Publish waits
	// for the Site to run it or a newer Release. The platform picks a new
	// tag up within minutes, and the rollout of a static Site takes a few
	// more.
	liveDeadline = 60 * time.Minute
	// lookTimeout bounds one look at GitHub and the rollout status service.
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
	rollouts   Rollouts
	reporter   Reporter
	now        func() time.Time

	mu     sync.Mutex
	recent map[key]*look
	// ran is the last version the rollout status service told a sweep each
	// Site runs, by its id, when it named a version: where production was
	// last seen, for a sweep to start from when the service no longer knows.
	// It is in memory only, and a progress never reads it.
	ran map[string]release.Version
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
// repository, tells a Publish's commit by its author, the login bot
// returns: the publisher App's, and asks rollouts whether production runs
// a Release. It answers that a Publish failed only once reporter has
// reported it to the Developer (P5). A nil reporter reports nothing, for
// what only reads, such as mise run check:progress.
func New[R Repository](repository func(site.Repository) R, bot func(context.Context) (string, error), rollouts Rollouts, reporter Reporter) *Tracker {
	return &Tracker{
		repository: func(r site.Repository) Repository { return repository(r) },
		bot:        bot,
		rollouts:   rollouts,
		reporter:   reporter,
		now:        time.Now,
		recent:     map[key]*look{},
		ran:        map[string]release.Version{},
	}
}

// Progress returns how far the Publish whose commit is sha has got on s, or
// ErrNotPublish. Requests for the same commit within a few seconds share one
// look at GitHub and the rollout status service. When the service gives no
// answer, the error is the service's *rollout.Error.
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
			p, err = Progress{}, fmt.Errorf("reporting that %s failed: %w", sha, err)
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

// workOut asks GitHub where the commit sha is, and the rollout status
// service once it is released, and, when the Publish failed, what to report
// to the Developer.
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
	v, ok := release.VersionOf(pkg)
	if !ok {
		return Progress{}, nil, fmt.Errorf("%s's package.json has no X.Y.Z version: %w", sha, ErrNotPublish)
	}
	p := Progress{Version: v}
	failed := func(run, reason, id string) (Progress, *Failure, error) {
		p.Stage, p.Run, p.Reason = Failed, run, reason
		return p, &Failure{SHA: sha, Message: commit.Message, Version: p.Version, Run: run, Reason: reason, ID: id}, nil
	}

	// A Release that contains the commit is the end of what GitHub tells,
	// whatever its own runs did: a newer Publish's Release may have
	// overtaken it. The rest is the rollout's.
	first, released, err := releasedIn(ctx, repo, sha, v)
	if err != nil {
		return Progress{}, nil, err
	}
	if released {
		return t.deployment(ctx, repo, s, p, first, func(kind, reason, rolloutReason string) (Progress, *Failure, error) {
			p, failure, err := failed("", reason, kind+":"+sha)
			failure.Released, failure.Rollout = true, rolloutReason
			return p, failure, err
		})
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
	if releases.named() && !releases.underWay() && now.Sub(ci.lastUpdated()) > releaseDeadline {
		// By when CI last started: CI run again, passing again with still no
		// Release after it, is another failure.
		return failed("", fmt.Sprintf("no Release %d minutes after CI passed", int(releaseDeadline.Minutes())),
			"no-release:"+sha+"@"+ci.lastStarted().UTC().Format(time.RFC3339))
	}
	p.Stage = Releasing
	return p, nil, nil
}

// releasedIn returns the Release that first contained the commit sha, whose
// version is v, and true once the highest vX.Y.Z tag contains it. Tags go
// on main's commits with rising versions, so a Release older than v can't
// contain it, the highest tells for every other, and the lowest from v on is
// the first that did: v's own, or the newer one that overtook it.
func releasedIn(ctx context.Context, repo Repository, sha string, v release.Version) (tag, bool, error) {
	tags, err := releaseTags(ctx, repo)
	if err != nil {
		return tag{}, false, err
	}
	if len(tags) == 0 || tags[len(tags)-1].version.Less(v) {
		return tag{}, false, nil
	}
	highest := tags[len(tags)-1]
	var compared struct {
		Status string `json:"status"`
	}
	if err := repo.Get(ctx, "compare/"+sha+"...v"+highest.version.String()+"?per_page=1", &compared); err != nil {
		return tag{}, false, err
	}
	// The tag is ahead of the commit, or on it.
	if compared.Status != "ahead" && compared.Status != "identical" {
		return tag{}, false, nil
	}
	for _, t := range tags {
		if !t.version.Less(v) {
			return t, true, nil
		}
	}
	return highest, true, nil
}

// LastRelease returns the tag of the Site's highest Release, such as
// v1.0.2, or "" when it has none. Tags only ever go on main with rising
// versions, so it is the last Release cut.
func LastRelease(ctx context.Context, repo Repository) (string, error) {
	tags, err := releaseTags(ctx, repo)
	if err != nil || len(tags) == 0 {
		return "", err
	}
	return "v" + tags[len(tags)-1].version.String(), nil
}

// A tag is one of the Site's vX.Y.Z tags: the Release of its version.
type tag struct {
	version release.Version
	// Object is what the tag's ref points to: an annotated tag, whose type
	// is "tag", or a commit for a lightweight tag.
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// releaseTags returns the Site's vX.Y.Z tags, lowest first.
func releaseTags(ctx context.Context, repo Repository) ([]tag, error) {
	var refs []struct {
		Ref string `json:"ref"`
		tag
	}
	err := repo.Get(ctx, "git/matching-refs/tags/v", &refs)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tags []tag
	for _, ref := range refs {
		name, ok := strings.CutPrefix(ref.Ref, "refs/tags/v")
		if v, isVersion := release.ParseVersion(name); ok && isVersion {
			ref.version = v
			tags = append(tags, ref.tag)
		}
	}
	slices.SortFunc(tags, func(a, b tag) int {
		switch {
		case a.version.Less(b.version):
			return -1
		case b.version.Less(a.version):
			return 1
		}
		return 0
	})
	return tags, nil
}

// taggedAt is when the tag t was made: an annotated tag's date, which
// release.yml sets as it tags, since it only pushes annotated tags. It is
// zero, which sets no deadline, when GitHub does not say, and for a
// lightweight tag, which has no date of its own: its commit's is not when
// it was tagged, as with a commit tagged by hand an hour later. GitHub is
// not asked about a lightweight tag.
func taggedAt(ctx context.Context, repo Repository, t tag) (time.Time, error) {
	if t.Object.Type == "commit" {
		return time.Time{}, nil
	}
	var annotated struct {
		Tagger struct {
			Date time.Time `json:"date"`
		} `json:"tagger"`
	}
	err := repo.Get(ctx, "git/tags/"+t.Object.SHA, &annotated)
	return annotated.Tagger.Date, err
}

// deployment works out where a released Publish p is, from the rollout
// status of s's production and, until the Site runs it, from when first,
// the Release that first contained it, was tagged. failed answers a
// Publish whose Release failed to go live: kind names the failure in its
// ID, reason says what failed, and rolloutReason is what the service said.
func (t *Tracker) deployment(ctx context.Context, repo Repository, s site.Site, p Progress, first tag,
	failed func(kind, reason, rolloutReason string) (Progress, *Failure, error),
) (Progress, *Failure, error) {
	status, err := t.rollouts.Status(ctx, s.ID, rollout.Production)
	if err != nil {
		return Progress{}, nil, err
	}
	stage, kind, reason := byStatus(p.Version, status)
	switch stage {
	case Live:
		p.Stage = Live
		return p, nil, nil
	case Failed:
		return failed(kind, reason, status.Reason)
	}
	// Only with the service's answer that the Site runs a version, older
	// than the Publish's, can the hour pass. With no version it knows
	// running, the Site is between two Pods, as when one restarts, and
	// failing then would tell the Developer of nothing, and use up the
	// Failure's ID for when something does fail. A reason can't tell a
	// restart from a failure: the service sets a discovery reason, such as
	// a failing image scan, whatever the phase. A failed rollout, a first
	// install's included, is a failed phase, which failed above.
	if !atLeast(status.Running, release.Version{}) {
		p.Stage = stage
		return p, nil, nil
	}
	at, err := taggedAt(ctx, repo, first)
	if err != nil {
		return Progress{}, nil, err
	}
	if !at.IsZero() && t.now().Sub(at) > liveDeadline {
		return failed("not-live", fmt.Sprintf("not live %d minutes after its Release", int(liveDeadline.Minutes())), status.Reason)
	}
	p.Stage = stage
	return p, nil, nil
}

// byStatus is where a released Publish whose version is v is by the rollout
// status alone: Live, Deploying, Released, or Failed, with kind naming the
// failure and reason saying what failed.
//
//   - Live once the Site runs v or a newer version, whatever else holds.
//   - Failed when the rollout of the target failed while the target is v or
//     newer, or when the service reports a failure while the target is
//     older than v and the Site runs a version it knows: the platform can't
//     see v, as when the image scan of a live Site fails. With no version it
//     knows running, as while the cluster recovers and the service answers
//     null for both, the reason can't tell that from the Site being
//     re-created, so it is Released.
//   - Deploying while the target is v or newer.
//   - Released otherwise, while the platform has yet to pick v up.
func byStatus(v release.Version, status rollout.Status) (stage Stage, kind, reason string) {
	targeted := atLeast(status.Target, v)
	switch {
	case atLeast(status.Running, v):
		return Live, "", ""
	case targeted && status.Phase == rollout.Failed:
		return Failed, "rollout", "its rollout failed"
	case targeted:
		return Deploying, "", ""
	case status.Reason != "" && atLeast(status.Running, release.Version{}):
		return Failed, "no-target", "the platform did not pick up its Release"
	}
	return Released, "", ""
}

// atLeast tells whether the tag, such as v1.0.3, is of version v or a newer
// one. A tag the service does not know, or that names no X.Y.Z version, is
// older than every version, as the service's contract has it.
func atLeast(tag string, v release.Version) bool {
	name, ok := strings.CutPrefix(tag, "v")
	w, isVersion := release.ParseVersion(name)
	return ok && isVersion && !w.Less(v)
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

// named tells whether the Site's Release on Publish workflow names its runs
// after the commits they release, "Release <sha>", as the Site template's
// does since RMYPG/static-astro-site-template#15: one of the runs is.
func (rs runs) named() bool {
	for _, r := range rs.Runs {
		if sha, ok := strings.CutPrefix(r.DisplayTitle, "Release "); ok && github.IsSHA(sha) {
			return true
		}
	}
	return false
}

// underWay tells whether any of the runs has yet to complete: it is queued,
// waiting or in progress.
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
