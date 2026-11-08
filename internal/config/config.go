// Package config reads the server's configuration from its environment:
//
//   - EDITY_ADDR, the address to listen on, ":8080" by default
//   - EDITY_SITES_FILE, the path of the Sites file
//   - EDITY_PUBLISHER_GITHUB_APP_ID and EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY,
//     the edity-publisher GitHub App's ID and PEM private key
//   - EDITY_ZITADEL_ISSUER and EDITY_ZITADEL_CLIENT_ID, the Zitadel instance
//     Clients log in through and the server's OIDC client on it
//   - EDITY_SESSION_KEY, the key that seals Clients' sessions, 32 bytes in
//     base64
package config

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/RMYPG/edity-server/internal/site"
)

// Config is what the server needs to start.
type Config struct {
	// Addr is the address the server listens on.
	Addr  string
	Sites *site.Registry
	// Publisher is the edity-publisher GitHub App, which commits each Publish.
	Publisher GitHubApp
	// Zitadel is where Clients log in.
	Zitadel Zitadel
	// SessionKey seals Clients' sessions. Restarting the server with the same
	// key keeps them.
	SessionKey []byte
}

// Zitadel is the server's OIDC client on a Zitadel instance.
type Zitadel struct {
	// Issuer is the instance's URL.
	Issuer   string
	ClientID string
}

// GitHubApp holds the credentials of a GitHub App.
type GitHubApp struct {
	ID         int64
	PrivateKey *rsa.PrivateKey
}

// Load reads the configuration from the environment variables that getenv
// returns, and reports every missing or invalid one at once.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{Addr: getenv("EDITY_ADDR")}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}

	var errs []error
	read := func(name string, parse func(string) error) {
		value := getenv(name)
		if value == "" {
			errs = append(errs, fmt.Errorf("%s: not set", name))
		} else if err := parse(value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	read("EDITY_SITES_FILE", func(path string) (err error) {
		cfg.Sites, err = readSites(path)
		return err
	})
	read("EDITY_PUBLISHER_GITHUB_APP_ID", func(id string) (err error) {
		cfg.Publisher.ID, err = parseAppID(id)
		return err
	})
	read("EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY", func(key string) (err error) {
		cfg.Publisher.PrivateKey, err = parsePrivateKey(key)
		return err
	})
	read("EDITY_ZITADEL_ISSUER", func(issuer string) error {
		if !isIssuer(issuer) {
			return errors.New("not an https URL")
		}
		// OIDC compares the issuer exactly, and Zitadel's has no trailing
		// slash, so every login would fail.
		if strings.HasSuffix(issuer, "/") {
			return errors.New("ends with a slash, which Zitadel's issuer does not")
		}
		cfg.Zitadel.Issuer = issuer
		return nil
	})
	read("EDITY_ZITADEL_CLIENT_ID", func(id string) error {
		cfg.Zitadel.ClientID = id
		return nil
	})
	read("EDITY_SESSION_KEY", func(key string) (err error) {
		cfg.SessionKey, err = parseSessionKey(key)
		return err
	})

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readSites(path string) (*site.Registry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The Developer chooses the path.
	if err != nil {
		return nil, err
	}
	return site.Parse(bytes.NewReader(data))
}

// parseAppID reads a GitHub App ID. Its error never quotes the value, which
// is the private key if the two secrets were swapped.
func parseAppID(text string) (int64, error) {
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("not a GitHub App ID")
	}
	return id, nil
}

// isIssuer tells whether text is the URL of an OIDC issuer the server can
// trust: over https, or over http only on loopback, as in development.
func isIssuer(text string) bool {
	u, err := url.Parse(text)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && site.IsLoopback(u.Hostname())
}

// parseSessionKey reads the 32 bytes of an AES-256 key in base64. Its error
// never quotes the key.
func parseSessionKey(text string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(key) != 32 {
		return nil, errors.New("not 32 bytes in base64")
	}
	return key, nil
}

// parsePrivateKey reads a PEM RSA key in the PKCS #1 form GitHub hands out,
// or in PKCS #8. Its errors never quote the key.
func parsePrivateKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := key.(*rsa.PrivateKey); ok {
			return key, nil
		}
	}
	return nil, errors.New("not an RSA private key")
}
