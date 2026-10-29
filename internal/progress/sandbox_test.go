//go:build sandbox

package progress_test

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/config"
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/site"
)

// TestProgressOnTheSandbox reads the progress of the two Publishes that
// mise run check:sandbox made on RMYPG/edity-sandbox on 2026-10-29, as the
// edity-publisher App that made them, with the configuration mise run dev
// reads: both are released, as v1.0.1 and v1.0.2. The Developer commit before
// them is not a Publish's. Asked again, GitHub answers every call 304, which
// its rate limit does not count. It only reads, and gets an installation
// token.
//
// mise run check:progress runs it. It is not part of mise run ci.
func TestProgressOnTheSandbox(t *testing.T) {
	ctx := t.Context()
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, ok := cfg.Sites.Lookup("localhost")
	if !ok || sandbox.Repository != (site.Repository{Owner: "RMYPG", Name: "edity-sandbox"}) {
		t.Fatalf("the Sites file does not serve RMYPG/edity-sandbox on localhost: %+v", sandbox)
	}
	calls := &countingTransport{}
	app := github.NewApp(cfg.Publisher.ID, cfg.Publisher.PrivateKey, github.API, &http.Client{Timeout: 30 * time.Second, Transport: calls})
	tracker := func() *progress.Tracker {
		// It only reads, so it tells no one of a failure.
		return progress.New(func(r site.Repository) progress.Repository { return app.Repo(r.Owner, r.Name) }, app.BotLogin, nil)
	}

	want := map[string]progress.Progress{
		"4bc931076ac8583bd8905a4c508c987877afc09d": {Stage: progress.Released, Version: release.Version{1, 0, 1}},
		"f50d2c45e28706d25da874225f0f64770cd1ac12": {Stage: progress.Released, Version: release.Version{1, 0, 2}},
	}
	first := tracker()
	for sha, w := range want {
		if p, err := first.Progress(ctx, sandbox, sha); err != nil || p != w {
			t.Errorf("Progress of %s = %+v, %v, want %+v", sha, p, err, w)
		}
	}
	developer := "31077c358f1b793aefc1c495c938055d99d3c227"
	if p, err := first.Progress(ctx, sandbox, developer); !errors.Is(err, progress.ErrNotPublish) {
		t.Errorf("Progress of the Developer's %s = %+v, %v, want ErrNotPublish", developer, p, err)
	}
	made, cached, used := calls.counts()
	t.Logf("first look: %d calls, %d of them 304, the installation's rate limit used %s", made, cached, used)

	// A new Tracker shares nothing with the first but the App's ETags.
	again := tracker()
	for sha, w := range want {
		if p, err := again.Progress(ctx, sandbox, sha); err != nil || p != w {
			t.Errorf("Progress of %s asked again = %+v, %v, want %+v", sha, p, err, w)
		}
	}
	total, notModified, usedAgain := calls.counts()
	total, notModified = total-made, notModified-cached
	t.Logf("asked again: %d calls, %d of them 304, the rate limit used %s", total, notModified, usedAgain)
	// The rate limit used is only logged: anything else the installation
	// does in the meantime counts too.
	if total == 0 || notModified != total {
		t.Errorf("asked again: %d calls, %d of them 304, want every call 304", total, notModified)
	}
}

// countingTransport counts the calls made with an installation token, those
// GitHub answered 304, and the installation's rate limit used, as GitHub
// last told.
type countingTransport struct {
	mu                 sync.Mutex
	calls, notModified int
	used               string
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || !strings.HasPrefix(r.URL.Path, "/repos/") || strings.HasSuffix(r.URL.Path, "/installation") {
		return res, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if res.StatusCode == http.StatusNotModified {
		c.notModified++
	}
	if used := res.Header.Get("X-RateLimit-Used"); used != "" {
		c.used = used
	}
	return res, nil
}

func (c *countingTransport) counts() (calls, notModified int, used string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.notModified, c.used
}
