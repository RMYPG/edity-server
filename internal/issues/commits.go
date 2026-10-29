package issues

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/site"
)

// developerCommits is the issue that reports to the Developer the commits
// on main that are not content, and that a Publish ships (V7).
var developerCommits = topic{
	marker: "<!-- edity:developer-commits -->",
	title:  "Edity: a Publish shipped commits that are not content",
	about: "A Client's Publish releases everything on `main`, and these commits on `main` change more than content: " +
		"files outside `src/content/`, or more of `package.json` than its version. " +
		"They go live with the Client's changes, so check that they were meant to.\n\n" +
		"Each Publish that ships more of them adds a comment here while this issue is open. " +
		"Close it once you have checked them: the next ones open a new issue.",
}

const (
	// maxCommits bounds the commits one Publish reads alone, since telling
	// a failure of the Site waits until they are read. Any after them are
	// read on the next Publish.
	maxCommits = 10
	// maxFiles is the most files GitHub lists for a comparison.
	maxFiles = 300
	// shownFiles is how many files a report names for a commit.
	shownFiles = 10
)

// A comparison is GitHub's comparison of two commits: the commits the head
// has that the base has not, oldest first, and the files they change
// together.
type comparison struct {
	Commits []struct {
		SHA string `json:"sha"`
		// Author is the GitHub user who authored the commit, or nil when
		// its email is no user's.
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	} `json:"commits"`
	Files []file `json:"files"`
}

// A commit is one commit as GitHub's REST API has it, with its files.
type commit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
	Files []file `json:"files"`
}

// A file is a file a commit changes.
type file struct {
	Filename string `json:"filename"`
	// Previous is the file's path before, when the commit renamed it.
	Previous string `json:"previous_filename"`
	Status   string `json:"status"`
	// Patch is the change, left out for a binary file or one too large.
	Patch string `json:"patch"`
}

// ReportDeveloperCommits reports on an issue the commits that the Publish p
// ships and that change more than content (V7): those on main since the
// last Release that the App did not author, which change files outside
// src/content/, or more of package.json than its version. Until the next
// Release every Publish ships them, so each is reported once.
func (r *Reporter) ReportDeveloperCommits(ctx context.Context, s site.Site, p publish.Published) error {
	repo := r.repository(s.Repository)
	bot, err := r.bot(ctx)
	if err != nil {
		return err
	}
	last, err := progress.LastRelease(ctx, repo)
	if err != nil || last == "" {
		// With no Release yet, there is nothing to compare with.
		return err
	}
	var since comparison
	if err := repo.Get(ctx, "compare/"+last+"..."+p.SHA, &since); err != nil {
		return err
	}
	if len(since.Files) < maxFiles && len(notContent(since.Files)) == 0 {
		// None of the commits changes more than content.
		return nil
	}
	var theirs []string
	for _, c := range since.Commits {
		if c.Author == nil || c.Author.Login != bot {
			theirs = append(theirs, c.SHA)
		}
	}
	return r.report(ctx, s, developerCommits, theirs, func(untold []string) (writeup, error) {
		// The comparison's files are those of every commit together, so each
		// commit is read alone.
		untold = untold[:min(len(untold), maxCommits)]
		var lines []string
		for _, sha := range untold {
			var c commit
			if err := repo.Get(ctx, "commits/"+sha, &c); err != nil {
				return writeup{}, err
			}
			if files := notContent(c.Files); len(files) > 0 {
				lines = append(lines, commitLine(c, files))
			}
		}
		if len(lines) == 0 {
			return writeup{keys: untold}, nil
		}
		text := fmt.Sprintf("The Publish of v%s, %s, ships these commits:\n\n%s", p.Version, p.SHA, strings.Join(lines, "\n"))
		return writeup{text: text, keys: untold}, nil
	})
}

// commitLine is the line that reports c, which changes files.
func commitLine(c commit, files []string) string {
	headline, _, _ := strings.Cut(c.Commit.Message, "\n")
	shown := files[:min(len(files), shownFiles)]
	more := ""
	if len(files) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(files)-len(shown))
	}
	return fmt.Sprintf("- %s %s, by %s, changes %s%s", c.SHA, escape(headline), escape(c.Commit.Author.Name), strings.Join(shown, ", "), more)
}

// notContent names the files that are not content, as Markdown: files
// outside src/content/, and package.json when more than its version
// changed.
func notContent(files []file) []string {
	var names []string
	for _, f := range files {
		inContent := strings.HasPrefix(f.Filename, "src/content/") && (f.Previous == "" || strings.HasPrefix(f.Previous, "src/content/"))
		switch {
		case inContent:
		case f.Filename == "package.json" && f.Previous == "" && f.Status == "modified" && versionOnly(f.Patch):
		case f.Filename == "package.json" && f.Previous == "":
			names = append(names, code(f.Filename)+" beyond its version")
		case f.Previous != "":
			names = append(names, code(f.Previous)+" to "+code(f.Filename))
		default:
			names = append(names, code(f.Filename))
		}
	}
	return names
}

// versionLine is the line of a package.json that holds its version.
var versionLine = regexp.MustCompile(`^\s*"version"\s*:\s*"[^"]*"\s*,?\s*$`)

// versionOnly tells whether patch, a unified diff of package.json, changes
// its version's line alone.
func versionOnly(patch string) bool {
	changed := false
	for line := range strings.SplitSeq(patch, "\n") {
		if line == "" || (line[0] != '+' && line[0] != '-') {
			continue
		}
		if !versionLine.MatchString(line[1:]) {
			return false
		}
		changed = true
	}
	return changed
}
