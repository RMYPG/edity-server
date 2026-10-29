package progress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/RMYPG/edity-server/internal/site"
)

const (
	// SweepPeriod is how often Sweeps looks at each Site.
	SweepPeriod = 5 * time.Minute
	// maxSwept bounds the Publishes one sweep of a Site asks the progress
	// of: the newest ones since its last Release. More are a Site whose
	// Releases have failed for a long while, which the Developer has been
	// told of.
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
	// Progress is how far each Publish swept has got, by its commit.
	Progress map[string]Progress
	// Left counts the Publishes since the last Release past the newest
	// maxSwept, which the sweep did not ask about.
	Left int
}

// Sweep asks the progress of the Publishes on s's main since its last
// Release, the newest maxSwept of them, so that one that failed is reported
// to the Developer even when no Client asks for it (V6): a Client who
// closes the tab stops asking. The Tracker's Reporter tells a failure once,
// however many sweeps find it, and the progress of each commit is a look
// that requests for it share. The Publishes whose progress GitHub did not
// tell are left to the next sweep, and the error joins their errors.
func (t *Tracker) Sweep(ctx context.Context, s site.Site) (Sweep, error) {
	repo := t.repository(s.Repository)
	bot, err := t.bot(ctx)
	if err != nil {
		return Sweep{}, err
	}
	last, err := LastRelease(ctx, repo)
	if err != nil {
		return Sweep{}, err
	}
	var commits []listedCommit
	if last == "" {
		// Every commit on main is since the last Release: GitHub lists its
		// newest first.
		err = repo.Get(ctx, "commits?sha=main&per_page=100", &commits)
	} else {
		// GitHub lists them oldest first, and ends with main's HEAD however
		// many there are.
		var compared struct {
			Commits []listedCommit `json:"commits"`
		}
		err = repo.Get(ctx, "compare/"+last+"...main", &compared)
		for i := len(compared.Commits) - 1; i >= 0; i-- {
			commits = append(commits, compared.Commits[i])
		}
	}
	if err != nil {
		return Sweep{}, err
	}

	sw := Sweep{Release: last, Progress: map[string]Progress{}}
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
	var errs []error
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
