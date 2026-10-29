// Package publish makes a Publish: it reads a page's content and Field
// definitions from the Site repository at main HEAD, finds the Changes that
// are Conflicts, merges the others, checks the content they make against
// the Field definitions, and commits it on top of that HEAD with a patch
// bump of package.json, which a workflow in the Site repository then cuts
// as a Release (ADR-0003).
//
// A Publish writes only the page's content file and package.json (V4), and
// nothing it commits breaks the Field definitions at the commit it goes on
// (N3).
package publish

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/RMYPG/edity-server/internal/content"
	"github.com/RMYPG/edity-server/internal/fields"
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/release"
	"github.com/RMYPG/edity-server/internal/site"
)

// branch is the branch every Publish commits on (ADR-0003).
const branch = "main"

// attempts is how many times a Publish reads main and commits on it before
// it gives up, when main moves in between each time.
const attempts = 3

// A Repository is a Site repository, as the publisher App reads and writes
// it. *github.Repo is one.
type Repository interface {
	Head(ctx context.Context, branch string) (string, error)
	// File returns github.ErrNotFound when there is no file at path.
	File(ctx context.Context, path, ref string) ([]byte, error)
	// CreateCommit returns github.ErrStale or github.ErrUncertain as
	// *github.Repo does.
	CreateCommit(ctx context.Context, c github.NewCommit) (string, error)
	GitCommit(ctx context.Context, sha string) (github.GitCommit, error)
}

// A Publisher makes Publishes. It is safe for concurrent use.
type Publisher struct {
	repository func(site.Repository) Repository
	// wait pauses before a Publish reads main again.
	wait func(ctx context.Context, attempt int) error
}

// New returns a Publisher that reaches a Site's repository through
// repository.
func New(repository func(site.Repository) Repository) *Publisher {
	return &Publisher{repository: repository, wait: backOff}
}

