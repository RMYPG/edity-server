package login

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
	"slices"
	"strings"
	"time"

	"github.com/RMYPG/edity-server/internal/publish"
	"github.com/RMYPG/edity-server/internal/site"
)

const (
	// listAuthorizations is the Connect procedure of Zitadel's authorization
	// service that lists role assignments. It needs the permission
	// user.grant.read.
	listAuthorizations = "/zitadel.authorization.v2.AuthorizationService/ListAuthorizations"
	jwtProfileGrant    = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// serviceScopes put Zitadel's own project in the access token's audience,
	// which its API requires.
	serviceScopes = "openid urn:zitadel:iam:org:project:id:zitadel:aud"
	// assertionBackdate keeps an assertion's iat in the past despite clock
	// drift: Zitadel refuses one more than a second ahead of its clock.
	assertionBackdate = 30 * time.Second
	// assertionLifetime is short: each assertion is used once.
	assertionLifetime = 5 * time.Minute
	// tokenMargin is how long before it expires an access token stops being
	// used, so that it does not expire on its way.
	tokenMargin = time.Minute
	// maxAnswer bounds what is read of an answer from Zitadel, which holds a
	// token, or the few role assignments of one person.
	maxAnswer = 1 << 20
)

// errTokenRefused is Zitadel's API refusing the service user's access token.
var errTokenRefused = errors.New("listing authorizations: Zitadel refused the access token")

// A ServiceUser is who the server asks Zitadel about roles as: a read-only
// service user, and its JWT-profile key.
type ServiceUser struct {
	ID    string
	KeyID string
	Key   *rsa.PrivateKey
}

// HoldsRole asks Zitadel whether c still holds s's role in the Edity project.
// It remembers no answer, so a role taken away counts from the next Publish.
// Its error means that Zitadel gave no answer, which says nothing about the
// role.
func (z *Zitadel) HoldsRole(ctx context.Context, s site.Site, c publish.Client) (bool, error) {
	if c.ID == "" {
		// A session sealed before sessions carried the user ID. Zitadel can't
		// be asked about it, and logging in again gives it one.
		return false, nil
	}
	token, err := z.accessToken(ctx, "")
	if err != nil {
		return false, err
	}
	list, err := z.authorizations(ctx, token, c.ID, s.ZitadelRole)
	if errors.Is(err, errTokenRefused) {
		// Zitadel can stop honouring a token sooner than it said. One more try
		// with a new one: a refusal of that too is no answer.
		if token, err = z.accessToken(ctx, token); err != nil {
			return false, err
		}
		list, err = z.authorizations(ctx, token, c.ID, s.ZitadelRole)
	}
	if err != nil {
		return false, err
	}
	held := slices.ContainsFunc(list.Authorizations, func(a authorization) bool {
		return a.grants(z.projectID, c.ID, s.ZitadelRole)
	})
	if !held && list.hides() {
		// Told apart from a role that is gone, or a service user without the
		// permission would end every Client's session.
		return false, errors.New("listing authorizations: Zitadel found some and showed fewer: the service user lacks user.grant.read")
	}
	return held, nil
}

// An authorizationList is the part of Zitadel's answer the check reads.
type authorizationList struct {
	Pagination *struct {
		// TotalResult is a number written as a string, as protojson writes a
		// uint64, and left out when it is zero.
		TotalResult json.Number `json:"totalResult"`
	} `json:"pagination"`
	Authorizations []authorization `json:"authorizations"`
}

// hides tells whether Zitadel found more assignments than it lists. Without
// user.grant.read it drops from the list those it found, but goes on counting
// them (internal/query/user_grant.go userGrantsCheckPermission, v4.19.2).
func (l authorizationList) hides() bool {
	found, err := l.Pagination.TotalResult.Int64()
	return err == nil && found > int64(len(l.Authorizations))
}

