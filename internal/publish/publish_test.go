package publish

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/RMYPG/edity-server/internal/content"
	"github.com/RMYPG/edity-server/internal/fields"
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/site"
)

// fakeRepo is a Site repository in memory, whose main a test can move
// between a Publish's reads and its commit.
type fakeRepo struct {
	mu      sync.Mutex
	commits map[string]fakeCommit
	head    string
	// beforeCommit runs as CreateCommit starts, before it checks the
	// parent.
	beforeCommit func(r *fakeRepo)
	// answers, while there are any, replace what CreateCommit returns, one
	// per call: landed makes the commit first, and err is returned.
	answers []answer
	made    int
}

type answer struct {
	landed bool
	err    error
}

type fakeCommit struct {
	parent  string
	message string
	files   map[string]string
}

func newRepo(t *testing.T, files map[string]string) *fakeRepo {
	t.Helper()
	r := &fakeRepo{commits: map[string]fakeCommit{}}
	r.head = r.add("", "Initial commit", files)
	return r
}

// add makes a commit on main with files changed.
func (r *fakeRepo) add(parent, message string, changed map[string]string) string {
	files := map[string]string{}
	if parent != "" {
		maps.Copy(files, r.commits[parent].files)
	}
	maps.Copy(files, changed)
	sha := fmt.Sprintf("%040x", len(r.commits)+1)
	r.commits[sha] = fakeCommit{parent: parent, message: message, files: files}
	r.head = sha
	return sha
}

// push is a Developer's commit on main.
func (r *fakeRepo) push(changed map[string]string) {
	r.add(r.head, "A Developer's commit", changed)
}

func (r *fakeRepo) Head(_ context.Context, branch string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if branch != "main" {
		return "", github.ErrNotFound
	}
	return r.head, nil
}

func (r *fakeRepo) File(_ context.Context, path, ref string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.commits[ref].files[path]
	if !ok {
		return nil, github.ErrNotFound
	}
	return []byte(f), nil
}

func (r *fakeRepo) CreateCommit(_ context.Context, c github.NewCommit) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.beforeCommit != nil {
		r.beforeCommit(r)
	}
	commit := func() string {
		files := map[string]string{}
		for _, f := range c.Files {
			files[f.Path] = string(f.Contents)
		}
		r.made++
		return r.add(c.Parent, c.Message, files)
	}
	if len(r.answers) > 0 {
		a := r.answers[0]
		r.answers = r.answers[1:]
		if a.landed {
			commit()
		}
		if a.err != nil {
			return "", a.err
		}
	}
	if c.Branch != "main" || c.Parent != r.head {
		return "", github.ErrStale
	}
	return commit(), nil
}

func (r *fakeRepo) GitCommit(_ context.Context, sha string) (github.GitCommit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.commits[sha]
	if !ok {
		return github.GitCommit{}, github.ErrNotFound
	}
	return github.GitCommit{SHA: sha, Message: c.message, Parents: []string{c.parent}}, nil
}

// file is a file of main HEAD.
func (r *fakeRepo) file(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits[r.head].files[path]
}

var sandbox = site.Site{ID: "edity-sandbox", Repository: site.Repository{Owner: "RMYPG", Name: "edity-sandbox"}}

// sandboxFiles are edity-sandbox's home page and package.json as Migration
// left them.
func sandboxFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for path, name := range map[string]string{
		"src/content/home.json":        "testdata/home.json",
		"src/content/home.schema.json": "testdata/home.schema.json",
		"package.json":                 "testdata/package.json",
	} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(b)
	}
	return files
}

func publisherFor(repo *fakeRepo) *Publisher {
	p := New(func(site.Repository) Repository { return repo })
	p.wait = func(context.Context, int) error { return nil }
	return p
}

func value(t *testing.T, json string) *content.Value {
	t.Helper()
	v, err := content.ParseChange([]byte(json))
	if err != nil {
		t.Fatalf("ParseChange(%s): %v", json, err)
	}
	return v
}

func change(t *testing.T, path, before, after string) Change {
	t.Helper()
	return Change{Path: path, Before: value(t, before), After: value(t, after)}
}

var ana = Client{Name: "Ana García"}