// backOff waits a little longer before each new attempt.
func backOff(ctx context.Context, attempt int) error {
	t := time.NewTimer(time.Duration(attempt) * 500 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// A Client is the person who publishes, as their session has them.
type Client struct {
	// Name is the Client's name in Zitadel, which the commit's Edity-Client
	// trailer carries.
	Name string
}

// A Draft is the Changes a Client publishes on one page.
type Draft struct {
	// Page is the page's name, its content collection's.
	Page    string
	Changes []Change
}

// A Change is an edit to the Field or List at Path, from Before to After.
type Change struct {
	Path   string
	Before *content.Value
	After  *content.Value
}

// pageName is the OpenAPI file's pattern for a page.
var pageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Check tells why d is not a Draft whatever the Site's content: a page whose
// name could lead out of src/content/, no Changes, a path that is not a
// content path, or two Changes for one Field or List, or one inside the
// other's, which the Editor never sends.
func (d Draft) Check() error {
	if !pageName.MatchString(d.Page) {
		return fmt.Errorf("%q is not a page name", d.Page)
	}
	if len(d.Changes) == 0 {
		return errors.New("no Changes")
	}
	paths := map[string]bool{}
	for _, c := range d.Changes {
		if c.Before == nil || c.After == nil {
			return fmt.Errorf("the Change to %s has no value before or after", c.Path)
		}
		keys := strings.Split(c.Path, ".")
		for _, key := range keys {
			if key == "" {
				return fmt.Errorf("%q is not a content path", c.Path)
			}
		}
		if paths[c.Path] {
			return fmt.Errorf("two Changes to %s", c.Path)
		}
		paths[c.Path] = true
	}
	for path := range paths {
		keys := strings.Split(path, ".")
		for n := 1; n < len(keys); n++ {
			if outer := strings.Join(keys[:n], "."); paths[outer] {
				return fmt.Errorf("a Change to %s inside the Change to %s", path, outer)
			}
		}
	}
	return nil
}

// Published is a Publish that was committed.
type Published struct {
	SHA string
	// Version is the version of the Release the commit becomes.
	Version release.Version
	// Conflicts are the paths of the Changes that did not merge.
	Conflicts []string
}

// A ConflictsError is a Publish in which every Change is a Conflict, so
// nothing was committed.
type ConflictsError struct {
	Paths []string
}

func (e *ConflictsError) Error() string {
	return "every Change is a Conflict: " + strings.Join(e.Paths, ", ")
}

// A ViolationsError is a Publish whose Changes, or the content they would
// make, break the page's Field definitions, so nothing was committed.
type ViolationsError struct {
	Violations []fields.Violation
}

func (e *ViolationsError) Error() string {
	return fmt.Sprintf("the Changes break the Field definitions: %v", e.Violations)
}

// A BrokenError is a Site repository whose main can't be published as it
// is: its content or Field definitions at main HEAD can't be read, or its
// package.json has no version to bump. The Developer has to fix it.
type BrokenError struct {
	Reason error
}

func (e *BrokenError) Error() string {
	return "the Site repository can't be published as it is: " + e.Reason.Error()
}

func (e *BrokenError) Unwrap() error { return e.Reason }

// ErrBusy is a Publish that main kept moving under.
var ErrBusy = errors.New("main moved each time the Publish went to commit")

// Publish publishes d on s for client. It returns a *ConflictsError, a
// *ViolationsError or a *BrokenError when it commits nothing for those
// reasons. When main moves between reading it and committing, it reads main
// again and starts over, and gives up with ErrBusy after a few attempts:
// it never commits on a HEAD it did not read.
func (p *Publisher) Publish(ctx context.Context, s site.Site, d Draft, client Client) (Published, error) {
	if err := d.Check(); err != nil {
		return Published{}, err
	}
	trailer := trailerValue(client.Name)
	if trailer == "" {
		return Published{}, errors.New("a Client with no name")
	}
	repo := p.repository(s.Repository)
	// unsure is a commit GitHub may have made without telling.
	var unsure *plan
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := p.wait(ctx, attempt); err != nil {
				return Published{}, err
			}
		}
		head, err := repo.Head(ctx, branch)
		if err != nil {
			return Published{}, err
		}
		if unsure != nil {
			if published, ok := landed(ctx, repo, head, unsure); ok {
				return published, nil
			}
		}
		pl, err := prepare(ctx, repo, head, d, trailer)
		if err != nil {
			return Published{}, err
		}
		sha, err := repo.CreateCommit(ctx, pl.commit)
		switch {
		case err == nil:
			return pl.published(sha), nil
		case errors.Is(err, github.ErrStale):
			// Main moved, or GitHub has yet to see it where Head read it.
		case errors.Is(err, github.ErrUncertain):
			unsure = pl
		default:
			return Published{}, err
		}
	}
	if unsure != nil {
		if head, err := repo.Head(ctx, branch); err == nil {
			if published, ok := landed(ctx, repo, head, unsure); ok {
				return published, nil
			}
		}
		return Published{}, fmt.Errorf("%w: GitHub did not tell whether it made the last commit", ErrBusy)
	}
	return Published{}, ErrBusy
}

// landed tells whether head is the commit of pl, which GitHub made without
// telling.
func landed(ctx context.Context, repo Repository, head string, pl *plan) (Published, bool) {
	if head == pl.commit.Parent {
		return Published{}, false
	}
	c, err := repo.GitCommit(ctx, head)
	if err != nil || len(c.Parents) != 1 || c.Parents[0] != pl.commit.Parent ||
		strings.TrimSpace(c.Message) != strings.TrimSpace(pl.commit.Message) {
		return Published{}, false
	}
	return pl.published(head), true
}

// A plan is the commit a Publish makes on one HEAD.
type plan struct {
	commit    github.NewCommit
	version   release.Version
	conflicts []string
}

func (pl *plan) published(sha string) Published {
	return Published{SHA: sha, Version: pl.version, Conflicts: pl.conflicts}
}

