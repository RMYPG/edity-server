//go:build sandbox

package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/config"
	"github.com/RMYPG/edity-server/internal/content"
	"github.com/RMYPG/edity-server/internal/fields"
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/site"
)

// releaseTimeout bounds how long a Publish's Release takes: the Site's CI,
// with its browser tests and image build, then Release on Publish.
const releaseTimeout = 30 * time.Minute

// TestPublishOnTheSandbox publishes on RMYPG/edity-sandbox for real, as the
// edity-publisher App, with the configuration mise run dev reads: it
// changes hero.cta and, once that Release is cut, puts it back, which makes
// two commits on main and two Releases. Each commit must be authored by the
// App's bot, carry the Edity-Client trailer, change only the page's content
// and package.json, pass the sandbox's CI, and be tagged by Release on
// Publish. A stale before and a broken Limit must commit nothing.
//
// mise run check:sandbox runs it. It is not part of mise run ci.
func TestPublishOnTheSandbox(t *testing.T) {
	ctx := t.Context()
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, ok := cfg.Sites.Lookup("localhost")
	if !ok || sandbox.Repository != (site.Repository{Owner: "RMYPG", Name: "edity-sandbox"}) {
		t.Fatalf("the Sites file does not serve RMYPG/edity-sandbox on localhost: %+v", sandbox)
	}
	app := github.NewApp(cfg.Publisher.ID, cfg.Publisher.PrivateKey, github.API, &http.Client{Timeout: 30 * time.Second})
	repo := app.Repo(sandbox.Repository.Owner, sandbox.Repository.Name)
	p := publish.New(func(site.Repository) publish.Repository { return repo })
	client := publish.Client{Name: "Edity end-to-end check"}
	bot, err := app.BotLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	start, err := repo.Head(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	original, err := repo.File(ctx, "src/content/home.json", start)
	if err != nil {
		t.Fatal(err)
	}
	page, err := content.Parse(original)
	if err != nil {
		t.Fatal(err)
	}
	cta, ok := page.At("hero.cta")
	if !ok {
		t.Fatal("home.json has no hero.cta")
	}
	was := cta.String()
	other := "Pide tu cita"
	if was == other {
		other = "Reserva tu cita"
	}
	t.Logf("main is at %s, hero.cta is %q", start, was)

	t.Run("a stale before commits nothing", func(t *testing.T) {
		d := publish.Draft{Page: "home", Changes: []publish.Change{changeOf(t, "hero.cta", was+" (stale)", other)}}
		_, err := p.Publish(ctx, sandbox, d, client)
		var conflicts *publish.ConflictsError
		if !errors.As(err, &conflicts) || !slices.Equal(conflicts.Paths, []string{"hero.cta"}) {
			t.Errorf("Publish: %v, want hero.cta as a Conflict (409)", err)
		}
		unmoved(t, repo, start)
	})
	t.Run("a broken Limit commits nothing", func(t *testing.T) {
		d := publish.Draft{Page: "home", Changes: []publish.Change{changeOf(t, "hero.cta", was, strings.Repeat("Reserva ", 4))}}
		_, err := p.Publish(ctx, sandbox, d, client)
		var violations *publish.ViolationsError
		if !errors.As(err, &violations) || !slices.Equal(violations.Violations, []fields.Violation{{Path: "hero.cta", Limit: fields.MaxChars}}) {
			t.Errorf("Publish: %v, want hero.cta's maxChars (422)", err)
		}
		unmoved(t, repo, start)
	})
	if t.Failed() {
		return
	}

	first := publishAndRelease(t, repo, p, sandbox, client, bot, start, changeOf(t, "hero.cta", was, other))
	if first == "" {
		return
	}
	last := publishAndRelease(t, repo, p, sandbox, client, bot, first, changeOf(t, "hero.cta", other, was))
	if last == "" {
		return
	}
	if back, err := repo.File(ctx, "src/content/home.json", last); err != nil || string(back) != string(original) {
		t.Errorf("home.json put back is not the file it was (%v):\n%s\nwas:\n%s", err, back, original)
	}
}

// publishAndRelease publishes c on top of head, checks the commit, and
// waits for its Release. It returns the commit's sha, or "" if it failed.
func publishAndRelease(t *testing.T, repo *github.Repo, p *publish.Publisher, s site.Site, client publish.Client, bot, head string, c publish.Change) string {
	t.Helper()
	ctx := t.Context()
	published, err := p.Publish(ctx, s, publish.Draft{Page: "home", Changes: []publish.Change{c}}, client)
	if err != nil {
		t.Errorf("Publish of %s: %v", c.Path, err)
		return ""
	}
	t.Logf("published %s → %s as %s, version %s", c.Before.String(), c.After.String(), published.SHA, published.Version)
	if len(published.Conflicts) > 0 {
		t.Errorf("Conflicts %v", published.Conflicts)
	}

	var commit struct {
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
		Commit struct {
			Message      string `json:"message"`
			Verification struct {
				Verified bool   `json:"verified"`
				Reason   string `json:"reason"`
			} `json:"verification"`
		} `json:"commit"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := repo.Get(ctx, "commits/"+published.SHA, &commit); err != nil {
		t.Errorf("reading the commit: %v", err)
		return ""
	}
	if commit.Author == nil || commit.Author.Login != bot {
		t.Errorf("the commit's author is %+v, want %s", commit.Author, bot)
	}
	if !commit.Commit.Verification.Verified {
		t.Errorf("the commit is not verified: %s", commit.Commit.Verification.Reason)
	}
	if len(commit.Parents) != 1 || commit.Parents[0].SHA != head {
		t.Errorf("the commit's parents are %+v, want %s", commit.Parents, head)
	}
	lines := strings.Split(strings.TrimSpace(commit.Commit.Message), "\n")
	if trailer := lines[len(lines)-1]; trailer != "Edity-Client: "+client.Name || !strings.Contains(commit.Commit.Message, "\n- "+c.Path+"\n") {
		t.Errorf("the commit message lacks the Field or the trailer:\n%s", commit.Commit.Message)
	}
	var files []string
	for _, f := range commit.Files {
		files = append(files, f.Filename)
	}
	slices.Sort(files)
	if !slices.Equal(files, []string{"package.json", "src/content/home.json"}) {
		t.Errorf("the commit changes %v", files)
	}

	if err := waitForRelease(ctx, t, repo, published); err != nil {
		t.Errorf("the Release of %s: %v", published.SHA, err)
		return ""
	}
	return published.SHA
}

// waitForRelease waits until CI passed on the commit and Release on Publish
// tagged it v<version>.
func waitForRelease(ctx context.Context, t *testing.T, repo *github.Repo, published publish.Published) error {
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	tag := "v" + published.Version.String()
	ciPassed := false
	for {
		if !ciPassed {
			var runs struct {
				WorkflowRuns []struct {
					Name       string `json:"name"`
					Status     string `json:"status"`
					Conclusion string `json:"conclusion"`
					URL        string `json:"html_url"`
				} `json:"workflow_runs"`
			}
			if err := repo.Get(ctx, "actions/runs?event=push&exclude_pull_requests=true&head_sha="+published.SHA, &runs); err != nil {
				return err
			}
			done := len(runs.WorkflowRuns) > 0
			for _, run := range runs.WorkflowRuns {
				if run.Status != "completed" {
					done = false
				} else if run.Conclusion != "success" && run.Conclusion != "skipped" && run.Conclusion != "neutral" {
					return fmt.Errorf("%s ended %s: %s", run.Name, run.Conclusion, run.URL)
				}
			}
			if done {
				ciPassed = true
				t.Logf("CI passed on %s", published.SHA)
			}
		}
		if ciPassed {
			tagged, err := taggedCommit(ctx, repo, tag)
			if err != nil {
				return err
			}
			if tagged != "" {
				if tagged != published.SHA {
					return fmt.Errorf("%s tags %s", tag, tagged)
				}
				return releaseRunPassed(ctx, t, repo, published.SHA, tag)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no %s after %v (CI passed: %v)", tag, releaseTimeout, ciPassed)
		case <-time.After(15 * time.Second):
		}
	}
}

// taggedCommit returns the commit an annotated tag is on, or "" if there is
// no such tag yet.
func taggedCommit(ctx context.Context, repo *github.Repo, tag string) (string, error) {
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	err := repo.Get(ctx, "git/ref/tags/"+tag, &ref)
	if errors.Is(err, github.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if ref.Object.Type != "tag" {
		return ref.Object.SHA, nil
	}
	var annotated struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := repo.Get(ctx, "git/tags/"+ref.Object.SHA, &annotated); err != nil {
		return "", err
	}
	return annotated.Object.SHA, nil
}

// releaseRunPassed checks that Release on Publish cut the tag: a run of it
// for the commit, which then was main's HEAD, ended in success.
func releaseRunPassed(ctx context.Context, t *testing.T, repo *github.Repo, sha, tag string) error {
	var runs struct {
		WorkflowRuns []struct {
			HeadSHA    string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"html_url"`
		} `json:"workflow_runs"`
	}
	for {
		if err := repo.Get(ctx, "actions/workflows/release-on-publish.yml/runs?event=workflow_run&per_page=20", &runs); err != nil {
			return err
		}
		for _, run := range runs.WorkflowRuns {
			if run.HeadSHA != sha {
				continue
			}
			if run.Status == "completed" && run.Conclusion == "success" {
				t.Logf("Release on Publish tagged %s %s: %s", sha, tag, run.URL)
				return nil
			}
			if run.Status == "completed" {
				return fmt.Errorf("Release on Publish ended %s: %s", run.Conclusion, run.URL)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s exists, but no Release on Publish run for %s ended", tag, sha)
		case <-time.After(10 * time.Second):
		}
	}
}

func unmoved(t *testing.T, repo *github.Repo, head string) {
	t.Helper()
	if now, err := repo.Head(t.Context(), "main"); err != nil || now != head {
		t.Errorf("main is at %s (%v), want it unmoved at %s", now, err, head)
	}
}

func changeOf(t *testing.T, path, before, after string) publish.Change {
	t.Helper()
	value := func(s string) *content.Value {
		text, _ := json.Marshal(s)
		v, err := content.ParseChange(text)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	return publish.Change{Path: path, Before: value(before), After: value(after)}
}
