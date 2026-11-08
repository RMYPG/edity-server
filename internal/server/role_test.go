package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/RMYPG/edity-server/internal/oidctest"
)

// anasRole is Ana's role assignment on edity-sandbox.
var anasRole = oidctest.Authorization{UserID: ana.Subject, ProjectID: oidctest.ProjectID, Roles: []string{"site:edity-sandbox"}}

// withRole is a browser where Ana logged in, while Zitadel says she holds the
// Site's role.
func (lt *loginTest) withRole() *browser {
	lt.t.Helper()
	lt.zitadel.SetAuthorizations(anasRole)
	b := lt.browser("sandbox.example.com")
	if rec := b.logIn(ana, "/_edity/login"); rec.Code != http.StatusSeeOther {
		lt.t.Fatalf("logging in: status = %d, want %d: %s", rec.Code, http.StatusSeeOther, rec.Body)
	}
	return b
}

// sessionEnded fails t unless rec cleared both of the session's cookies and
// the browser is logged out.
func sessionEnded(t *testing.T, b *browser, rec *httptest.ResponseRecorder) {
	t.Helper()
	for name, path := range map[string]string{"edity_session": "/_edity/api", "edity": "/"} {
		if c := responseCookie(t, rec, name); c == nil || c.MaxAge >= 0 || c.Path != path {
			t.Errorf("cookie %s = %+v, want it cleared on %s", name, c, path)
		}
	}
	if rec := b.session(); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session: status = %d, want 401: %s", rec.Code, rec.Body)
	}
}

func TestAPublishGoesThroughWhileTheClientHoldsTheRole(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()

	rec := b.publish()

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
	if lt.publisher.calls() != 1 {
		t.Errorf("committed %d times, want once", lt.publisher.calls())
	}
	if rec := b.session(); rec.Code != http.StatusOK {
		t.Errorf("GET session: status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// The request's exact JSON is what Zitadel's proto and Connect's JSON mapping
// say: proto/zitadel/authorization/v2 at v4.19.2.
func TestAPublishAsksZitadelForTheUsersRoleInTheProject(t *testing.T) {
	lt := newLoginTest(t)

	lt.withRole().publish()

	requests := lt.zitadel.ListRequests()
	if len(requests) != 1 {
		t.Fatalf("%d ListAuthorizations calls, want 1", len(requests))
	}
	var got any
	if err := json.Unmarshal([]byte(requests[0]), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"filters": []any{
		map[string]any{"inUserIds": map[string]any{"ids": []any{ana.Subject}}},
		map[string]any{"projectId": map[string]any{"id": oidctest.ProjectID}},
		map[string]any{"roleKey": map[string]any{"key": "site:edity-sandbox", "method": "TEXT_FILTER_METHOD_EQUALS"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the request = %s, want %v", requests[0], want)
	}
}

func TestAPublishAfterTheRoleWasRemovedEndsTheSession(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	lt.zitadel.SetAuthorizations()

	rec := b.publish()

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusUnauthorized, rec.Body)
	}
	if lt.publisher.calls() != 0 {
		t.Errorf("committed %d times, want none", lt.publisher.calls())
	}
	sessionEnded(t, b, rec)
}

func TestAPublishNeverRemembersZitadelsAnswer(t *testing.T) {
	lt := newLoginTest(t)
	b := lt.withRole()
	if rec := b.publish(); rec.Code != http.StatusAccepted {
		t.Fatalf("first Publish: status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body)
	}
	lt.zitadel.SetAuthorizations()

	if rec := b.publish(); rec.Code != http.StatusUnauthorized {
		t.Errorf("second Publish: status = %d, want %d: %s", rec.Code, http.StatusUnauthorized, rec.Body)
	}
	if got := len(lt.zitadel.ListRequests()); got != 2 {
		t.Errorf("%d ListAuthorizations calls, want one per Publish", got)
	}
}
