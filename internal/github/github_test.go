package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// fakeGitHub answers the calls the App makes on RMYPG/edity-sandbox, as
// GitHub documents them, and checks how each is authenticated.
type fakeGitHub struct {
	t   *testing.T
	now time.Time

	mu sync.Mutex
	// tokens are those issued, and refused those GitHub no longer takes.
	tokens  []string
	refused map[string]bool
	// head is main's sha, files the repository's files at it, and commits
	// the messages of the commits made.
	head      string
	files     map[string]string
	installed bool
	// mutation answers createCommitOnBranch instead of the fake, when set.
	mutation   func(w http.ResponseWriter, input map[string]any)
	lastCommit map[string]any
	// reads counts the GETs made with a token, and notModified those of
	// them answered 304.
	reads, notModified int
	// apps counts the calls to GET /app.
	apps int
}

func newFake(t *testing.T) (*fakeGitHub, *App) {
	f := &fakeGitHub{
		t:         t,
		now:       time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
		refused:   map[string]bool{},
		head:      "aaaa000000000000000000000000000000000000",
		files:     map[string]string{"src/content/home.json": `{"a": 1}`},
		installed: true,
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	app := NewApp(5466528, testKey(), srv.URL, srv.Client())
	app.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	return f, app
}

// ServeHTTP gives each answer to a GET made with a token an ETag, as GitHub
// does, and answers 304 when the call's If-None-Match is still the answer's.
func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/repos/") || strings.HasSuffix(r.URL.Path, "/installation") {
		f.serve(w, r)
		return
	}
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	if rec.Code == http.StatusOK || rec.Code == http.StatusNotFound {
		f.reads++
	}
	if rec.Code == http.StatusOK {
		etag := fmt.Sprintf(`W/"%x"`, sha256.Sum256(rec.Body.Bytes()))
		rec.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			f.notModified++
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-GitHub-Api-Version") != apiVersion || r.Header.Get("User-Agent") == "" {
		f.t.Errorf("%s %s: no API version or User-Agent", r.Method, r.URL.Path)
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		f.checkJWT(auth)
		f.apps++
		writeJSON(w, http.StatusOK, map[string]any{"id": 5466528, "slug": "edity-publisher"})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/RMYPG/edity-sandbox/installation":
		f.checkJWT(auth)
		if !f.installed {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 77, "app_slug": "edity-publisher"})
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/77/access_tokens":
		f.checkJWT(auth)
		var body struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Repositories) != 1 || body.Repositories[0] != "edity-sandbox" {
			f.t.Errorf("a token for %v, want for edity-sandbox alone", body.Repositories)
		}
		token := fmt.Sprintf("ghs_%d", len(f.tokens)+1)
		f.tokens = append(f.tokens, token)
		writeJSON(w, http.StatusCreated, map[string]any{"token": token, "expires_at": f.now.Add(time.Hour)})
	case !f.validToken(auth):
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/RMYPG/edity-sandbox/git/ref/heads/main":
		writeJSON(w, http.StatusOK, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"type": "commit", "sha": f.head}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/RMYPG/edity-sandbox/contents/"):
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			f.t.Errorf("contents asked for as %s, not raw", r.Header.Get("Accept"))
		}
		content, ok := f.files[strings.TrimPrefix(r.URL.Path, "/repos/RMYPG/edity-sandbox/contents/")]
		if !ok || r.URL.Query().Get("ref") != f.head {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		_, _ = io.WriteString(w, content)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/RMYPG/edity-sandbox/git/commits/"+f.head:
		writeJSON(w, http.StatusOK, map[string]any{
			"sha": f.head, "message": "content: publish home\n\nEdity-Client: Ana",
			"author":    map[string]any{"name": "edity-publisher[bot]", "email": "347795330+edity-publisher[bot]@users.noreply.github.com", "date": "2026-10-29T11:59:00Z"},
			"committer": map[string]any{"name": "GitHub", "email": "noreply@github.com", "date": "2026-10-29T11:59:00Z"},
			"parents":   []map[string]any{{"sha": "1111000000000000000000000000000000000000"}},
		})
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				Input map[string]any `json:"input"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Query, "createCommitOnBranch") {
			f.t.Errorf("not a createCommitOnBranch: %v %q", err, body.Query)
		}
		input := body.Variables.Input
		f.lastCommit = input
		if f.mutation != nil {
			f.mutation(w, input)
			return
		}
		if input["expectedHeadOid"] != f.head {
			writeJSON(w, http.StatusOK, map[string]any{
				"data":   map[string]any{"createCommitOnBranch": nil},
				"errors": []map[string]any{{"type": "STALE_DATA", "message": fmt.Sprintf("Expected branch to point to %q but it did not.", input["expectedHeadOid"])}},
			})
			return
		}
		f.head = "bbbb000000000000000000000000000000000000"
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": f.head}}}})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	}
}

func (f *fakeGitHub) validToken(token string) bool {
	for _, t := range f.tokens {
		if t == token && !f.refused[t] {
			return true
		}
	}
	return false
}

// checkJWT checks a JSON Web Token as GitHub does.
func (f *fakeGitHub) checkJWT(token string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		f.t.Errorf("not a JWT: %q", token)
		return
	}
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		f.t.Errorf("the JWT's signature: %v", err)
	}
	var header struct{ Alg string }
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	c, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(h, &header) != nil || json.Unmarshal(c, &claims) != nil || header.Alg != "RS256" {
		f.t.Errorf("the JWT's header %s or claims %s", h, c)
	}
	now := f.now.Unix()
	if claims.Iss != "5466528" || claims.Iat > now || claims.Exp <= now || claims.Exp-claims.Iat > 600 {
		f.t.Errorf("the JWT's claims %s at %d", c, now)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestCallsCarryAnInstallationTokenForTheRepositoryAlone(t *testing.T) {
	f, app := newFake(t)
	repo := app.Repo("RMYPG", "edity-sandbox")

	head, err := repo.Head(t.Context(), "main")
	if err != nil || head != f.head {
		t.Fatalf("Head = %q, %v, want %q", head, err, f.head)
	}
	file, err := repo.File(t.Context(), "src/content/home.json", head)
	if err != nil || string(file) != `{"a": 1}` {
		t.Fatalf("File = %q, %v", file, err)
	}
	if _, err := repo.File(t.Context(), "src/content/missing.json", head); !errors.Is(err, ErrNotFound) {
		t.Errorf("File of a missing file: %v, want ErrNotFound", err)
	}
	if len(f.tokens) != 1 {
		t.Errorf("%d tokens for three calls, want one", len(f.tokens))
	}
}

func TestAGetGitHubSaysStillHoldsReturnsTheAnswerItHad(t *testing.T) {
	f, app := newFake(t)
	repo := app.Repo("RMYPG", "edity-sandbox")
	for range 2 {
		if head, err := repo.Head(t.Context(), "main"); err != nil || head != f.head {
			t.Fatalf("Head = %q, %v, want %q", head, err, f.head)
		}
		if file, err := repo.File(t.Context(), "src/content/home.json", f.head); err != nil || string(file) != `{"a": 1}` {
			t.Fatalf("File = %q, %v", file, err)
		}
	}
	if f.reads != 4 || f.notModified != 2 {
		t.Errorf("%d reads, %d of them 304, want the second Head and File to be 304s", f.reads, f.notModified)
	}

	// Once main moves, GitHub answers anew.
	f.mu.Lock()
	f.head = "cccc000000000000000000000000000000000000"
	f.mu.Unlock()
	if head, err := repo.Head(t.Context(), "main"); err != nil || head != f.head {
		t.Errorf("Head after main moved = %q, %v, want %q", head, err, f.head)
	}
	if f.notModified != 2 {
		t.Errorf("%d reads answered 304, want main's move answered in full", f.notModified)
	}
}

func TestTheCacheForgetsTheAnswersUsedLeastRecently(t *testing.T) {
	c := newCache(10)
	c.put("a", `"1"`, answer{body: []byte("aaaa")})
	c.put("b", `"2"`, answer{body: []byte("bbbb")})
	c.get("a")
	c.put("c", `"3"`, answer{body: []byte("cccc")})
	if c.get("b") != nil || c.get("a") == nil || c.get("c") == nil || c.size != 8 {
		t.Errorf("after a third answer, b is %v and the cache holds %d bytes, want b forgotten and 8", c.get("b"), c.size)
	}
	c.put("a", `"4"`, answer{body: []byte("aa")})
	if got := c.get("a"); got == nil || got.etag != `"4"` || c.size != 6 {
		t.Errorf("a = %+v, %d bytes, want its new answer in 6", got, c.size)
	}
	c.put("huge", `"5"`, answer{body: make([]byte, maxCachedBody+1)})
	if c.get("huge") != nil {
		t.Error("the cache kept an answer larger than it keeps")
	}
}

func TestATokenIsRenewedFiveMinutesBeforeItExpires(t *testing.T) {
	f, app := newFake(t)
	repo := app.Repo("RMYPG", "edity-sandbox")
	for _, after := range []time.Duration{0, 54 * time.Minute, time.Minute} {
		f.mu.Lock()
		f.now = f.now.Add(after)
		f.mu.Unlock()
		if _, err := repo.Head(t.Context(), "main"); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.tokens) != 2 {
		t.Errorf("%d tokens, want a second one 55 minutes after the first", len(f.tokens))
	}
}

func TestATokenGitHubNoLongerTakesIsReplaced(t *testing.T) {
	f, app := newFake(t)
	repo := app.Repo("RMYPG", "edity-sandbox")
	if _, err := repo.Head(t.Context(), "main"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.refused[f.tokens[0]] = true
	f.mu.Unlock()
	if _, err := repo.Head(t.Context(), "main"); err != nil {
		t.Fatalf("Head after the token was revoked: %v", err)
	}
	if len(f.tokens) != 2 {
		t.Errorf("%d tokens, want a new one", len(f.tokens))
	}
}

func TestARepositoryTheAppIsNotInstalledOn(t *testing.T) {
	f, app := newFake(t)
	f.installed = false
	if _, err := app.Repo("RMYPG", "edity-sandbox").Head(t.Context(), "main"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Head: %v, want ErrNotInstalled", err)
	}
}

func TestBotLogin(t *testing.T) {
	f, app := newFake(t)
	for range 2 {
		if login, err := app.BotLogin(t.Context()); err != nil || login != "edity-publisher[bot]" {
			t.Errorf("BotLogin = %q, %v", login, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.apps != 1 {
		t.Errorf("BotLogin asked GitHub %d times, want once", f.apps)
	}
}

func TestCreateCommitMovesTheBranchOnlyFromItsParent(t *testing.T) {
	f, app := newFake(t)
	repo := app.Repo("RMYPG", "edity-sandbox")
	commit := NewCommit{
		Branch:  "main",
		Parent:  f.head,
		Message: "content: publish home\n\nChanged:\n- hero.title\n\nEdity-Client: Ana",
		Files:   []File{{Path: "src/content/home.json", Contents: []byte("{}\n")}, {Path: "package.json", Contents: []byte(`{"version": "1.0.1"}`)}},
	}
	sha, err := repo.CreateCommit(t.Context(), commit)
	if err != nil || sha != "bbbb000000000000000000000000000000000000" {
		t.Fatalf("CreateCommit = %q, %v", sha, err)
	}
	want := map[string]any{
		"branch":          map[string]any{"repositoryNameWithOwner": "RMYPG/edity-sandbox", "branchName": "main"},
		"expectedHeadOid": "aaaa000000000000000000000000000000000000",
		"message":         map[string]any{"headline": "content: publish home", "body": "Changed:\n- hero.title\n\nEdity-Client: Ana"},
		"fileChanges": map[string]any{"additions": []any{
			map[string]any{"path": "src/content/home.json", "contents": base64.StdEncoding.EncodeToString([]byte("{}\n"))},
			map[string]any{"path": "package.json", "contents": base64.StdEncoding.EncodeToString([]byte(`{"version": "1.0.1"}`))},
		}},
	}
	got, _ := json.Marshal(f.lastCommit)
	wanted, _ := json.Marshal(want)
	if string(got) != string(wanted) {
		t.Errorf("createCommitOnBranch's input:\n%s\nwant:\n%s", got, wanted)
	}

	// main is now at the new commit, so the same commit is stale.
	if _, err := repo.CreateCommit(t.Context(), commit); !errors.Is(err, ErrStale) {
		t.Errorf("CreateCommit on a stale parent: %v, want ErrStale", err)
	}
}

func TestCreateCommitTellsWhenItCannotTell(t *testing.T) {
	f, app := newFake(t)
	repo := app.Repo("RMYPG", "edity-sandbox")
	commit := NewCommit{Branch: "main", Parent: f.head, Message: "content: publish home"}
	for name, answer := range map[string]func(http.ResponseWriter, map[string]any){
		"a 502": func(w http.ResponseWriter, _ map[string]any) {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Server Error"})
		},
		"a broken connection": func(w http.ResponseWriter, _ map[string]any) {
			conn, _, _ := http.NewResponseController(w).Hijack()
			_ = conn.Close()
		},
		"no commit": func(w http.ResponseWriter, _ map[string]any) {
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"createCommitOnBranch": nil}})
		},
	} {
		f.mu.Lock()
		f.mutation = answer
		f.mu.Unlock()
		if _, err := repo.CreateCommit(t.Context(), commit); !errors.Is(err, ErrUncertain) {
			t.Errorf("after %s: %v, want ErrUncertain", name, err)
		}
	}

	f.mu.Lock()
	f.mutation = func(w http.ResponseWriter, _ map[string]any) {
		writeJSON(w, http.StatusOK, map[string]any{"errors": []map[string]any{{"type": "FORBIDDEN", "message": "Resource not accessible by integration"}}})
	}
	f.mu.Unlock()
	var gerr *Error
	if _, err := repo.CreateCommit(t.Context(), commit); !errors.As(err, &gerr) || errors.Is(err, ErrUncertain) {
		t.Errorf("after a FORBIDDEN: %v, want an Error", err)
	}
}

func TestALimitedCallSaysHowLongToWait(t *testing.T) {
	f, app := newFake(t)
	f.mutation = func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "You have exceeded a secondary rate limit."})
	}
	_, err := app.Repo("RMYPG", "edity-sandbox").CreateCommit(t.Context(), NewCommit{Branch: "main", Parent: f.head, Message: "x"})
	var gerr *Error
	if !errors.As(err, &gerr) || gerr.RetryAfter != 30*time.Second {
		t.Errorf("CreateCommit when limited: %v, want an Error to retry after 30s", err)
	}
}

func TestGitCommit(t *testing.T) {
	f, app := newFake(t)
	c, err := app.Repo("RMYPG", "edity-sandbox").GitCommit(context.Background(), f.head)
	if err != nil {
		t.Fatal(err)
	}
	if c.SHA != f.head || c.Message != "content: publish home\n\nEdity-Client: Ana" || len(c.Parents) != 1 || c.Parents[0] != "1111000000000000000000000000000000000000" ||
		c.Author != "edity-publisher[bot]" || !c.Committed.Equal(time.Date(2026, 10, 29, 11, 59, 0, 0, time.UTC)) {
		t.Errorf("GitCommit = %+v", c)
	}
	if _, err := app.Repo("RMYPG", "edity-sandbox").GitCommit(context.Background(), "2222000000000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GitCommit of no commit: %v, want ErrNotFound", err)
	}
}
