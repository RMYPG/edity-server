package login_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RMYPG/edity-server/internal/login"
	"github.com/RMYPG/edity-server/internal/oidctest"
	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/site"
)

var (
	sandbox = site.Site{ID: "edity-sandbox", ZitadelRole: "site:edity-sandbox"}
	ana     = publish.Client{ID: "2200", Name: "Ana García"}
)

func newZitadel(t *testing.T, z *oidctest.Provider) *login.Zitadel {
	t.Helper()
	zitadel, err := login.New(login.Options{
		Issuer: z.Issuer(), ClientID: "337@edity", Key: []byte("0123456789abcdef0123456789abcdef"), ProjectID: oidctest.ProjectID,
		ServiceUser: login.ServiceUser{ID: oidctest.ServiceUserID, KeyID: oidctest.ServiceKeyID, Key: oidctest.ServiceKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return zitadel
}

func TestTheRoleCheckGivesUpWithItsContext(t *testing.T) {
	z := oidctest.New(t, "337@edity")
	z.SetAuthorizations(oidctest.Authorization{UserID: ana.ID, ProjectID: oidctest.ProjectID, Roles: []string{sandbox.ZitadelRole}})
	z.Hang = true
	zitadel := newZitadel(t, z)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	held, err := zitadel.HoldsRole(ctx, sandbox, ana)

	if err == nil || held {
		t.Errorf("HoldsRole = %v, %v, want an error", held, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("it took %v, want it to give up with its context", took)
	}
}

// A session sealed before sessions carried Zitadel's user ID can't be asked
// about, however long it still lasts: ending it sends the Client to log in.
func TestASessionWithNoUserIDHoldsNoRole(t *testing.T) {
	z := oidctest.New(t, "337@edity")
	zitadel := newZitadel(t, z)

	held, err := zitadel.HoldsRole(t.Context(), sandbox, publish.Client{Name: "Ana García"})

	if err != nil || held {
		t.Errorf("HoldsRole = %v, %v, want false", held, err)
	}
	if got := len(z.ListRequests()); got != 0 {
		t.Errorf("%d ListAuthorizations calls, want none", got)
	}
}

func TestNewRefusesAMissingServiceUser(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	user := login.ServiceUser{ID: "1", KeyID: "2", Key: oidctest.ServiceKey()}
	for name, options := range map[string]login.Options{
		"no project": {Key: key, ServiceUser: user},
		"no user":    {Key: key, ProjectID: "3", ServiceUser: login.ServiceUser{KeyID: "2", Key: user.Key}},
		"no key ID":  {Key: key, ProjectID: "3", ServiceUser: login.ServiceUser{ID: "1", Key: user.Key}},
		"no key":     {Key: key, ProjectID: "3", ServiceUser: login.ServiceUser{ID: "1", KeyID: "2"}},
	} {
		if _, err := login.New(options); err == nil || !strings.Contains(err.Error(), "service user") {
			t.Errorf("%s: New = %v, want an error about the service user", name, err)
		}
	}
}
