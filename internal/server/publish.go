package server

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RMYPG/edity-server/internal/content"
	"github.com/RMYPG/edity-server/internal/fields"
	"github.com/RMYPG/edity-server/internal/github"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/site"
)

// Sessions tells which Client a request's session is for. Logging in
// through Zitadel (ticket 25) implements it.
type Sessions interface {
	// Client returns the Client of the session r carries for s, and false
	// when r carries none, or an expired one.
	Client(r *http.Request, s site.Site) (publish.Client, bool)
}

// NoSessions is Sessions while no Client can log in: no request has a
// session.
type NoSessions struct{}

// Client returns false.
func (NoSessions) Client(*http.Request, site.Site) (publish.Client, bool) {
	return publish.Client{}, false
}

// A Publisher publishes a Draft on a Site. *publish.Publisher is one.
type Publisher interface {
	Publish(ctx context.Context, s site.Site, d publish.Draft, client publish.Client) (publish.Published, error)
}

// A Notifier tells the Developer of the commits a Publish ships that are
// not content (V7). *issues.Reporter is one.
type Notifier interface {
	NoteDeveloperCommits(ctx context.Context, s site.Site, p publish.Published) error
}

// maxPublishBody bounds a Publish's body. A whole page's content is a few
// kilobytes, and a Draft holds at most every value in it twice.
const maxPublishBody = 1 << 20

// publishTimeout bounds a Publish, within the server's write timeout. It
// runs to the end even if the browser goes away, so that a commit on its
// way is not cut off.
const publishTimeout = 50 * time.Second

// busyRetry is how long a Client waits to publish again after main kept
// moving under a Publish.
const busyRetry = 10 * time.Second

// noteTimeout bounds noting what a Publish shipped, which runs once the
// Publish is answered.
const noteTimeout = time.Minute

type publishHandler struct {
	sessions  Sessions
	publisher Publisher
	// notifier is nil when nothing notes what a Publish ships.
	notifier Notifier
	limit    *limiter
	logger   *slog.Logger
}

// ServeHTTP answers POST /_edity/api/publish.
func (h *publishHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, _ := site.FromContext(r.Context())
	if !sameOrigin(r) {
		http.Error(w, "The request does not come from the Site's own origin.", http.StatusForbidden)
		return
	}
	client, ok := h.sessions.Client(r, s)
	if !ok {
		http.Error(w, "No session: log in to publish.", http.StatusUnauthorized)
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		http.Error(w, "Not a Publish: the body is not sent as application/json.", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPublishBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, fmt.Sprintf("The body is larger than %d bytes.", tooLarge.Limit), http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "The body could not be read.", http.StatusBadRequest)
		return
	}
	draft, err := readDraft(body)
	if err == nil {
		err = draft.Check()
	}
	if err != nil {
		http.Error(w, "Not a Publish: "+err.Error()+".", http.StatusBadRequest)
		return
	}
	if wait, ok := h.limit.take(s.ID); !ok {
		w.Header().Set("Retry-After", seconds(wait))
		http.Error(w, "The Site has published too often lately.", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), publishTimeout)
	defer cancel()
	published, err := h.publisher.Publish(ctx, s, draft, client)
	if err != nil {
		h.fail(w, r, s, err)
		return
	}
	h.logger.LogAttrs(r.Context(), slog.LevelInfo, "published",
		slog.String("site", s.ID), slog.String("page", draft.Page),
		slog.String("sha", published.SHA), slog.String("version", published.Version),
		slog.Int("conflicts", len(published.Conflicts)))
	answer := struct {
		SHA       string     `json:"sha"`
		Version   string     `json:"version"`
		Conflicts []conflict `json:"conflicts,omitempty"`
	}{published.SHA, published.Version, conflicts(published.Conflicts)}
	writeJSON(w, http.StatusAccepted, answer)
	if h.notifier != nil {
		// Apart from the Publish, which it neither delays nor fails.
		go h.note(context.WithoutCancel(r.Context()), s, published)
	}
}

