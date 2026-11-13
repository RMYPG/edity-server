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

// failures is the issue that reports to the Developer the Publishes whose
// Release failed, or failed to go live (V6). Its marker is still the one it
// had when only Releases failed, so that the open issue stays the one.
var failures = topic{
	marker: "<!-- edity:release-failures -->",
	title:  "Edity: a Publish is not live",
	about: "A Client published changes, which are saved on `main`, but their Release or its rollout failed, so they are not live yet. " +
		"The Client was told so, and that you have been notified.\n\n" +
		"Each failure is a comment here while this issue is open. Close it once they are fixed: the next failure opens a new issue.",
}

// maxRollout bounds, in characters, what a failure quotes of the rollout
// status service's reason. The service lists every signal that holds, each
// with its object's message.
const maxRollout = 4000

// ReportFailure reports to the Developer that a Publish failed, unless one
// of the App's issues told it already: it comments on the Site's open failure
// issue, or opens one (V6). A failure is told once, by its ID: polled again
// it tells nothing more, and the same run failing again on another attempt
// is another failure.
func (r *Reporter) ReportFailure(ctx context.Context, s site.Site, f progress.Failure) error {
	return r.report(ctx, s, failures, []string{f.ID}, func([]string) (writeup, error) {
		return writeup{text: failureText(f), keys: []string{f.ID}}, nil
	})
}

// failureText says what failed, for which Publish, and where to see the
// run, or what the rollout status service said.
func failureText(f progress.Failure) string {
	var b strings.Builder
	what := "was not released"
	if f.Released {
		what = "was released but is not live"
	}
	fmt.Fprintf(&b, "### v%s %s\n\n%s.", f.Version, what, upperFirst(f.Reason))
	if f.Run != "" {
		fmt.Fprintf(&b, " [See the run](%s).", f.Run)
	}
	if f.Rollout != "" {
		fmt.Fprintf(&b, " The platform's rollout status service says:\n\n%s", rolloutReason(f.Rollout))
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

// rolloutReason is the rollout status service's reason as a code block,
// which GitHub shows as it is, with each signal on its own line: the service
// separates them with "; ".
func rolloutReason(reason string) string {
	if r := []rune(reason); len(r) > maxRollout {
		reason = string(r[:maxRollout]) + "…"
	}
	text := strings.ReplaceAll(reason, "; ", "\n")
	// A fence longer than any run of backticks in the text.
	longest, run := 0, 0
	for _, c := range text {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + text + "\n" + fence
}

func upperFirst(s string) string {
	c, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(c)) + s[size:]
}