func TestAPublishCommitsTheChangesAndAPatchBumpOnHead(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	head := repo.head
	draft := Draft{Page: "home", Changes: []Change{
		change(t, "hero.title", `"Fisioterapia cerca de ti, sin esperas"`, `"Fisioterapia en casa 中文"`),
		change(t, "plans.1.price", `49.5`, `52.00`),
		change(t, "services",
			`["Fisioterapia deportiva", "Suelo pélvico", "Punción seca", "Rehabilitación tras cirugía 🦴"]`,
			`["Punción seca", "Suelo pélvico"]`),
	}}

	published, err := publisherFor(repo).Publish(t.Context(), sandbox, draft, ana)
	if err != nil {
		t.Fatal(err)
	}
	if published.SHA != repo.head || published.Version != "1.0.1" || published.Conflicts != nil {
		t.Errorf("Publish = %+v, want main HEAD %s at 1.0.1, no conflicts", published, repo.head)
	}
	commit := repo.commits[repo.head]
	if commit.parent != head {
		t.Errorf("the commit's parent is %s, want the HEAD it read, %s", commit.parent, head)
	}
	wantMessage := "content: publish home\n\nChanged:\n- hero.title\n- plans.1.price\n- services\n\nEdity-Client: Ana García"
	if commit.message != wantMessage {
		t.Errorf("message:\n%s\nwant:\n%s", commit.message, wantMessage)
	}

	before := sandboxFiles(t)
	var changed []string
	for path, f := range commit.files {
		if f != repo.commits[head].files[path] {
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	if !slices.Equal(changed, []string{"package.json", "src/content/home.json"}) {
		t.Errorf("the commit changes %v, want the page's content and package.json alone", changed)
	}
	if got, want := commit.files["package.json"], strings.Replace(before["package.json"], `"version": "1.0.0"`, `"version": "1.0.1"`, 1); got != want {
		t.Errorf("package.json:\n%s\nwant only its version changed", got)
	}
	// The file's own key order, values no Change touched as they were, and
	// the Changes' values written as the Editor writes them.
	want := strings.NewReplacer(
		`"title": "Fisioterapia cerca de ti, sin esperas"`, `"title": "Fisioterapia en casa 中文"`,
		`"price": 49.5`, `"price": 52`,
		`"services": [
    "Fisioterapia deportiva",
    "Suelo pélvico",
    "Punción seca",
    "Rehabilitación tras cirugía 🦴"
  ]`, `"services": ["Punción seca", "Suelo pélvico"]`,
	).Replace(before["src/content/home.json"])
	if got := commit.files["src/content/home.json"]; got != want {
		t.Errorf("home.json:\n%s\nwant:\n%s", got, want)
	}
}

func TestAChangeWhoseStoredValueMovedIsAConflictAndTheRestMerge(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	draft := Draft{Page: "home", Changes: []Change{
		change(t, "hero.cta", `"Pide tu cita"`, `"Ven a vernos"`),
		change(t, "visit.price", `35`, `40`),
		// The Item is gone from main.
		change(t, "plans.2.name", `"Premium"`, `"Premium plus"`),
		// The List's Items are not the ones the Client reordered.
		change(t, "plans.0.features", `["Una sesión al mes"]`, `["Dos sesiones al mes"]`),
	}}
	published, err := publisherFor(repo).Publish(t.Context(), sandbox, draft, ana)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hero.cta", "plans.2.name", "plans.0.features"}; !slices.Equal(published.Conflicts, want) {
		t.Errorf("Conflicts = %v, want %v", published.Conflicts, want)
	}
	if f := repo.file("src/content/home.json"); !strings.Contains(f, `"price": 40`) || !strings.Contains(f, `"cta": "Reserva tu cita"`) {
		t.Errorf("home.json does not hold the merged Change alone:\n%s", f)
	}
	if msg := repo.commits[repo.head].message; strings.Contains(msg, "hero.cta") || !strings.Contains(msg, "- visit.price\n") {
		t.Errorf("the message lists more or less than what merged:\n%s", msg)
	}
}

func TestAPublishInWhichEveryChangeIsAConflictCommitsNothing(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	head := repo.head
	draft := Draft{Page: "home", Changes: []Change{
		change(t, "hero.cta", `"Pide tu cita"`, `"Ven a vernos"`),
		change(t, "stats.patients", `12000`, `13000`),
	}}
	_, err := publisherFor(repo).Publish(t.Context(), sandbox, draft, ana)
	var conflicts *ConflictsError
	if !errors.As(err, &conflicts) || !slices.Equal(conflicts.Paths, []string{"hero.cta", "stats.patients"}) {
		t.Errorf("Publish: %v, want both Conflicts", err)
	}
	if repo.head != head {
		t.Error("main moved")
	}
}

func TestAPublishThatBreaksALimitCommitsNothing(t *testing.T) {
	for name, c := range map[string]struct {
		changes []Change
		want    []fields.Violation
	}{
		"too long": {
			[]Change{change(t, "hero.cta", `"Reserva tu cita"`, `"`+strings.Repeat("a", 25)+`"`)},
			[]fields.Violation{{Path: "hero.cta", Limit: fields.MaxChars}},
		},
		"too many Items": {
			[]Change{change(t, "plans.0.features", `["Una sesión al mes", "Seguimiento por WhatsApp"]`, `["a", "b", "c", "d", "e", "f"]`)},
			[]fields.Violation{{Path: "plans.0.features", Limit: fields.MaxItems}},
		},
		"text for a number": {
			[]Change{change(t, "visit.price", `35`, `"35"`)},
			[]fields.Violation{{Path: "visit.price", Limit: fields.Type}},
		},
		"not a Field or a List": {
			[]Change{
				change(t, "hero", `{}`, `{}`),
				change(t, "plans.1", `{}`, `{}`),
				change(t, "brand.slogan", `"a"`, `"b"`),
				change(t, "visit.price", `35`, `36`),
			},
			[]fields.Violation{{Path: "hero", Limit: fields.Unknown}, {Path: "plans.1", Limit: fields.Unknown}, {Path: "brand.slogan", Limit: fields.Unknown}},
		},
		"both": {
			[]Change{change(t, "nope", `1`, `2`), change(t, "visit.price", `35`, `36.123`)},
			[]fields.Violation{{Path: "nope", Limit: fields.Unknown}, {Path: "visit.price", Limit: fields.Decimals}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t, sandboxFiles(t))
			head := repo.head
			_, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: c.changes}, ana)
			var violations *ViolationsError
			if !errors.As(err, &violations) || !slices.Equal(violations.Violations, c.want) {
				t.Errorf("Publish: %v, want violations %v", err, c.want)
			}
			if repo.head != head {
				t.Error("main moved")
			}
		})
	}
}

