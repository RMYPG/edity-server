package issues

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/site"
)

// The sandbox's two Publishes of 2026-10-29, and the Developer's last two
// commits before them, which testdata/record.sh records GitHub's answers
// about.
const (
	first     = "4bc931076ac8583bd8905a4c508c987877afc09d" // v1.0.1
	second    = "f50d2c45e28706d25da874225f0f64770cd1ac12" // v1.0.2
	developer = "7b2ba675cfb202e9860bae12a70ace8bf6ba2d44"
	// merge merged developer into main, as v1.0.0.
	merge = "31077c358f1b793aefc1c495c938055d99d3c227"

	// publishMessage is both Publishes' commit message.
	publishMessage = "content: publish home\n\nChanged:\n- hero.cta\n\nEdity-Client: Edity end-to-end check"
)

var sandbox = site.Site{ID: "edity-sandbox", Repository: site.Repository{Owner: "RMYPG", Name: "edity-sandbox"}}

// The failures progress tells of the two Publishes, had their runs failed.
var (
	ciFailed = progress.Failure{
		SHA: first, Message: publishMessage, Version: "1.0.1",
		Run: "https://github.com/RMYPG/edity-sandbox/actions/runs/40934506192", Reason: "CI ended failure",
		ID: "run:40934506192/1",
	}
	// ciFailedAgain is CI run again on the first Publish, and failing again.
	ciFailedAgain = progress.Failure{
		SHA: first, Message: publishMessage, Version: "1.0.1",
		Run: "https://github.com/RMYPG/edity-sandbox/actions/runs/40934506192", Reason: "CI ended failure",
		ID: "run:40934506192/2",
	}
	releaseFailed = progress.Failure{
		SHA: second, Message: publishMessage, Version: "1.0.2",
		Run: "https://github.com/RMYPG/edity-sandbox/actions/runs/40935264906", Reason: "Release on Publish ended failure",
		ID: "run:40935264906/1",
	}
	noCI = progress.Failure{
		SHA: second, Message: publishMessage, Version: "1.0.2",
		Reason: "no CI run 10 minutes after the commit",
		ID:     "no-ci:" + second,
	}
)

// A response is one of GitHub's answers.
type response struct {
	status int
	header http.Header
	body   []byte
}

// recorded reads an answer testdata/record.sh recorded: a status line,
// headers and the body.
func recorded(t testing.TB, name string) response {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(data)))
	line, err := r.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	_, code, _ := strings.Cut(line, " ")
	status, err := strconv.Atoi(code[:3])
	if err != nil {
		t.Fatalf("%s: status line %q", name, line)
	}
	header, err := r.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	body, err := io.ReadAll(r.R)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: status, header: http.Header(header), body: body}
}

// edited is r with its JSON body changed by edit.
func edited(t testing.TB, r response, edit func(body map[string]any)) response {
	t.Helper()
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	edit(body)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: r.status, header: r.header, body: b}
}

