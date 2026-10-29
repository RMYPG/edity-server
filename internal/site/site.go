// Package site holds the Sites the server is configured for and tells which
// one a request is for.
package site

import (
	"bytes"
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

// Site is one Astro landing page the server works for, as listed in the Sites
// file.
type Site struct {
	ID          string     `json:"id"`
	Hostnames   []string   `json:"hostnames"`
	Repository  Repository `json:"githubRepository"`
	Locale      string     `json:"locale"`
	ZitadelRole string     `json:"zitadelRole"`
	Kubernetes  Kubernetes `json:"kubernetes"`
}

// Repository is a GitHub repository, written "owner/name" in the Sites file.
type Repository struct {
	Owner string
	Name  string
}

// GitHub's rules for account and repository names.
var (
	githubOwner = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubName  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// UnmarshalText reads "owner/name".
func (r *Repository) UnmarshalText(text []byte) error {
	owner, name, ok := strings.Cut(string(text), "/")
	if !ok || !githubOwner.MatchString(owner) || !githubName.MatchString(name) || name == "." || name == ".." {
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
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading the Sites file: %w", err)
	}
	if err := noRepeatedKeys(data); err != nil {
		return nil, fmt.Errorf("reading the Sites file: %w", err)
	}
	var file struct {
		Sites []Site `json:"sites"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("reading the Sites file: %w", err)
	}
	// Decode reads one JSON value and leaves the rest, which would otherwise
	// be ignored without a word.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("reading the Sites file: it holds more than one JSON value")
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

// noRepeatedKeys refuses an object that has the same key twice. Decoding
// keeps the last one without a word, and matches keys whatever their case,
// so "locale" and "Locale" count as the same key.
func noRepeatedKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var value func(path string) error
	value = func(path string) error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				token, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := token.(string)
				at := key
				if path != "" {
					at = path + "." + key
				}
				if seen[strings.ToLower(key)] {
					return fmt.Errorf("%q appears twice", at)
				}
				seen[strings.ToLower(key)] = true
				if err := value(at); err != nil {
					return err
				}
			}
		case json.Delim('['):
			for i := 0; dec.More(); i++ {
				if err := value(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		default:
			return nil
		}
		_, err = dec.Token() // the closing } or ]
		return err
	}
	return value("")
}

var (
	// hostname is a DNS name with no scheme, port or path.
	hostname = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.?$`)
	// languageTag is a BCP 47 language, with an optional script and region,
	// such as es, es-ES or zh-Hant-TW.
	languageTag = regexp.MustCompile(`^(?i)[a-z]{2,3}(-[a-z]{4})?(-([a-z]{2}|[0-9]{3}))?$`)
	noSpaces    = regexp.MustCompile(`^\S+$`)
	// Kubernetes names a namespace with a DNS label, and a Deployment with a
	// DNS subdomain (RFC 1123).
	kubernetesNamespace  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	kubernetesObjectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

func (s Site) problems() []string {
	var problems []string
	required := func(key, value string) bool {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, key+" is required")
			return false
		}
		return true
	}
	matches := func(key, value string, format *regexp.Regexp, isNot string) {
		if required(key, value) && !format.MatchString(value) {
			problems = append(problems, fmt.Sprintf("%s: %q %s", key, value, isNot))
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
	required("githubRepository", s.Repository.Owner)
	matches("locale", s.Locale, languageTag, "is not a language tag, such as es or es-ES")
	matches("zitadelRole", s.ZitadelRole, noSpaces, "has spaces")
	matches("kubernetes.namespace", s.Kubernetes.Namespace, kubernetesNamespace,
		"is not a Kubernetes namespace name: lowercase letters, digits and -, at most 63")
	if len(s.Kubernetes.Deployment) > 253 {
		problems = append(problems, "kubernetes.deployment: longer than 253 characters")
	} else {
		matches("kubernetes.deployment", s.Kubernetes.Deployment, kubernetesObjectName,
			"is not a Kubernetes object name: lowercase letters, digits, - and .")
	}
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

// HasHostname tells whether host, a hostname with no port, is one of the
// Site's, whatever its case.
func (s Site) HasHostname(host string) bool {
	for _, h := range s.Hostnames {
		if canonicalHost(h) == canonicalHost(host) {
			return true
		}
	}
	return false
}

// IsLoopback tells whether host, a hostname with no port, names the machine
// it is on: localhost or a name under it (RFC 6761), or a loopback address.
func IsLoopback(host string) bool {
	host = canonicalHost(host)
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
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
