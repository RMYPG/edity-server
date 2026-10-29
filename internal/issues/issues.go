// Package issues reports to the Developer, on GitHub issues of a Site's
// repository, what they need to know of its Publishes: a Release that failed
// (V6), and commits on main that a Publish ships and that change more than
// content (V7).
//
// It stores nothing. Each kind of news has one open issue on the Site
// repository at a time, which the publisher App opened, and which a hidden
// marker in its body tells from any other issue. A second marker lists what
// the issue has told, so that a failure polled again, or by a server that
// restarted, is told once. One call lists the App's latest issues, closed
// ones too, and GitHub does not count it against the rate limit while
// nothing changed. Once the Developer closes the issue, the next news opens
// a new one, and what the closed one told is not told again.
package issues

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/site"
)

// A Repository is a Site repository, as the publisher App reads it and
// writes its issues. *github.Repo is one.
type Repository interface {
	progress.Repository
	Post(ctx context.Context, path string, body, out any) error
	Patch(ctx context.Context, path string, body, out any) error
}

// A Reporter writes the App's issues on the Sites' repositories. It is safe
// for concurrent use.
type Reporter struct {
	repository func(site.Repository) Repository
	bot        func(context.Context) (string, error)
	logger     *slog.Logger

	mu    sync.Mutex
	sites map[string]*siteIssues
}

// siteIssues is what a Reporter knows of one Site's issues.
type siteIssues struct {
	// mu lets one call at a time write the Site's issues, so that two
	// failures at once open one issue.
	mu sync.Mutex
	// told are the news, by topic and key, that the Reporter wrote or saw
	// on an issue, so that it need not look again. It is what GitHub has,
	// and forgetting it costs a look.
	told map[string]bool
}

// maxRemembered bounds the news a Reporter remembers for each Site.
const maxRemembered = 1000

// New returns a Reporter that reaches a Site's repository through
// repository, and tells the App's issues by their author, the login bot
// returns: the publisher App's.
func New[R Repository](repository func(site.Repository) R, bot func(context.Context) (string, error), logger *slog.Logger) *Reporter {
	return &Reporter{
		repository: func(r site.Repository) Repository { return repository(r) },
		bot:        bot,
		logger:     logger,
		sites:      map[string]*siteIssues{},
	}
}

func (r *Reporter) site(id string) *siteIssues {
	r.mu.Lock()
	defer r.mu.Unlock()
	in, ok := r.sites[id]
	if !ok {
		in = &siteIssues{told: map[string]bool{}}
		r.sites[id] = in
	}
	return in
}

func (in *siteIssues) remember(t topic, keys []string) {
	if len(in.told)+len(keys) > maxRemembered {
		clear(in.told)
	}
	for _, k := range keys {
		in.told[t.marker+" "+k] = true
	}
}

// A topic is one kind of news, with its own issue.
type topic struct {
	// marker tells the topic's issues from any other.
	marker string
	title  string
	// about opens the body of a new issue, before its first news.
	about string
}

// toldMarker is the marker that lists the keys of what an issue told.
var toldMarker = regexp.MustCompile(`<!-- edity:told([^>]*)-->`)

// maxTold bounds the keys an issue's body lists, well within GitHub's
// 65,536 characters.
const maxTold = 200

// told writes the marker that lists keys, the latest maxTold of them.
func told(keys []string) string {
	return "<!-- edity:told " + strings.Join(keys[max(0, len(keys)-maxTold):], " ") + " -->"
}

// toldIn reads the keys of what the body of an issue told.
func toldIn(body string) []string {
	m := toldMarker.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	return strings.Fields(m[1])
}

// withTold is body with its marker listing keys instead, or with one at
// the end when it has none.
func withTold(body string, keys []string) string {
	if loc := toldMarker.FindStringIndex(body); loc != nil {
		return body[:loc[0]] + told(keys) + body[loc[1]:]
	}
	return body + "\n" + told(keys)
}

// An issue is one of the App's issues, or pull requests, as GitHub lists
// them.
type issue struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Body   string `json:"body"`
	// PullRequest is there only for a pull request.
	PullRequest *struct{} `json:"pull_request"`
}

// listed is how many of the App's issues a look reads: those updated last,
// which hold what any failure still polled was told on.
const listed = 30

func issuesPath(bot string) string {
	return fmt.Sprintf("issues?state=all&creator=%s&sort=updated&direction=desc&per_page=%d", url.QueryEscape(bot), listed)
}

// A writeup is what an issue is to tell of some news: its text, empty when
// there is nothing to tell, and the keys of the news it deals with, which
// are not looked at again.
type writeup struct {
	text string
	keys []string
}

// report makes sure that one of the App's issues of topic t on s's
// repository tells the news of each key. It asks write what to say of those no issue
// of t dealt with yet, and comments it on the topic's open issue, or opens
// one with it.
func (r *Reporter) report(ctx context.Context, s site.Site, t topic, keys []string, write func(untold []string) (writeup, error)) error {
	in := r.site(s.ID)
	in.mu.Lock()
	defer in.mu.Unlock()
	keys = slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return in.told[t.marker+" "+k] })
	if len(keys) == 0 {
		return nil
	}

	repo := r.repository(s.Repository)
	bot, err := r.bot(ctx)
	if err != nil {
		return err
	}
	var issues []issue
	if err := repo.Get(ctx, issuesPath(bot), &issues); err != nil {
		return err
	}
	seen := map[string]bool{}
	var open *issue
	for i, is := range issues {
		if is.PullRequest != nil || !strings.Contains(is.Body, t.marker) {
			continue
		}
		for _, k := range toldIn(is.Body) {
			seen[k] = true
		}
		if is.State == "open" && (open == nil || is.Number > open.Number) {
			open = &issues[i]
		}
	}
	var untold, already []string
	for _, k := range keys {
		if seen[k] {
			already = append(already, k)
		} else {
			untold = append(untold, k)
		}
	}
	in.remember(t, already)
	if len(untold) == 0 {
		return nil
	}

	w, err := write(untold)
	if err != nil {
		return err
	}
	if w.text == "" {
		in.remember(t, w.keys)
		return nil
	}
	if open == nil {
		body := t.about + "\n\n" + w.text + "\n\n" + t.marker + "\n" + told(w.keys)
		if err := repo.Post(ctx, "issues", map[string]string{"title": t.title, "body": body}, nil); err != nil {
			return err
		}
	} else {
		if err := repo.Post(ctx, fmt.Sprintf("issues/%d/comments", open.Number), map[string]string{"body": w.text}, nil); err != nil {
			return err
		}
		// The comment reaches the Developer, so the news is told. If the
		// body is not updated, a server that restarts may tell it again.
		body := withTold(open.Body, append(toldIn(open.Body), w.keys...))
		if err := repo.Patch(ctx, fmt.Sprintf("issues/%d", open.Number), map[string]string{"body": body}, nil); err != nil {
			r.logger.LogAttrs(ctx, slog.LevelWarn, "issue not updated with what it told",
				slog.String("site", s.ID), slog.Int("issue", open.Number), slog.String("error", err.Error()))
		}
	}
	in.remember(t, w.keys)
	return nil
}

// code is s as Markdown's inline code.
func code(s string) string {
	if strings.Contains(s, "`") {
		return "`` " + s + " ``"
	}
	return "`" + s + "`"
}

// escape is s as Markdown text, with nothing in it that GitHub would read
// as formatting, HTML, a link or a mention. An issue's number, such as a
// pull request's #3, still links to it.
func escape(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune("\\`*_[]<>@~", c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}
