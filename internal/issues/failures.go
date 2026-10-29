package issues

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/RMYPG/edity-server/internal/progress"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/site"
)

// failures is the issue that tells the Developer of the Publishes whose
// Release failed (V6).
var failures = topic{
	marker: "<!-- edity:release-failures -->",
	title:  "Edity: a Publish was not released",
	about: "A Client published changes, which are saved on `main`, but their Release failed, so they are not live yet. " +
		"The Client was told so, and that you have been notified.\n\n" +
		"Each failure is a comment here while this issue is open. Close it once they are fixed: the next failure opens a new issue.",
}

// ReportFailure tells the Developer that a Publish failed, unless one of
// the App's issues told it already: it comments on the Site's open failure
// issue, or opens one (V6). A failure is told once, by its ID: polled again
// it tells nothing more, and the same run failing again on another attempt
// is another failure.
func (r *Reporter) ReportFailure(ctx context.Context, s site.Site, f progress.Failure) error {
	return r.tell(ctx, s, failures, []string{f.ID}, func([]string) (writeup, error) {
		return writeup{text: failureText(f), keys: []string{f.ID}}, nil
	})
}

// failureText says what failed, for which Publish, and where to see the
// run.
func failureText(f progress.Failure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### v%s was not released\n\n%s.", f.Version, upperFirst(f.Reason))
	if f.Run != "" {
		fmt.Fprintf(&b, " [See the run](%s).", f.Run)
	}
	m, ok := publish.ParseMessage(f.Message)
	if !ok {
		fmt.Fprintf(&b, "\n\nThe Publish is %s.", f.SHA)
		return b.String()
	}
	fmt.Fprintf(&b, "\n\n%s published %s in %s, which changed:\n", escape(m.Client), code(m.Page), f.SHA)
	for _, path := range m.Changed {
		fmt.Fprintf(&b, "\n- %s", code(path))
	}
	return b.String()
}

func upperFirst(s string) string {
	c, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(c)) + s[size:]
}
