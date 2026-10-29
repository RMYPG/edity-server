// Package github calls GitHub's REST and GraphQL APIs as a GitHub App, on
// the repositories it is installed on: the edity-publisher App, on the
// Sites' repositories.
//
// It authenticates as GitHub documents for an App: a JSON Web Token signed
// with the App's private key finds the App's installation on a repository
// and gets an installation access token for that repository alone, which
// each call then carries. A token lasts an hour, and is kept until five
// minutes before it expires.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// API is GitHub's API, for github.com.
const API = "https://api.github.com"

// apiVersion is the version of the REST API the calls are written for.
const apiVersion = "2026-03-10"

// maxBody bounds what a response may hold. A content file is a few
// kilobytes.
const maxBody = 16 << 20

// ErrNotFound is GitHub's 404: no such file, commit or branch.
var ErrNotFound = errors.New("not found on GitHub")

// ErrNotInstalled is a repository the App is not installed on.
var ErrNotInstalled = errors.New("the GitHub App is not installed on the repository")

// An Error is any other answer from GitHub that is not a success.
type Error struct {
	// Call is the method and path of the call, such as "GET
	// /repos/RMYPG/edity-sandbox/git/ref/heads/main".
	Call    string
	Status  int
	Message string
	// RetryAfter is how long GitHub asked to wait, when it limits the App's
	// requests, and 0 otherwise.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("GitHub answered %s with %d: %s", e.Call, e.Status, e.Message)
}

// An App is a GitHub App, which holds an installation access token for each
// repository it calls. It is safe for concurrent use.
type App struct {
	id     int64
	key    *rsa.PrivateKey
	api    string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	repos map[string]*installation
}

// installation is the App's installation on one repository, and the token
// it last got for it.
type installation struct {
	mu      sync.Mutex
	id      int64
	token   string
	expires time.Time
}

// NewApp returns the App with the given ID and private key, which calls the
// API at api, such as API, through client.
func NewApp(id int64, key *rsa.PrivateKey, api string, client *http.Client) *App {
	return &App{
		id:     id,
		key:    key,
		api:    strings.TrimSuffix(api, "/"),
		client: client,
		now:    time.Now,
		repos:  map[string]*installation{},
	}
}