// N3 over L7: content that breaks the Field definitions at HEAD is never
// committed, though no Change touched it.
func TestContentNoChangeTouchedMustPassTheFieldDefinitionsToo(t *testing.T) {
	files := sandboxFiles(t)
	files["src/content/home.json"] = strings.Replace(files["src/content/home.json"], `"Llámanos"`, `"Llámanos al 600 000 000 o escríbenos"`, 1)
	repo := newRepo(t, files)
	_, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `36`)}}, ana)
	var violations *ViolationsError
	if !errors.As(err, &violations) || !slices.Equal(violations.Violations, []fields.Violation{{Path: "contact.call", Limit: fields.MaxChars}}) {
		t.Errorf("Publish: %v, want contact.call's maxChars", err)
	}
}

func TestAPageWithNoContentHasNoFields(t *testing.T) {
	for _, missing := range []string{"src/content/home.json", "src/content/home.schema.json"} {
		files := sandboxFiles(t)
		delete(files, missing)
		repo := newRepo(t, files)
		_, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `36`)}}, ana)
		var violations *ViolationsError
		if !errors.As(err, &violations) || !slices.Equal(violations.Violations, []fields.Violation{{Path: "visit.price", Limit: fields.Unknown}}) {
			t.Errorf("without %s: %v, want visit.price unknown", missing, err)
		}
	}
}

func TestARepositoryThatCantBePublishedAsItIs(t *testing.T) {
	for name, breakIt := range map[string]func(map[string]string){
		"content that is not JSON": func(f map[string]string) { f["src/content/home.json"] += "}" },
		"a key twice":              func(f map[string]string) { f["src/content/home.json"] = `{"brand": {}, "brand": {}}` },
		"Field definitions in another dialect": func(f map[string]string) {
			f["src/content/home.schema.json"] = `{"$schema": "https://json-schema.org/draft/2020-12/schema"}`
		},
		"no package.json":   func(f map[string]string) { delete(f, "package.json") },
		"a version 1.0":     func(f map[string]string) { f["package.json"] = `{"version": "1.0"}` },
		"no version":        func(f map[string]string) { f["package.json"] = `{"name": "site"}` },
		"a version as text": func(f map[string]string) { f["package.json"] = `{"version": 1}` },
	} {
		files := sandboxFiles(t)
		breakIt(files)
		repo := newRepo(t, files)
		_, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `36`)}}, ana)
		var broken *BrokenError
		if !errors.As(err, &broken) {
			t.Errorf("with %s: %v, want a BrokenError", name, err)
		}
	}
}