// tagsUpTo is the recorded tag list down to the tags up to last.
func tagsUpTo(t testing.TB, last string) response {
	t.Helper()
	r := recorded(t, "tags.http")
	var refs []map[string]any
	if err := json.Unmarshal(r.body, &refs); err != nil {
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
	return response{status: r.status, header: r.header, body: b}
}

// fakeGitHub answers the App's calls on RMYPG/edity-sandbox: its GETs from
// answers, and its issues from the issues it holds, which it lists, opens,
// comments on and changes in the shape the issues API has, recorded in
// testdata/pull-4.http. Each answer to a GET has an ETag, which depends on
// the token too, and a call whose If-None-Match is still its answer's gets
// 304. It counts the calls made with an installation token, which count
// against the installation's rate limit unless they get 304.
type fakeGitHub struct {
	t *testing.T

	mu      sync.Mutex
	answers map[string]response
	// shape is an item of an issue list as GitHub has it: pull request #4.
	shape    map[string]any
	issues   []*fakeIssue
	comments map[int][]string
	// tick counts the writes, for each issue's updated_at.
	tick int
	// fail answers "<method> <path>" instead of the fake.
	fail map[string]response
	// slowList holds the issue list's answer back, so that calls at once
	// would all list before any writes.
	slowList time.Duration
	tokens   map[string]bool
	// calls counts the calls made with a token, notModified those of them
	// answered 304, and writes the POSTs and PATCHes.
	calls, notModified, writes int
}

type fakeIssue struct {
	number             int
	creator            string
	title, body, state string
	pull               bool
	updated            int
}

const bot = "edity-publisher[bot]"

func newFake(t *testing.T) *fakeGitHub {
	t.Helper()
	pull := recorded(t, "pull-4.http")
	var shape map[string]any
	if err := json.Unmarshal(pull.body, &shape); err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{
		t:     t,
		shape: shape,
		answers: map[string]response{
			"git/matching-refs/tags/v": tagsUpTo(t, "v1.0.0"),
			// As it was before the second Publish.
			"compare/v1.0.0..." + first: recorded(t, "compare-v1.0.0-4bc9310.http"),
			"commits/" + developer:      recorded(t, "commit-7b2ba67.http"),
			"commits/" + merge:          recorded(t, "commit-31077c3.http"),
		},
		// The sandbox's own pull request, which the issues API lists too.
		issues:   []*fakeIssue{{number: 4, creator: "MarioRP-01", title: shape["title"].(string), body: shape["body"].(string), state: "open", pull: true}},
		comments: map[int][]string{},
		fail:     map[string]response{},
		tokens:   map[string]bool{},
	}
	if issues := recorded(t, "issues.http"); string(bytes.TrimSpace(issues.body)) != "[]" {
		t.Fatalf("the sandbox has issues of the App's: %s", issues.body)
	}
	return f
}

// reporter is a Reporter of the App on f, as a server that just started
// has it.
func (f *fakeGitHub) reporter(t *testing.T) *Reporter {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	app := github.NewApp(5466528, testKey(), srv.URL, srv.Client())
	return New(func(r site.Repository) Repository { return app.Repo(r.Owner, r.Name) }, app.BotLogin, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/RMYPG/edity-sandbox/issues") {
		f.mu.Lock()
		slow := f.slowList
		f.mu.Unlock()
		time.Sleep(slow)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON := func(status int, body any) {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		writeJSON(http.StatusOK, map[string]any{"id": 5466528, "slug": "edity-publisher"})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/repos/RMYPG/edity-sandbox/installation":
		writeJSON(http.StatusOK, map[string]any{"id": 77})
		return
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/77/access_tokens":
		token := fmt.Sprintf("ghs_%d", len(f.tokens)+1)
		f.tokens[token] = true
		writeJSON(http.StatusCreated, map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour)})
		return
	}

	f.calls++
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !f.tokens[token] {
		writeJSON(http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/RMYPG/edity-sandbox/")
	if !ok {
		writeJSON(http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}
	if r.Method != http.MethodGet {
		f.writes++
	}
	if failed, ok := f.fail[r.Method+" "+path]; ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(failed.status)
		_, _ = w.Write(failed.body)
		return
	}

	var answer response
	switch {
	case r.Method == http.MethodGet && path == "issues":
		answer = f.list(r)
	case r.Method == http.MethodGet:
		var found bool
		if answer, found = f.answers[strings.TrimPrefix(r.URL.RequestURI(), "/repos/RMYPG/edity-sandbox/")]; !found {
			writeJSON(http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
	case r.Method == http.MethodPost && path == "issues":
		var body map[string]string
		f.decode(r, &body, "title", "body")
		f.tick++
		is := &fakeIssue{number: len(f.issues) + 4, creator: bot, title: body["title"], body: body["body"], state: "open", updated: f.tick}
		f.issues = append(f.issues, is)
		writeJSON(http.StatusCreated, f.render(is))
		return
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
		is := f.issue(strings.TrimSuffix(strings.TrimPrefix(path, "issues/"), "/comments"))
		if is == nil {
			writeJSON(http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		var body map[string]string
		f.decode(r, &body, "body")
		f.tick++
		is.updated = f.tick
		f.comments[is.number] = append(f.comments[is.number], body["body"])
		// As GitHub documents a new comment.
		writeJSON(http.StatusCreated, map[string]any{
			"id":                 f.tick,
			"node_id":            fmt.Sprintf("IC_%d", f.tick),
			"url":                fmt.Sprintf("https://api.github.com/repos/RMYPG/edity-sandbox/issues/comments/%d", f.tick),
			"html_url":           fmt.Sprintf("https://github.com/RMYPG/edity-sandbox/issues/%d#issuecomment-%d", is.number, f.tick),
			"body":               body["body"],
			"user":               map[string]any{"login": bot, "type": "Bot"},
			"created_at":         "2026-10-29T19:00:00Z",
			"updated_at":         "2026-10-29T19:00:00Z",
			"issue_url":          fmt.Sprintf("https://api.github.com/repos/RMYPG/edity-sandbox/issues/%d", is.number),
			"author_association": "NONE",
		})
		return
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "issues/"):
		is := f.issue(strings.TrimPrefix(path, "issues/"))
		if is == nil {
			writeJSON(http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		var body map[string]string
		f.decode(r, &body, "body")
		f.tick++
		is.body, is.updated = body["body"], f.tick
		writeJSON(http.StatusOK, f.render(is))
		return
	default:
		writeJSON(http.StatusNotFound, map[string]any{"message": "Not Found"})
		return
	}

	etag := fmt.Sprintf(`W/"%x"`, sha256.Sum256(append([]byte(token), answer.body...)))
	if r.Header.Get("If-None-Match") == etag {
		f.notModified++
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", answer.header.Get("Content-Type"))
	w.Header().Set("ETag", etag)
	w.WriteHeader(answer.status)
	_, _ = w.Write(answer.body)
}

// decode reads a write's JSON body, which may hold only the keys allowed.
func (f *fakeGitHub) decode(r *http.Request, body *map[string]string, allowed ...string) {
	if err := json.NewDecoder(r.Body).Decode(body); err != nil {
		f.t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
	}
	for k := range *body {
		if !slices.Contains(allowed, k) {
			f.t.Errorf("%s %s sent %s", r.Method, r.URL.Path, k)
		}
	}
}

func (f *fakeGitHub) issue(number string) *fakeIssue {
	for _, is := range f.issues {
		if strconv.Itoa(is.number) == number {
			return is
		}
	}
	return nil
}

// list answers the issue list, as GitHub filters and sorts it.
func (f *fakeGitHub) list(r *http.Request) response {
	q := r.URL.Query()
	if q.Get("state") != "all" || q.Get("sort") != "updated" || q.Get("direction") != "desc" || q.Get("creator") == "" {
		f.t.Errorf("issues listed with %s", r.URL.RawQuery)
	}
	issues := slices.Clone(f.issues)
	slices.SortStableFunc(issues, func(a, b *fakeIssue) int { return b.updated - a.updated })
	items := []map[string]any{}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	for _, is := range issues {
		if is.creator == q.Get("creator") && len(items) < perPage {
			items = append(items, f.render(is))
		}
	}
	b, err := json.Marshal(items)
	if err != nil {
		f.t.Fatal(err)
	}
	return response{status: http.StatusOK, header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, body: b}
}

// render is is as the issues API has an issue, or a pull request.
func (f *fakeGitHub) render(is *fakeIssue) map[string]any {
	item := maps.Clone(f.shape)
	if !is.pull {
		delete(item, "pull_request")
		delete(item, "draft")
	}
	item["number"] = is.number
	item["url"] = fmt.Sprintf("https://api.github.com/repos/RMYPG/edity-sandbox/issues/%d", is.number)
	item["html_url"] = fmt.Sprintf("https://github.com/RMYPG/edity-sandbox/issues/%d", is.number)
	item["title"], item["body"], item["state"] = is.title, is.body, is.state
	item["user"] = map[string]any{"login": is.creator}
	item["comments"] = len(f.comments[is.number])
	item["updated_at"] = time.Date(2026, 10, 29, 19, 0, is.updated, 0, time.UTC).Format(time.RFC3339)
	return item
}

// opened returns the App's issues, the first opened first.
func (f *fakeGitHub) opened() []fakeIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	var mine []fakeIssue
	for _, is := range f.issues {
		if is.creator == bot && !is.pull {
			mine = append(mine, *is)
		}
	}
	return mine
}

func (f *fakeGitHub) commentsOn(number int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments[number])
}

// close is the Developer closing an issue.
func (f *fakeGitHub) close(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tick++
	is := f.issue(strconv.Itoa(number))
	is.state, is.updated = "closed", f.tick
}

func (f *fakeGitHub) counts() (calls, counted, writes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.calls - f.notModified, f.writes
}

var toldIDs = regexp.MustCompile(`<!-- edity:told ([^>]*) -->`)

// toldOn returns the keys the body of an issue lists as told.
func toldOn(t *testing.T, is fakeIssue) string {
	t.Helper()
	m := toldIDs.FindStringSubmatch(is.body)
	if m == nil {
		t.Fatalf("issue #%d lists nothing told:\n%s", is.number, is.body)
	}
	return m[1]
}

func TestRepeatedFailuresUpdateTheSameOpenIssue(t *testing.T) {
	f := newFake(t)
	r := f.reporter(t)
	report := func(r *Reporter, failure progress.Failure) {
		t.Helper()
		if err := r.ReportFailure(t.Context(), sandbox, failure); err != nil {
			t.Fatalf("ReportFailure(%s): %v", failure.ID, err)
		}
	}

	// The first failure opens the issue.
	report(r, ciFailed)
	opened := f.opened()
	if len(opened) != 1 || opened[0].number != 5 || opened[0].title != "Edity: a Publish was not released" || opened[0].state != "open" {
		t.Fatalf("issues opened: %+v, want one, #5", opened)
	}
	if told := toldOn(t, opened[0]); told != ciFailed.ID {
		t.Errorf("issue #5 lists %q as told, want %q", told, ciFailed.ID)
	}
	if !strings.Contains(opened[0].body, failureText(ciFailed)) || !strings.Contains(opened[0].body, "<!-- edity:release-failures -->") {
		t.Errorf("issue #5 does not tell what failed:\n%s", opened[0].body)
	}

	// Polled again, by this server or one that restarts, it tells nothing
	// more; and this server need not even look.
	calls, _, writes := f.counts()
	report(r, ciFailed)
	if again, _, _ := f.counts(); again != calls {
		t.Errorf("telling the same failure again made %d calls, want none", again-calls)
	}
	restarted := f.reporter(t)
	report(restarted, ciFailed)
	report(restarted, ciFailed)
	if _, _, after := f.counts(); after != writes || len(f.commentsOn(5)) != 0 {
		t.Errorf("telling the same failure again wrote %d times, and %d comments, want none", after-writes, len(f.commentsOn(5)))
	}

	// Another failure, and the same run failing again, are comments on it.
	report(restarted, releaseFailed)
	report(r, ciFailedAgain)
	report(f.reporter(t), releaseFailed)
	report(f.reporter(t), ciFailedAgain)
	opened = f.opened()
	comments := f.commentsOn(5)
	if len(opened) != 1 || len(comments) != 2 || comments[0] != failureText(releaseFailed) || comments[1] != failureText(ciFailedAgain) {
		t.Fatalf("issues %+v, with comments %q, want #5 alone with the two failures", opened, comments)
	}
	if want := ciFailed.ID + " " + releaseFailed.ID + " " + ciFailedAgain.ID; toldOn(t, opened[0]) != want {
		t.Errorf("issue #5 lists %q as told, want %q", toldOn(t, opened[0]), want)
	}

	// Closed, it stays closed: what it told is not told again, and the next
	// failure opens a new issue.
	f.close(5)
	report(f.reporter(t), releaseFailed)
	report(f.reporter(t), noCI)
	opened = f.opened()
	if len(opened) != 2 || opened[0].state != "closed" || opened[1].number != 6 || opened[1].state != "open" || len(f.commentsOn(5)) != 2 {
		t.Fatalf("issues %+v, want #5 closed and #6 open", opened)
	}
	if told := toldOn(t, opened[1]); told != noCI.ID {
		t.Errorf("issue #6 lists %q as told, want %q", told, noCI.ID)
	}
	report(f.reporter(t), ciFailedAgain)
	if len(f.commentsOn(6)) != 0 {
		t.Errorf("issue #6 told %q again", f.commentsOn(6))
	}
}

func TestAFailureSaysWhatFailedForWhichPublish(t *testing.T) {
	for _, c := range []struct {
		failure progress.Failure
		want    string
	}{
		{ciFailed, "### v1.0.1 was not released\n\n" +
			"CI ended failure. [See the run](https://github.com/RMYPG/edity-sandbox/actions/runs/40934506192).\n\n" +
			"Edity end-to-end check published `home` in 4bc931076ac8583bd8905a4c508c987877afc09d, which changed:\n\n" +
			"- `hero.cta`"},
		{noCI, "### v1.0.2 was not released\n\n" +
			"No CI run 10 minutes after the commit.\n\n" +
			"Edity end-to-end check published `home` in f50d2c45e28706d25da874225f0f64770cd1ac12, which changed:\n\n" +
			"- `hero.cta`"},
		{progress.Failure{
			SHA: first, Version: "1.0.1", Reason: "CI ended failure", ID: "run:1/1",
			Message: "content: publish home\n\nChanged:\n- hero.title\n- plans.1.price\n\nEdity-Client: @MarioRP-01 *Ana* <b>",
		}, "### v1.0.1 was not released\n\n" +
			"CI ended failure.\n\n" +
			"\\@MarioRP-01 \\*Ana\\* \\<b\\> published `home` in 4bc931076ac8583bd8905a4c508c987877afc09d, which changed:\n\n" +
			"- `hero.title`\n- `plans.1.price`"},
		{progress.Failure{SHA: first, Version: "1.0.1", Reason: "CI ended failure", ID: "run:1/1", Message: "Not a Publish's"},
			"### v1.0.1 was not released\n\nCI ended failure.\n\nThe Publish is 4bc931076ac8583bd8905a4c508c987877afc09d."},
	} {
		if got := failureText(c.failure); got != c.want {
			t.Errorf("failureText(%s):\n%s\nwant:\n%s", c.failure.ID, got, c.want)
		}
	}
}

func TestTwoFailuresAtOnceOpenOneIssue(t *testing.T) {
	f := newFake(t)
	f.slowList = 50 * time.Millisecond
	r := f.reporter(t)
	var wg sync.WaitGroup
	for _, failure := range []progress.Failure{ciFailed, releaseFailed} {
		wg.Go(func() {
			if err := r.ReportFailure(t.Context(), sandbox, failure); err != nil {
				t.Errorf("ReportFailure(%s): %v", failure.ID, err)
			}
		})
	}
	wg.Wait()
	if opened := f.opened(); len(opened) != 1 || len(f.commentsOn(opened[0].number)) != 1 {
		t.Errorf("issues %+v, want one, with a comment", opened)
	}
}

func TestAFailureIsToldOnlyOnceTheIssueIsWritten(t *testing.T) {
	f := newFake(t)
	r := f.reporter(t)
	f.fail["POST issues"] = response{status: http.StatusBadGateway, body: []byte(`{"message": "Server Error"}`)}
	var gerr *github.Error
	if err := r.ReportFailure(t.Context(), sandbox, ciFailed); !errors.As(err, &gerr) || gerr.Status != http.StatusBadGateway {
		t.Fatalf("ReportFailure while GitHub fails: %v, want its 502", err)
	}
	delete(f.fail, "POST issues")
	if err := r.ReportFailure(t.Context(), sandbox, ciFailed); err != nil || len(f.opened()) != 1 {
		t.Fatalf("ReportFailure once GitHub is back: %v, issues %+v, want one", err, f.opened())
	}

	// A comment not written is not told.
	f.fail["POST issues/5/comments"] = response{status: http.StatusServiceUnavailable, body: []byte(`{"message": "Unavailable"}`)}
	if err := r.ReportFailure(t.Context(), sandbox, releaseFailed); !errors.As(err, &gerr) || gerr.Status != http.StatusServiceUnavailable {
		t.Fatalf("ReportFailure while comments fail: %v, want GitHub's 503", err)
	}
	delete(f.fail, "POST issues/5/comments")

	// A comment written is told, even if the body that lists what was told
	// could not be updated.
	f.fail["PATCH issues/5"] = response{status: http.StatusBadGateway, body: []byte(`{"message": "Server Error"}`)}
	if err := r.ReportFailure(t.Context(), sandbox, releaseFailed); err != nil {
		t.Fatalf("ReportFailure with the comment written: %v", err)
	}
	if err := r.ReportFailure(t.Context(), sandbox, releaseFailed); err != nil || len(f.commentsOn(5)) != 1 {
		t.Errorf("ReportFailure again: %v, comments %q, want the one", err, f.commentsOn(5))
	}

	// Issues turned off on the repository: the Developer can't be told.
	f = newFake(t)
	f.fail["POST issues"] = response{status: http.StatusGone, body: []byte(`{"message": "Issues are disabled for this repo"}`)}
	if err := f.reporter(t).ReportFailure(t.Context(), sandbox, ciFailed); !errors.As(err, &gerr) || gerr.Status != http.StatusGone {
		t.Errorf("ReportFailure with Issues turned off: %v, want GitHub's 410", err)
	}
}

func TestOnlyTheAppsFailureIssuesCount(t *testing.T) {
	f := newFake(t)
	// The App's issue of Developer commits, and a pull request of the App's
	// whose body has the markers, neither of which is the failure issue.
	f.issues = append(f.issues,
		&fakeIssue{number: 5, creator: bot, title: developerCommits.title, body: developerCommits.about + "\n\n" + developerCommits.marker + "\n" + told([]string{developer}), state: "open"},
		&fakeIssue{number: 6, creator: bot, title: "A pull request", body: failures.marker + "\n" + told([]string{ciFailed.ID}), state: "open", pull: true},
		// Someone else's, which the list leaves out.
		&fakeIssue{number: 7, creator: "MarioRP-01", title: failures.title, body: failures.marker + "\n" + told([]string{ciFailed.ID}), state: "open"},
	)
	if err := f.reporter(t).ReportFailure(t.Context(), sandbox, ciFailed); err != nil {
		t.Fatal(err)
	}
	opened := f.opened()
	if len(opened) != 2 || opened[1].number != 8 || opened[1].title != failures.title || len(f.commentsOn(5)) != 0 {
		t.Errorf("issues %+v, want a failure issue opened, #8", opened)
	}
}

func TestWithToldKeepsTheRestOfTheBody(t *testing.T) {
	body := "What the Developer wrote.\n\n<!-- edity:release-failures -->\n<!-- edity:told a b -->\n\nMore of it."
	if got, want := withTold(body, []string{"a", "b", "c"}), "What the Developer wrote.\n\n<!-- edity:release-failures -->\n<!-- edity:told a b c -->\n\nMore of it."; got != want {
		t.Errorf("withTold = %q, want %q", got, want)
	}
	if got, want := withTold("The marker deleted.", []string{"a"}), "The marker deleted.\n<!-- edity:told a -->"; got != want {
		t.Errorf("withTold = %q, want %q", got, want)
	}
	var many []string
	for i := range maxTold + 5 {
		many = append(many, fmt.Sprintf("run:%d/1", i))
	}
	if kept := toldIn(told(many)); len(kept) != maxTold || kept[0] != "run:5/1" {
		t.Errorf("told keeps %d keys from %s, want the last %d", len(kept), kept[0], maxTold)
	}
}
