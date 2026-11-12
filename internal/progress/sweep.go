package progress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/rollout"
	"github.com/RMYPG/edity-server/internal/site"
)

const (
	// SweepPeriod is how often Sweeps looks at each Site.
	SweepPeriod = 5 * time.Minute
	// maxSwept bounds the Publishes one sweep of a Site asks the progress
	// of: the newest ones that production does not run. More are a Site
	// whose Releases or rollouts have failed for a long while, which the
	// Developer has been told of.
	maxSwept = 10
	// sweepTimeout bounds one sweep of a Site, in which each look at GitHub
	// takes up to lookTimeout.
	sweepTimeout = 2 * time.Minute
)

// A Sweep is what one sweep of a Site found.
type Sweep struct {
	// Release is the tag of the Site's last Release, such as v1.0.2, or ""
	// when it has none.
	Release string
	// Running is the tag of the Release production runs, as the rollout
	// status service told it, or "" when it did not.
	Running string
	// Progress is how far each Publish swept has got, by its commit.
	Progress map[string]Progress
	// Left counts the Publishes past the newest maxSwept, which the sweep
	// did not ask about.
	Left int
}

// Sweep asks the progress of the Publishes on s's main that production
// does not run yet, the newest maxSwept of them, so that one that failed is
// reported to the Developer even when no Client asks for it (V6): a Client
// who closes the tab stops asking, and a Release whose rollout fails does so
// after it was cut. Those Publishes are the ones since the Release
// production runs, as the rollout status service tells it. When it tells
// that production runs no version it knows, such as when the Site has no
// Pod available, they are the ones since the Release production was last
// seen running, which the Tracker remembers from its sweeps' answers, when
// that is still one of the Site's tags. With none remembered, as after the
// server starts, since the Tracker keeps it in memory only, they are the
// ones since the Release before the target, so that the target's own are
// swept too, when the target is a Release older than the last, and
// otherwise since the Release before the last, so that the last Release's
// own are; or all of main's when there is no Release before it. When the
// service does not answer, or tells a Release GitHub has no tag of, they
// are the ones since the last Release, which GitHub alone can fail. The Tracker's Reporter tells a failure once, however many sweeps
// find it, and the progress of each commit is a look that requests for it
// share. The Publishes whose progress was not told are left to the next
// sweep, and the error joins their errors, and the service's.
func (t *Tracker) Sweep(ctx context.Context, s site.Site) (Sweep, error) {
	repo := t.repository(s.Repository)
	bot, err := t.bot(ctx)
	if err != nil {
		return Sweep{}, err
	}
	tags, err := releaseTags(ctx, repo)
	if err != nil {
		return Sweep{}, err
	}
	// The last Release, and the one before it, as tags, or "".
	var last, previous string
	if n := len(tags); n > 0 {
		last = "v" + tags[n-1].version.String()
		if n > 1 {
			previous = "v" + tags[n-2].version.String()
		}
	}
	sw := Sweep{Release: last, Progress: map[string]Progress{}}
	var errs []error
	since := last
	if status, err := t.rollouts.Status(ctx, s.ID, rollout.Production); err != nil {
		// What is since the last Release can still fail on GitHub.
		errs = append(errs, err)
	} else {
		sw.Running = status.Running
		switch {
		case !atLeast(status.Running, release.Version{}):
			// No version runs that the service knows: production is where
			// it was last seen, or, with nothing remembered, before the
			// target, whose rollout may be the one that failed, or before
			// the last Release.
			if seen, ok := t.lastRan(s.ID, tags); ok {
				since = seen
			} else if before, ok := releaseBefore(tags, status.Target); ok {
				since = before
			} else {
				since = previous
			}
		default:
			t.remember(s.ID, status.Running)
			if older(status.Running, last) {
				since = status.Running
			}
		}
	}
	commits, err := commitsSince(ctx, repo, since)
	if errors.Is(err, github.ErrNotFound) && since != last {
		commits, err = commitsSince(ctx, repo, last)
	}
	if err != nil {
		return sw, errors.Join(append(errs, err)...)
	}

	var publishes []string
	for _, c := range commits {
		// The author Progress tells a Publish's commit by.
		if c.Commit.Author.Name == bot {
			publishes = append(publishes, c.SHA)
		}
	}
	if len(publishes) > maxSwept {
		sw.Left, publishes = len(publishes)-maxSwept, publishes[:maxSwept]
	}
	for _, sha := range publishes {
		if ctx.Err() != nil {
			return sw, ctx.Err()
		}
		p, err := t.Progress(ctx, s, sha)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sha, err))
			continue
		}
		sw.Progress[sha] = p
	}
	return sw, errors.Join(errs...)
}