// The acceptance criterion: if main moves between read and commit, the
// commit is retried or rejected, never written on a stale base.
func TestWhenMainMovesThePublishStartsOverOnTheNewHead(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	// The Developer changes hero.cta between the Publish's reads and its
	// commit.
	moved := ""
	repo.beforeCommit = func(r *fakeRepo) {
		if moved == "" {
			files := r.commits[r.head].files
			r.push(map[string]string{"src/content/home.json": strings.Replace(files["src/content/home.json"], "Reserva tu cita", "Pide cita", 1)})
			moved = r.head
		}
	}
	draft := Draft{Page: "home", Changes: []Change{
		change(t, "hero.cta", `"Reserva tu cita"`, `"Ven a vernos"`),
		change(t, "visit.price", `35`, `40`),
	}}
	published, err := publisherFor(repo).Publish(t.Context(), sandbox, draft, ana)
	if err != nil {
		t.Fatal(err)
	}
	if repo.made != 1 || repo.commits[published.SHA].parent != moved {
		t.Errorf("made %d commits, the last on %s, want one on the moved HEAD %s", repo.made, repo.commits[published.SHA].parent, moved)
	}
	// Read again on the new HEAD, hero.cta is a Conflict now.
	if !slices.Equal(published.Conflicts, []string{"hero.cta"}) {
		t.Errorf("Conflicts = %v, want hero.cta", published.Conflicts)
	}
	if f := repo.file("src/content/home.json"); !strings.Contains(f, `"cta": "Pide cita"`) || !strings.Contains(f, `"price": 40`) {
		t.Errorf("home.json:\n%s", f)
	}
}

func TestAPublishGivesUpWhenMainKeepsMoving(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	repo.beforeCommit = func(r *fakeRepo) { r.push(map[string]string{"README.md": r.head}) }
	_, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `40`)}}, ana)
	if !errors.Is(err, ErrBusy) {
		t.Errorf("Publish: %v, want ErrBusy", err)
	}
	if repo.made != 0 {
		t.Errorf("made %d commits", repo.made)
	}
}

// GitHub sometimes answers STALE_DATA though main has not moved.
func TestAStaleAnswerOnAnUnmovedHeadIsRetried(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	head := repo.head
	repo.answers = []answer{{err: github.ErrStale}}
	published, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `40`)}}, ana)
	if err != nil || repo.made != 1 || repo.commits[published.SHA].parent != head {
		t.Errorf("Publish = %+v, %v, made %d, want one commit on %s", published, err, repo.made, head)
	}
}

func TestACommitGitHubMadeWithoutTellingIsFoundAndNotMadeAgain(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	repo.answers = []answer{{landed: true, err: github.ErrUncertain}}
	published, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `40`)}}, ana)
	if err != nil {
		t.Fatal(err)
	}
	if repo.made != 1 || published.SHA != repo.head || published.Version != "1.0.1" {
		t.Errorf("Publish = %+v, made %d, want the one commit at HEAD %s", published, repo.made, repo.head)
	}
}

func TestACommitGitHubDidNotMakeIsMadeOnce(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	repo.answers = []answer{{err: github.ErrUncertain}}
	published, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `40`)}}, ana)
	if err != nil || repo.made != 1 || published.SHA != repo.head {
		t.Errorf("Publish = %+v, %v, made %d", published, err, repo.made)
	}
}

func TestAnErrorFromGitHubCommitsNothing(t *testing.T) {
	repo := newRepo(t, sandboxFiles(t))
	failed := &github.Error{Call: "createCommitOnBranch", Status: 200, Message: "Resource not accessible by integration"}
	repo.answers = []answer{{err: failed}}
	_, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `40`)}}, ana)
	if !errors.Is(err, failed) || repo.made != 0 {
		t.Errorf("Publish: %v, made %d, want GitHub's error and no commit", err, repo.made)
	}
}

