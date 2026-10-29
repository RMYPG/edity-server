package issues

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/RMYPG/edity-server/internal/githubtest"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/release"
)

// firstPublish is the sandbox's first Publish of 2026-10-29.
var firstPublish = publish.Published{SHA: first, Version: release.Version{1, 0, 1}}

// shipsDeveloperCommits makes the last Release before the first Publish
// v1.0.0 as if it had been cut before the Developer's last two commits, so
// that the Publish ships them.
func shipsDeveloperCommits(t *testing.T, f *fakeGitHub, sha string) {
	t.Helper()
	f.answers["compare/v1.0.0..."+sha] = githubtest.Recorded(t, "compare-0345f67-4bc9310.http")
}

// theirFiles are the files both Developer commits change.
const theirFiles = "`Dockerfile`, `README.md`, `package.json` beyond its version, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, " +
	"`vendor/README.md`, `vendor/edity-astro-0.1.0.tgz`, `vendor/edity-core-0.1.0.tgz`"

func TestAPublishThatShipsDeveloperCommitsNotesThemOnce(t *testing.T) {
	f := newFake(t)
	shipsDeveloperCommits(t, f, first)
	if err := f.reporter(t).ReportDeveloperCommits(t.Context(), sandbox, firstPublish); err != nil {
		t.Fatal(err)
	}
	opened := f.opened()
	if len(opened) != 1 || opened[0].title != "Edity: a Publish shipped commits that are not content" {
		t.Fatalf("issues %+v, want one of Developer commits", opened)
	}
	report := "The Publish of v1.0.1, " + first + ", ships these commits:\n\n" +
		"- " + developer + " build: take \\@edity/core and \\@edity/astro 0.1.0 from npm, by Mario Rojas Padron, changes " + theirFiles + "\n" +
		"- " + merge + " Take \\@edity/core and \\@edity/astro 0.1.0 from npm (#3), by Mario Rojas Padrón, changes " + theirFiles
	if !strings.Contains(opened[0].body, report) || !strings.Contains(opened[0].body, "<!-- edity:developer-commits -->") {
		t.Errorf("the issue's body:\n%s\nwant it to report:\n%s", opened[0].body, report)
	}
	if told := toldOn(t, opened[0]); told != developer+" "+merge {
		t.Errorf("the issue lists %q as told, want both commits", told)
	}

	// Until the next Release, every Publish ships them, and none notes them
	// again, nor reads them again.
	second := publish.Published{SHA: second, Version: release.Version{1, 0, 2}}
	shipsDeveloperCommits(t, f, second.SHA)
	r := f.reporter(t)
	for range 2 {
		calls, _, writes := f.counts()
		if err := r.ReportDeveloperCommits(t.Context(), sandbox, second); err != nil {
			t.Fatal(err)
		}
		// The tags, the comparison and the issue list, or the issue list
		// only the first time.
		if after, _, afterWrites := f.counts(); after-calls > 3 || afterWrites != writes {
			t.Errorf("noting them again made %d calls and %d writes, want at most 3, and none", after-calls, afterWrites-writes)
		}
	}
	if len(f.opened()) != 1 || len(f.commentsOn(opened[0].number)) != 0 {
		t.Errorf("issues %+v, with comments %q, want the one alone", f.opened(), f.commentsOn(opened[0].number))
	}

	// Closed, what it reported is not reported again.
	f.close(opened[0].number)
	if err := f.reporter(t).ReportDeveloperCommits(t.Context(), sandbox, second); err != nil || len(f.opened()) != 1 {
		t.Errorf("noting them once the issue is closed: %v, issues %+v, want no new one", err, f.opened())
	}
}

func TestAPublishOfContentAloneNotesNothing(t *testing.T) {
	f := newFake(t)
	// As recorded: the Publish is the only commit since v1.0.0.
	if err := f.reporter(t).ReportDeveloperCommits(t.Context(), sandbox, firstPublish); err != nil {
		t.Fatal(err)
	}
	// The tags and the comparison.
	if calls, _, writes := f.counts(); calls != 2 || writes != 0 || len(f.opened()) != 0 {
		t.Errorf("%d calls, %d writes, issues %+v, want 2 calls and nothing written", calls, writes, f.opened())
	}

	// The Developer's own commits of content alone are content too.
	f = newFake(t)
	shipsDeveloperCommits(t, f, first)
	contentOnly := func(body map[string]any) {
		body["files"] = []any{
			map[string]any{"filename": "src/content/home.json", "status": "modified", "patch": "@@ -1 +1 @@\n-{}\n+{\"a\": 1}"},
			map[string]any{"filename": "package.json", "status": "modified", "patch": "@@ -2,7 +2,7 @@\n   \"private\": true,\n-  \"version\": \"1.0.0\",\n+  \"version\": \"1.0.2\",\n   \"engines\": {"},
		}
	}
	for _, sha := range []string{developer, merge} {
		f.answers["commits/"+sha] = githubtest.Edited(t, f.answers["commits/"+sha], contentOnly)
	}
	if err := f.reporter(t).ReportDeveloperCommits(t.Context(), sandbox, firstPublish); err != nil || len(f.opened()) != 0 {
		t.Errorf("Developer commits of content: %v, issues %+v, want none", err, f.opened())
	}
	f.answers["compare/v1.0.0..."+first] = githubtest.Edited(t, f.answers["compare/v1.0.0..."+first], contentOnly)
	calls, _, _ := f.counts()
	if err := f.reporter(t).ReportDeveloperCommits(t.Context(), sandbox, firstPublish); err != nil || len(f.opened()) != 0 {
		t.Errorf("Developer commits of content: %v, issues %+v, want none", err, f.opened())
	}
	if after, _, _ := f.counts(); after-calls != 2 {
		t.Errorf("with every file content, noting made %d calls, want the tags and the comparison", after-calls)
	}
}