// An authorization is a role assignment, as Zitadel writes it.
type authorization struct {
	State   string `json:"state"`
	Project struct {
		ID string `json:"id"`
	} `json:"project"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Roles []assignedRole `json:"roles"`
}

type assignedRole struct {
	Key string `json:"key"`
}

// grants tells whether the assignment gives userID role on the project. The
// answer is read whole, not trusted to be what was asked: only an active
// assignment counts, and Zitadel lists deactivated ones too.
func (a authorization) grants(projectID, userID, role string) bool {
	return a.State == "STATE_ACTIVE" && a.Project.ID == projectID && a.User.ID == userID &&
		slices.Contains(a.Roles, assignedRole{Key: role})
}

// authorizations lists the assignments of userID's role on the project.
func (z *Zitadel) authorizations(ctx context.Context, token, userID, role string) (authorizationList, error) {
	body, err := json.Marshal(map[string]any{"filters": []map[string]any{
		{"inUserIds": map[string]any{"ids": []string{userID}}},
		{"projectId": map[string]any{"id": z.projectID}},
		{"roleKey": map[string]any{"key": role, "method": "TEXT_FILTER_METHOD_EQUALS"}},
	}})
	if err != nil {
		return authorizationList{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, z.issuer+listAuthorizations, bytes.NewReader(body))
	if err != nil {
		return authorizationList{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := z.api.Do(req)
	if err != nil {
		return authorizationList{}, fmt.Errorf("listing authorizations: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusUnauthorized {
		return authorizationList{}, errTokenRefused
	}
	if res.StatusCode != http.StatusOK {
		return authorizationList{}, fmt.Errorf("listing authorizations: HTTP %d", res.StatusCode)
	}
	var list authorizationList
	if err := json.NewDecoder(io.LimitReader(res.Body, maxAnswer)).Decode(&list); err != nil {
		return authorizationList{}, fmt.Errorf("listing authorizations: its answer: %w", err)
	}
	if list.Pagination == nil {
		return authorizationList{}, errors.New("listing authorizations: its answer is no list")
	}
	return list, nil
}

// accessToken returns an access token of the service user, kept until
// shortly before it expires. refused, if not empty, is a token Zitadel has
// just refused, which it will not return.
func (z *Zitadel) accessToken(ctx context.Context, refused string) (string, error) {
	if err := z.tokenLock.acquire(ctx); err != nil {
		return "", fmt.Errorf("getting an access token: %w", err)
	}
	defer z.tokenLock.release()
	if z.token != "" && z.token != refused && z.now().Add(tokenMargin).Before(z.tokenExpires) {
		return z.token, nil
	}
	provider, err := z.discover(ctx)
	if err != nil {
		return "", err
	}
	assertion, err := z.assertion()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {jwtProfileGrant}, "assertion": {assertion}, "scope": {serviceScopes}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.Endpoint().TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := z.api.Do(req)
	if err != nil {
		return "", fmt.Errorf("getting an access token: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	decoded := json.NewDecoder(io.LimitReader(res.Body, maxAnswer)).Decode(&answer)
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("getting an access token: HTTP %d %s", res.StatusCode, answer.Error)
	}
	if decoded != nil || answer.AccessToken == "" {
		return "", errors.New("getting an access token: Zitadel answered with none")
	}
	z.token, z.tokenExpires = answer.AccessToken, z.now().Add(time.Duration(answer.ExpiresIn)*time.Second)
	return z.token, nil
}

// assertion signs the service user's JWT-profile assertion: RS256, with the
// key named by kid, the user as iss and sub, and the issuer as aud.
func (z *Zitadel) assertion() (string, error) {
	now := z.now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": z.serviceUser.KeyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": z.serviceUser.ID,
		"sub": z.serviceUser.ID,
		"aud": z.issuer,
		"iat": now.Add(-assertionBackdate).Unix(),
		"exp": now.Add(assertionLifetime).Unix(),
	})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(nil, z.serviceUser.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