func TestDraftCheck(t *testing.T) {
	ok := change(t, "hero.title", `"a"`, `"b"`)
	for name, d := range map[string]Draft{
		"a page out of src/content": {Page: "../home", Changes: []Change{ok}},
		"a page with a dot":         {Page: "home.schema", Changes: []Change{ok}},
		"no page":                   {Changes: []Change{ok}},
		"no Changes":                {Page: "home"},
		"an empty key":              {Page: "home", Changes: []Change{change(t, "hero..title", `"a"`, `"b"`)}},
		"no path":                   {Page: "home", Changes: []Change{change(t, "", `"a"`, `"b"`)}},
		"one path twice":            {Page: "home", Changes: []Change{ok, ok}},
		"one inside another":        {Page: "home", Changes: []Change{change(t, "plans.1.features.0", `"a"`, `"b"`), change(t, "plans", `[]`, `[]`)}},
		"no value after":            {Page: "home", Changes: []Change{{Path: "hero.title", Before: value(t, `"a"`)}}},
	} {
		if err := d.Check(); err == nil {
			t.Errorf("%s: Check passed", name)
		}
	}
	d := Draft{Page: "home_2-b", Changes: []Change{ok, change(t, "plans.1.features", `[]`, `[]`), change(t, "plans.10.price", `1`, `2`), change(t, "plan", `[]`, `[]`)}}
	if err := d.Check(); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func TestTheTrailerIsOnOneLine(t *testing.T) {
	for name, want := range map[string]string{
		"Ana García":               "Ana García",
		"  Ana\n\nEdity-Client: x": "Ana Edity-Client: x",
		"Ana García\t":             "Ana García",
		"\n \r":                    "",
	} {
		if got := trailerValue(name); got != want {
			t.Errorf("trailerValue(%q) = %q, want %q", name, got, want)
		}
	}
	repo := newRepo(t, sandboxFiles(t))
	if _, err := publisherFor(repo).Publish(t.Context(), sandbox, Draft{Page: "home", Changes: []Change{change(t, "visit.price", `35`, `40`)}}, Client{Name: "\n"}); err == nil {
		t.Error("published for a Client with no name")
	}
}

func TestParseMessageReadsWhatAPublishWrote(t *testing.T) {
	m := message("home", []string{"hero.title", "plans.1.price", "services"}, "Ana García")
	got, ok := ParseMessage(m)
	if want := (Message{Page: "home", Changed: []string{"hero.title", "plans.1.price", "services"}, Client: "Ana García"}); !ok ||
		got.Page != want.Page || !slices.Equal(got.Changed, want.Changed) || got.Client != want.Client {
		t.Errorf("ParseMessage(%q) = %+v, %v, want %+v", m, got, ok, want)
	}
	// As GitHub has the sandbox's first Publish, with a newline at the end.
	if got, ok := ParseMessage("content: publish home\n\nChanged:\n- hero.cta\n\nEdity-Client: Edity end-to-end check\n"); !ok ||
		got.Page != "home" || !slices.Equal(got.Changed, []string{"hero.cta"}) || got.Client != "Edity end-to-end check" {
		t.Errorf("ParseMessage of the sandbox's Publish = %+v, %v", got, ok)
	}
	for _, other := range []string{
		"Take @edity/core and @edity/astro 0.1.0 from npm (#3)\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>",
		"content: publish home",
		"content: publish ../home\n\nChanged:\n- a\n\nEdity-Client: Ana",
		"content: publish home\n\nChanged:\n\nEdity-Client: Ana",
		"content: publish home\n\nChanged:\n- a\n\nEdity-Client: ",
	} {
		if got, ok := ParseMessage(other); ok {
			t.Errorf("ParseMessage(%q) = %+v, want false", other, got)
		}
	}
}

func TestBumpPatchChangesTheVersionAlone(t *testing.T) {
	pkg := "{\n  \"name\": \"site\",\n  \"engines\": {\"version\": \"9.9.9\"},\n  \"version\":   \"1.4.9\" ,\n  \"private\": true\n}\n"
	version, bumped, err := bumpPatch([]byte(pkg))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(pkg, `"1.4.9"`, `"1.4.10"`, 1); version != "1.4.10" || string(bumped) != want {
		t.Errorf("bumpPatch = %s,\n%s\nwant 1.4.10,\n%s", version, bumped, want)
	}
	for _, broken := range []string{
		`{"version": "1.4.9", "version": "1.5.0"}`,
		`{"version": "01.4.9"}`,
		`{"version": "1.4.9-beta"}`,
		`{"version": "1.4.9"} {}`,
		`{"version": "1.4.9", "name": }`,
		`["1.4.9"]`,
		`{"version": "1.4.18446744073709551615"}`,
	} {
		if _, _, err := bumpPatch([]byte(broken)); err == nil {
			t.Errorf("bumpPatch(%s) passed", broken)
		}
	}
}