// prepare reads the page at head and works out the commit d makes on it.
func prepare(ctx context.Context, repo Repository, head string, d Draft, trailer string) (*plan, error) {
	contentPath := "src/content/" + d.Page + ".json"
	file, err := readFile(ctx, repo, contentPath, head)
	if err != nil {
		return nil, err
	}
	schema, err := readFile(ctx, repo, "src/content/"+d.Page+".schema.json", head)
	if err != nil {
		return nil, err
	}
	if file == nil || schema == nil {
		// No such page, so none of its Fields or Lists.
		var unknown []fields.Violation
		for _, c := range d.Changes {
			unknown = append(unknown, fields.Violation{Path: c.Path, Limit: fields.Unknown})
		}
		return nil, &ViolationsError{Violations: unknown}
	}
	defs, err := fields.Compile(schema)
	if err != nil {
		return nil, &BrokenError{fmt.Errorf("%s at %s: %w", contentPath, head, err)}
	}
	page, err := content.Parse(file)
	if err != nil {
		return nil, &BrokenError{fmt.Errorf("%s at %s: %w", contentPath, head, err)}
	}
	pkg, err := readFile(ctx, repo, "package.json", head)
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, &BrokenError{fmt.Errorf("no package.json at %s", head)}
	}
	version, bumped, err := bumpPatch(pkg)
	if err != nil {
		return nil, &BrokenError{fmt.Errorf("package.json at %s: %w", head, err)}
	}

	var violations []fields.Violation
	var conflicts, merged []string
	for _, c := range d.Changes {
		if defs.KindOf(c.Path) == fields.NotEditable {
			violations = append(violations, fields.Violation{Path: c.Path, Limit: fields.Unknown})
			continue
		}
		// A Change whose Field, List or Item is gone is one too.
		if stored, ok := page.At(c.Path); !ok || !content.Equal(stored, c.Before) {
			conflicts = append(conflicts, c.Path)
			continue
		}
		page.Replace(c.Path, c.After)
		merged = append(merged, c.Path)
	}
	if len(merged) == 0 && len(violations) == 0 {
		return nil, &ConflictsError{Paths: conflicts}
	}
	if len(merged) > 0 {
		// The whole content, which no Field definitions at head may refuse,
		// changed or not (N3).
		violations = append(violations, defs.Violations(page.Decoded())...)
	}
	for _, v := range violations {
		if v.Path == "" {
			return nil, &BrokenError{fmt.Errorf("%s at %s is not the page's content", contentPath, head)}
		}
	}
	if len(violations) > 0 {
		return nil, &ViolationsError{Violations: violations}
	}

	return &plan{
		commit: github.NewCommit{
			Branch:  branch,
			Parent:  head,
			Message: message(d.Page, merged, trailer),
			Files: []github.File{
				{Path: contentPath, Contents: page.Format()},
				{Path: "package.json", Contents: bumped},
			},
		},
		version:   version,
		conflicts: conflicts,
	}, nil
}

// readFile returns nil for a file that is not there.
func readFile(ctx context.Context, repo Repository, path, ref string) ([]byte, error) {
	b, err := repo.File(ctx, path, ref)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if b == nil && err == nil {
		b = []byte{}
	}
	return b, err
}

// message is the commit message: the paths of the Changes that merged, and
// the Edity-Client trailer last, which Git reads as a trailer.
func message(page string, paths []string, client string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "content: publish %s\n\nChanged:\n", page)
	for _, path := range paths {
		fmt.Fprintf(&b, "- %s\n", path)
	}
	fmt.Fprintf(&b, "\nEdity-Client: %s", client)
	return b.String()
}

// A Message is what the commit message of a Publish says.
type Message struct {
	Page string
	// Changed are the paths of the Changes that merged.
	Changed []string
	// Client is the name in the Edity-Client trailer.
	Client string
}

// ParseMessage reads the commit message of a Publish, as Publish writes it.
// It returns false for any other message.
func ParseMessage(m string) (Message, bool) {
	headline, rest, _ := strings.Cut(m, "\n\n")
	page, ok := strings.CutPrefix(headline, "content: publish ")
	if !ok || !pageName.MatchString(page) {
		return Message{}, false
	}
	i := strings.LastIndex(rest, "\n\nEdity-Client: ")
	if i < 0 {
		return Message{}, false
	}
	list, ok := strings.CutPrefix(rest[:i], "Changed:\n")
	client := strings.TrimRight(rest[i+len("\n\nEdity-Client: "):], "\n")
	if !ok || client == "" || strings.Contains(client, "\n") {
		return Message{}, false
	}
	msg := Message{Page: page, Client: client}
	for line := range strings.SplitSeq(list, "\n") {
		path, ok := strings.CutPrefix(line, "- ")
		if !ok || path == "" {
			return Message{}, false
		}
		msg.Changed = append(msg.Changed, path)
	}
	return msg, true
}

// trailerValue is name on one line, which a trailer's value is.
func trailerValue(name string) string {
	return strings.Join(strings.FieldsFunc(name, func(r rune) bool {
		return r < 0x20 || r == 0x7F || r == ' ' || r == '\u0085' || r == ' ' || r == ' '
	}), " ")
}