// jwt is a JSON Web Token for the App, as GitHub asks for one: RS256, issued
// a minute in the past against clock drift, and valid for nine minutes, one
// less than GitHub allows.
func (a *App) jwt() (string, error) {
	now := a.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.id, 10),
	})
	if err != nil {
		return "", err
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(nil, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// BotLogin is the login of the App's bot user, which its commits are
// authored by: its slug with [bot].
func (a *App) BotLogin(ctx context.Context) (string, error) {
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := a.call(ctx, http.MethodGet, "/app", jwt, nil, &app); err != nil {
		return "", err
	}
	return app.Slug + "[bot]", nil
}

// A Repo is a repository the App is installed on.
type Repo struct {
	app   *App
	owner string
	name  string
}

// Repo returns the repository owner/name. Nothing is called until one of
// its methods is.
func (a *App) Repo(owner, name string) *Repo {
	return &Repo{app: a, owner: owner, name: name}
}

func (r *Repo) String() string {
	return r.owner + "/" + r.name
}

// token returns an installation access token for the repository.
func (r *Repo) token(ctx context.Context) (string, error) {
	a := r.app
	a.mu.Lock()
	in, ok := a.repos[r.String()]
	if !ok {
		in = &installation{}
		a.repos[r.String()] = in
	}
	a.mu.Unlock()

	in.mu.Lock()
	defer in.mu.Unlock()
	if in.token != "" && a.now().Before(in.expires.Add(-5*time.Minute)) {
		return in.token, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	if in.id == 0 {
		var found struct {
			ID int64 `json:"id"`
		}
		err := a.call(ctx, http.MethodGet, "/repos/"+r.path("installation"), jwt, nil, &found)
		if errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("%s: %w", r, ErrNotInstalled)
		}
		if err != nil {
			return "", err
		}
		in.id = found.ID
	}
	var created struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{"repositories": []string{r.name}}
	err = a.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", in.id), jwt, body, &created)
	if errors.Is(err, ErrNotFound) {
		// Uninstalled, or installed again under another ID.
		in.id = 0
		return "", fmt.Errorf("%s: %w", r, ErrNotInstalled)
	}
	if err != nil {
		return "", err
	}
	in.token, in.expires = created.Token, created.ExpiresAt
	return in.token, nil
}

// forget drops a token GitHub no longer takes.
func (r *Repo) forget(token string) {
	r.app.mu.Lock()
	in := r.app.repos[r.String()]
	r.app.mu.Unlock()
	if in == nil {
		return
	}
	in.mu.Lock()
	if in.token == token {
		in.token = ""
	}
	in.mu.Unlock()
}

// path is a path under the repository's REST URL, with each part escaped.
func (r *Repo) path(parts ...string) string {
	escaped := []string{url.PathEscape(r.owner), url.PathEscape(r.name)}
	for _, part := range parts {
		for segment := range strings.SplitSeq(part, "/") {
			escaped = append(escaped, url.PathEscape(segment))
		}
	}
	return strings.Join(escaped, "/")
}

// do makes a call with an installation token, and once more with a new
// token if GitHub no longer takes the one it had.
func (r *Repo) do(ctx context.Context, req request) (answer, error) {
	for attempt := 0; ; attempt++ {
		token, err := r.token(ctx)
		if err != nil {
			return answer{}, err
		}
		req.auth = token
		a, err := r.app.send(ctx, req)
		if err == nil && a.status == http.StatusUnauthorized && attempt == 0 {
			r.forget(token)
			continue
		}
		return a, err
	}
}

// Get reads a REST resource of the repository as JSON into out. path is
// under /repos/<owner>/<name>/, escaped, with its query, such as
// "actions/runs?head_sha=…".
func (r *Repo) Get(ctx context.Context, path string, out any) error {
	return r.get(ctx, "/repos/"+r.path()+"/"+path, out)
}

func (r *Repo) get(ctx context.Context, path string, out any) error {
	a, err := r.do(ctx, request{method: http.MethodGet, path: path})
	if err != nil {
		return err
	}
	return a.decode(out)
}

// Head returns the sha of the commit branch points to.
func (r *Repo) Head(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := r.get(ctx, "/repos/"+r.path("git/ref/heads", branch), &ref); err != nil {
		return "", err
	}
	if ref.Object.Type != "commit" || ref.Object.SHA == "" {
		return "", fmt.Errorf("%s: branch %s points to a %q, not a commit", r, branch, ref.Object.Type)
	}
	return ref.Object.SHA, nil
}

// File returns the content of the file at path in the commit ref, or
// ErrNotFound when there is none.
func (r *Repo) File(ctx context.Context, path, ref string) ([]byte, error) {
	a, err := r.do(ctx, request{
		method: http.MethodGet,
		path:   "/repos/" + r.path("contents", path) + "?ref=" + url.QueryEscape(ref),
		accept: "application/vnd.github.raw+json",
	})
	if err != nil {
		return nil, err
	}
	if err := a.failure(); err != nil {
		return nil, err
	}
	return a.body, nil
}

// A GitCommit is a commit as Git has it.
type GitCommit struct {
	SHA     string
	Message string
	Parents []string
}

// GitCommit returns the commit sha.
func (r *Repo) GitCommit(ctx context.Context, sha string) (GitCommit, error) {
	var commit struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := r.get(ctx, "/repos/"+r.path("git/commits", sha), &commit); err != nil {
		return GitCommit{}, err
	}
	c := GitCommit{SHA: commit.SHA, Message: commit.Message}
	for _, p := range commit.Parents {
		c.Parents = append(c.Parents, p.SHA)
	}
	return c, nil
}

// request is one call to GitHub.
type request struct {
	method string
	// path is under the API's URL, or the URL of GraphQL's endpoint.
	path   string
	auth   string
	accept string
	body   any
}

// call makes a call with auth, and decodes its JSON answer into out.
func (a *App) call(ctx context.Context, method, path, auth string, body, out any) error {
	answer, err := a.send(ctx, request{method: method, path: path, auth: auth, body: body})
	if err != nil {
		return err
	}
	return answer.decode(out)
}

// An answer is GitHub's response to a call, read whole.
type answer struct {
	// call is the call's method and path.
	call   string
	status int
	header http.Header
	body   []byte
}

func (a *App) send(ctx context.Context, r request) (answer, error) {
	var body io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return answer{}, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, a.api+r.path, body)
	if err != nil {
		return answer{}, err
	}
	accept := r.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+r.auth)
	req.Header.Set("User-Agent", "edity-server")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return answer{}, err
	}
	call := r.method + " " + req.URL.Path
	if len(data) > maxBody {
		return answer{}, fmt.Errorf("GitHub answered %s with more than %d bytes", call, maxBody)
	}
	return answer{call: call, status: res.StatusCode, header: res.Header, body: data}, nil
}

// decode reads a successful JSON answer into out.
func (a answer) decode(out any) error {
	if err := a.failure(); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(a.body, out); err != nil {
		return fmt.Errorf("GitHub answered %s with %w", a.call, err)
	}
	return nil
}

// failure is the error an answer that is not a success stands for.
func (a answer) failure() error {
	if a.status >= 200 && a.status < 300 {
		return nil
	}
	if a.status == http.StatusNotFound {
		return fmt.Errorf("%s: %w", a.call, ErrNotFound)
	}
	e := &Error{Call: a.call, Status: a.status}
	var message struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(a.body, &message) == nil && message.Message != "" {
		e.Message = message.Message
	} else {
		e.Message = http.StatusText(a.status)
	}
	e.RetryAfter = a.retryAfter()
	return e
}

// retryAfter is how long GitHub asks to wait when it limits requests: its
// Retry-After, or until its rate limit resets once none is left, as GitHub
// documents.
func (a answer) retryAfter() time.Duration {
	if a.status != http.StatusForbidden && a.status != http.StatusTooManyRequests {
		return 0
	}
	if seconds, err := strconv.Atoi(a.header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if a.header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(a.header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if wait := time.Until(time.Unix(reset, 0)); wait > 0 {
				return wait
			}
		}
		return time.Minute
	}
	if a.status == http.StatusTooManyRequests {
		return time.Minute
	}
	return 0
}