// note notes the Developer commits that published ships, if any.
func (h *publishHandler) note(ctx context.Context, s site.Site, published publish.Published) {
	ctx, cancel := context.WithTimeout(ctx, noteTimeout)
	defer cancel()
	if err := h.notifier.NoteDeveloperCommits(ctx, s, published); err != nil {
		h.logger.LogAttrs(ctx, slog.LevelWarn, "Developer commits not noted",
			slog.String("site", s.ID), slog.String("sha", published.SHA), slog.String("error", err.Error()))
	}
}

// fail answers a Publish that committed nothing.
func (h *publishHandler) fail(w http.ResponseWriter, r *http.Request, s site.Site, err error) {
	var (
		inConflict *publish.ConflictsError
		violations *publish.ViolationsError
		broken     *publish.BrokenError
	)
	switch {
	case errors.As(err, &inConflict):
		writeJSON(w, http.StatusConflict, struct {
			Conflicts []conflict `json:"conflicts"`
		}{conflicts(inConflict.Paths)})
		return
	case errors.As(err, &violations):
		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Violations []fields.Violation `json:"violations"`
		}{violations.Violations})
		return
	}

	h.logger.LogAttrs(r.Context(), slog.LevelError, "publish failed", slog.String("site", s.ID), slog.String("error", err.Error()))
	switch {
	case errors.As(err, &broken), errors.Is(err, github.ErrNotInstalled), errors.Is(err, github.ErrNotFound):
		// No main, or the App can't see the repository.
		http.Error(w, "The Site repository can't be published as it is: the Developer has to fix it.", http.StatusInternalServerError)
	case errors.Is(err, publish.ErrBusy):
		w.Header().Set("Retry-After", seconds(busyRetry))
		http.Error(w, "The Site repository's main kept moving: publish again in a moment.", http.StatusServiceUnavailable)
	default:
		failedOnGitHub(w, err, "publish again")
	}
}

// readDraft reads a Publish's body, as the OpenAPI file's PublishRequest
// describes it: no key it does not name, and none twice.
func readDraft(body []byte) (publish.Draft, error) {
	var request struct {
		Page    *string `json:"page"`
		Changes *[]struct {
			Path   *string        `json:"path"`
			Before jsontext.Value `json:"before"`
			After  jsontext.Value `json:"after"`
		} `json:"changes"`
	}
	if err := jsonv2.Unmarshal(body, &request, jsonv2.RejectUnknownMembers(true)); err != nil {
		return publish.Draft{}, errors.New("the body is not JSON of the shape of a PublishRequest")
	}
	if request.Page == nil || request.Changes == nil {
		return publish.Draft{}, errors.New("it needs a page and its changes")
	}
	d := publish.Draft{Page: *request.Page}
	for i, c := range *request.Changes {
		if c.Path == nil || c.Before == nil || c.After == nil {
			return publish.Draft{}, fmt.Errorf("change %d needs a path, before and after", i+1)
		}
		before, err := content.ParseChange(c.Before)
		if err != nil {
			return publish.Draft{}, fmt.Errorf("change %d: before: %w", i+1, err)
		}
		after, err := content.ParseChange(c.After)
		if err != nil {
			return publish.Draft{}, fmt.Errorf("change %d: after: %w", i+1, err)
		}
		d.Changes = append(d.Changes, publish.Change{Path: *c.Path, Before: before, After: after})
	}
	return d, nil
}

// sameOrigin tells whether r comes from a page on the Site's own origin, its
// scheme and the host it was sent to (A4). A browser sends Origin with
// every POST.
func sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || (origin.Scheme != "https" && origin.Scheme != "http") ||
		origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	return strings.EqualFold(origin.Host, r.Host)
}

type conflict struct {
	Path string `json:"path"`
}

func conflicts(paths []string) []conflict {
	var list []conflict
	for _, p := range paths {
		list = append(list, conflict{Path: p})
	}
	return list
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// seconds writes d as Retry-After does, in whole seconds, at least one.
func seconds(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}
