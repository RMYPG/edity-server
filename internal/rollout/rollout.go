// Package rollout asks the platform's rollout status service how a Site's
// rollout is going (ADR-0003): the version the platform has picked up for
// one of the Site's environments, the version the Site serves there, and what
// failed. The service is the platform's: it watches the cluster, answers from
// it on each request and stores nothing, so the server holds no Kubernetes
// credentials and only makes this request. Its HTTP contract is
// docs/contract.md in RMYPG/site-status.
package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Production is the environment a Publish goes live in.
const Production = "production"

// A Phase is how the service sums up an environment's rollout.
type Phase string

// The phases the service answers.
const (
	// Live is the Site running the version the platform picked up.
	Live Phase = "live"
	// Deploying is anything that is neither live nor failed, such as a
	// rollout under way, or a version the service does not know.
	Deploying Phase = "deploying"
	// Failed is a rollout that failed while the Site runs an older version
	// than the one picked up.
	Failed Phase = "failed"
)

// A Status is how an environment's rollout stands, as the service tells it.
type Status struct {
	// Target is the tag of the newest version the platform has picked up,
	// such as v1.0.3, and Running the tag of the version the Site serves.
	// Each is empty when the service does not know it.
	Target, Running string
	Phase           Phase
	// Reason says what failed, for the Developer, and is empty when nothing
	// did. It can be set while Phase is Live: a failing image scan stops
	// Target from moving, so Target and Running can't show it.
	Reason string
}

// An Error is the service giving no status: its answer was not one, or it
// could not be reached. Either way it says nothing about the rollout.
type Error struct {
	// Call is the request's method and path.
	Call string
	// Status is the service's HTTP status, or 0 when it gave none, such as
	// when it could not be reached.
	Status int
	// Message is what the service said, or what went wrong.
	Message string
	// Err is what went wrong on the way, when something did.
	Err error
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("the rollout status service did not answer %s: %s", e.Call, e.Message)
	}
	return fmt.Sprintf("the rollout status service answered %s with %d: %s", e.Call, e.Status, e.Message)
}

func (e *Error) Unwrap() error {
	return e.Err
}

// Refused tells whether the service refused the request rather than failing
// to answer it: 404 for a Site or environment it is not configured for, or a
// path it does not have, and 405 for a method other than GET. Asking again
// changes nothing until the configuration of the server or the service is
// fixed. Anything else, such as 503 while its watches resync or a request
// that never reached it, may pass.
func (e *Error) Refused() bool {
	return e.Status >= 400 && e.Status < 500
}

// maxBody bounds what an answer may hold. A status is a few hundred bytes,
// with a reason of a few kilobytes at most.
const maxBody = 1 << 20

// A Client asks the service at one address. It is safe for concurrent use.
type Client struct {
	base   string
	client *http.Client
}

// New returns a Client of the service at base, such as
// http://site-status.site-status.svc:8080, which it calls through client.
func New(base string, client *http.Client) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), client: client}
}

// Status returns how the rollout of the Site whose id is site stands in its
// environment. Every error it returns is an *Error.
func (c *Client) Status(ctx context.Context, site, environment string) (Status, error) {
	path := "/v1/sites/" + url.PathEscape(site) + "/environments/" + url.PathEscape(environment)
	call := http.MethodGet + " " + path
	failed := func(status int, message string, err error) (Status, error) {
		return Status{}, &Error{Call: call, Status: status, Message: message, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return failed(0, err.Error(), err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "edity-server")
	res, err := c.client.Do(req)
	if err != nil {
		return failed(0, err.Error(), err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return failed(0, err.Error(), err)
	}
	if len(data) > maxBody {
		return failed(res.StatusCode, fmt.Sprintf("more than %d bytes", maxBody), nil)
	}
	if res.StatusCode != http.StatusOK {
		// Error bodies are {"error": "message"}.
		var e struct {
			Error string `json:"error"`
		}
		message := http.StatusText(res.StatusCode)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			message = e.Error
		}
		return failed(res.StatusCode, message, nil)
	}

	// A null target or running is a version the service does not know, but
	// a status without the key is not one the contract describes: read as
	// null, it would count as an older version, and could fail a Publish.
	// The contract answers every key, null or not.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return failed(res.StatusCode, "not a status: "+err.Error(), err)
	}
	for _, key := range []string{"target", "running", "phase", "reason"} {
		if _, ok := keys[key]; !ok {
			return failed(res.StatusCode, "a status with no "+key, nil)
		}
	}
	var answer struct {
		Target  *string `json:"target"`
		Running *string `json:"running"`
		Phase   *Phase  `json:"phase"`
		Reason  *string `json:"reason"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return failed(res.StatusCode, "not a status: "+err.Error(), err)
	}
	if answer.Phase == nil {
		return failed(res.StatusCode, "a status with no phase", nil)
	}
	switch *answer.Phase {
	case Live, Deploying, Failed:
	default:
		// A phase this server does not know says nothing it can go by.
		return failed(res.StatusCode, fmt.Sprintf("the phase %q", *answer.Phase), errors.ErrUnsupported)
	}
	return Status{
		Target:  deref(answer.Target),
		Running: deref(answer.Running),
		Phase:   *answer.Phase,
		Reason:  deref(answer.Reason),
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
