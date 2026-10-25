// Package site holds the Sites the server is configured for and tells which
// one a request is for.
package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// Site is one Astro landing page the server edits, as listed in the Sites file.
type Site struct {
	ID          string     `json:"id"`
	Hostnames   []string   `json:"hostnames"`
	GitHub      Repository `json:"githubRepository"`
	Locale      string     `json:"locale"`
	ZitadelRole string     `json:"zitadelRole"`
	Kubernetes  Kubernetes `json:"kubernetes"`
}

// Repository is a GitHub repository, written "owner/name" in the Sites file.
type Repository struct {
	Owner string
	Name  string
}

// UnmarshalText reads "owner/name".
func (r *Repository) UnmarshalText(text []byte) error {
	owner, name, ok := strings.Cut(string(text), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("githubRepository: %q is not owner/name", text)
	}
	r.Owner, r.Name = owner, name
	return nil
}

// Kubernetes is where the Site's Releases run.
type Kubernetes struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
}

// Registry is the list of Sites, indexed by hostname.
type Registry struct {
	byHost map[string]Site
}

// Parse reads a Sites file and checks that every Site in it is complete and
// has its own id and hostnames.
func Parse(r io.Reader) (*Registry, error) {
	var file struct {
		Sites []Site `json:"sites"`
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("reading the Sites file: %w", err)
	}
	if len(file.Sites) == 0 {
		return nil, errors.New("the Sites file lists no Sites")
	}

	reg := &Registry{byHost: map[string]Site{}}
	ids := map[string]bool{}
	var errs []error
	for i, s := range file.Sites {
		if problems := s.problems(); len(problems) > 0 {
			name := fmt.Sprintf("site %q", s.ID)
			if s.ID == "" {
				name = fmt.Sprintf("site %d", i+1)
			}
			for _, p := range problems {
				errs = append(errs, fmt.Errorf("%s: %s", name, p))
			}
			continue
		}
		if ids[s.ID] {
			errs = append(errs, fmt.Errorf("two Sites have the id %q", s.ID))
		}
		ids[s.ID] = true
		for _, h := range s.Hostnames {
			h = canonicalHost(h)
			if _, taken := reg.byHost[h]; taken {
				errs = append(errs, fmt.Errorf("two Sites have the hostname %q", h))
			}
			reg.byHost[h] = s
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid Sites file: %w", err)
	}
	return reg, nil
}

// hostname is a DNS name with no scheme, port or path.
var hostname = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.?$`)

func (s Site) problems() []string {
	var problems []string
	required := func(field, value string) {
		if value == "" {
			problems = append(problems, field+" is required")
		}
	}
	required("id", s.ID)
	if len(s.Hostnames) == 0 {
		problems = append(problems, "hostnames: at least one is required")
	}
	for _, h := range s.Hostnames {
		if !hostname.MatchString(h) {
			problems = append(problems, fmt.Sprintf("hostnames: %q is not a bare hostname", h))
		}
	}
	required("githubRepository", s.GitHub.Owner)
	required("locale", s.Locale)
	required("zitadelRole", s.ZitadelRole)
	required("kubernetes.namespace", s.Kubernetes.Namespace)
	required("kubernetes.deployment", s.Kubernetes.Deployment)
	return problems
}

// Lookup returns the Site served at host, the value of a request's Host
// header.
func (reg *Registry) Lookup(host string) (Site, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	s, ok := reg.byHost[canonicalHost(host)]
	return s, ok
}

// canonicalHost ignores case and the trailing dot of a fully qualified name.
func canonicalHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// Require passes on only the requests for a known Site, with that Site in
// their context, and answers 404 to the rest.
func (reg *Registry) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := reg.Lookup(r.Host)
		if !ok {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, s)))
	})
}

type contextKey struct{}

// FromContext returns the Site of a request that went through Require.
func FromContext(ctx context.Context) (Site, bool) {
	s, ok := ctx.Value(contextKey{}).(Site)
	return s, ok
}