// older tells whether running, the tag production runs, is of an older
// Release than last, the tag of the last Release. A running that names no
// version is older than none.
func older(running, last string) bool {
	v, ok := strings.CutPrefix(last, "v")
	l, isVersion := release.ParseVersion(v)
	return ok && isVersion && atLeast(running, release.Version{}) && !atLeast(running, l)
}

// remember keeps running, a tag that names a version, as the version the
// Site whose id is site was last seen running, in place of any earlier one.
func (t *Tracker) remember(site, running string) {
	name, _ := strings.CutPrefix(running, "v")
	v, ok := release.ParseVersion(name)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ran[site] = v
}

// lastRan returns the tag of the version the Site whose id is site was last
// seen running, and true when there is one and it is still one of tags.
func (t *Tracker) lastRan(site string, tags []tag) (string, bool) {
	t.mu.Lock()
	v, ok := t.ran[site]
	t.mu.Unlock()
	if !ok {
		return "", false
	}
	for _, tag := range tags {
		if tag.version == v {
			return "v" + v.String(), true
		}
	}
	return "", false
}

// releaseBefore returns the tag of the Release before target, or "" when
// there is none, and true when target is one of tags older than the last.
func releaseBefore(tags []tag, target string) (string, bool) {
	name, ok := strings.CutPrefix(target, "v")
	v, isVersion := release.ParseVersion(name)
	if !ok || !isVersion {
		return "", false
	}
	for i, t := range tags[:max(0, len(tags)-1)] {
		if t.version == v {
			if i == 0 {
				return "", true
			}
			return "v" + tags[i-1].version.String(), true
		}
	}
	return "", false
}

// commitsSince lists the commits on main since the tag since, newest
// first, or every commit on main when since is "".
func commitsSince(ctx context.Context, repo Repository, since string) ([]listedCommit, error) {
	var commits []listedCommit
	if since == "" {
		// GitHub lists them newest first.
		err := repo.Get(ctx, "commits?sha=main&per_page=100", &commits)
		return commits, err
	}
	// GitHub lists them oldest first, and ends with main's HEAD however
	// many there are.
	var compared struct {
		Commits []listedCommit `json:"commits"`
	}
	if err := repo.Get(ctx, "compare/"+since+"...main", &compared); err != nil {
		return nil, err
	}
	for i := len(compared.Commits) - 1; i >= 0; i-- {
		commits = append(commits, compared.Commits[i])
	}
	return commits, nil
}

// listedCommit is a commit as GitHub lists them.
type listedCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
}

// Sweeps sweeps each of sites every period, one Site after another, from
// now until ctx is done, and logs what each sweep found. It returns once ctx
// is done and the sweep under way has stopped.
func (t *Tracker) Sweeps(ctx context.Context, sites []site.Site, period time.Duration, logger *slog.Logger) {
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		for _, s := range sites {
			if ctx.Err() != nil {
				return
			}
			t.sweepAndLog(ctx, s, logger)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (t *Tracker) sweepAndLog(ctx context.Context, s site.Site, logger *slog.Logger) {
	start := time.Now()
	swept, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	sw, err := t.Sweep(swept, s)
	if ctx.Err() != nil {
		// The server is stopping.
		return
	}
	failed := 0
	for _, p := range sw.Progress {
		if p.Stage == Failed {
			failed++
		}
	}
	attrs := []slog.Attr{
		slog.String("site", s.ID),
		slog.String("release", sw.Release),
		slog.String("running", sw.Running),
		slog.Int("publishes", len(sw.Progress)),
		slog.Int("failed", failed),
		slog.Int("left", sw.Left),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
	}
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "sweep failed", append(attrs, slog.String("error", err.Error()))...)
		return
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "swept", attrs...)
}