func TestASiteWithNoReleaseHasNothingToNote(t *testing.T) {
	f := newFake(t)
	delete(f.answers, "git/matching-refs/tags/v")
	if err := f.reporter(t).ReportDeveloperCommits(t.Context(), sandbox, firstPublish); err != nil || len(f.opened()) != 0 {
		t.Errorf("with no Release: %v, issues %+v, want nothing", err, f.opened())
	}
}

func TestWhatIsNotContent(t *testing.T) {
	versionPatch := "@@ -2,7 +2,7 @@\n   \"type\": \"module\",\n-  \"version\": \"1.0.0\",\n+  \"version\": \"1.0.1\",\n   \"engines\": {"
	for name, c := range map[string]struct {
		files []file
		want  []string
	}{
		"a content file": {[]file{{Filename: "src/content/home.json", Status: "modified"}}, nil},
		"a new page's":   {[]file{{Filename: "src/content/prices.schema.json", Status: "added"}}, nil},
		"the version":    {[]file{{Filename: "package.json", Status: "modified", Patch: versionPatch}}, nil},
		"the version, with no newline at the end": {
			[]file{{Filename: "package.json", Status: "modified", Patch: "@@ -2 +2 @@\n-  \"version\": \"1.0.0\"\n\\ No newline at end of file\n+  \"version\": \"1.0.1\"\n\\ No newline at end of file"}},
			nil,
		},
		"the version on one line with the rest": {
			[]file{{Filename: "package.json", Status: "modified", Patch: "@@ -1 +1 @@\n-{\"version\": \"1.0.0\"}\n+{\"version\": \"1.0.1\"}"}},
			[]string{"`package.json` beyond its version"},
		},
		"more than the version": {
			[]file{{Filename: "package.json", Status: "modified", Patch: versionPatch + "\n-    \"astro\": \"7.2.0\"\n+    \"astro\": \"7.3.0\""}},
			[]string{"`package.json` beyond its version"},
		},
		"package.json too large to diff": {[]file{{Filename: "package.json", Status: "modified"}}, []string{"`package.json` beyond its version"}},
		"package.json removed":           {[]file{{Filename: "package.json", Status: "removed", Patch: "@@ -1 +0,0 @@\n-  \"version\": \"1.0.0\""}}, []string{"`package.json` beyond its version"}},
		"another package.json":           {[]file{{Filename: "packages/x/package.json", Status: "modified", Patch: versionPatch}}, []string{"`packages/x/package.json`"}},
		"code":                           {[]file{{Filename: "src/pages/index.astro", Status: "modified"}, {Filename: ".github/workflows/ci.yml", Status: "modified"}}, []string{"`src/pages/index.astro`", "`.github/workflows/ci.yml`"}},
		"a file moved into the content":  {[]file{{Filename: "src/content/home.json", Previous: "src/data/home.json", Status: "renamed"}}, []string{"`src/data/home.json` to `src/content/home.json`"}},
		"a file moved out of it":         {[]file{{Filename: "src/data/home.json", Previous: "src/content/home.json", Status: "renamed"}}, []string{"`src/content/home.json` to `src/data/home.json`"}},
		"src/contents":                   {[]file{{Filename: "src/contents/home.json", Status: "added"}}, []string{"`src/contents/home.json`"}},
	} {
		if got := notContent(c.files); !slices.Equal(got, c.want) {
			t.Errorf("%s: notContent = %q, want %q", name, got, c.want)
		}
	}
}

func TestANoteNamesTenFilesOfACommit(t *testing.T) {
	var files []string
	for i := range 12 {
		files = append(files, fmt.Sprintf("`f%d`", i))
	}
	c := commit{SHA: developer}
	c.Commit.Message = "fix: *the* page\n\nWhy."
	c.Commit.Author.Name = "Mario"
	want := "- " + developer + " fix: \\*the\\* page, by Mario, changes `f0`, `f1`, `f2`, `f3`, `f4`, `f5`, `f6`, `f7`, `f8`, `f9` and 2 more"
	if got := commitLine(c, files); got != want {
		t.Errorf("commitLine = %q, want %q", got, want)
	}
}
